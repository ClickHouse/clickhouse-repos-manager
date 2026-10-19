package deb

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/deb/debtest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/sign/signtest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/tgz"
)

var (
	clock = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reqA  = release.LockRequest{Tag: "v26.3.36.6-lts", Date: "2026-09-29T12:29:02Z"}
	reqB  = release.LockRequest{Tag: "v26.8.2.1-stable", Date: "2026-10-01T08:00:00Z"}
	reqC  = release.LockRequest{Tag: "v26.8.3.1-stable", Date: "2026-10-08T08:00:00Z"}
)

func debType(t testing.TB) Type {
	k := signtest.New(t)
	return Type{SignWith: k.SignWith, Signer: k.Signer, Now: func() time.Time { return clock }}
}

func releaser(t testing.TB, b store.Bucket) *release.Releaser {
	return &release.Releaser{Bucket: b, Types: []release.PackageType{debType(t), tgz.Type{}}, Version: "test", Now: func() time.Time { return clock }}
}

// backends runs fn on the in-memory bucket and, when configured, on the test bucket.
func backends(t *testing.T, fn func(t *testing.T, b store.Bucket)) {
	t.Parallel()
	t.Run("mem", func(t *testing.T) { fn(t, storetest.NewMem()) })
	t.Run("bucket", func(t *testing.T) { fn(t, storetest.New(t, config.SnapshotsPrefix)) })
}

// releaseFiles writes the DEB and TGZ files of a release: clickhouse-client
// and clickhouse-server (Source: clickhouse) for amd64 and arm64, and
// clickhouse-docs for all.
func releaseFiles(t *testing.T, tag string) []string {
	t.Helper()
	tg, err := release.ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	v, dir := tg.Version(), t.TempDir()
	var paths []string
	add := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		sum := hashes.Bytes([]byte(body)).SHA512
		if err := os.WriteFile(p+".sha512", []byte(sum+"  "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p+".sha512")
	}
	for _, pkg := range []string{"clickhouse-client", "clickhouse-server"} {
		for _, arch := range []string{"amd64", "arm64"} {
			paths = append(paths, debtest.Write(t, dir, pkg, v, arch, "clickhouse", tag+pkg+arch))
			add(pkg+"-"+v+"-"+arch+".tgz", tag+pkg+arch)
		}
	}
	paths = append(paths, debtest.Write(t, dir, "clickhouse-docs", v, "all", "", tag+"docs"))
	add("clickhouse-docs-"+v+".tgz", tag+"docs")
	return paths
}

func mustLock(t *testing.T, r *release.Releaser, req release.LockRequest) {
	t.Helper()
	if _, err := r.Lock(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func publish(t *testing.T, b store.Bucket, req release.LockRequest) []string {
	t.Helper()
	r := releaser(t, b)
	mustLock(t, r, req)
	files := releaseFiles(t, req.Tag)
	if _, err := r.Publish(context.Background(), files, nil); err != nil {
		t.Fatal(err)
	}
	return files
}

func unlock(t *testing.T, b store.Bucket, req release.LockRequest) release.Unlocked {
	t.Helper()
	u, err := releaser(t, b).Unlock(context.Background(), req.Tag)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func wantErr(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("error %v, want %q", err, msg)
	}
}

func get(t *testing.T, b store.Bucket, key string) []byte {
	t.Helper()
	body, _, err := b.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return body
}

// objects maps every public key to its ETag and stamp.
func objects(t *testing.T, b store.Bucket) map[string]string {
	t.Helper()
	ctx := context.Background()
	m := map[string]string{}
	for _, prefix := range []string{"deb/", "tgz/"} {
		err := b.List(ctx, prefix, func(o store.Object) error {
			h, err := b.Head(ctx, o.Key)
			m[o.Key] = h.ETag + " " + h.Release
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// checkRepo verifies the chain of every codename, that each holds the
// packages of its releases, and the headers and by-hash files.
func checkRepo(t *testing.T, b store.Bucket, want map[string][]string) {
	t.Helper()
	ctx := context.Background()
	typ := debType(t)
	if _, err := typ.Live(ctx, b); err != nil {
		t.Fatal(err)
	}
	for cn, versions := range want {
		c, err := typ.Chain(ctx, b, cn)
		if err != nil {
			t.Fatal(err)
		}
		for _, arch := range config.Architectures {
			var got []string
			for _, p := range c.Indexes[arch].Stanzas() {
				got = append(got, KeyOf(p).String())
				obj, err := b.Head(ctx, Prefix+p.Value("Filename"))
				if err != nil || obj.ContentType != typeDeb || fmt.Sprint(obj.Size) != p.Value("Size") {
					t.Fatalf("%s: %+v %v", p.Value("Filename"), obj, err)
				}
			}
			var exp []string
			for _, v := range versions {
				exp = append(exp, "clickhouse-client_"+v+"_"+arch, "clickhouse-docs_"+v+"_all", "clickhouse-server_"+v+"_"+arch)
			}
			if !reflect.DeepEqual(got, exp) {
				t.Fatalf("%s %s: %q, want %q", cn, arch, got, exp)
			}
		}
		for _, k := range ByHashKeys(cn, c.Release) {
			if _, err := b.Head(ctx, k); err != nil {
				t.Fatalf("%s: %v", k, err)
			}
		}
		for name, ct := range map[string]string{
			"InRelease": "", "Release": "", "Release.gpg": "", "Public.key": "",
			"main/binary-amd64/Packages": "", "main/binary-amd64/Release": "",
			"main/binary-amd64/Packages.gz": typeGzip, "main/binary-amd64/Packages.xz": typeXZ,
		} {
			obj, err := b.Head(ctx, Dists(cn)+name)
			if ct == "" && obj.ContentType == "application/octet-stream" && storetest.Backend() == "miniflare" {
				// Miniflare stores it when none was sent; R2 stores none.
				obj.ContentType = ""
			}
			if err != nil || obj.ContentType != ct || obj.CacheControl != "" {
				t.Fatalf("%s %s: content type %q, want %q: %v", cn, name, obj.ContentType, ct, err)
			}
		}
	}
}

// TestReleases: a DEB and TGZ release into an empty repository, an -lts and
// a -stable one on top, a rerun, and a rollback.
func TestReleases(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx := context.Background()
		publish(t, b, reqA)
		checkRepo(t, b, map[string][]string{"lts": {"26.3.36.6"}, "stable": {"26.3.36.6"}})
		unlock(t, b, reqA)
		ltsBefore := objects(t, b)
		oldStable := get(t, b, "deb/dists/stable/main/binary-amd64/Packages")

		files := publish(t, b, reqB)
		checkRepo(t, b, map[string][]string{"lts": {"26.3.36.6"}, "stable": {"26.3.36.6", "26.8.2.1"}})
		if newStable := get(t, b, "deb/dists/stable/main/binary-amd64/Packages"); !bytes.HasPrefix(newStable, oldStable) {
			t.Fatal("the old stable stanzas are no prefix")
		}
		after := objects(t, b)
		for k, v := range ltsBefore {
			if strings.HasPrefix(k, "deb/dists/lts/") && after[k] != v {
				t.Fatalf("a -stable release changed %s", k)
			}
		}
		if _, err := releaser(t, b).Publish(ctx, files, nil); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(objects(t, b), after) {
			t.Fatal("a rerun changed the repository")
		}
		unlock(t, b, reqB)

		r := releaser(t, b)
		mustLock(t, r, reqC)
		if _, err := r.Publish(ctx, releaseFiles(t, reqC.Tag), nil); err != nil {
			t.Fatal(err)
		}
		checkRepo(t, b, map[string][]string{"stable": {"26.3.36.6", "26.8.2.1", "26.8.3.1"}})
		if _, err := r.Rollback(ctx, reqC.Tag); err != nil {
			t.Fatal(err)
		}
		if got := objects(t, b); !reflect.DeepEqual(got, after) {
			t.Fatalf("after rollback:\n%v\nwant\n%v", got, after)
		}
	})
}

// TestPublishFaults: a fault at any request of a DEB publish, then a rerun.
func TestPublishFaults(t *testing.T) {
	storetest.ForEachFault(t, func(*testing.T) store.Bucket { return storetest.NewMem() }, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		ctx := context.Background()
		mustLock(t, releaser(t, b), reqA)
		files := releaseFiles(t, reqA.Tag)
		r := releaser(t, f)
		r.Concurrency = 1
		if _, err := r.Publish(ctx, files, nil); err != nil && (!f.Injected() || f.Mode == storetest.Retried) {
			t.Fatalf("faulty run (%s): %v", f.Mode, err)
		}
		if _, err := releaser(t, b).Publish(ctx, files, nil); err != nil {
			t.Fatal(err)
		}
		checkRepo(t, b, map[string][]string{"lts": {"26.3.36.6"}, "stable": {"26.3.36.6"}})
	})
}

// TestPrune: unlock deletes the by-hash files that only pruned snapshots
// named, and keeps unstamped ones.
func TestPrune(t *testing.T) {
	ctx, m := context.Background(), storetest.NewMem()
	foreign := "deb/dists/stable/main/binary-amd64/by-hash/SHA512/foreign"
	var first []string
	for i := range config.SnapshotRetention + 2 {
		req := release.LockRequest{Tag: fmt.Sprintf("v26.9.%d.1-stable", i+1), Date: time.Date(2026, 10, 1, 0, i, 0, 0, time.UTC).Format("2006-01-02T15:04:05Z")}
		publish(t, m, req)
		if i == 0 {
			c, err := debType(t).Chain(ctx, m, "stable")
			if err != nil {
				t.Fatal(err)
			}
			first = ByHashKeys("stable", c.Release)
			sort.Strings(first)
			// Before the first release it would make a partial codename at adopt.
			m.PutForeign(foreign, []byte("foreign"))
		}
		u := unlock(t, m, req)
		// The adopted snapshot and the first release are pruned together, as the 31st release unlocks.
		if i == config.SnapshotRetention {
			if len(u.Pruned) != 1 || !reflect.DeepEqual(u.Pruned[0].Deleted, first) || !reflect.DeepEqual(u.Pruned[0].NotStamped, []string{foreign}) {
				t.Fatalf("pruned %+v, want %q", u.Pruned, first)
			}
		}
	}
	if _, err := m.Head(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for _, k := range first {
		if _, err := m.Head(ctx, k); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

// productionRepo copies the production DEB metadata into b with InRelease and
// Release.gpg signed again by the test key, as a seeded packages-dev has it.
func productionRepo(t *testing.T, b store.Bucket) map[string][]byte {
	t.Helper()
	k := signtest.New(t)
	files := map[string][]byte{}
	for _, cn := range config.Codenames {
		rel := productionFile(t, Dists(cn)+"Release")
		r, err := ParseRelease(rel)
		if err != nil {
			t.Fatal(err)
		}
		files[Dists(cn)+"Release"] = rel
		for _, p := range r.Paths {
			files[Dists(cn)+p] = productionFile(t, Dists(cn)+p)
		}
		if files[Dists(cn)+"InRelease"], err = k.Signer.Clearsign(rel); err != nil {
			t.Fatal(err)
		}
		if files[Dists(cn)+"Release.gpg"], err = k.Signer.DetachSign(rel); err != nil {
			t.Fatal(err)
		}
	}
	for key, body := range files {
		if _, err := b.Put(context.Background(), key, body, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// TestAdoptProduction adopts the production metadata and publishes on top.
func TestAdoptProduction(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx := context.Background()
		old := productionRepo(t, b)
		publish(t, b, reqC)
		for _, cn := range []string{"lts", "stable"} {
			for _, arch := range config.Architectures {
				key := Dists(cn) + archDir(arch) + "Packages"
				oldIdx, err := ParseIndex(old[key])
				if err != nil {
					t.Fatal(err)
				}
				body := get(t, b, key)
				x, err := ParseIndex(body)
				if err != nil {
					t.Fatal(err)
				}
				want := oldIdx.Len()
				if cn == "stable" {
					want += 3
				}
				if !bytes.HasPrefix(body, old[key]) || x.Len() != want {
					t.Fatalf("%s: prefix %v, %d stanzas, want %d", key, bytes.HasPrefix(body, old[key]), x.Len(), want)
				}
				if cn == "lts" {
					continue
				}
				gz := get(t, b, key+".gz")
				zr, err := gzip.NewReader(bytes.NewReader(gz))
				if err != nil {
					t.Fatal(err)
				}
				if plain, _ := io.ReadAll(zr); !bytes.Equal(plain, body) || len(gz)%10240 == 0 {
					t.Fatalf("%s.gz: %d bytes", key, len(gz))
				}
			}
		}
		// lts was not part of the release and stays as adopted.
		if !bytes.Equal(get(t, b, "deb/dists/lts/InRelease"), old["deb/dists/lts/InRelease"]) {
			t.Fatal("lts changed")
		}
		if _, err := debType(t).Live(ctx, b); err != nil {
			t.Fatal(err)
		}
	})
}

// TestLiveProduction verifies the production metadata with the production
// public key, as a shadow run without the signing key does.
func TestLiveProduction(t *testing.T) {
	ctx, m := context.Background(), storetest.NewMem()
	pub := productionFile(t, "rpm/lts/repodata/repomd.xml.key")
	for _, cn := range config.Codenames {
		for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
			m.PutForeign(Dists(cn)+name, productionFile(t, Dists(cn)+name))
		}
		r, err := ParseRelease(productionFile(t, Dists(cn)+"Release"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range r.Paths {
			m.PutForeign(Dists(cn)+p, productionFile(t, Dists(cn)+p))
		}
		m.PutForeign(Dists(cn)+"Public.key", pub)
	}
	typ := Type{SignWith: "885E2BDCF96B0B45ABF058453E4AD4719DDE9A38"}
	files, err := typ.Live(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2*10 {
		t.Fatalf("%d files", len(files))
	}
	typ.SignWith = signtest.New(t).SignWith
	_, err = typ.Live(ctx, m)
	wantErr(t, err, "Public.key")
}

func TestLiveRejects(t *testing.T) {
	other := signtest.Other(t)
	for name, tc := range map[string]struct {
		edit func(t *testing.T, m *storetest.Mem)
		typ  func(Type) Type
		want string
	}{
		"bad InRelease signature": {edit: func(t *testing.T, m *storetest.Mem) {
			body := get(t, m, "deb/dists/stable/InRelease")
			m.PutForeign("deb/dists/stable/InRelease", bytes.Replace(body, []byte("Codename: stable"), []byte("Codename: stablE"), 1))
		}, want: "InRelease"},
		"bad Release.gpg": {edit: func(t *testing.T, m *storetest.Mem) {
			sig, _ := other.Signer.DetachSign(get(t, m, "deb/dists/stable/Release"))
			m.PutForeign("deb/dists/stable/Release.gpg", sig)
		}, want: "Release.gpg"},
		"Release changed": {edit: func(t *testing.T, m *storetest.Mem) {
			m.PutForeign("deb/dists/stable/Release", append(get(t, m, "deb/dists/stable/Release"), "X: y\n"...))
		}, want: "Release.gpg"},
		"InRelease of other text": {edit: func(t *testing.T, m *storetest.Mem) {
			signed, err := signtest.New(t).Signer.Clearsign(append(get(t, m, "deb/dists/stable/Release"), "X: y\n"...))
			if err != nil {
				t.Fatal(err)
			}
			m.PutForeign("deb/dists/stable/InRelease", signed)
		}, want: "InRelease signs other text than Release"},
		"wrong fingerprint": {typ: func(typ Type) Type { typ.SignWith = other.SignWith; return typ }, want: "Public.key"},
		"Public.key of another key": {edit: func(t *testing.T, m *storetest.Mem) {
			m.PutForeign("deb/dists/stable/Public.key", other.Public)
		}, want: "Public.key"},
		"no key at all": {edit: func(t *testing.T, m *storetest.Mem) {
			_ = m.Delete(context.Background(), "deb/dists/stable/Public.key")
		}, typ: func(typ Type) Type { typ.Signer = nil; return typ }, want: "no Public.key"},
		"hash mismatch": {edit: func(t *testing.T, m *storetest.Mem) {
			m.PutForeign("deb/dists/stable/main/binary-arm64/Packages.xz", []byte("x"))
		}, want: "main/binary-arm64/Packages.xz: size 1"},
		"missing listed file": {edit: func(t *testing.T, m *storetest.Mem) {
			_ = m.Delete(context.Background(), "deb/dists/stable/main/binary-amd64/Release")
		}, want: "main/binary-amd64/Release is missing"},
		"partial codename": {edit: func(t *testing.T, m *storetest.Mem) {
			m.PutForeign("deb/dists/lts/Public.key", get(t, m, "deb/dists/stable/Public.key"))
		}, want: "lts: deb/dists/lts/InRelease is missing"},
	} {
		t.Run(name, func(t *testing.T) {
			m := storetest.NewMem()
			publish(t, m, reqB)
			typ := debType(t)
			if _, err := typ.Live(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			if tc.edit != nil {
				tc.edit(t, m)
			}
			if tc.typ != nil {
				typ = tc.typ(typ)
			}
			_, err := typ.Live(context.Background(), m)
			wantErr(t, err, tc.want)
		})
	}
}

func TestRemove(t *testing.T) {
	ctx, m := context.Background(), storetest.NewMem()
	publish(t, m, reqA)
	unlock(t, m, reqA)
	publish(t, m, reqB)
	unlock(t, m, reqB)
	typ := debType(t)
	files, err := typ.Live(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	prev := release.LivePrevious(m, files)
	pool := func(pkg, v, arch string) string {
		return "deb/pool/main/c/" + map[bool]string{true: pkg, false: "clickhouse"}[pkg == "clickhouse-docs"] + "/" + pkg + "_" + v + "_" + arch + ".deb"
	}
	for name, tc := range map[string]struct {
		sel          release.Selector
		gone         []string // keys of the removed stanzas, per architecture index
		unreferenced []string
	}{
		"version in both channels stays referenced": {
			sel:  release.Selector{Channel: "stable", Version: "26.3.36.6"},
			gone: []string{"clickhouse-client_26.3.36.6_amd64", "clickhouse-docs_26.3.36.6_all", "clickhouse-server_26.3.36.6_amd64", "clickhouse-client_26.3.36.6_arm64", "clickhouse-docs_26.3.36.6_all", "clickhouse-server_26.3.36.6_arm64"},
		},
		"one package": {
			sel:          release.Selector{Channel: "stable", Version: "26.8.2.1", Packages: []string{"clickhouse-server"}},
			gone:         []string{"clickhouse-server_26.8.2.1_amd64", "clickhouse-server_26.8.2.1_arm64"},
			unreferenced: []string{pool("clickhouse-server", "26.8.2.1", "amd64"), pool("clickhouse-server", "26.8.2.1", "arm64")},
		},
		"one architecture": {
			sel:          release.Selector{Channel: "stable", Version: "26.8.2.1", Arch: "arm64"},
			gone:         []string{"clickhouse-client_26.8.2.1_arm64", "clickhouse-server_26.8.2.1_arm64"},
			unreferenced: []string{pool("clickhouse-client", "26.8.2.1", "arm64"), pool("clickhouse-server", "26.8.2.1", "arm64")},
		},
		"all": {
			sel:          release.Selector{Channel: "stable", Version: "26.8.2.1", Arch: "all"},
			gone:         []string{"clickhouse-docs_26.8.2.1_all", "clickhouse-docs_26.8.2.1_all"},
			unreferenced: []string{pool("clickhouse-docs", "26.8.2.1", "all")},
		},
		"lts": {
			sel:  release.Selector{Channel: "lts", Version: "26.3.36.6", Packages: []string{"clickhouse-client"}},
			gone: []string{"clickhouse-client_26.3.36.6_amd64", "clickhouse-client_26.3.36.6_arm64"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := typ.Remove(ctx, prev, tc.sel)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Unreferenced, tc.unreferenced) {
				t.Fatalf("unreferenced %q, want %q", plan.Unreferenced, tc.unreferenced)
			}
			var gone []string
			built := map[string][]byte{}
			for _, md := range plan.Metadata {
				built[md.Key] = md.Body
			}
			for _, arch := range config.Architectures {
				key := Dists(tc.sel.Channel) + archDir(arch) + "Packages"
				oldBody, _, _ := prev.Get(ctx, key)
				old, _ := ParseIndex(oldBody)
				x, err := ParseIndex(built[key])
				if err != nil {
					t.Fatal(err)
				}
				// The new index is the old bytes minus exactly the removed stanzas.
				want := oldBody
				keep := map[Key]bool{}
				for _, p := range x.Stanzas() {
					keep[KeyOf(p)] = true
				}
				for _, p := range old.Stanzas() {
					if !keep[KeyOf(p)] {
						gone = append(gone, KeyOf(p).String())
						want = bytes.Replace(want, append(append([]byte{}, p.Bytes()...), '\n'), nil, 1)
					}
				}
				if !bytes.Equal(built[key], want) {
					t.Fatalf("%s: other bytes changed", key)
				}
			}
			if !reflect.DeepEqual(gone, tc.gone) {
				t.Fatalf("removed %q, want %q", gone, tc.gone)
			}
			if _, ok := built[Dists(tc.sel.Channel)+"InRelease"]; !ok {
				t.Fatal("no InRelease")
			}
		})
	}
	for _, sel := range []release.Selector{
		{Channel: "stable", Version: "26.1.1.1"},
		{Channel: "lts", Version: "26.8.2.1"},
		{Channel: "stable", Version: "26.8.2.1", Packages: []string{"clickhouse-keeper"}},
		{Channel: "testing", Version: "26.8.2.1"},
		{Channel: "stable", Version: "26.8.2.1", Arch: "x86_64"},
		{Channel: "stable"},
	} {
		if _, err := typ.Remove(ctx, prev, sel); err == nil {
			t.Errorf("%+v: no error", sel)
		}
	}
}

// TestCheck refuses a control file that does not match its file name.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(name, control string) release.Package {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, debtest.Build(control, nil), 0o644); err != nil {
			t.Fatal(err)
		}
		pkg, arch, _, err := ParseName(name, "1.2.3.4")
		if err != nil {
			t.Fatal(err)
		}
		return release.Package{Name: name, Pkg: pkg, Arch: arch, Path: p}
	}
	ok := write("a1_1.2.3.4_amd64.deb", debtest.Control("a1", "1.2.3.4", "amd64", "src"))
	if err := (Type{}).Check([]release.Package{ok}); err != nil {
		t.Fatal(err)
	}
	for name, control := range map[string]string{
		"other package": debtest.Control("a2", "1.2.3.4", "amd64", ""),
		"other version": debtest.Control("a1", "1.2.3.5", "amd64", ""),
		"other arch":    debtest.Control("a1", "1.2.3.4", "all", ""),
		"bad source":    debtest.Control("a1", "1.2.3.4", "amd64", "Src"),
		"index field":   debtest.Control("a1", "1.2.3.4", "amd64", "") + "SHA256: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := (Type{}).Check([]release.Package{write("a1_1.2.3.4_amd64.deb", control)}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
