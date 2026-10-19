package deb

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// ProductionURL serves the live production repository; tests only read it.
const productionURL = "https://packages.clickhouse.com/"

var production sync.Map

// productionFile downloads a production file once per test run, or skips
// when packages.clickhouse.com is unreachable.
func productionFile(t *testing.T, key string) []byte {
	t.Helper()
	if b, ok := production.Load(key); ok {
		return b.([]byte)
	}
	if testing.Short() {
		t.Skip("production metadata is not downloaded with -short")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(productionURL + key)
	if err != nil {
		t.Skipf("packages.clickhouse.com unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s", key, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Skipf("%s: %v", key, err)
	}
	production.Store(key, b)
	return b
}

func para(t *testing.T, s string) Paragraph {
	t.Helper()
	p, err := ParseParagraph([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseParagraph(t *testing.T) {
	p := para(t, "Package: a\nDescription: x\n y\n .\n\tz\nMD5sum: 1\n")
	if p.Value("description") != "x\n y\n .\n\tz" || p.Value("MD5SUM") != "1" {
		t.Fatalf("%q", p.fields)
	}
	for _, bad := range []string{"", "Package: a", "Package: a\n\n", " x\n", "Package\n", "Pack age: a\n", ": a\n", "A: 1\na: 2\n"} {
		if _, err := ParseParagraph([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestNewStanza(t *testing.T) {
	control := "Package: a1\nVersion: 1\nArchitecture: amd64\nDescription: x\n y\n"
	sums := hashes.Bytes([]byte("payload"))
	s, err := NewStanza(para(t, control), "pool/main/a/a1/a1_1_amd64.deb", sums)
	if err != nil {
		t.Fatal(err)
	}
	want := control + "Filename: pool/main/a/a1/a1_1_amd64.deb\nSize: 7\nMD5sum: " + sums.MD5 + "\nSHA1: " + sums.SHA1 +
		"\nSHA256: " + sums.SHA256 + "\nSHA512: " + sums.SHA512 + "\n"
	if string(s.Bytes()) != want {
		t.Fatalf("stanza\n%s\nwant\n%s", s.Bytes(), want)
	}
	for _, f := range indexFields {
		if _, err := NewStanza(para(t, control+f+": x\n"), "f", sums); err == nil {
			t.Errorf("control with %s accepted", f)
		}
	}
}

func stanza(t *testing.T, pkg, version, arch, file, md5 string) Paragraph {
	t.Helper()
	return para(t, "Package: "+pkg+"\nVersion: "+version+"\nArchitecture: "+arch+"\nFilename: "+file+"\nSize: 1\nMD5sum: "+md5+"\n")
}

func TestIndex(t *testing.T) {
	old := "Package: a\nVersion: 1\nArchitecture: amd64\nFilename: pool/a1\nSize: 1\nMD5sum: m\n\n" +
		"Package: b\nVersion: 1\nArchitecture: all\nFilename: pool/b1\nSize: 1\nMD5sum: m\nSHA512: s\n\n"
	x, err := ParseIndex([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if x.Len() != 2 || string(x.Bytes()) != old {
		t.Fatalf("round trip: %d %q", x.Len(), x.Bytes())
	}
	// An old stanza without SHA512 matches a new one with it.
	same := para(t, "Package: a\nVersion: 1\nArchitecture: amd64\nFilename: pool/a1\nSize: 1\nMD5sum: m\nSHA512: new\n")
	if added, err := x.Add(same); added || err != nil {
		t.Fatalf("same package: %v %v", added, err)
	}
	for name, p := range map[string]Paragraph{
		"other hash":     stanza(t, "a", "1", "amd64", "pool/a1", "other"),
		"other filename": stanza(t, "a", "1", "amd64", "pool/a1x", "m"),
		"filename taken": stanza(t, "c", "1", "amd64", "pool/a1", "m"),
		"other sha512":   para(t, "Package: b\nVersion: 1\nArchitecture: all\nFilename: pool/b1\nSize: 1\nMD5sum: m\nSHA512: t\n"),
	} {
		if _, err := x.Add(p); err == nil {
			t.Errorf("%s: added", name)
		}
	}
	c := stanza(t, "c", "1", "amd64", "pool/c1", "m")
	if added, err := x.Add(c); !added || err != nil {
		t.Fatalf("new package: %v %v", added, err)
	}
	if got := string(x.Bytes()); got != old+string(c.Bytes())+"\n" || x.Len() != 3 {
		t.Fatalf("after add:\n%s", got)
	}
	gone := x.Remove(func(p Paragraph) bool { return p.Value("Package") == "b" })
	if len(gone) != 1 || string(x.Bytes()) != old[:strings.Index(old, "Package: b")]+string(c.Bytes())+"\n" {
		t.Fatalf("after remove: %d\n%s", len(gone), x.Bytes())
	}
}

func TestIndexEnds(t *testing.T) {
	c := stanza(t, "c", "1", "amd64", "pool/c1", "m")
	for old, want := range map[string]string{
		"":     "",
		"\n\n": "\n\n",
		"Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\nSize: 1\n":     "Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\nSize: 1\n\n",
		"Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\nSize: 1":       "Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\nSize: 1\n\n",
		"Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\nSize: 1\n\n\n": "Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\nSize: 1\n\n\n",
	} {
		x, err := ParseIndex([]byte(old))
		if err != nil {
			t.Fatalf("%q: %v", old, err)
		}
		if string(x.Bytes()) != old {
			t.Fatalf("%q: round trip %q", old, x.Bytes())
		}
		if _, err := x.Add(c); err != nil {
			t.Fatal(err)
		}
		if got := string(x.Bytes()); got != want+string(c.Bytes())+"\n" || !strings.HasPrefix(got, old) {
			t.Errorf("%q: after add %q", old, got)
		}
	}
}

func TestParseIndexRejects(t *testing.T) {
	a := "Package: a\nVersion: 1\nArchitecture: amd64\nFilename: f\n\n"
	for name, b := range map[string]string{
		"duplicate key":      a + a,
		"duplicate filename": a + strings.Replace(a, "Package: a", "Package: b", 1),
		"no filename":        "Package: a\nVersion: 1\nArchitecture: amd64\n\n",
		"no version":         "Package: a\nArchitecture: amd64\nFilename: f\n\n",
		"bad line":           a + "garbage\n",
	} {
		if _, err := ParseIndex([]byte(b)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// TestProductionIndex appends to and removes from the production Packages:
// the old bytes stay a prefix, and a removal cuts out exactly one stanza.
func TestProductionIndex(t *testing.T) {
	old := productionFile(t, "deb/dists/stable/main/binary-amd64/Packages")
	x, err := ParseIndex(old)
	if err != nil {
		t.Fatal(err)
	}
	n := x.Len()
	if n < 1000 || !bytes.Equal(x.Bytes(), old) {
		t.Fatalf("%d stanzas, round trip equal %v", n, bytes.Equal(x.Bytes(), old))
	}
	sums := hashes.Bytes([]byte("new"))
	s, err := NewStanza(para(t, "Package: clickhouse-new\nVersion: 99.1.1.1\nArchitecture: amd64\n"), "pool/main/c/clickhouse/clickhouse-new_99.1.1.1_amd64.deb", sums)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := x.Add(s); !added || err != nil {
		t.Fatal(added, err)
	}
	for _, p := range x.Stanzas()[:n] {
		if added, err := x.Add(p); added || err != nil {
			t.Fatalf("%s: %v %v", KeyOf(p), added, err)
		}
	}
	got := x.Bytes()
	if !bytes.HasPrefix(got, old) || x.Len() != n+1 {
		t.Fatalf("old bytes are no prefix, or count %d", x.Len())
	}
	if _, err := ParseIndex(got); err != nil {
		t.Fatal(err)
	}
	first := x.Stanzas()[0]
	x.Remove(func(p Paragraph) bool { return KeyOf(p) == KeyOf(first) })
	if want := append(bytes.TrimPrefix(old, first.Bytes())[1:], append(s.Bytes(), '\n')...); !bytes.Equal(x.Bytes(), want) {
		t.Fatal("remove changed other bytes")
	}
}
