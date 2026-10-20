package deb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/sign"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Prefix is the public key prefix of the DEB repository.
const Prefix = "deb/"

// Content types; every other DEB file has none, as in production.
const (
	typeDeb  = "application/x-deb"
	typeGzip = "application/gzip"
	typeXZ   = "application/x-xz"
)

// Type is the DEB package type.
type Type struct {
	// Signer signs Release; without it nothing can be staged.
	Signer *sign.Signer
	// PublicKey verifies a codename without Public.key, before Signer.
	PublicKey *sign.Verifier
	// Now is the Date of Release; time.Now when nil.
	Now func() time.Time
	// Unsigned stages without Signer: Release only, no InRelease, Release.gpg
	// or Public.key. Only for publish --dry-run.
	Unsigned bool
}

var (
	_ release.PackageType = Type{}
	_ release.Pruner      = Type{}
	_ release.Remover     = Type{}
	_ release.Referencer  = Type{}
)

func (Type) Name() string   { return "deb" }
func (Type) Prefix() string { return Prefix }
func (Type) Indexed() bool  { return true }

func (Type) Parse(name, version string) (pkg, arch string, ok bool, err error) {
	return ParseName(name, version)
}

// Dists is the directory of a codename.
func Dists(codename string) string { return Prefix + "dists/" + codename + "/" }

func archDir(arch string) string { return config.Component + "/binary-" + arch + "/" }

func (t Type) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// built is a package ready for the index.
type built struct {
	pkg    release.Package
	stanza Paragraph
	key    string // the pool key
}

// Check reads every control file and checks it against the file name.
func (t Type) Check(pkgs []release.Package) error {
	if t.Signer == nil && !t.Unsigned {
		return errors.New("no signing key")
	}
	_, err := buildAll(pkgs)
	return err
}

func buildAll(pkgs []release.Package) ([]built, error) {
	out := make([]built, 0, len(pkgs))
	for _, p := range pkgs {
		control, err := ReadControl(p.Path)
		if err != nil {
			return nil, err
		}
		version := strings.TrimSuffix(strings.TrimPrefix(p.Name, p.Pkg+"_"), "_"+p.Arch+suffix)
		if k := KeyOf(control); k != (Key{p.Pkg, version, p.Arch}) {
			return nil, fmt.Errorf("%s: the control file is for %s", p.Name, k)
		}
		src, err := SourceName(control)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		filename := PoolPath(src, p.Name)
		st, err := NewStanza(control, filename, p.Sums)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		out = append(out, built{pkg: p, stanza: st, key: Prefix + filename})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pkg.Name < out[j].pkg.Name })
	return out, nil
}

// indexArchs are the indexes a package of arch goes into.
func indexArchs(arch string) []string {
	if arch == "all" {
		return config.Architectures
	}
	return []string{arch}
}

// Stage uploads the pool files once and rebuilds the indexes of every channel
// of the tag on top of previous.
func (t Type) Stage(ctx context.Context, prev *release.Previous, tag release.Tag, pkgs []release.Package) (release.Plan, error) {
	if t.Signer == nil && !t.Unsigned {
		return release.Plan{}, errors.New("no signing key")
	}
	all, err := buildAll(pkgs)
	if err != nil {
		return release.Plan{}, err
	}
	var plan release.Plan
	for _, b := range all {
		plan.Assets = append(plan.Assets, release.Asset{Key: b.key, Path: b.pkg.Path, Sums: b.pkg.Sums, Opts: store.PutOptions{ContentType: typeDeb}})
	}
	for _, cn := range tag.Channels {
		idx, err := indexes(ctx, prev, cn, false)
		if err != nil {
			return release.Plan{}, err
		}
		for _, b := range all {
			for _, arch := range indexArchs(b.pkg.Arch) {
				if _, err := idx[arch].Add(b.stanza); err != nil {
					return release.Plan{}, fmt.Errorf("%s %s: %w", cn, arch, err)
				}
			}
		}
		if err := t.build(cn, idx, &plan); err != nil {
			return release.Plan{}, err
		}
	}
	return plan, nil
}

// indexes reads the Packages of a codename from previous; a missing one is
// empty unless must is set.
func indexes(ctx context.Context, prev *release.Previous, codename string, must bool) (map[string]*Index, error) {
	idx := map[string]*Index{}
	for _, arch := range config.Architectures {
		key := Dists(codename) + archDir(arch) + "Packages"
		body, found, err := prev.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found && must {
			return nil, fmt.Errorf("%s: no such index", key)
		}
		if idx[arch], err = ParseIndex(body); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return idx, nil
}

// build adds the index metadata of a codename to plan in commit order, and
// the by-hash copies of the indexes as assets.
func (t Type) build(codename string, idx map[string]*Index, plan *release.Plan) error {
	dir := Dists(codename)
	signed := t.Signer != nil
	if signed {
		pub, err := t.Signer.PublicKey()
		if err != nil {
			return err
		}
		plan.Metadata = append(plan.Metadata, release.Metadata{Key: dir + "Public.key", Body: pub})
	}
	var listed []Listed
	for _, arch := range config.Architectures {
		packages := idx[arch].Bytes()
		gz, err := Gzip(packages)
		if err != nil {
			return err
		}
		xz, err := XZ(packages)
		if err != nil {
			return err
		}
		for _, f := range []struct {
			name, contentType string
			body              []byte
		}{
			{"Packages", "", packages},
			{"Packages.gz", typeGzip, gz},
			{"Packages.xz", typeXZ, xz},
			{"Release", "", ArchRelease(arch)},
		} {
			rel := archDir(arch) + f.name
			opts := store.PutOptions{ContentType: f.contentType}
			sums := hashes.Bytes(f.body)
			plan.Metadata = append(plan.Metadata, release.Metadata{Key: dir + rel, Body: f.body, Opts: opts})
			listed = append(listed, Listed{rel, sums})
			if f.name != "Release" {
				plan.Assets = append(plan.Assets, release.Asset{Key: dir + ByHash(rel, sums.SHA512), Body: f.body, Opts: opts})
			}
		}
	}
	rel := BuildRelease(codename, t.now(), listed)
	if !signed {
		plan.Metadata = append(plan.Metadata, release.Metadata{Key: dir + "Release", Body: rel})
		return nil
	}
	inRelease, err := t.Signer.Clearsign(rel)
	if err != nil {
		return err
	}
	gpg, err := t.Signer.DetachSign(rel)
	if err != nil {
		return err
	}
	plan.Metadata = append(plan.Metadata,
		release.Metadata{Key: dir + "Release", Body: rel},
		release.Metadata{Key: dir + "Release.gpg", Body: gpg},
		release.Metadata{Key: dir + "InRelease", Body: inRelease})
	return nil
}

// Referenced returns the by-hash keys that the live Release files name.
func (t Type) Referenced(ctx context.Context, b store.Bucket) ([]release.File, error) {
	var out []release.File
	for _, cn := range config.Codenames {
		empty, err := emptyPrefix(ctx, b, Dists(cn))
		if err != nil {
			return nil, err
		}
		if empty {
			continue
		}
		c, err := t.Chain(ctx, b, cn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cn, err)
		}
		out = append(out, ByHashFiles(cn, c.Release)...)
	}
	return out, nil
}

// ByHashFiles are the by-hash keys a Release names, with the size and SHA256
// of the index each copies. Only a Release with Acquire-By-Hash names any:
// reprepro wrote none.
func ByHashFiles(codename string, r Release) []release.File {
	if r.Value("Acquire-By-Hash") != "yes" {
		return nil
	}
	var out []release.File
	for _, p := range r.Paths {
		if s := r.Files[p]; strings.HasPrefix(path.Base(p), "Packages") && s.SHA512 != "" {
			out = append(out, release.File{Key: Dists(codename) + ByHash(p, s.SHA512), Size: s.Size, SHA256: s.SHA256})
		}
	}
	return out
}

// Live verifies every codename that has objects and returns its index metadata.
func (t Type) Live(ctx context.Context, b store.Bucket) ([]release.File, error) {
	var files []release.File
	for _, cn := range config.Codenames {
		empty, err := emptyPrefix(ctx, b, Dists(cn))
		if err != nil {
			return nil, err
		}
		if empty {
			continue
		}
		c, err := t.Chain(ctx, b, cn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cn, err)
		}
		files = append(files, c.Files...)
	}
	return files, nil
}

var errStop = errors.New("stop")

func emptyPrefix(ctx context.Context, b store.Bucket, prefix string) (bool, error) {
	empty := true
	err := b.List(ctx, prefix, func(store.Object) error { empty = false; return errStop })
	if err != nil && !errors.Is(err, errStop) {
		return false, err
	}
	return empty, nil
}

// Codename is the verified index metadata of a codename.
type Codename struct {
	Name    string
	Release Release
	// Indexes are the parsed Packages per architecture.
	Indexes map[string]*Index
	// Files are the index metadata files, with SHA256 of the verified bytes.
	Files []release.File
	// Signers are the fingerprints of the keys that signed InRelease and Release.gpg.
	Signers []string
	// Signature is why the signatures do not verify; nil when they do.
	Signature error
}

// Chain verifies a codename completely, as Read does; a signature that does
// not verify is an error.
func (t Type) Chain(ctx context.Context, b store.Bucket, codename string) (*Codename, error) {
	c, err := t.Read(ctx, b, codename)
	if err != nil {
		return nil, err
	}
	if c.Signature != nil {
		return nil, c.Signature
	}
	return c, nil
}

// Read checks a codename: Release, every file it lists by size and hashes, a
// parseable Packages for every architecture, and, kept in Signature, the
// InRelease and Release.gpg signatures and InRelease equal to Release.
func (t Type) Read(ctx context.Context, b store.Bucket, codename string) (*Codename, error) {
	dir := Dists(codename)
	c := &Codename{Name: codename, Indexes: map[string]*Index{}}
	get := func(name string, must bool) ([]byte, bool, error) {
		body, obj, err := b.Get(ctx, dir+name)
		if errors.Is(err, store.ErrNotFound) {
			if must {
				return nil, false, fmt.Errorf("%s is missing", dir+name)
			}
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		c.Files = append(c.Files, release.File{Key: dir + name, Size: obj.Size, ETag: obj.ETag, SHA256: hashes.Bytes(body).SHA256})
		return body, true, nil
	}
	pub, hasPub, err := get("Public.key", false)
	if err != nil {
		return nil, err
	}
	rel, _, err := get("Release", true)
	if err != nil {
		return nil, err
	}
	inRelease, hasIn, err := get("InRelease", false)
	if err != nil {
		return nil, err
	}
	gpg, hasGpg, err := get("Release.gpg", false)
	if err != nil {
		return nil, err
	}
	var v *sign.Verifier
	switch {
	case hasPub:
		if v, err = sign.LoadPublic(pub); err != nil {
			c.Signature = fmt.Errorf("Public.key: %w", err)
		}
	case t.PublicKey != nil:
		v = t.PublicKey
	case t.Signer != nil:
		v = t.Signer.Verifier()
	default:
		c.Signature = errors.New("no Public.key, and no public or signing key to verify with")
	}
	if c.Signature == nil {
		c.Signature = c.verify(v, rel, inRelease, hasIn, gpg, hasGpg)
	}
	if c.Release, err = ParseRelease(rel); err != nil {
		return nil, fmt.Errorf("Release: %w", err)
	}
	if got := c.Release.Value("Codename"); got != codename {
		return nil, fmt.Errorf("Release is for codename %q", got)
	}
	for _, arch := range config.Architectures {
		if _, ok := c.Release.Files[archDir(arch)+"Packages"]; !ok {
			return nil, fmt.Errorf("Release lists no %sPackages", archDir(arch))
		}
	}
	for _, p := range c.Release.Paths {
		body, _, err := get(p, true)
		if err != nil {
			return nil, err
		}
		if err := c.Release.Match(p, hashes.Bytes(body)); err != nil {
			return nil, err
		}
		if path.Base(p) != "Packages" {
			continue
		}
		arch := strings.TrimPrefix(path.Dir(p), config.Component+"/binary-")
		if c.Indexes[arch], err = ParseIndex(body); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return c, nil
}

// verify checks the signatures of Release with v and records who made them.
func (c *Codename) verify(v *sign.Verifier, rel, inRelease []byte, hasIn bool, gpg []byte, hasGpg bool) error {
	dir := Dists(c.Name)
	if !hasIn {
		return fmt.Errorf("%sInRelease is missing", dir)
	}
	if !hasGpg {
		return fmt.Errorf("%sRelease.gpg is missing", dir)
	}
	plain, inSigner, err := v.VerifyClearsigned(inRelease)
	if err != nil {
		return fmt.Errorf("InRelease: %w", err)
	}
	if !bytes.Equal(bytes.TrimRight(plain, "\n"), bytes.TrimRight(rel, "\n")) {
		return errors.New("InRelease signs other text than Release")
	}
	gpgSigner, err := v.VerifyDetached(rel, gpg)
	if err != nil {
		return fmt.Errorf("Release.gpg: %w", err)
	}
	c.Signers = []string{inSigner}
	if gpgSigner != inSigner {
		c.Signers = append(c.Signers, gpgSigner)
	}
	return nil
}

// ByHashKeys are the by-hash keys of the indexes a Release lists.
func ByHashKeys(codename string, r Release) []string {
	var keys []string
	for _, f := range ByHashFiles(codename, r) {
		keys = append(keys, f.Key)
	}
	return keys
}

// Prune deletes every by-hash file that the Release of no retained snapshot names.
func (Type) Prune(ctx context.Context, b store.Bucket, retained []*release.Previous) (release.Pruned, error) {
	referenced := map[string]bool{}
	for _, prev := range retained {
		for _, cn := range config.Codenames {
			body, found, err := prev.Get(ctx, Dists(cn)+"Release")
			if err != nil {
				return release.Pruned{}, err
			}
			if !found {
				continue
			}
			r, err := ParseRelease(body)
			if err != nil {
				return release.Pruned{}, fmt.Errorf("%s %s: %w", prev.Name, cn, err)
			}
			for _, k := range ByHashKeys(cn, r) {
				referenced[k] = true
			}
		}
	}
	var keys []string
	err := b.List(ctx, Prefix+"dists/", func(o store.Object) error {
		if strings.Contains(o.Key, "/by-hash/") && !referenced[o.Key] {
			keys = append(keys, o.Key)
		}
		return nil
	})
	if err != nil {
		return release.Pruned{}, err
	}
	var res release.Pruned
	for _, key := range keys {
		switch r, err := b.DeleteStamped(ctx, key); {
		case err != nil:
			return release.Pruned{}, err
		case r == store.Deleted:
			res.Deleted = append(res.Deleted, key)
		case r == store.NotStamped:
			res.NotStamped = append(res.NotStamped, key)
		}
	}
	return res, nil
}

// Remove rebuilds the indexes of one codename without the selected packages.
func (t Type) Remove(ctx context.Context, prev *release.Previous, sel release.Selector) (release.Plan, error) {
	if t.Signer == nil {
		return release.Plan{}, errors.New("no signing key")
	}
	if !slices.Contains(config.Codenames, sel.Channel) {
		return release.Plan{}, fmt.Errorf("unknown channel %q", sel.Channel)
	}
	if sel.Version == "" {
		return release.Plan{}, errors.New("no version")
	}
	if sel.Arch != "" && !archs[sel.Arch] {
		return release.Plan{}, fmt.Errorf("unknown architecture %q", sel.Arch)
	}
	idx, err := indexes(ctx, prev, sel.Channel, true)
	if err != nil {
		return release.Plan{}, err
	}
	match := func(p Paragraph) bool {
		k := KeyOf(p)
		return k.Version == sel.Version && (len(sel.Packages) == 0 || slices.Contains(sel.Packages, k.Package)) &&
			(sel.Arch == "" || k.Architecture == sel.Arch)
	}
	removed := map[string]bool{}
	for _, arch := range config.Architectures {
		for _, p := range idx[arch].Remove(match) {
			removed[p.Value("Filename")] = true
		}
	}
	if len(removed) == 0 {
		return release.Plan{}, fmt.Errorf("%s has no package that matches %+v", sel.Channel, sel)
	}
	var plan release.Plan
	if err := t.build(sel.Channel, idx, &plan); err != nil {
		return release.Plan{}, err
	}
	referenced := map[string]bool{}
	for _, cn := range config.Codenames {
		cur := idx
		if cn != sel.Channel {
			if cur, err = indexes(ctx, prev, cn, false); err != nil {
				return release.Plan{}, err
			}
		}
		for _, x := range cur {
			for _, p := range x.Stanzas() {
				referenced[p.Value("Filename")] = true
			}
		}
	}
	for _, f := range slices.Sorted(maps.Keys(removed)) {
		if !referenced[f] {
			plan.Unreferenced = append(plan.Unreferenced, Prefix+f)
		}
	}
	return plan, nil
}
