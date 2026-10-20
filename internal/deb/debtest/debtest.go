// Package debtest builds small .deb files for tests, valid ones and broken ones.
package debtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Member is one ar member.
type Member struct {
	Name string
	Body []byte
}

// AR builds an ar archive in the format dpkg-deb writes.
func AR(members ...Member) []byte {
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.Name, 0, 0, 0, "100644", len(m.Body))
		b.Write(m.Body)
		if len(m.Body)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// TarGz builds a gzip compressed tar of regular files, and directories for
// names ending in a slash, in the given order.
func TarGz(files ...Member) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: 0o644, Size: int64(len(f.Body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(f.Name, "/") {
			h.Mode, h.Typeflag = 0o755, tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			panic(err)
		}
		if _, err := tw.Write(f.Body); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// Build returns a .deb with the control text and a data archive holding data
// in /usr/share/doc/<Package>/payload, so that packages install side by side.
func Build(control string, data []byte) []byte {
	pkg := "unnamed"
	for _, line := range strings.Split(control, "\n") {
		if name, ok := strings.CutPrefix(line, "Package: "); ok {
			pkg = name
		}
	}
	doc := "./usr/share/doc/" + pkg + "/"
	return AR(
		Member{"debian-binary", []byte("2.0\n")},
		Member{"control.tar.gz", TarGz(Member{"./control", []byte(control)}, Member{"./md5sums", []byte("")})},
		Member{"data.tar.gz", TarGz(Member{"./", nil}, Member{"./usr/", nil}, Member{"./usr/share/", nil},
			Member{"./usr/share/doc/", nil}, Member{doc, nil}, Member{doc + "payload", data})},
	)
}

// Control is a control file like the ones nfpm writes for ClickHouse.
// source, when not empty, adds a Source field; extra are more "Name: value"
// lines, before Description.
func Control(pkg, version, arch, source string, extra ...string) string {
	c := fmt.Sprintf("Package: %s\nVersion: %s\nSection: database\nPriority: optional\nArchitecture: %s\n"+
		"Maintainer: ClickHouse Dev Team <packages+linux@clickhouse.com>\nInstalled-Size: 10\n", pkg, version, arch)
	if source != "" {
		c += "Source: " + source + "\n"
	}
	for _, line := range extra {
		c += line + "\n"
	}
	return c + "Homepage: https://clickhouse.com\nDescription: Test package " + pkg + "\n second line of the description.\n .\n after an empty line.\n"
}

// Write writes a .deb named <pkg>_<version>_<arch>.deb into dir and returns its path.
// data makes the content unique.
func Write(t testing.TB, dir, pkg, version, arch, source, data string) string {
	t.Helper()
	path := filepath.Join(dir, pkg+"_"+version+"_"+arch+".deb")
	if err := os.WriteFile(path, Build(Control(pkg, version, arch, source), []byte(data)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
