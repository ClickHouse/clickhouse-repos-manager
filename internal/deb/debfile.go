package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Limits for the parts of a .deb read into memory; control data of real
// packages is a few kilobytes.
const (
	maxControlArchive = 1 << 20
	maxControlTar     = 16 << 20
	maxControlFile    = 1 << 20
)

// ReadControl reads the control file of a .deb: an ar archive of debian-binary
// "2.0", control.tar.gz and data.tar.*, in that order and nothing else.
func ReadControl(path string) (Paragraph, error) {
	f, err := os.Open(path)
	if err != nil {
		return Paragraph{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Paragraph{}, err
	}
	p, err := readControl(f, st.Size())
	if err != nil {
		return Paragraph{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

type member struct {
	name      string
	off, size int64
}

func readControl(r io.ReaderAt, size int64) (Paragraph, error) {
	members, err := arMembers(r, size)
	if err != nil {
		return Paragraph{}, err
	}
	if len(members) != 3 || members[0].name != "debian-binary" || !strings.HasPrefix(members[1].name, "control.tar") || !strings.HasPrefix(members[2].name, "data.tar") {
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = m.name
		}
		return Paragraph{}, fmt.Errorf("members %q, want debian-binary, control.tar.gz, data.tar.*", names)
	}
	if v, err := readMember(r, members[0], 64); err != nil || string(v) != "2.0\n" {
		return Paragraph{}, fmt.Errorf("debian-binary is %q, want \"2.0\\n\"", v)
	}
	if members[1].name != "control.tar.gz" {
		return Paragraph{}, fmt.Errorf("%s: only control.tar.gz is supported", members[1].name)
	}
	gz, err := readMember(r, members[1], maxControlArchive)
	if err != nil {
		return Paragraph{}, err
	}
	body, err := controlFile(gz)
	if err != nil {
		return Paragraph{}, fmt.Errorf("control.tar.gz: %w", err)
	}
	body = append(bytes.TrimRight(body, "\n"), '\n')
	p, err := ParseParagraph(body)
	if err != nil {
		return Paragraph{}, fmt.Errorf("control: %w", err)
	}
	return p, nil
}

// arMembers lists the members of an ar archive, which must fill size exactly.
func arMembers(r io.ReaderAt, size int64) ([]member, error) {
	magic := make([]byte, 8)
	if _, err := r.ReadAt(magic, 0); err != nil || string(magic) != "!<arch>\n" {
		return nil, errors.New("not an ar archive")
	}
	var out []member
	hdr := make([]byte, 60)
	for off := int64(8); off < size; {
		if _, err := r.ReadAt(hdr, off); err != nil {
			return nil, fmt.Errorf("ar header at %d: %w", off, err)
		}
		if string(hdr[58:60]) != "`\n" {
			return nil, fmt.Errorf("bad ar header at %d", off)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad ar member size at %d", off)
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(hdr[:16])), "/")
		off += 60
		if n > size-off {
			return nil, fmt.Errorf("ar member %s runs past the end of the file", name)
		}
		out = append(out, member{name: name, off: off, size: n})
		off += n + n%2
		if off > size {
			return nil, fmt.Errorf("ar member %s lacks its padding byte", name)
		}
	}
	return out, nil
}

func readMember(r io.ReaderAt, m member, limit int64) ([]byte, error) {
	if m.size > limit {
		return nil, fmt.Errorf("%s: %d bytes, more than %d", m.name, m.size, limit)
	}
	b := make([]byte, m.size)
	if _, err := r.ReadAt(b, m.off); err != nil {
		return nil, fmt.Errorf("%s: %w", m.name, err)
	}
	return b, nil
}

// controlFile returns ./control from a control.tar.gz.
func controlFile(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(io.LimitReader(zr, maxControlTar))
	var control []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(h.Name, "./") != "control" {
			continue
		}
		if h.Typeflag != tar.TypeReg || control != nil {
			return nil, errors.New("control is no single regular file")
		}
		if h.Size > maxControlFile {
			return nil, fmt.Errorf("control: %d bytes, more than %d", h.Size, maxControlFile)
		}
		if control, err = io.ReadAll(tr); err != nil {
			return nil, err
		}
	}
	if control == nil {
		return nil, errors.New("no control file")
	}
	return control, nil
}
