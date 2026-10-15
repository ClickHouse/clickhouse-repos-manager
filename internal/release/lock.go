package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// PackageType is DEB, RPM or TGZ, as far as the lock and adopt need it.
type PackageType interface {
	Name() string
	// Prefix is the public key prefix, for example "deb/".
	Prefix() string
	// Live lists the live metadata files, after verifying them (signatures,
	// hashes one level up). SHA256 is computed from the bytes it verified.
	// An empty repository has no files.
	Live(ctx context.Context, b store.Bucket) ([]File, error)
}

// Releaser runs the release protocol on a bucket.
type Releaser struct {
	Bucket store.Bucket
	Types  []PackageType
	// Version is recorded in every attempt.
	Version string
	// Now defaults to time.Now.
	Now func() time.Time
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
	var diff []string
	for _, f := range m.Files {
		obj, err := r.Bucket.Head(ctx, f.Key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			diff = append(diff, f.Key+": missing")
		case err != nil:
			return nil, err
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
			obj, err := r.Bucket.CopyIfSourceMatch(ctx, f.Key, f.ETag, SnapshotDir(AdoptedSnapshot)+f.Key)
			if err != nil {
				return fmt.Errorf("adopt %s: %w", t.Name(), err)
			}
			if obj.ETag != f.ETag || obj.Size != f.Size {
				return fmt.Errorf("adopt %s: copy of %s has etag %s size %d, want %s %d", t.Name(), f.Key, obj.ETag, obj.Size, f.ETag, f.Size)
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

// Break frees the lock without touching any file, from the facts: the live
// metadata must equal the previous snapshot. With force a mismatch is recorded
// and the next lock fails until an engineer cleans up.
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
