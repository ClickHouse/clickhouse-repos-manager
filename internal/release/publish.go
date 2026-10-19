package release

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Package is one input package.
type Package struct {
	// Name is the file name; the public keys of the package end in it.
	Name string
	// Pkg and Arch are parsed from Name by its package type.
	Pkg, Arch string
	Path      string
	Sums      hashes.Sums
}

// Asset is a public object a release creates: a package or content named metadata.
type Asset struct {
	Key string
	// Path is a package on disk; Body is used when it is empty.
	Path string
	Body []byte
	// CopyOf is the key of another asset of the plan; the asset is a server
	// side copy of it, and Path and Body are not used.
	CopyOf string
	// Sums of Path; computed from Body, or taken from CopyOf, otherwise.
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
	// Unreferenced is set only by Remover.Remove.
	Unreferenced []string
}

// Previous is the snapshot a release builds on.
type Previous struct {
	Name     string
	Manifest Manifest
	bucket   store.Bucket
	files    map[string]File
	live     bool
}

// LivePrevious reads the live keys of files, as a package type's Live
// returned them, instead of a snapshot; every read is checked against files.
func LivePrevious(b store.Bucket, files []File) *Previous {
	return &Previous{Name: "live", Manifest: Manifest{Schema: Schema, Files: files}, bucket: b, files: byKey(files), live: true}
}

// Get reads key from the previous snapshot, checked against its manifest.
// found is false when the snapshot has no such key.
func (p *Previous) Get(ctx context.Context, key string) (body []byte, found bool, err error) {
	f, ok := p.files[key]
	if !ok {
		return nil, false, nil
	}
	full := key
	if !p.live {
		full = SnapshotDir(p.Name) + key
	}
	body, obj, err := p.bucket.Get(ctx, full)
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

// Published is the result of Publish.
type Published struct {
	Held Held
	// PublicChecked is the number of package keys checked at PublicURL; 0 without it.
	PublicChecked int
}

// Publish publishes the packages at paths for the release that holds the lock,
// or continues an interrupted publish of it. only, when not empty, names the
// package types to publish; the file set rule then covers just them.
func (r *Releaser) Publish(ctx context.Context, paths, only []string) (Published, error) {
	cur, found, err := r.read(ctx)
	if err != nil {
		return Published{}, err
	}
	if !found {
		return Published{}, errors.New("no lock (not adopted)")
	}
	switch cur.Lock.State {
	case Locked, Staging, Committing, Committed:
	default:
		return Published{}, fmt.Errorf("publish needs the lock held by a release, the state is %s", cur.Lock.State)
	}
	tag, err := ParseTag(cur.Lock.Tag)
	if err != nil {
		return Published{}, err
	}
	byType, all, err := r.packages(ctx, paths, tag.Version(), only)
	if err != nil {
		return Published{}, err
	}
	if cur.Lock.State == Locked || cur.Lock.State == Staging {
		if cur, err = r.stage(ctx, cur, tag, byType); err != nil {
			return Published{}, fmt.Errorf("stage: %w", err)
		}
	} else if err := r.checkListed(ctx, cur.Lock.Snapshot, all); err != nil {
		return Published{}, err
	}
	if cur, err = r.commit(ctx, cur); err != nil {
		return Published{}, fmt.Errorf("commit: %w", err)
	}
	lines, err := r.verify(ctx, cur.Lock, all)
	if err != nil {
		return Published{}, fmt.Errorf("verify: %w", err)
	}
	n, err := r.checkPublic(ctx, lines)
	if err != nil {
		return Published{}, fmt.Errorf("public URL check: %w", err)
	}
	return Published{Held: cur, PublicChecked: n}, nil
}

// packages checks the inputs before anything is written: names, version, the
// file set rule and each package type's own check. It hashes every file once.
func (r *Releaser) packages(ctx context.Context, paths []string, version string, only []string) (map[string][]Package, []Package, error) {
	selected, err := r.selected(only)
	if err != nil {
		return nil, nil, err
	}
	var all []Package
	owner := map[string]PackageType{}
	for _, p := range paths {
		name := filepath.Base(p)
		if owner[name] != nil {
			return nil, nil, fmt.Errorf("%s: given twice", name)
		}
		var pkg Package
		for _, t := range r.Types {
			pk, arch, ok, err := t.Parse(name, version)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", p, err)
			}
			if ok {
				owner[name], pkg = t, Package{Name: name, Pkg: pk, Arch: arch, Path: p}
				break
			}
		}
		if owner[name] == nil {
			return nil, nil, fmt.Errorf("%s: not a package of any known package type", p)
		}
		if !selected[owner[name].Name()] {
			return nil, nil, fmt.Errorf("%s: a %s package, but only %s are published", p, owner[name].Name(), strings.Join(only, ", "))
		}
		all = append(all, pkg)
	}
	group := func() map[string][]Package {
		m := map[string][]Package{}
		for _, p := range all {
			m[owner[p.Name].Name()] = append(m[owner[p.Name].Name()], p)
		}
		return m
	}
	if err := fileSet(r.Types, selected, group()); err != nil {
		return nil, nil, err
	}

	err = r.each(ctx, len(all), func(_ context.Context, i int) error {
		sums, err := hashes.File(all[i].Path)
		all[i].Sums = sums
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	byType := group()
	for _, t := range r.Types {
		if pkgs := byType[t.Name()]; len(pkgs) > 0 {
			if err := t.Check(pkgs); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", t.Name(), err)
			}
		}
	}
	return byType, all, nil
}

// selected returns the names of the package types to publish.
func (r *Releaser) selected(only []string) (map[string]bool, error) {
	known := map[string]bool{}
	for _, t := range r.Types {
		known[t.Name()] = true
	}
	if len(only) == 0 {
		return known, nil
	}
	sel := map[string]bool{}
	for _, name := range only {
		if !known[name] {
			return nil, fmt.Errorf("package type %s not in this build", name)
		}
		sel[name] = true
	}
	return sel, nil
}

// fileSet checks that every selected package type has packages, that each
// package comes for exactly amd64 and arm64 or exactly for all, and that all
// package types have the same (package, architecture) pairs.
func fileSet(types []PackageType, selected map[string]bool, byType map[string][]Package) error {
	sets := map[string]map[string]bool{}
	union := map[string]bool{}
	var errs []string
	for _, t := range types {
		if !selected[t.Name()] {
			continue
		}
		set := map[string]bool{}
		archs := map[string]map[string]bool{}
		for _, p := range byType[t.Name()] {
			set[p.Pkg+" "+p.Arch] = true
			union[p.Pkg+" "+p.Arch] = true
			if archs[p.Pkg] == nil {
				archs[p.Pkg] = map[string]bool{}
			}
			archs[p.Pkg][p.Arch] = true
		}
		if len(set) == 0 {
			errs = append(errs, "no "+t.Name()+" packages")
		}
		for _, pkg := range slices.Sorted(maps.Keys(archs)) {
			a := archs[pkg]
			if !(len(a) == 2 && a["amd64"] && a["arm64"]) && !(len(a) == 1 && a["all"]) {
				errs = append(errs, fmt.Sprintf("%s %s comes for %s, want amd64 and arm64, or all", t.Name(), pkg, strings.Join(slices.Sorted(maps.Keys(a)), " and ")))
			}
		}
		sets[t.Name()] = set
	}
	for _, t := range types {
		set, ok := sets[t.Name()]
		if !ok || len(set) == 0 {
			continue
		}
		var missing []string
		for pair := range union {
			if !set[pair] {
				missing = append(missing, pair)
			}
		}
		sort.Strings(missing)
		for _, pair := range missing {
			errs = append(errs, fmt.Sprintf("%s lacks %s", t.Name(), pair))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("the file sets differ:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
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
		planned := map[string]Asset{}
		for _, a := range plan.Assets {
			if !strings.HasPrefix(a.Key, t.Prefix()) {
				return Held{}, fmt.Errorf("%s: asset %s is outside %s", t.Name(), a.Key, t.Prefix())
			}
			switch {
			case a.CopyOf != "":
				src, ok := planned[a.CopyOf]
				if !ok || src.CopyOf != "" {
					return Held{}, fmt.Errorf("%s: asset %s copies %s, which is no uploaded asset planned before it", t.Name(), a.Key, a.CopyOf)
				}
				a.Sums = src.Sums
			case a.Path == "":
				a.Sums = hashes.Bytes(a.Body)
			}
			planned[a.Key] = a
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
	if err := r.uploadAll(ctx, assets, tag.Name); err != nil {
		return Held{}, err
	}

	// No manifest means unfinished while the snapshot is rewritten.
	if err := r.Bucket.Delete(ctx, ManifestKey(l.Snapshot)); err != nil {
		return Held{}, err
	}
	built := map[string]bool{}
	for _, md := range metas {
		if built[md.Key] {
			return Held{}, fmt.Errorf("metadata %s is built twice", md.Key)
		}
		built[md.Key] = true
	}
	files := make([]File, len(metas))
	err = r.each(ctx, len(metas), func(ctx context.Context, i int) error {
		md := metas[i]
		opts := md.Opts
		opts.Release = tag.Name
		obj, err := r.Bucket.Put(ctx, SnapshotDir(l.Snapshot)+md.Key, md.Body, opts)
		files[i] = File{Key: md.Key, Size: obj.Size, ETag: obj.ETag, SHA256: hashes.Bytes(md.Body).SHA256}
		return err
	})
	if err != nil {
		return Held{}, err
	}
	var kept []File
	for _, f := range prev.Manifest.Files {
		if !built[f.Key] {
			kept = append(kept, f)
		}
	}
	err = r.each(ctx, len(kept), func(ctx context.Context, i int) error {
		return r.copyKnown(ctx, SnapshotDir(prev.Name)+kept[i].Key, SnapshotDir(l.Snapshot)+kept[i].Key, kept[i])
	})
	if err != nil {
		return Held{}, err
	}
	m := Manifest{Schema: Schema, Tag: l.Tag, TagDate: l.TagDate, Files: append(files, kept...)}
	if err := r.checkTypes(ctx, view{b: r.Bucket, dir: SnapshotDir(l.Snapshot)}, m); err != nil {
		return Held{}, fmt.Errorf("snapshot %s: %w", l.Snapshot, err)
	}
	if _, err := r.Bucket.Put(ctx, ManifestKey(l.Snapshot), encode(m), jsonOpts); err != nil {
		return Held{}, err
	}
	return r.setState(ctx, cur, Committing)
}

// uploadAll uploads the assets in parallel, then makes the copies from them.
func (r *Releaser) uploadAll(ctx context.Context, assets []Asset, tag string) error {
	var uploads, copies []int
	for i, a := range assets {
		if a.CopyOf == "" {
			uploads = append(uploads, i)
		} else {
			copies = append(copies, i)
		}
	}
	objs := make([]store.Object, len(assets))
	err := r.each(ctx, len(uploads), func(ctx context.Context, j int) error {
		i := uploads[j]
		obj, err := r.upload(ctx, assets[i], tag)
		objs[i] = obj
		return err
	})
	if err != nil {
		return err
	}
	src := map[string]store.Object{}
	for _, i := range uploads {
		src[assets[i].Key] = objs[i]
	}
	return r.each(ctx, len(copies), func(ctx context.Context, j int) error {
		a := assets[copies[j]]
		opts := a.Opts
		opts.Release = tag
		_, err := r.Bucket.CopyIfAbsent(ctx, a.CopyOf, src[a.CopyOf].ETag, a.Key, a.Sums, opts)
		_, err = r.checkWritten(ctx, a, tag, err)
		return err
	})
}

// upload creates an asset and returns the object as checked.
func (r *Releaser) upload(ctx context.Context, a Asset, tag string) (store.Object, error) {
	opts := a.Opts
	opts.Release = tag
	var err error
	if a.Path != "" {
		_, err = r.Bucket.PutFileIfAbsent(ctx, a.Key, a.Path, a.Sums, opts)
	} else {
		_, err = r.Bucket.PutIfAbsent(ctx, a.Key, a.Body, opts)
	}
	return r.checkWritten(ctx, a, tag, err)
}

// checkWritten checks an asset by HEAD after its write. With err nil the
// write created it, and it must carry our stamp. ErrPrecondition means it
// exists: equal content by stored metadata is fine whoever created it.
func (r *Releaser) checkWritten(ctx context.Context, a Asset, tag string, err error) (store.Object, error) {
	stamp := tag
	if errors.Is(err, store.ErrPrecondition) {
		stamp = ""
	} else if err != nil {
		return store.Object{}, err
	}
	obj, err := r.Bucket.Head(ctx, a.Key)
	if err != nil {
		return store.Object{}, err
	}
	return obj, checkAsset(obj, a.Sums, stamp)
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
	objs, found, err := r.heads(ctx, m.Files)
	if err != nil {
		return Held{}, err
	}
	var todo []File
	var bad []string
	for i, f := range m.Files {
		obj, live := objs[i], found[i]
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

// verify checks the published result through the S3 API and returns the
// assets lines of the packages.
func (r *Releaser) verify(ctx context.Context, l Lock, pkgs []Package) ([]AssetLine, error) {
	m, err := r.manifest(ctx, l.Snapshot)
	if err != nil {
		return nil, err
	}
	if diff, err := r.diffLive(ctx, m); err != nil {
		return nil, err
	} else if len(diff) > 0 {
		return nil, fmt.Errorf("live metadata differs from snapshot %s:\n  %s", l.Snapshot, strings.Join(diff, "\n  "))
	}
	if err := r.checkTypes(ctx, r.Bucket, m); err != nil {
		return nil, err
	}
	// Content named metadata of an earlier staging attempt may never have been
	// uploaded, so only the packages are checked here; Live checks the metadata.
	_, all, _, err := r.readAssets(ctx, l.Snapshot)
	if err != nil {
		return nil, err
	}
	var lines []AssetLine
	for _, p := range pkgs {
		lines = append(lines, listedLines(all, p.Name)...)
	}
	err = r.each(ctx, len(lines), func(ctx context.Context, i int) error {
		obj, err := r.Bucket.Head(ctx, lines[i].Key)
		if err != nil {
			return err
		}
		if obj.Size != lines[i].Size {
			return fmt.Errorf("%s: size %d, assets has %d", lines[i].Key, obj.Size, lines[i].Size)
		}
		return nil
	})
	return lines, err
}

// checkPublic compares the size of every key at PublicURL with its assets
// line and returns the number of keys checked; nothing without PublicURL.
func (r *Releaser) checkPublic(ctx context.Context, lines []AssetLine) (int, error) {
	if r.PublicURL == "" {
		return 0, nil
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	err := r.each(ctx, len(lines), func(ctx context.Context, i int) error {
		u := strings.TrimSuffix(r.PublicURL, "/") + "/" + escapeKey(lines[i].Key)
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ContentLength != lines[i].Size {
			return fmt.Errorf("%s: status %d, size %d, assets has %d", u, resp.StatusCode, resp.ContentLength, lines[i].Size)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(lines), nil
}

func escapeKey(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
