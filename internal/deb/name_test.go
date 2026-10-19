package deb

import (
	"strings"
	"testing"
)

func TestParseName(t *testing.T) {
	good := map[string][2]string{
		"clickhouse-client_26.7.19.5_amd64.deb":     {"clickhouse-client", "amd64"},
		"clickhouse-keeper-dbg_26.7.19.5_arm64.deb": {"clickhouse-keeper-dbg", "arm64"},
		"clickhouse-docs_26.7.19.5_all.deb":         {"clickhouse-docs", "all"},
		"lib+x.y_26.7.19.5_all.deb":                 {"lib+x.y", "all"},
	}
	for name, want := range good {
		pkg, arch, ok, err := ParseName(name, "26.7.19.5")
		if !ok || err != nil || pkg != want[0] || arch != want[1] {
			t.Errorf("%s: %q %q %v %v", name, pkg, arch, ok, err)
		}
	}
	bad := []string{
		"clickhouse-client_26.7.19.4_amd64.deb",
		"clickhouse-client_26.7.19.5_i386.deb",
		"clickhouse-client_26.7.19.5_x86_64.deb",
		"clickhouse-client-26.7.19.5_amd64.deb",
		"clickhouse-client_26.7.19.5.deb",
		"_26.7.19.5_amd64.deb",
		"Clickhouse_26.7.19.5_amd64.deb",
		"c_26.7.19.5_amd64.deb",
		"-x_26.7.19.5_amd64.deb",
		"a_b_26.7.19.5_amd64.deb",
		".deb",
	}
	for _, name := range bad {
		if _, _, ok, err := ParseName(name, "26.7.19.5"); !ok || err == nil {
			t.Errorf("%s: ok %v err %v, want a claimed error", name, ok, err)
		}
	}
	for _, name := range []string{"clickhouse-client-26.7.19.5-amd64.tgz", "x.rpm", "x.deb.sha512", "x.udeb"} {
		if _, _, ok, _ := ParseName(name, "26.7.19.5"); ok {
			t.Errorf("%s claimed", name)
		}
	}
}

func TestSourceAndPool(t *testing.T) {
	cases := []struct{ control, source, pool string }{
		{"Package: clickhouse-client\n", "clickhouse-client", "pool/main/c/clickhouse-client/f.deb"},
		{"Package: clickhouse-client\nSource: clickhouse\n", "clickhouse", "pool/main/c/clickhouse/f.deb"},
		{"Package: x\nSource: clickhouse (26.7.19.5)\n", "clickhouse", "pool/main/c/clickhouse/f.deb"},
		{"Package: libfoo1\nSource: libfoo\n", "libfoo", "pool/main/libf/libfoo/f.deb"},
		{"Package: lib\n", "lib", "pool/main/l/lib/f.deb"},
		{"Package: libx\n", "libx", "pool/main/libx/libx/f.deb"},
	}
	for _, c := range cases {
		p, err := ParseParagraph([]byte(c.control))
		if err != nil {
			t.Fatal(err)
		}
		src, err := SourceName(p)
		if err != nil || src != c.source {
			t.Errorf("%q: source %q %v, want %q", c.control, src, err, c.source)
			continue
		}
		if got := PoolPath(src, "f.deb"); got != c.pool {
			t.Errorf("%q: pool %q, want %q", c.control, got, c.pool)
		}
	}
	for _, src := range []string{"clickhouse (", "clickhouse ()", "clickhouse (1) x", "clickhouse 1", "Click", "clickhouse (1 2)", "clickhouse  (1)"} {
		p, err := ParseParagraph([]byte("Package: x\nSource: " + src + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SourceName(p); err == nil || !strings.Contains(err.Error(), "Source") {
			t.Errorf("Source %q: %v", src, err)
		}
	}
}
