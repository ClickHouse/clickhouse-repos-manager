package release

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Package is one input package.
type Package struct {
	// Name is the file name; the public keys of the package end in it.
	Name string
	Path string
	Sums hashes.Sums
}

// Asset is a public object a release creates: a package or content named metadata.
type Asset struct {
	Key string
	// Path is a package on disk; Body is used when it is empty.
	Path string
	Body []byte
	// Sums of Path; computed from Body otherwise.
	Sums hashes.Sums
	// Opts without Release, which the release sets.
	Opts store.PutOptions
}

// Metadata is one index metadata file a release builds.
type Metadata struct {
	Key  string
	Body []byte
	Opts store.PutOptions
}

// Plan is what a package type stages.
type Plan struct {
	Assets []Asset
	// Metadata is the index metadata the release builds, in commit order.
	Metadata []Metadata
}

// Previous is the snapshot a release builds on.
type Previous struct {
	Name     string
	Manifest Manifest
	bucket   store.Bucket
	files    map[string]File
}

// Get reads key from the previous snapshot, checked against its manifest.
// found is false when the snapshot has no such key.
func (p *Previous) Get(ctx context.Context, key string) (body []byte, found bool, err error) {
	f, ok := p.files[key]
	if !ok {
		return nil, false, nil
	}
	body, obj, err := p.bucket.Get(ctx, SnapshotDir(p.Name)+key)
	if err != nil {
		return nil, false, err
	}
	if obj.ETag != f.ETag || int64(len(body)) != f.Size || hashes.Bytes(body).SHA256 != f.SHA256 {
		return nil, false, fmt.Errorf("snapshot %s: %s does not match its manifest", p.Name, key)
	}
	return body, true, nil
}

func (r *Releaser) previous(ctx context.Context, name string) (*Previous, error) {
	m, err := r.manifest(ctx, name)
	if err != nil {
		return nil, err
	}
	return &Previous{Name: name, Manifest: m, bucket: r.Bucket, files: byKey(m.Files)}, nil
}

// Publish publishes the packages at paths for the release that holds the lock,
// or continues an interrupted publish of it.
func (r *Releaser) Publish(ctx context.Context, paths []string) (Held, error) {
	cur, found, err := r.read(ctx)
	if err != nil {
		return Held{}, err
	}
	if !found {
		return Held{}, errors.New("no lock (not adopted)")
	}
	switch cur.Lock.State {
	case Locked, Staging, Committing, Committed:
	default:
		return Held{}, fmt.Errorf("publish needs the lock held by a release, the state is %s", cur.Lock.State)
	}
	tag, err := ParseTag(cur.Lock.Tag)
	if err != nil {
		return Held{}, err
	}
	byType, all, err := r.packages(paths)
	if err != nil {
		return Held{}, err
	}
	if cur.Lock.State == Locked || cur.Lock.State == Staging {
		if cur, err = r.stage(ctx, cur, tag, byType); err != nil {
			return Held{}, fmt.Errorf("stage: %w", err)
		}
	} else if err := r.checkListed(ctx, cur.Lock.Snapshot, all); err != nil {
		return Held{}, err
	}
	if cur, err = r.commit(ctx, cur); err != nil {
		return Held{}, fmt.Errorf("commit: %w", err)
	}
	if err := r.verify(ctx, cur.Lock, all); err != nil {
		return Held{}, fmt.Errorf("verify: %w", err)
	}
	return cur, nil
}

// packages hashes the inputs once and gives each to the package type that claims it.
func (r *Releaser) packages(paths []string) (map[string][]Package, []Package, error) {
	byType := map[string][]Package{}
	var all []Package
	seen := map[string]bool{}
	for _, p := range paths {
		name := filepath.Base(p)
		if seen[name] {
			return nil, nil, fmt.Errorf("%s: given twice", name)
		}
		seen[name] = true
		var owner PackageType
		for _, t := range r.Types {
			if t.Match(name) {
				owner = t
				break
			}
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("%s: not a package of any known package type", p)
		}
		sums, err := hashes.File(p)
		if err != nil {
			return nil, nil, err
		}
		pkg := Package{Name: name, Path: p, Sums: sums}
		byType[owner.Name()] = append(byType[owner.Name()], pkg)
		all = append(all, pkg)
	}
	return byType, all, nil
}

func (r *Releaser) stage(ctx context.Context, cur Held, tag Tag, byType map[string][]Package) (Held, error) {
	var err error
	if cur.Lock.State == Locked {
		if cur, err = r.setState(ctx, cur, Staging); err != nil {
			return Held{}, err
		}
	}
	l := cur.Lock
	prev, err := r.previous(ctx, l.Previous)
	if err != nil {
		return Held{}, err
	}
	var assets []Asset
	var metas []Metadata
	for _, t := range r.Types {
		pkgs := byType[t.Name()]
		if len(pkgs) == 0 {
			continue
		}
		plan, err := t.Stage(ctx, prev, tag, pkgs)
		if err != nil {
			return Held{}, fmt.Errorf("%s: %w", t.Name(), err)
		}
		for _, a := range plan.Assets {
			if !strings.HasPrefix(a.Key, t.Prefix()) {
				return Held{}, fmt.Errorf("%s: asset %s is outside %s", t.Name(), a.Key, t.Prefix())
			}
			if a.Path == "" {
				a.Sums = hashes.Bytes(a.Body)
			}
			assets = append(assets, a)
		}
		for _, m := range plan.Metadata {
			if !strings.HasPrefix(m.Key, t.Prefix()) {
				return Held{}, fmt.Errorf("%s: metadata %s is outside %s", t.Name(), m.Key, t.Prefix())
			}
			metas = append(metas, m)
		}
	}

	lines := make([]AssetLine, 0, len(assets))
	listed := map[string]AssetLine{}
	for _, a := range assets {
		line := lineOf(a.Key, a.Sums)
		if o, ok := listed[a.Key]; ok && o != line {
			return Held{}, fmt.Errorf("asset %s is planned twice with other content", a.Key)
		}
		listed[a.Key] = line
		lines = append(lines, line)
	}
	if err := r.appendAssets(ctx, l.Snapshot, lines); err != nil {
		return Held{}, err
	}
	for _, a := range assets {
		if err := r.upload(ctx, a, tag.Name); err != nil {
			return Held{}, err
		}
	}

	// No manifest means unfinished while the snapshot is rewritten.
	if err := r.Bucket.Delete(ctx, ManifestKey(l.Snapshot)); err != nil {
		return Held{}, err
	}
	m := Manifest{Schema: Schema, Tag: l.Tag, TagDate: l.TagDate, Files: []File{}}
	built := map[string]bool{}
	for _, md := range metas {
		if built[md.Key] {
			return Held{}, fmt.Errorf("metadata %s is built twice", md.Key)
		}
		built[md.Key] = true
		opts := md.Opts
		opts.Release = tag.Name
		obj, err := r.Bucket.Put(ctx, SnapshotDir(l.Snapshot)+md.Key, md.Body, opts)
		if err != nil {
			return Held{}, err
		}
		m.Files = append(m.Files, File{Key: md.Key, Size: obj.Size, ETag: obj.ETag, SHA256: hashes.Bytes(md.Body).SHA256})
	}
	for _, f := range prev.Manifest.Files {
		if built[f.Key] {
			continue
		}
		if err := r.copyKnown(ctx, SnapshotDir(prev.Name)+f.Key, SnapshotDir(l.Snapshot)+f.Key, f); err != nil {
			return Held{}, err
		}
		m.Files = append(m.Files, f)
	}
	if err := r.checkTypes(ctx, view{b: r.Bucket, dir: SnapshotDir(l.Snapshot)}, m); err != nil {
		return Held{}, fmt.Errorf("snapshot %s: %w", l.Snapshot, err)
	}
	if _, err := r.Bucket.Put(ctx, ManifestKey(l.Snapshot), encode(m), jsonOpts); err != nil {
		return Held{}, err
	}
	return r.setState(ctx, cur, Committing)
}

// upload creates an asset. An existing object with the same content is fine,
// whoever created it; other content is a hard fail.
func (r *Releaser) upload(ctx context.Context, a Asset, tag string) error {
	opts := a.Opts
	opts.Release = tag
	var err error
	if a.Path != "" {
		_, err = r.Bucket.PutFileIfAbsent(ctx, a.Key, a.Path, a.Sums, opts)
	} else {
		_, err = r.Bucket.PutIfAbsent(ctx, a.Key, a.Body, opts)
	}
	if !errors.Is(err, store.ErrPrecondition) {
		return err
	}
	ok, err := r.sameContent(ctx, a.Key, a.Sums)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s exists with other content", a.Key)
	}
	return nil
}

// listedLines returns the assets lines of a package: its public keys end in its name.
func listedLines(lines []AssetLine, name string) []AssetLine {
	var out []AssetLine
	for _, l := range lines {
		if l.Key == name || strings.HasSuffix(l.Key, "/"+name) {
			out = append(out, l)
		}
	}
	return out
}

// checkListed checks, once the commit started, that every package is in assets with its hashes.
func (r *Releaser) checkListed(ctx context.Context, name string, pkgs []Package) error {
	_, lines, _, err := r.readAssets(ctx, name)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		listed := listedLines(lines, p.Name)
		if len(listed) == 0 {
			return fmt.Errorf("%s has no line in assets, and the commit already started", p.Name)
		}
		for _, l := range listed {
			if l != lineOf(l.Key, p.Sums) {
				return fmt.Errorf("%s differs from %s in assets (sha256 %s, assets %s)", p.Name, l.Key, p.Sums.SHA256, l.SHA256)
			}
		}
	}
	return nil
}

// commit copies the snapshot to the live keys in manifest order. Before the
// first copy every live file must still be previous or already the snapshot.
// In state committed it recovers: whatever differs from the snapshot is copied back.
func (r *Releaser) commit(ctx context.Context, cur Held) (Held, error) {
	l := cur.Lock
	m, err := r.manifest(ctx, l.Snapshot)
	if err != nil {
		return Held{}, err
	}
	prev, err := r.manifest(ctx, l.Previous)
	if err != nil {
		return Held{}, err
	}
	prevBy := byKey(prev.Files)
	var todo []File
	var bad []string
	for _, f := range m.Files {
		obj, err := r.Bucket.Head(ctx, f.Key)
		live := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return Held{}, err
		}
		p, inPrev := prevBy[f.Key]
		switch {
		case live && same(obj, f):
		case l.State == Committed, !live && !inPrev, live && inPrev && same(obj, p):
			todo = append(todo, f)
		case !live:
			bad = append(bad, f.Key+": missing")
		default:
			bad = append(bad, fmt.Sprintf("%s: etag %s size %d", f.Key, obj.ETag, obj.Size))
		}
	}
	if len(bad) > 0 {
		return Held{}, fmt.Errorf("live metadata matches neither %s nor %s, clean up by hand:\n  %s", l.Previous, l.Snapshot, strings.Join(bad, "\n  "))
	}
	for _, f := range todo {
		if err := r.copyKnown(ctx, SnapshotDir(l.Snapshot)+f.Key, f.Key, f); err != nil {
			return Held{}, err
		}
	}
	if l.State != Committed {
		return r.setState(ctx, cur, Committed)
	}
	return cur, nil
}

// verify checks the published result through the S3 API.
func (r *Releaser) verify(ctx context.Context, l Lock, pkgs []Package) error {
	m, err := r.manifest(ctx, l.Snapshot)
	if err != nil {
		return err
	}
	if diff, err := r.diffLive(ctx, m); err != nil {
		return err
	} else if len(diff) > 0 {
		return fmt.Errorf("live metadata differs from snapshot %s:\n  %s", l.Snapshot, strings.Join(diff, "\n  "))
	}
	if err := r.checkTypes(ctx, r.Bucket, m); err != nil {
		return err
	}
	// Content named metadata of an earlier staging attempt may never have been
	// uploaded, so only the packages are checked here; Live checks the metadata.
	_, lines, _, err := r.readAssets(ctx, l.Snapshot)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		for _, line := range listedLines(lines, p.Name) {
			obj, err := r.Bucket.Head(ctx, line.Key)
			if err != nil {
				return err
			}
			if obj.Size != line.Size {
				return fmt.Errorf("%s: size %d, assets has %d", line.Key, obj.Size, line.Size)
			}
		}
	}
	return nil
}
