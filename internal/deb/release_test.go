package deb

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

func TestCompression(t *testing.T) {
	in := bytes.Repeat([]byte("Package: clickhouse-client\n"), 1000)
	g1, err := Gzip(in)
	if err != nil {
		t.Fatal(err)
	}
	g2, _ := Gzip(in)
	if !bytes.Equal(g1, g2) {
		t.Fatal("gzip is not deterministic")
	}
	zr, err := gzip.NewReader(bytes.NewReader(g1))
	if err != nil {
		t.Fatal(err)
	}
	if !zr.ModTime.IsZero() || zr.Name != "" {
		t.Fatalf("gzip header: mtime %v name %q", zr.ModTime, zr.Name)
	}
	if out, _ := io.ReadAll(zr); !bytes.Equal(out, in) {
		t.Fatal("gzip round trip")
	}
	if len(g1)%10240 == 0 {
		t.Fatalf("gzip size %d looks padded", len(g1))
	}
	x1, err := XZ(in)
	if err != nil {
		t.Fatal(err)
	}
	x2, _ := XZ(in)
	if !bytes.Equal(x1, x2) {
		t.Fatal("xz is not deterministic")
	}
	xr, err := xz.NewReader(bytes.NewReader(x1))
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := io.ReadAll(xr); !bytes.Equal(out, in) {
		t.Fatal("xz round trip")
	}
}

func TestArchRelease(t *testing.T) {
	if got := string(ArchRelease("amd64")); got != "Component: main\nOrigin: ClickHouse\nLabel: ClickHouse\nArchitecture: amd64\n" || len(got) != 73 {
		t.Fatalf("%q", got)
	}
}

func TestByHash(t *testing.T) {
	if got := ByHash("main/binary-amd64/Packages.gz", "abc"); got != "main/binary-amd64/by-hash/SHA512/abc" {
		t.Fatal(got)
	}
}

func TestBuildRelease(t *testing.T) {
	a, b := hashes.Bytes([]byte("a")), hashes.Bytes([]byte("bb"))
	date := time.Date(2026, 10, 19, 12, 1, 2, 0, time.FixedZone("x", 3600))
	got := BuildRelease("lts", date, []Listed{{"main/binary-arm64/Packages", b}, {"main/binary-amd64/Packages", a}})
	want := "Origin: ClickHouse\nLabel: ClickHouse\nCodename: lts\nDate: Mon, 19 Oct 2026 11:01:02 UTC\nArchitectures: amd64 arm64\nComponents: main\nAcquire-By-Hash: yes\n" +
		"MD5Sum:\n " + a.MD5 + " 1 main/binary-amd64/Packages\n " + b.MD5 + " 2 main/binary-arm64/Packages\n" +
		"SHA1:\n " + a.SHA1 + " 1 main/binary-amd64/Packages\n " + b.SHA1 + " 2 main/binary-arm64/Packages\n" +
		"SHA256:\n " + a.SHA256 + " 1 main/binary-amd64/Packages\n " + b.SHA256 + " 2 main/binary-arm64/Packages\n" +
		"SHA512:\n " + a.SHA512 + " 1 main/binary-amd64/Packages\n " + b.SHA512 + " 2 main/binary-arm64/Packages\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	r, err := ParseRelease(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Paths) != 2 || r.Match("main/binary-amd64/Packages", a) != nil || r.Match("main/binary-arm64/Packages", a) == nil || r.Match("x", a) == nil {
		t.Fatalf("%+v", r)
	}
}

func TestParseReleaseRejects(t *testing.T) {
	head := "Origin: ClickHouse\n"
	for name, b := range map[string]string{
		"no hashes":     head,
		"bad line":      head + "MD5Sum:\n abc main/x\n",
		"bad size":      head + "MD5Sum:\n abc -1 main/x\n",
		"twice":         head + "MD5Sum:\n a 1 main/x\n b 1 main/x\n",
		"size mismatch": head + "MD5Sum:\n a 1 main/x\nSHA1:\n b 2 main/x\n",
		"file missing":  head + "MD5Sum:\n a 1 main/x\n a 1 main/y\nSHA1:\n b 1 main/x\n",
		"file extra":    head + "MD5Sum:\n a 1 main/x\nSHA1:\n b 1 main/x\n b 1 main/y\n",
	} {
		if _, err := ParseRelease([]byte(b)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// TestProductionRelease parses the production Release and checks the files it lists.
func TestProductionRelease(t *testing.T) {
	r, err := ParseRelease(productionFile(t, "deb/dists/lts/Release"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Value("Codename") != "lts" || len(r.Paths) < 6 {
		t.Fatalf("%+v", r.Paths)
	}
	for _, path := range r.Paths {
		if !strings.HasSuffix(path, "/Packages") && !strings.HasSuffix(path, "/Release") {
			continue
		}
		if err := r.Match(path, hashes.Bytes(productionFile(t, "deb/dists/lts/"+path))); err != nil {
			t.Error(err)
		}
	}
}
