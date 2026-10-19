package deb

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// DateFormat is the Date of Release, in UTC.
const DateFormat = "Mon, 02 Jan 2006 15:04:05 UTC"

// Gzip compresses deterministically: best compression, no name, mtime 0.
func Gzip(b []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// XZ compresses with the default xz settings, which carry no timestamps.
func XZ(b []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := xz.NewWriter(&out)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ArchRelease is main/binary-<arch>/Release, as reprepro writes it.
func ArchRelease(arch string) []byte {
	return fmt.Appendf(nil, "Component: %s\nOrigin: %s\nLabel: %s\nArchitecture: %s\n", config.Component, config.Origin, config.Label, arch)
}

// ByHash is the content named key of an index file, relative to dists/<codename>/.
func ByHash(path, sha512 string) string {
	return path[:strings.LastIndexByte(path, '/')+1] + "by-hash/SHA512/" + sha512
}

// Listed is a file of Release, relative to dists/<codename>/.
type Listed struct {
	Path string
	Sums hashes.Sums
}

// BuildRelease builds dists/<codename>/Release over files, sorted by path.
func BuildRelease(codename string, date time.Time, files []Listed) []byte {
	files = append([]Listed(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var b bytes.Buffer
	fmt.Fprintf(&b, "Origin: %s\nLabel: %s\nCodename: %s\nDate: %s\nArchitectures: %s\nComponents: %s\nAcquire-By-Hash: yes\n",
		config.Origin, config.Label, codename, date.UTC().Format(DateFormat), strings.Join(config.Architectures, " "), config.Component)
	for _, h := range releaseHashes {
		b.WriteString(h.name + ":\n")
		for _, f := range files {
			fmt.Fprintf(&b, " %s %d %s\n", h.get(f.Sums), f.Sums.Size, f.Path)
		}
	}
	return b.Bytes()
}

var releaseHashes = []struct {
	name string
	get  func(hashes.Sums) string
}{
	{"MD5Sum", func(s hashes.Sums) string { return s.MD5 }},
	{"SHA1", func(s hashes.Sums) string { return s.SHA1 }},
	{"SHA256", func(s hashes.Sums) string { return s.SHA256 }},
	{"SHA512", func(s hashes.Sums) string { return s.SHA512 }},
}

// Release is a parsed dists/<codename>/Release.
type Release struct {
	Paragraph
	// Files maps a path to its size and the hashes Release gives; a hash
	// Release lacks stays empty.
	Files map[string]hashes.Sums
	// Paths in the order of the first hash list.
	Paths []string
}

// ParseRelease parses a Release file. Every hash list must name the same
// files with the same sizes.
func ParseRelease(b []byte) (Release, error) {
	p, err := ParseParagraph(b)
	if err != nil {
		return Release{}, err
	}
	r := Release{Paragraph: p, Files: map[string]hashes.Sums{}}
	found := 0
	for _, h := range releaseHashes {
		v, ok := p.Get(h.name)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimPrefix(v, "\n"), "\n") {
			fs := strings.Fields(line)
			if len(fs) != 3 {
				return Release{}, fmt.Errorf("%s: bad line %q", h.name, line)
			}
			size, err := strconv.ParseInt(fs[1], 10, 64)
			if err != nil || size < 0 {
				return Release{}, fmt.Errorf("%s: bad size in %q", h.name, line)
			}
			path := fs[2]
			if seen[path] {
				return Release{}, fmt.Errorf("%s lists %s twice", h.name, path)
			}
			seen[path] = true
			s, known := r.Files[path]
			switch {
			case !known && found > 0:
				return Release{}, fmt.Errorf("%s lists %s, which the other hash lists lack", h.name, path)
			case known && s.Size != size:
				return Release{}, fmt.Errorf("%s gives %s size %d, another hash list %d", h.name, path, size, s.Size)
			case !known:
				r.Paths = append(r.Paths, path)
			}
			s.Size = size
			switch h.name {
			case "MD5Sum":
				s.MD5 = fs[0]
			case "SHA1":
				s.SHA1 = fs[0]
			case "SHA256":
				s.SHA256 = fs[0]
			case "SHA512":
				s.SHA512 = fs[0]
			}
			r.Files[path] = s
		}
		if found > 0 && len(seen) != len(r.Files) {
			return Release{}, fmt.Errorf("%s lists %d files, another hash list %d", h.name, len(seen), len(r.Files))
		}
		found++
	}
	if found == 0 {
		return Release{}, fmt.Errorf("no hash list")
	}
	return r, nil
}

// Match reports whether sums agree with what Release lists for path.
func (r Release) Match(path string, sums hashes.Sums) error {
	want, ok := r.Files[path]
	if !ok {
		return fmt.Errorf("%s is not in Release", path)
	}
	for _, c := range []struct{ name, want, got string }{
		{"size", strconv.FormatInt(want.Size, 10), strconv.FormatInt(sums.Size, 10)},
		{"MD5", want.MD5, sums.MD5}, {"SHA1", want.SHA1, sums.SHA1},
		{"SHA256", want.SHA256, sums.SHA256}, {"SHA512", want.SHA512, sums.SHA512},
	} {
		if c.want != "" && c.want != c.got {
			return fmt.Errorf("%s: %s %s, Release has %s", path, c.name, c.got, c.want)
		}
	}
	return nil
}
