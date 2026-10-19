package tgz

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

const version = "26.3.36.6"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name, pkg, arch string
		ok              bool
		err             string
	}{
		{name: "clickhouse-common-static-dbg-26.3.36.6-arm64.tgz", pkg: "clickhouse-common-static-dbg", arch: "arm64", ok: true},
		{name: "clickhouse-client-26.3.36.6-amd64.tgz.sha512", pkg: "clickhouse-client", arch: "amd64", ok: true},
		{name: "clickhouse-docs-26.3.36.6.tgz", pkg: "clickhouse-docs", arch: "all", ok: true},
		{name: "clickhouse-docs-26.3.36.6.tgz.sha512", pkg: "clickhouse-docs", arch: "all", ok: true},
		{name: "-26.3.36.6.tgz", ok: true, err: `unknown architecture "26.3.36.6"`},
		{name: "clickhouse-client-26.3.36.6-all.tgz", ok: true, err: `unknown architecture "all"`},
		{name: "clickhouse-client_26.3.36.6_amd64.deb"},
		{name: "clickhouse-client-26.3.36.6.x86_64.rpm"},
		{name: "clickhouse-client-26.3.36.6-amd64.tgz.sha256"},
		{name: "clickhouse-client-26.3.36.6-x86_64.tgz", ok: true, err: `unknown architecture "x86_64"`},
		{name: "clickhouse-client-21.1.9.41.tgz", ok: true, err: `unknown architecture "21.1.9.41"`},
		{name: "clickhouse-client-26.3.36.7-amd64.tgz", ok: true, err: "the version of the release"},
		{name: "clickhouse-client-126.3.36.6-amd64.tgz", ok: true, err: "the version of the release"},
		{name: "-26.3.36.6-amd64.tgz", ok: true, err: "the version of the release"},
		{name: "amd64.tgz", ok: true, err: "want <package>-26.3.36.6-<arch>.tgz"},
	} {
		pkg, arch, ok, err := Type{}.Parse(tc.name, version)
		if pkg != tc.pkg || arch != tc.arch || ok != tc.ok || (err == nil) != (tc.err == "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("Parse(%q) = %q %q %v %v, want %q %q %v %q", tc.name, pkg, arch, ok, err, tc.pkg, tc.arch, tc.ok, tc.err)
		}
	}
}

// pair writes an archive and its checksum file; sum overrides the checksum content.
func pair(t *testing.T, dir, name string, sum func(sha512 string) string) []release.Package {
	t.Helper()
	write := func(name, body string) release.Package {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sums, err := hashes.File(path)
		if err != nil {
			t.Fatal(err)
		}
		return release.Package{Name: name, Path: path, Sums: sums}
	}
	a := write(name, "archive "+name)
	body := a.Sums.SHA512 + "  " + name + "\n"
	if sum != nil {
		body = sum(a.Sums.SHA512)
	}
	return []release.Package{a, write(name+".sha512", body)}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	const name = "clickhouse-client-26.3.36.6-amd64.tgz"
	good := pair(t, dir, name, nil)
	if err := (Type{}).Check(good); err != nil {
		t.Fatal(err)
	}
	bad := func(sum func(string) string) []release.Package { return pair(t, t.TempDir(), name, sum) }
	for _, tc := range []struct {
		pkgs []release.Package
		want string
	}{
		{good[:1], name + " has no " + name + ".sha512"},
		{good[1:], name + ".sha512 has no " + name},
		{bad(func(s string) string { return s + "  other.tgz\n" }), "names other.tgz, not " + name},
		{bad(func(string) string { return strings.Repeat("0", 128) + "  " + name + "\n" }), "but " + name + " has " + good[0].Sums.SHA512},
		{bad(func(s string) string { return s + "  " + name }), "want"},
		{bad(func(s string) string { return s + " " + name + "\n" }), "want"},
		{bad(func(s string) string { return s + "  " + name + "\n\n" }), "want"},
		{bad(func(string) string { return strings.Repeat("x", maxChecksumSize+1) }), "too large"},
	} {
		if err := (Type{}).Check(tc.pkgs); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Check: %v, want %q", err, tc.want)
		}
	}
}

func TestStage(t *testing.T) {
	pkgs := pair(t, t.TempDir(), "clickhouse-client-26.3.36.6-amd64.tgz", nil)
	tag, err := release.ParseTag("v26.3.36.6-lts")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Type{}.Stage(context.Background(), nil, tag, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	gz := store.PutOptions{ContentType: "application/gzip", SHA512: pkgs[0].Sums.SHA512}
	txt := store.PutOptions{ContentType: "text/plain; charset=utf-8", SHA512: pkgs[1].Sums.SHA512}
	a, c := "tgz/lts/"+pkgs[0].Name, "tgz/lts/"+pkgs[1].Name
	want := release.Plan{Assets: []release.Asset{
		{Key: a, Path: pkgs[0].Path, Sums: pkgs[0].Sums, Opts: gz},
		{Key: "tgz/stable/" + pkgs[0].Name, CopyOf: a, Opts: gz},
		{Key: c, Path: pkgs[1].Path, Sums: pkgs[1].Sums, Opts: txt},
		{Key: "tgz/stable/" + pkgs[1].Name, CopyOf: c, Opts: txt},
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan %+v\nwant %+v", plan, want)
	}
}

// A TGZ release with the real package type: old TGZ objects without metadata
// do not stop adopt, and rollback removes exactly the release's objects.
func TestRelease(t *testing.T) {
	ctx, b := context.Background(), storetest.NewMem()
	b.PutForeign("tgz/stable/clickhouse-client-21.1.9.41.tgz", []byte("old"))
	r := &release.Releaser{Bucket: b, Types: []release.PackageType{Type{}}, Version: "test"}
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"clickhouse-client-26.3.36.6-amd64.tgz", "clickhouse-client-26.3.36.6-arm64.tgz"} {
		for _, p := range pair(t, dir, n, nil) {
			paths = append(paths, p.Path)
		}
	}
	if _, err := r.Lock(ctx, release.LockRequest{Tag: "v26.3.36.6-lts", Date: "2026-09-29T12:29:02Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(ctx, paths[:3], nil); err == nil || !strings.Contains(err.Error(), "has no") {
		t.Fatalf("publish without a checksum file: %v", err)
	}
	if _, err := r.Publish(ctx, paths, nil); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"lts", "stable"} {
		obj, err := b.Head(ctx, "tgz/"+ch+"/clickhouse-client-26.3.36.6-arm64.tgz.sha512")
		if err != nil || obj.Release != "v26.3.36.6-lts" || obj.ContentType != "text/plain; charset=utf-8" || obj.SHA512 == "" {
			t.Fatalf("%s: %+v, %v", ch, obj, err)
		}
	}
	if _, err := r.Rollback(ctx, "v26.3.36.6-lts"); err != nil {
		t.Fatal(err)
	}
	var left []string
	if err := b.List(ctx, "tgz/", func(o store.Object) error { left = append(left, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, []string{"tgz/stable/clickhouse-client-21.1.9.41.tgz"}) {
		t.Fatalf("left %q", left)
	}
}
