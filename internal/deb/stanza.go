package deb

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// Paragraph is one deb822 stanza: its bytes as they are, with the fields parsed.
type Paragraph struct {
	raw    []byte
	fields []field
}

type field struct{ name, value string }

// ParseParagraph parses a single stanza: "Name: value" lines with
// continuation lines starting with a space or tab, ending in one newline.
func ParseParagraph(b []byte) (Paragraph, error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return Paragraph{}, errors.New("stanza does not end with a newline")
	}
	p := Paragraph{raw: b}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b[:len(b)-1]), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			return Paragraph{}, errors.New("blank line inside a stanza")
		case line[0] == ' ' || line[0] == '\t':
			if len(p.fields) == 0 {
				return Paragraph{}, errors.New("stanza starts with a continuation line")
			}
			f := &p.fields[len(p.fields)-1]
			f.value += "\n" + line
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok || name == "" || strings.ContainsAny(name, " \t") {
				return Paragraph{}, fmt.Errorf("line %q is no field", line)
			}
			if key := strings.ToLower(name); seen[key] {
				return Paragraph{}, fmt.Errorf("field %s given twice", name)
			} else {
				seen[key] = true
			}
			p.fields = append(p.fields, field{name, strings.TrimSpace(value)})
		}
	}
	return p, nil
}

// Bytes returns the stanza as it was parsed.
func (p Paragraph) Bytes() []byte { return p.raw }

// Get returns a field value, its continuation lines included; names are case insensitive.
func (p Paragraph) Get(name string) (string, bool) {
	for _, f := range p.fields {
		if strings.EqualFold(f.name, name) {
			return f.value, true
		}
	}
	return "", false
}

// Value returns a field value, empty when it is absent.
func (p Paragraph) Value(name string) string {
	v, _ := p.Get(name)
	return v
}

// Fields returns the field names in order.
func (p Paragraph) Fields() []string {
	names := make([]string, len(p.fields))
	for i, f := range p.fields {
		names[i] = f.name
	}
	return names
}

// Key identifies a package in an index.
type Key struct{ Package, Version, Architecture string }

func (k Key) String() string { return k.Package + "_" + k.Version + "_" + k.Architecture }

// KeyOf returns the key of a stanza.
func KeyOf(p Paragraph) Key {
	return Key{p.Value("Package"), p.Value("Version"), p.Value("Architecture")}
}

// indexFields are added to the control paragraph in the index, in this order.
var indexFields = []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256", "SHA512"}

// NewStanza returns the index stanza of a package: its control paragraph as it
// is, then Filename, Size, MD5sum, SHA1, SHA256 and SHA512.
func NewStanza(control Paragraph, filename string, sums hashes.Sums) (Paragraph, error) {
	for _, name := range indexFields {
		if _, ok := control.Get(name); ok {
			return Paragraph{}, fmt.Errorf("the control file has the index field %s", name)
		}
	}
	var b bytes.Buffer
	b.Write(control.raw)
	for i, v := range []string{filename, strconv.FormatInt(sums.Size, 10), sums.MD5, sums.SHA1, sums.SHA256, sums.SHA512} {
		fmt.Fprintf(&b, "%s: %s\n", indexFields[i], v)
	}
	return ParseParagraph(b.Bytes())
}

// Index is a Packages file as a set of stanzas. The bytes of every stanza stay
// as they are: adding appends to the old bytes, removing cuts exactly the
// removed stanzas out of them.
type Index struct {
	head  []byte // blank lines before the first stanza
	items []item
}

type item struct {
	p    Paragraph
	tail []byte // the blank lines after the stanza
}

// ParseIndex parses a Packages file. A key or a Filename given twice is an error.
func ParseIndex(b []byte) (*Index, error) {
	x := &Index{}
	i := 0
	for i < len(b) && b[i] == '\n' {
		i++
	}
	x.head = b[:i]
	for i < len(b) {
		end := bytes.Index(b[i:], []byte("\n\n"))
		if end < 0 {
			end = len(b)
		} else {
			end += i + 1
		}
		raw := b[i:end]
		if raw[len(raw)-1] != '\n' {
			raw = append(raw[:len(raw):len(raw)], '\n')
		}
		p, err := ParseParagraph(raw)
		if err != nil {
			return nil, fmt.Errorf("stanza %d: %w", len(x.items)+1, err)
		}
		j := end
		for j < len(b) && b[j] == '\n' {
			j++
		}
		x.items = append(x.items, item{p: p, tail: b[end:j]})
		if end == len(b) {
			// No newline at the end of the file: Bytes gives it back without one.
			x.items[len(x.items)-1].p.raw = b[i:end]
		}
		i = j
	}
	keys := map[Key]bool{}
	files := map[string]bool{}
	for _, it := range x.items {
		k := KeyOf(it.p)
		if k.Package == "" || k.Version == "" || k.Architecture == "" {
			return nil, fmt.Errorf("stanza %s lacks Package, Version or Architecture", k)
		}
		if keys[k] {
			return nil, fmt.Errorf("%s is in the index twice", k)
		}
		keys[k] = true
		f := it.p.Value("Filename")
		if f == "" {
			return nil, fmt.Errorf("%s has no Filename", k)
		}
		if files[f] {
			return nil, fmt.Errorf("%s is in the index twice", f)
		}
		files[f] = true
	}
	return x, nil
}

// Len is the number of stanzas.
func (x *Index) Len() int { return len(x.items) }

// Stanzas returns the stanzas in order.
func (x *Index) Stanzas() []Paragraph {
	out := make([]Paragraph, len(x.items))
	for i, it := range x.items {
		out[i] = it.p
	}
	return out
}

// Add puts a new stanza into the index. A stanza with the same key, Filename,
// Size and the hashes the old one has is already there: Add returns false.
// The same key or Filename with anything else is an error.
func (x *Index) Add(p Paragraph) (bool, error) {
	k, file := KeyOf(p), p.Value("Filename")
	for _, it := range x.items {
		ok, of := KeyOf(it.p), it.p.Value("Filename")
		if ok != k && of != file {
			continue
		}
		if ok != k || of != file {
			return false, fmt.Errorf("%s (%s) conflicts with %s (%s) in the index", k, file, ok, of)
		}
		for _, name := range []string{"Size", "MD5sum", "SHA1", "SHA256", "SHA512"} {
			if old, has := it.p.Get(name); has && old != p.Value(name) {
				return false, fmt.Errorf("%s is in the index with %s %s, the package has %s", k, name, old, p.Value(name))
			}
		}
		if _, has := it.p.Get("Size"); !has {
			return false, fmt.Errorf("%s is in the index without Size", k)
		}
		return false, nil
	}
	if n := len(x.items); n > 0 {
		last := &x.items[n-1]
		switch {
		case len(last.tail) > 0:
		case bytes.HasSuffix(last.p.raw, []byte("\n")):
			last.tail = []byte("\n")
		default:
			last.tail = []byte("\n\n")
		}
	}
	x.items = append(x.items, item{p: p, tail: []byte("\n")})
	return true, nil
}

// Remove takes the stanzas for which match is true out of the index and
// returns them; the bytes of all other stanzas stay.
func (x *Index) Remove(match func(Paragraph) bool) []Paragraph {
	var gone []Paragraph
	kept := x.items[:0:0]
	for _, it := range x.items {
		if match(it.p) {
			gone = append(gone, it.p)
		} else {
			kept = append(kept, it)
		}
	}
	x.items = kept
	return gone
}

// Bytes returns the Packages file.
func (x *Index) Bytes() []byte {
	b := append([]byte(nil), x.head...)
	for _, it := range x.items {
		b = append(b, it.p.raw...)
		b = append(b, it.tail...)
	}
	return b
}
