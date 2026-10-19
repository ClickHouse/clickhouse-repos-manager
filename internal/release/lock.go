package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// PackageType is DEB, RPM or TGZ.
type PackageType interface {
	Name() string
	// Prefix is the public key prefix, for example "deb/".
	Prefix() string
	// Parse claims an input package by its file name: ok when the name is of
	// this package type, err when it is but does not parse for version (another
	// version, an unknown architecture). Architectures are amd64, arm64 and all.
	Parse(name, version string) (pkg, arch string, ok bool, err error)
	// Indexed reports whether the package type has index metadata. Without
	// it (TGZ) adopt finds nothing to adopt, and objects under Prefix are fine.
	Indexed() bool
	// Check validates the packages of a release together, before anything is
	// written.
	Check(packages []Package) error
	// Stage builds the assets and the index metadata of a release on top of
	// previous. It returns only the index metadata it builds; the snapshot takes
	// every other key from previous.
	Stage(ctx context.Context, previous *Previous, tag Tag, packages []Package) (Plan, error)
	// Live lists the live metadata files, after verifying them (signatures,
	// hashes one level up). SHA256 is computed from the bytes it verified.
	// An empty repository has no files.
	Live(ctx context.Context, b store.Bucket) ([]File, error)
}

// Pruner is a package type with content named metadata that unlock prunes.
type Pruner interface {
	// Prune deletes, with store.DeleteStamped, the content named metadata that
	// no retained snapshot references.
	Prune(ctx context.Context, b store.Bucket, retained []*Previous) (Pruned, error)
}

// Pruned is what a Pruner did.
type Pruned struct {
	// Type is the package type name; unlock sets it.
	Type    string
	Deleted []string
	// NotStamped are unreferenced objects without a stamp; they stay.
	NotStamped []string
}

// Selector names the packages a remove takes out of one channel.
type Selector struct {
	Channel, Version string
	// Packages, when not empty, narrows the remove to these package names.
	Packages []string
	// Arch, when not empty, narrows the remove to amd64, arm64 or all.
	Arch string
}

// Remover is a package type with an index that a remove can shrink.
type Remover interface {
	// Remove builds the index metadata of previous without the selected
	// packages. Plan.Unreferenced lists the package keys that no index of any
	// channel references any more. A selector that matches nothing is an error.
	Remove(ctx context.Context, previous *Previous, sel Selector) (Plan, error)
}

// Releaser runs the release protocol on a bucket.
type Releaser struct {
	Bucket store.Bucket
	Types  []PackageType
	// Version is recorded in every attempt.
	Version string
	// Now defaults to time.Now.
	Now func() time.Time
	// Concurrency is the number of uploads, copies and read only checks run at
	// once. Default store.DefaultUploadConcurrency.
	Concurrency int
	// PublicURL is the public base URL of the keys; publish checks the
	// packages there when it is set.
	PublicURL string
	// HTTP defaults to a client with a timeout.
	HTTP *http.Client
}

// Held is the lock as last written or read, with its ETag for the next conditional write.
type Held struct {
	Lock Lock
	ETag string
}

// LockRequest are the arguments of lock.
type LockRequest struct {
	Tag    string
	Date   string
	RunID  string
	RunURL string
}

var jsonOpts = store.PutOptions{ContentType: "application/json"}

func (r *Releaser) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Lock takes the global lock for a release, or resumes a release that holds it.
// The first lock on a bucket adopts the live state.
func (r *Releaser) Lock(ctx context.Context, req LockRequest) (Held, error) {
	tag, err := ParseTag(req.Tag)
	if err != nil {
		return Held{}, err
	}
	date, err := ParseDate(req.Date)
	if err != nil {
		return Held{}, err
	}
	attempt := Attempt{RunID: req.RunID, RunURL: req.RunURL, Started: r.now().UTC().Format(DateFormat), Version: r.Version}
	snapshot := SnapshotName(tag, date)
	if frozen, err := r.released(ctx, tag.Name); err != nil {
		return Held{}, err
	} else if frozen {
		return Held{}, fmt.Errorf("%s is released and frozen; it is never published again", tag.Name)
	}

	// A second round only after losing the race to create the lock.
	for round := 0; ; round++ {
		held, found, err := r.read(ctx)
		if err != nil {
			return Held{}, err
		}
		if !found {
			held, err = r.create(ctx, Lock{
				Schema: Schema, Tag: tag.Name, TagDate: req.Date,
				Snapshot: snapshot, Previous: AdoptedSnapshot, State: Locked,
				Attempts: []Attempt{attempt},
			})
			if errors.Is(err, store.ErrPrecondition) && round == 0 {
				continue
			}
		} else {
			held, err = r.relock(ctx, held, tag.Name, req.Date, snapshot, attempt)
		}
		if err != nil {
			return Held{}, err
		}
		if held.Lock.Previous == AdoptedSnapshot {
			if err := r.adopt(ctx); err != nil {
				return Held{}, err
			}
		}
		return held, nil
	}
}

func (r *Releaser) create(ctx context.Context, l Lock) (Held, error) {
	var snapshots bool
	err := r.Bucket.List(ctx, config.SnapshotsPrefix, func(store.Object) error { snapshots = true; return errStop })
	if err != nil && !errors.Is(err, errStop) {
		return Held{}, err
	}
	if snapshots {
		return Held{}, fmt.Errorf("no lock, but %s holds snapshots: the lock was deleted; restore it by hand", config.SnapshotsPrefix)
	}
	return r.write(ctx, l, "")
}

func (r *Releaser) relock(ctx context.Context, cur Held, tag, date, snapshot string, attempt Attempt) (Held, error) {
	l := cur.Lock
	switch {
	case l.State == Free:
		m, err := r.manifest(ctx, l.Snapshot)
		if err != nil {
			return Held{}, err
		}
		if diff, err := r.diffLive(ctx, m); err != nil {
			return Held{}, err
		} else if len(diff) > 0 {
			return Held{}, fmt.Errorf("live metadata differs from snapshot %s, clean up by hand:\n  %s", l.Snapshot, strings.Join(diff, "\n  "))
		}
		return r.write(ctx, Lock{
			Schema: Schema, Tag: tag, TagDate: date,
			Snapshot: snapshot, Previous: l.Snapshot, State: Locked,
			Attempts: []Attempt{attempt},
		}, cur.ETag)
	case l.Tag == tag && l.TagDate == date:
		l.Attempts = append(l.Attempts, attempt)
		return r.write(ctx, l, cur.ETag)
	case l.Tag == tag:
		return Held{}, fmt.Errorf("lock is held by %s with tag date %s, not %s", l.Tag, l.TagDate, date)
	default:
		return Held{}, fmt.Errorf("lock is held by another release, a release engineer must act:\n%s", encode(l))
	}
}

// write puts the lock conditionally: absent when etag is empty, else If-Match.
// After a lost response it reads the lock back and goes on if it holds exactly our body.
func (r *Releaser) write(ctx context.Context, l Lock, etag string) (Held, error) {
	body := encode(l)
	var obj store.Object
	var err error
	if etag == "" {
		obj, err = r.Bucket.PutIfAbsent(ctx, LockKey, body, jsonOpts)
	} else {
		obj, err = r.Bucket.PutIfMatch(ctx, LockKey, etag, body, jsonOpts)
	}
	if err == nil {
		return Held{Lock: l, ETag: obj.ETag}, nil
	}
	got, gotObj, gerr := r.Bucket.Get(ctx, LockKey)
	if gerr == nil && bytes.Equal(got, body) {
		return Held{Lock: l, ETag: gotObj.ETag}, nil
	}
	if errors.Is(err, store.ErrPrecondition) && etag != "" {
		return Held{}, fmt.Errorf("the lock changed under this run: %w", err)
	}
	return Held{}, err
}

func (r *Releaser) read(ctx context.Context) (Held, bool, error) {
	body, obj, err := r.Bucket.Get(ctx, LockKey)
	if errors.Is(err, store.ErrNotFound) {
		return Held{}, false, nil
	}
	if err != nil {
		return Held{}, false, err
	}
	l, err := ParseLock(body)
	if err != nil {
		return Held{}, false, err
	}
	return Held{Lock: l, ETag: obj.ETag}, true, nil
}

// Status returns the lock, or found false when the bucket was never adopted.
func (r *Releaser) Status(ctx context.Context) (Held, bool, error) {
	return r.read(ctx)
}

func (r *Releaser) manifest(ctx context.Context, snapshot string) (Manifest, error) {
	body, _, err := r.Bucket.Get(ctx, ManifestKey(snapshot))
	if errors.Is(err, store.ErrNotFound) {
		return Manifest{}, fmt.Errorf("snapshot %s is unfinished (no %s): %w", snapshot, manifestName, err)
	}
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(body)
}

// diffLive lists the live files that are missing or differ from the manifest.
func (r *Releaser) diffLive(ctx context.Context, m Manifest) ([]string, error) {
	objs, found, err := r.heads(ctx, m.Files)
	if err != nil {
		return nil, err
	}
	var diff []string
	for i, f := range m.Files {
		switch obj := objs[i]; {
		case !found[i]:
			diff = append(diff, f.Key+": missing")
		case obj.ETag != f.ETag || obj.Size != f.Size:
			diff = append(diff, fmt.Sprintf("%s: etag %s size %d, snapshot has etag %s size %d", f.Key, obj.ETag, obj.Size, f.ETag, f.Size))
		}
	}
	return diff, nil
}

var errStop = errors.New("stop listing")

// adopt copies the verified live metadata into AdoptedSnapshot and writes its manifest last.
func (r *Releaser) adopt(ctx context.Context) error {
	key := ManifestKey(AdoptedSnapshot)
	if _, err := r.Bucket.Head(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var files []File
	for _, t := range r.Types {
		live, err := t.Live(ctx, r.Bucket)
		if err != nil {
			return fmt.Errorf("adopt %s: %w", t.Name(), err)
		}
		if len(live) == 0 {
			if !t.Indexed() {
				continue
			}
			var first string
			err := r.Bucket.List(ctx, t.Prefix(), func(o store.Object) error { first = o.Key; return errStop })
			if err != nil && !errors.Is(err, errStop) {
				return err
			}
			if first != "" {
				return fmt.Errorf("adopt %s: %s holds objects (%s) but no metadata", t.Name(), t.Prefix(), first)
			}
			continue
		}
		for _, f := range live {
			if !strings.HasPrefix(f.Key, t.Prefix()) {
				return fmt.Errorf("adopt %s: %s is outside %s", t.Name(), f.Key, t.Prefix())
			}
			if err := r.copyKnown(ctx, f.Key, SnapshotDir(AdoptedSnapshot)+f.Key, f); err != nil {
				return fmt.Errorf("adopt %s: %w", t.Name(), err)
			}
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Key < files[j].Key })
	if files == nil {
		files = []File{}
	}
	body := encode(Manifest{Schema: Schema, Files: files})
	if _, err := r.Bucket.PutIfAbsent(ctx, key, body, jsonOpts); err != nil {
		got, _, gerr := r.Bucket.Get(ctx, key)
		if gerr != nil || !bytes.Equal(got, body) {
			return fmt.Errorf("adopt: write %s: %w", key, err)
		}
	}
	return nil
}

// BreakResult is what lock break did.
type BreakResult struct {
	Held Held
	// Differs lists the live files that match no snapshot; only with force.
	Differs []string
}

// Break frees the lock without touching any file, from the facts: live equal
// to the release snapshot makes it the published one, live equal to previous
// goes back to previous. With force a mismatch records previous, and the next
// lock fails until an engineer cleans up. DANGEROUS: break glass only.
func (r *Releaser) Break(ctx context.Context, tag string, force bool) (BreakResult, error) {
	cur, found, err := r.read(ctx)
	if err != nil {
		return BreakResult{}, err
	}
	if !found {
		return BreakResult{}, errors.New("no lock (not adopted)")
	}
	l := cur.Lock
	if l.State == Free {
		return BreakResult{}, errors.New("the lock is free")
	}
	if l.Tag != tag {
		return BreakResult{}, fmt.Errorf("the lock is held by %s, not %s", l.Tag, tag)
	}
	if m, err := r.manifest(ctx, l.Snapshot); err == nil {
		diff, err := r.diffLive(ctx, m)
		if err != nil {
			return BreakResult{}, err
		}
		if len(diff) == 0 {
			l.State = Free
			held, err := r.write(ctx, l, cur.ETag)
			return BreakResult{Held: held}, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return BreakResult{}, err
	}
	m, err := r.manifest(ctx, l.Previous)
	if err != nil {
		return BreakResult{}, err
	}
	diff, err := r.diffLive(ctx, m)
	if err != nil {
		return BreakResult{}, err
	}
	if len(diff) > 0 && !force {
		return BreakResult{}, fmt.Errorf("live metadata differs from snapshot %s; refused without --force:\n  %s", l.Previous, strings.Join(diff, "\n  "))
	}
	l.State, l.Snapshot = Free, l.Previous
	held, err := r.write(ctx, l, cur.ETag)
	if err != nil {
		return BreakResult{}, err
	}
	return BreakResult{Held: held, Differs: diff}, nil
}
