package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/deb/debtest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/sign/signtest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: nil, code: 2, stderr: "Usage: ch-deploy"},
		{args: []string{"help"}, code: 0, stderr: "check"},
		{args: []string{"nope"}, code: 2, stderr: `unknown command "nope"`},
		{args: []string{"version"}, code: 0, stdout: "ch-deploy dev"},
		{args: []string{"version", "extra"}, code: 2, stdout: "unexpected arguments"},
		{args: []string{"check", "--bad-flag"}, code: 2, stdout: "flag provided but not defined"},
		{args: []string{"check", "-h"}, code: 0, stdout: "-sign-with"},
		{args: []string{"help"}, code: 0, stderr: "lock break"},
		{args: []string{"lock", "-h"}, code: 0, stdout: "-run-url"},
		{args: []string{"lock"}, code: 2, stdout: "--tag and --date are required"},
		{args: []string{"lock", "--tag", "v1.2.3.4-lts"}, code: 2, stdout: "--tag and --date are required"},
		{args: []string{"lock", "--tag", "v1.2.3.4-lts", "--date", "2026-09-29T12:29:02Z"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"lock", "extra"}, code: 2, stdout: "unexpected arguments"},
		{args: []string{"lock", "status", "-h"}, code: 0, stdout: "-endpoint"},
		{args: []string{"lock", "status"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"lock", "break"}, code: 2, stdout: "--tag is required"},
		{args: []string{"lock", "break", "-h"}, code: 0, stdout: "-force"},
		{args: []string{"help"}, code: 0, stderr: "rollback"},
		{args: []string{"unlock"}, code: 2, stdout: "--tag is required"},
		{args: []string{"unlock", "-h"}, code: 0, stdout: "-tag"},
		{args: []string{"rollback", "--tag", "v1.2.3.4-lts"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"publish"}, code: 2, stdout: "no files given"},
		{args: []string{"publish", "-h"}, code: 0, stdout: "-only-tgz"},
		{args: []string{"publish", "-h"}, code: 0, stdout: "-upload-concurrency"},
		{args: []string{"publish", "--only-tgz", "a.tgz"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"publish", "--part-size", "big", "a.tgz"}, code: 1, stderr: "want a positive number"},
		{args: []string{"publish", "-h"}, code: 0, stdout: "-dry-run"},
		{args: []string{"publish", "--dry-run", "a.tgz"}, code: 2, stdout: "--dry-run needs --tag and --out"},
		{args: []string{"publish", "--dry-run", "--tag", "v1.2.3.4-lts", "a.tgz"}, code: 2, stdout: "--dry-run needs --tag and --out"},
		{args: []string{"publish", "--tag", "v1.2.3.4-lts", "a.tgz"}, code: 2, stdout: "only a dry run takes them"},
		{args: []string{"publish", "--dry-run", "--tag", "v1.2.3.4-lts", "--out", "o", "a.tgz"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"verify"}, code: 2, stdout: "Usage: ch-deploy verify deb"},
		{args: []string{"verify", "rpm"}, code: 1, stderr: "package type not in this build"},
		{args: []string{"verify", "deb", "-h"}, code: 0, stdout: "-channel"},
		{args: []string{"verify", "deb", "-h"}, code: 0, stdout: "-public-key-file"},
		{args: []string{"verify", "deb"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"help"}, code: 0, stderr: "verify"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("CH_DEPLOY_ENDPOINT", "")
			t.Setenv("CH_DEPLOY_BUCKET", "")
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, &stdout, &stderr)
			}
			if !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("stdout %q (want %q), stderr %q (want %q)", &stdout, tc.stdout, &stderr, tc.stderr)
			}
		})
	}
}

func TestLockCommands(t *testing.T) {
	env, prefix := storetest.NewPrefix(t)
	t.Setenv("CH_DEPLOY_ENDPOINT", env.Endpoint)
	t.Setenv("CH_DEPLOY_BUCKET", env.Bucket)
	t.Setenv("CH_DEPLOY_PREFIX", prefix)
	sh := func(code int, want string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got != code || !strings.Contains(stdout.String()+stderr.String(), want) {
			t.Fatalf("%q: exit %d (want %d), want %q\nstdout: %s\nstderr: %s", args, got, code, want, &stdout, &stderr)
		}
	}
	sh(0, "no lock (not adopted)\n", "lock", "status")
	sh(1, "no lock (not adopted)", "lock", "break", "--tag", "v26.3.36.6-lts")
	sh(0, `"state": "locked"`, "lock", "--tag", "v26.3.36.6-lts", "--date", "2026-09-29T12:29:02Z", "--run-id", "42", "--run-url", "https://ci/42")
	sh(0, `"run_url": "https://ci/42"`, "lock", "status")
	sh(0, "\netag: ", "lock", "status")
	sh(1, "held by another release", "lock", "--tag", "v26.7.19.5-stable", "--date", "2026-10-01T08:00:00Z")
	sh(1, `tag "v1-lts"`, "lock", "--tag", "v1-lts", "--date", "2026-10-01T08:00:00Z")
	sh(0, `"state": "free"`, "lock", "break", "--tag", "v26.3.36.6-lts")
	sh(1, "the lock is free", "lock", "break", "--tag", "v26.3.36.6-lts")
	sh(0, `"previous": "00000000T000000Z-adopted"`, "lock", "--tag", "v26.7.19.5-stable", "--date", "2026-10-01T08:00:00Z")
	sh(1, "unlock needs state committed, the state is locked", "unlock", "--tag", "v26.7.19.5-stable")
	sh(1, "held by v26.7.19.5-stable, not v26.3.36.6-lts", "rollback", "--tag", "v26.3.36.6-lts")
	sh(0, `"state": "free"`, "rollback", "--tag", "v26.7.19.5-stable")
	sh(1, "the lock is free", "unlock", "--tag", "v26.7.19.5-stable")
}

// writeRelease writes a .deb, a .tgz and its .tgz.sha512 for two packages on
// two architectures; with big one archive is above the 5 MiB part size.
func writeRelease(t *testing.T, version string, big bool) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for _, pkg := range []string{"clickhouse-client", "clickhouse-server"} {
		for _, arch := range []string{"amd64", "arm64"} {
			paths = append(paths, debtest.Write(t, dir, pkg, version, arch, "clickhouse", version+pkg+arch))
			name := pkg + "-" + version + "-" + arch + ".tgz"
			body := make([]byte, 1000)
			if big && len(paths) == 1 {
				body = make([]byte, store.MinPartSize+12345)
			}
			if _, err := rand.Read(body); err != nil {
				t.Fatal(err)
			}
			sum := sha512.Sum512(body)
			for file, content := range map[string][]byte{
				name:             body,
				name + ".sha512": []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"),
			} {
				path := filepath.Join(dir, file)
				if err := os.WriteFile(path, content, 0o644); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// publicURL is CH_DEPLOY_TEST_PUBLIC_URL, the public URL of the test bucket,
// or a local server that answers HEAD and GET from the bucket as a public URL does.
func publicURL(t *testing.T, env storetest.Env) string {
	if u := os.Getenv("CH_DEPLOY_TEST_PUBLIC_URL"); u != "" {
		return u
	}
	root, err := env.Store("", false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/")
		if req.Method == http.MethodGet {
			body, _, err := root.Get(req.Context(), key)
			if err != nil {
				http.NotFound(w, req)
				return
			}
			_, _ = w.Write(body)
			return
		}
		obj, err := root.Head(req.Context(), key)
		if err != nil || req.Method != http.MethodHead {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The exit criteria of M1d and M2: a DEB and TGZ release with lock, publish,
// verify and unlock, then a dry run of a second one, which is then published
// and rolled back, through the command line.
func TestPublishCommands(t *testing.T) {
	env, prefix := storetest.NewPrefix(t)
	key := signtest.New(t)
	t.Setenv("CH_DEPLOY_SIGNING_KEY", base64.StdEncoding.EncodeToString(key.Secret))
	t.Setenv("CH_DEPLOY_SIGNING_KEY_FILE", "")
	t.Setenv("CH_DEPLOY_SIGN_WITH", key.SignWith)
	t.Setenv("CH_DEPLOY_ENDPOINT", env.Endpoint)
	t.Setenv("CH_DEPLOY_BUCKET", env.Bucket)
	t.Setenv("CH_DEPLOY_PREFIX", prefix)
	t.Setenv("CH_DEPLOY_PART_SIZE", strconv.Itoa(store.MinPartSize))
	t.Setenv("CH_DEPLOY_UPLOAD_CONCURRENCY", "3")
	t.Setenv("CH_DEPLOY_PUBLIC_URL", publicURL(t, env))
	testStoreConfig = func(c *store.Config) { c.NoResumeUploads = storetest.Backend() == "miniflare" }
	t.Cleanup(func() { testStoreConfig = nil })
	sh := func(code int, want string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got != code || !strings.Contains(stdout.String()+stderr.String(), want) {
			t.Fatalf("%q: exit %d (want %d), want %q\nstdout: %s\nstderr: %s", args, got, code, want, &stdout, &stderr)
		}
	}
	s, err := env.Store(prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	head := func(key string) (store.Object, error) { return s.Head(context.Background(), key) }

	first := writeRelease(t, "26.3.36.6", true)
	sh(1, "no lock (not adopted)", append([]string{"publish"}, first...)...)
	sh(0, `"state": "locked"`, "lock", "--tag", "v26.3.36.6-lts", "--date", "2026-09-29T12:29:02Z")
	// 16 TGZ and 4 DEB files, 24 DEB index metadata files, 12 by-hash files.
	sh(0, "public URL check: 56 objects", append([]string{"publish"}, first...)...)
	var firstTGZ []string
	for _, p := range first {
		if !strings.HasSuffix(p, ".deb") {
			firstTGZ = append(firstTGZ, p)
		}
	}
	sh(0, `"state": "committed"`, append([]string{"publish", "--only-tgz"}, firstTGZ...)...)
	sh(0, "deb: 2 codenames, 8 index entries, 4 pool files, 12 by-hash files\ndeb lts: signed by "+key.SignWith+"\ndeb stable: signed by "+key.SignWith+"\npublic URL check: 36 objects", "verify", "deb")
	sh(0, "deb: 1 codenames, 4 index entries, 4 pool files, 6 by-hash files", "verify", "deb", "--channel", "lts")
	sh(0, `"state": "free"`, "unlock", "--tag", "v26.3.36.6-lts")
	big := filepath.Base(first[0])
	for _, ch := range []string{"lts", "stable"} {
		if obj, err := head("tgz/" + ch + "/" + big); err != nil || obj.Release != "v26.3.36.6-lts" || obj.Size != store.MinPartSize+12345 {
			t.Fatalf("%s: %+v, %v", ch, obj, err)
		}
	}

	second := writeRelease(t, "26.3.37.1", true)
	sh(1, "the state is free", append([]string{"publish"}, second...)...)
	before := listAll(t, s)
	out := t.TempDir()
	sh(0, "+Package: clickhouse-client\n+Version: 26.3.37.1", append([]string{"publish", "--dry-run", "--tag", "v26.3.37.1-stable", "--out", out}, second...)...)
	if !reflect.DeepEqual(listAll(t, s), before) {
		t.Fatal("the dry run changed the bucket")
	}
	if _, err := os.Stat(filepath.Join(out, "deb/dists/stable/InRelease")); err != nil {
		t.Fatal(err)
	}
	sh(0, `"previous": "20260929T122902Z-v26.3.36.6-lts"`, "lock", "--tag", "v26.3.37.1-stable", "--date", "2026-10-05T10:00:00Z")
	sh(1, ".sha512 has no ", append([]string{"publish"}, second[1:]...)...)
	sh(1, "package type rpm not in this build", append([]string{"publish", "--only-rpm"}, second...)...)
	sh(1, "want <package>-26.3.37.1-<arch>.tgz, the version of the release", append([]string{"publish"}, first...)...)
	sh(1, "not a package of any known package type", "publish", filepath.Join(t.TempDir(), "clickhouse-client-26.3.37.1.x86_64.rpm"))
	// 8 TGZ and 4 DEB files, 24 DEB index metadata files, 12 by-hash files.
	sh(0, "public URL check: 48 objects", append([]string{"publish"}, second...)...)
	sh(0, "deb: 2 codenames, 12 index entries, 8 pool files", "verify", "deb")
	if _, err := head("tgz/stable/" + filepath.Base(second[0])); err != nil {
		t.Fatal(err)
	}
	sh(0, `"snapshot": "20260929T122902Z-v26.3.36.6-lts"`, "rollback", "--tag", "v26.3.37.1-stable")
	sh(0, "deb: 2 codenames, 8 index entries, 4 pool files, 12 by-hash files", "verify", "deb")
	// The public key of a package: TGZ in the stable directory, DEB in the pool.
	keyOf := func(p string) string {
		if strings.HasSuffix(p, ".deb") {
			return "deb/pool/main/c/clickhouse/" + filepath.Base(p)
		}
		return "tgz/stable/" + filepath.Base(p)
	}
	for _, p := range second {
		if _, err := head(keyOf(p)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s after rollback: %v", keyOf(p), err)
		}
	}
	for _, p := range first {
		if _, err := head(keyOf(p)); err != nil {
			t.Fatalf("%s of the first release: %v", keyOf(p), err)
		}
	}
}

// listAll maps every key of s to its ETag.
func listAll(t *testing.T, s store.Bucket) map[string]string {
	t.Helper()
	m := map[string]string{}
	if err := s.List(context.Background(), "", func(o store.Object) error { m[o.Key] = o.ETag; return nil }); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckNeedsSomething(t *testing.T) {
	for _, env := range []string{"CH_DEPLOY_ENDPOINT", "CH_DEPLOY_BUCKET", "CH_DEPLOY_SIGNING_KEY", "CH_DEPLOY_SIGNING_KEY_FILE", "CH_DEPLOY_SIGN_WITH"} {
		t.Setenv(env, "")
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "nothing to check") {
		t.Fatalf("exit %d, stderr %q", code, &stderr)
	}
}
