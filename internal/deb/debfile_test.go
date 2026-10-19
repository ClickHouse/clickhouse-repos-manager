package deb

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/deb/debtest"
)

func writeFile(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.deb")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadControl(t *testing.T) {
	control := debtest.Control("clickhouse-client", "26.7.19.5", "amd64", "clickhouse")
	for name, data := range map[string]string{"even": "ab", "odd": "abc"} {
		t.Run(name, func(t *testing.T) {
			p, err := ReadControl(writeFile(t, debtest.Build(control+"\n\n", []byte(data))))
			if err != nil {
				t.Fatal(err)
			}
			if string(p.Bytes()) != control {
				t.Fatalf("control %q, want %q", p.Bytes(), control)
			}
			if p.Value("Package") != "clickhouse-client" || !strings.Contains(p.Value("Description"), "\n .\n") {
				t.Fatalf("fields %q", p.Fields())
			}
		})
	}
}

func TestReadControlBroken(t *testing.T) {
	control := []byte(debtest.Control("x1", "1", "amd64", ""))
	ctl := debtest.TarGz(debtest.Member{Name: "./control", Body: control})
	data := debtest.TarGz(debtest.Member{Name: "./x", Body: []byte("x")})
	bin := debtest.Member{Name: "debian-binary", Body: []byte("2.0\n")}
	ok := debtest.AR(bin, debtest.Member{Name: "control.tar.gz", Body: ctl}, debtest.Member{Name: "data.tar.gz", Body: data})
	big := bytes.Repeat([]byte("x"), maxControlArchive+1)
	cases := map[string][]byte{
		"empty":             nil,
		"bad magic":         append([]byte("!<arch>x"), ok[8:]...),
		"truncated":         ok[:len(ok)-3],
		"trailing garbage":  append(append([]byte(nil), ok...), "x"...),
		"debian-binary 3.0": debtest.AR(debtest.Member{Name: "debian-binary", Body: []byte("3.0\n")}, debtest.Member{Name: "control.tar.gz", Body: ctl}, debtest.Member{Name: "data.tar.gz", Body: data}),
		"no control":        debtest.AR(bin, debtest.Member{Name: "data.tar.gz", Body: data}),
		"control.tar.xz":    debtest.AR(bin, debtest.Member{Name: "control.tar.xz", Body: ctl}, debtest.Member{Name: "data.tar.gz", Body: data}),
		"control.tar.zst":   debtest.AR(bin, debtest.Member{Name: "control.tar.zst", Body: ctl}, debtest.Member{Name: "data.tar.gz", Body: data}),
		"oversized control": debtest.AR(bin, debtest.Member{Name: "control.tar.gz", Body: big}, debtest.Member{Name: "data.tar.gz", Body: data}),
		"extra member":      debtest.AR(bin, debtest.Member{Name: "control.tar.gz", Body: ctl}, debtest.Member{Name: "data.tar.gz", Body: data}, debtest.Member{Name: "x", Body: nil}),
		"wrong order":       debtest.AR(debtest.Member{Name: "control.tar.gz", Body: ctl}, bin, debtest.Member{Name: "data.tar.gz", Body: data}),
		"no control file":   debtest.AR(bin, debtest.Member{Name: "control.tar.gz", Body: debtest.TarGz(debtest.Member{Name: "./md5sums"})}, debtest.Member{Name: "data.tar.gz", Body: data}),
		"control not gzip":  debtest.AR(bin, debtest.Member{Name: "control.tar.gz", Body: []byte("plain")}, debtest.Member{Name: "data.tar.gz", Body: data}),
		"two paragraphs":    debtest.Build(string(control)+"\nPackage: y\n", nil),
		"bad field":         debtest.Build("Package x\n", nil),
		"empty control":     debtest.Build("", nil),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if p, err := ReadControl(writeFile(t, body)); err == nil {
				t.Fatalf("read %q", p.Bytes())
			}
		})
	}
	if _, err := ReadControl(writeFile(t, ok)); err != nil {
		t.Fatalf("the valid base case fails: %v", err)
	}
}
