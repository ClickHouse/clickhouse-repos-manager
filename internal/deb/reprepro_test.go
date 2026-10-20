//go:build reprepro

package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/deb/debtest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/sign/signtest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

// The Ubuntu releases whose apt must read both repositories alike.
var ubuntus = []string{"14.04", "16.04", "18.04", "20.04", "22.04", "24.04", "26.04"}

// TestReprepro publishes an lts and a stable release through ch-deploy and
// through reprepro 5.5.1 as the release job ran it, compares the parsed
// repositories, then has apt on every Ubuntu release read, show and install
// from both. make test-reprepro builds the image and runs it.
func TestReprepro(t *testing.T) {
	image := os.Getenv("CH_DEPLOY_REPREPRO_IMAGE")
	if image == "" {
		t.Fatal("CH_DEPLOY_REPREPRO_IMAGE is not set: run make test-reprepro")
	}
	vLTS, vStable := mustVersion(t, reqA.Tag), mustVersion(t, reqB.Tag)
	lts, stable := matchFiles(t, vLTS), matchFiles(t, vStable)

	ctx, m := context.Background(), storetest.NewMem()
	r := &release.Releaser{Bucket: m, Types: []release.PackageType{debType(t)}, Version: "test", Now: func() time.Time { return clock }}
	mustLock(t, r, reqA)
	if _, err := r.Publish(ctx, lts, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Unlock(ctx, reqA.Tag); err != nil {
		t.Fatal(err)
	}
	mustLock(t, r, reqB)
	if _, err := r.Publish(ctx, stable, nil); err != nil {
		t.Fatal(err)
	}
	ours := map[string][]byte{}
	if err := m.List(ctx, "deb/", func(o store.Object) error {
		ours[strings.TrimPrefix(o.Key, "deb/")] = get(t, m, o.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	theirs := runReprepro(t, image, vLTS, lts, stable)

	t.Run("files", func(t *testing.T) { compareRepos(t, ours, theirs) })
	t.Run("apt", func(t *testing.T) { aptMatrix(t, image, ours, theirs, vLTS, vStable) })
}

func mustVersion(t *testing.T, tag string) string {
	t.Helper()
	tg, err := release.ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	return tg.Version()
}

// matchFiles writes clickhouse-client for amd64 and arm64, with Source,
// a Depends on the docs of its version and a field no tool knows, and
// clickhouse-docs for all, without Source.
func matchFiles(t *testing.T, v string) []string {
	t.Helper()
	dir := t.TempDir()
	write := func(pkg, arch, source string, extra ...string) string {
		p := filepath.Join(dir, pkg+"_"+v+"_"+arch+".deb")
		body := debtest.Build(debtest.Control(pkg, v, arch, source, extra...), []byte(v+pkg+arch))
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var files []string
	for _, arch := range []string{"amd64", "arm64"} {
		files = append(files, write("clickhouse-client", arch, "clickhouse",
			"Depends: clickhouse-docs (= "+v+")", "X-ClickHouse-Test: kept as it is"))
	}
	return append(files, write("clickhouse-docs", "all", ""))
}

// runReprepro runs the release job commands in the image: includedeb lts and
// copy stable lts for the lts release, includedeb stable for the stable one,
// signed with the test key, and returns the files under deb/.
func runReprepro(t *testing.T, image, vLTS string, lts, stable []string) map[string][]byte {
	t.Helper()
	key := signtest.New(t)
	var conf strings.Builder
	for _, cn := range config.Codenames {
		fmt.Fprintf(&conf, "Origin: %s\nLabel: %s\nCodename: %s\nArchitectures: %s\nComponents: %s\nLimit: -1\nSignWith: %s\n\n",
			config.Origin, config.Label, cn, strings.Join(config.Architectures, " "), config.Component, key.SignWith)
	}
	in := map[string][]byte{"conf/distributions": []byte(conf.String()), "key": key.Secret}
	for dir, files := range map[string][]string{"lts": lts, "stable": stable} {
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			in[dir+"/"+filepath.Base(f)] = body
		}
	}
	script := `set -eu
export GNUPGHOME=/tmp/gnupg
mkdir -m 700 $GNUPGHOME /in
tar x -C /in
gpg --batch --quiet --import /in/key >&2
R="reprepro --ignore=unknownfield --basedir /in --outdir /out"
$R includedeb lts /in/lts/*.deb >&2
$R copy stable lts clickhouse-client=` + vLTS + ` clickhouse-docs=` + vLTS + ` >&2
$R includedeb stable /in/stable/*.deb >&2
tar c -C /out dists pool`
	out, stderr, err := docker(tarOf(in), "run", "--rm", "-i", image, "sh", "-c", script)
	if err != nil {
		t.Fatalf("reprepro: %v\n%s", err, stderr)
	}
	return untar(t, out)
}

// compareRepos compares the pool, which packages each index holds and their
// fields, and the Release files, apart from what ch-deploy adds on purpose:
// Acquire-By-Hash with the by-hash files, Packages.xz and Public.key.
func compareRepos(t *testing.T, ours, theirs map[string][]byte) {
	pool := func(files map[string][]byte) map[string][]byte {
		p := map[string][]byte{}
		for k, v := range files {
			if strings.HasPrefix(k, "pool/") {
				p[k] = v
			}
		}
		return p
	}
	if o, th := pool(ours), pool(theirs); !reflect.DeepEqual(o, th) {
		t.Errorf("pool: ours %v, reprepro %v", sortedKeys(o), sortedKeys(th))
	}
	for _, cn := range config.Codenames {
		dists := "dists/" + cn + "/"
		o, err := ParseRelease(ours[dists+"Release"])
		if err != nil {
			t.Fatalf("%s ours: %v", cn, err)
		}
		th, err := ParseRelease(theirs[dists+"Release"])
		if err != nil {
			t.Fatalf("%s reprepro: %v", cn, err)
		}
		header := func(r Release) map[string]string {
			h := fields(r.Paragraph)
			for _, name := range []string{"Date", "MD5Sum", "SHA1", "SHA256", "SHA512"} {
				delete(h, name)
			}
			return h
		}
		want := header(th)
		want["Acquire-By-Hash"] = "yes"
		if got := header(o); !reflect.DeepEqual(got, want) {
			t.Errorf("%s Release header: ours %v, reprepro plus Acquire-By-Hash %v", cn, got, want)
		}
		wantPaths := append([]string(nil), th.Paths...)
		for _, arch := range config.Architectures {
			wantPaths = append(wantPaths, "main/binary-"+arch+"/Packages.xz")
		}
		sort.Strings(wantPaths)
		gotPaths := append([]string(nil), o.Paths...)
		sort.Strings(gotPaths)
		if !reflect.DeepEqual(gotPaths, wantPaths) {
			t.Errorf("%s Release lists: ours %v, reprepro plus Packages.xz %v", cn, gotPaths, wantPaths)
		}
		for _, p := range th.Paths {
			a, b := ours[dists+p], theirs[dists+p]
			switch path.Base(p) {
			case "Packages.gz":
				a, b = gunzip(t, a), gunzip(t, b)
				fallthrough
			case "Packages":
				compareIndex(t, cn+" "+p, a, b)
			case "Release":
				pa, errA := ParseParagraph(a)
				pb, errB := ParseParagraph(b)
				if errA != nil || errB != nil || !reflect.DeepEqual(fields(pa), fields(pb)) {
					t.Errorf("%s %s: ours %q, reprepro %q", cn, p, a, b)
				}
			default:
				t.Errorf("%s: reprepro lists %s", cn, p)
			}
		}
	}
}

// compareIndex compares two Packages files as sets of entries with their
// fields; order is not compared.
func compareIndex(t *testing.T, name string, ours, theirs []byte) {
	t.Helper()
	entries := func(b []byte) map[Key]map[string]string {
		x, err := ParseIndex(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		e := map[Key]map[string]string{}
		for _, p := range x.Stanzas() {
			e[KeyOf(p)] = fields(p)
		}
		return e
	}
	o, th := entries(ours), entries(theirs)
	if len(o) == 0 {
		t.Errorf("%s: empty", name)
	}
	for k, f := range th {
		if !reflect.DeepEqual(o[k], f) {
			t.Errorf("%s %s:\nours     %v\nreprepro %v", name, k, o[k], f)
		}
	}
	for k := range o {
		if th[k] == nil {
			t.Errorf("%s: only ours has %s", name, k)
		}
	}
}

func fields(p Paragraph) map[string]string {
	f := map[string]string{}
	for _, name := range p.Fields() {
		f[name] = p.Value(name)
	}
	return f
}

// The apt client script: for each repository and codename, update, the
// candidates, the records, an install of the newest client with the docs it
// depends on, and in stable an install of the older version.
const aptScript = `set -eu
export DEBIAN_FRONTEND=noninteractive
echo "$KEYRING" | base64 -d > /etc/apt/trusted.gpg.d/ch-deploy-test.gpg
rm -f /etc/apt/sources.list.d/*
run() { "$@" > /tmp/log 2>&1 || { cat /tmp/log; exit 1; }; }
for repo in ours theirs; do
  for cn in lts stable; do
    echo "deb http://repo/$repo/deb $cn main" > /etc/apt/sources.list
    rm -rf /var/lib/apt/lists/*
    echo "== $repo $cn update"; apt-get update 2>&1
    echo "== $repo $cn madison"; apt-cache madison clickhouse-client clickhouse-docs
    echo "== $repo $cn show"; apt-cache show clickhouse-client clickhouse-docs
    run apt-get install -y clickhouse-client
    echo "== $repo $cn installed"; dpkg-query -W clickhouse-client clickhouse-docs
    run apt-get purge -y clickhouse-client clickhouse-docs
    if [ $cn = stable ]; then
      run apt-get install -y clickhouse-client=$OLD clickhouse-docs=$OLD
      echo "== $repo $cn pinned"; dpkg-query -W clickhouse-client clickhouse-docs
      run apt-get purge -y clickhouse-client clickhouse-docs
    fi
  done
done
echo "== done"`

// aptMatrix serves both repositories from one container and runs aptScript
// on every Ubuntu release, natively, on a private network.
func aptMatrix(t *testing.T, image string, ours, theirs map[string][]byte, vLTS, vStable string) {
	id := fmt.Sprintf("ch-deploy-apt-%d", time.Now().UnixNano())
	mustDocker(t, nil, "network", "create", id)
	t.Cleanup(func() { _, _, _ = docker(nil, "network", "rm", id) })
	mustDocker(t, nil, "create", "--name", id, "--network", id, "--network-alias", "repo", image,
		"python3", "-m", "http.server", "-d", "/srv", "80")
	t.Cleanup(func() { _, _, _ = docker(nil, "rm", "-f", id) })
	srv := map[string][]byte{}
	for k, v := range ours {
		srv["srv/ours/deb/"+k] = v
	}
	for k, v := range theirs {
		srv["srv/theirs/deb/"+k] = v
	}
	mustDocker(t, tarOf(srv), "cp", "-", id+":/")
	mustDocker(t, nil, "start", id)
	ready := false
	for i := 0; i < 50 && !ready; i++ {
		_, _, err := docker(nil, "exec", id, "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost/ours/deb/dists/lts/Release')")
		ready = err == nil
		if !ready {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatal("the repository server did not start")
	}
	keyring := dearmor(t, signtest.New(t).Public)

	for _, u := range ubuntus {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			out, stderr, err := docker(nil, "run", "--rm", "--network", id,
				"-e", "KEYRING="+base64.StdEncoding.EncodeToString(keyring), "-e", "OLD="+vLTS,
				"ubuntu:"+u, "sh", "-c", aptScript)
			if err != nil {
				t.Fatalf("%v\n%s\n%s", err, out, stderr)
			}
			checkApt(t, sections(string(out)), vLTS, vStable)
		})
	}
}

// sections splits the script output at its "== <repo> <codename> <step>" lines.
func sections(out string) map[string]string {
	s, name := map[string]string{}, ""
	for _, line := range strings.SplitAfter(out, "\n") {
		if strings.HasPrefix(line, "== ") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "== "))
			s[name] = ""
			continue
		}
		s[name] += line
	}
	return s
}

func checkApt(t *testing.T, s map[string]string, vLTS, vStable string) {
	t.Helper()
	if _, ok := s["done"]; !ok {
		t.Fatalf("the script did not finish:\n%v", s)
	}
	for _, repo := range []string{"ours", "theirs"} {
		for _, cn := range config.Codenames {
			for _, line := range strings.Split(s[repo+" "+cn+" update"], "\n") {
				if strings.HasPrefix(line, "W:") || strings.HasPrefix(line, "E:") || strings.HasPrefix(line, "Err") {
					t.Errorf("%s %s update: %s", repo, cn, line)
				}
			}
		}
	}
	want := map[string]string{
		"lts installed":    fmt.Sprintf("clickhouse-client\t%s\nclickhouse-docs\t%s\n", vLTS, vLTS),
		"stable installed": fmt.Sprintf("clickhouse-client\t%s\nclickhouse-docs\t%s\n", vStable, vStable),
		"stable pinned":    fmt.Sprintf("clickhouse-client\t%s\nclickhouse-docs\t%s\n", vLTS, vLTS),
	}
	for step, w := range want {
		if got := s["ours "+step]; got != w {
			t.Errorf("ours %s: %q, want %q", step, got, w)
		}
	}
	norm := map[string]func(string) string{
		"madison":   func(b string) string { return strings.ReplaceAll(b, "/ours/", "/REPO/") },
		"show":      func(b string) string { return canonicalRecords(t, b) },
		"installed": func(b string) string { return b },
		"pinned":    func(b string) string { return b },
	}
	for _, cn := range config.Codenames {
		versions := strings.Count(s["ours "+cn+" madison"], "\n")
		if wantN := map[string]int{"lts": 2, "stable": 4}[cn]; versions != wantN {
			t.Errorf("ours %s madison: %d lines, want %d:\n%s", cn, versions, wantN, s["ours "+cn+" madison"])
		}
		for step, f := range norm {
			if step == "pinned" && cn != "stable" {
				continue
			}
			o, th := s["ours "+cn+" "+step], s["theirs "+cn+" "+step]
			if step == "madison" {
				th = strings.ReplaceAll(th, "/theirs/", "/REPO/")
			}
			if f(o) != f(th) {
				t.Errorf("%s %s differs:\nours:\n%s\nreprepro:\n%s", cn, step, o, th)
			}
		}
	}
}

// canonicalRecords turns apt-cache show output into sorted records of sorted
// fields.
func canonicalRecords(t *testing.T, out string) string {
	var recs []string
	for _, rec := range strings.Split(strings.TrimSpace(out), "\n\n") {
		p, err := ParseParagraph([]byte(strings.TrimSpace(rec) + "\n"))
		if err != nil {
			t.Errorf("apt-cache show: %v\n%s", err, rec)
			continue
		}
		var lines []string
		for name, v := range fields(p) {
			lines = append(lines, name+": "+v)
		}
		sort.Strings(lines)
		recs = append(recs, strings.Join(lines, "\n"))
	}
	sort.Strings(recs)
	return strings.Join(recs, "\n\n")
}

func dearmor(t *testing.T, b []byte) []byte {
	t.Helper()
	block, err := armor.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(block.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	// reprepro 5.5.1 pads Packages.gz with zeros, which are no next stream.
	zr.Multistream(false)
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func docker(stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command("docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func mustDocker(t *testing.T, stdin []byte, args ...string) {
	t.Helper()
	if _, stderr, err := docker(stdin, args...); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
}

// tarOf builds a tar of files with their parent directories.
func tarOf(files map[string][]byte) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	dirs := map[string]bool{}
	for _, name := range sortedKeys(files) {
		for d := path.Dir(name); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var dl []string
	for d := range dirs {
		dl = append(dl, d)
	}
	sort.Strings(dl)
	for _, d := range dl {
		if err := tw.WriteHeader(&tar.Header{Name: d + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			panic(err)
		}
	}
	for _, name := range sortedKeys(files) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			panic(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// untar returns the regular files of a tar.
func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[path.Clean(h.Name)] = body
	}
}
