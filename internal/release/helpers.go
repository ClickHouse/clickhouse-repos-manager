package release

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// copyKnown is the only copy of metadata: from src as it was recorded in want,
// and the destination must then match want.
func (r *Releaser) copyKnown(ctx context.Context, src, dst string, want File) error {
	obj, err := r.Bucket.CopyIfSourceMatch(ctx, src, want.ETag, dst)
	if err != nil {
		return err
	}
	// Files adopted from production carry no SHA256 metadata.
	if obj.ETag != want.ETag || obj.Size != want.Size || (obj.SHA256 != "" && obj.SHA256 != want.SHA256) {
		return fmt.Errorf("copy %s to %s: got etag %s size %d sha256 %q, want %s %d %s", src, dst, obj.ETag, obj.Size, obj.SHA256, want.ETag, want.Size, want.SHA256)
	}
	return nil
}

func same(obj store.Object, f File) bool { return obj.ETag == f.ETag && obj.Size == f.Size }

func byKey(files []File) map[string]File {
	m := make(map[string]File, len(files))
	for _, f := range files {
		m[f.Key] = f
	}
	return m
}

func (r *Releaser) setState(ctx context.Context, cur Held, s State) (Held, error) {
	l := cur.Lock
	l.State = s
	return r.write(ctx, l, cur.ETag)
}

// held returns the lock held by tag.
func (r *Releaser) held(ctx context.Context, tag string) (Held, error) {
	cur, found, err := r.read(ctx)
	switch {
	case err != nil:
		return Held{}, err
	case !found:
		return Held{}, errors.New("no lock (not adopted)")
	case cur.Lock.State == Free:
		return Held{}, errors.New("the lock is free")
	case cur.Lock.Tag != tag:
		return Held{}, fmt.Errorf("the lock is held by %s, not %s", cur.Lock.Tag, tag)
	}
	return cur, nil
}

func (r *Releaser) released(ctx context.Context, tag string) (bool, error) {
	_, err := r.Bucket.Head(ctx, ReleasedKey(tag))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// snapshots lists the snapshot names under SnapshotsPrefix, sorted.
func (r *Releaser) snapshots(ctx context.Context) ([]string, error) {
	var names []string
	err := r.Bucket.List(ctx, config.SnapshotsPrefix, func(o store.Object) error {
		name, _, _ := strings.Cut(strings.TrimPrefix(o.Key, config.SnapshotsPrefix), "/")
		if len(names) == 0 || names[len(names)-1] != name {
			names = append(names, name)
		}
		return nil
	})
	return names, err
}

// deleteSnapshot removes a snapshot directory. The manifest goes first when
// pruning, so a half pruned snapshot reads as unfinished, and last in rollback,
// which reads it on a rerun.
func (r *Releaser) deleteSnapshot(ctx context.Context, name string, manifestLast bool) error {
	var keys []string
	if err := r.Bucket.List(ctx, SnapshotDir(name), func(o store.Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		return err
	}
	mk := ManifestKey(name)
	if !manifestLast {
		if err := r.Bucket.Delete(ctx, mk); err != nil {
			return err
		}
	}
	keys = slices.DeleteFunc(keys, func(k string) bool { return k == mk })
	if err := r.each(ctx, len(keys), func(ctx context.Context, i int) error { return r.Bucket.Delete(ctx, keys[i]) }); err != nil {
		return err
	}
	if manifestLast {
		return r.Bucket.Delete(ctx, mk)
	}
	return nil
}

// checkTypes compares what each package type finds and verifies in b with the manifest.
func (r *Releaser) checkTypes(ctx context.Context, b store.Bucket, m Manifest) error {
	want := byKey(m.Files)
	for _, t := range r.Types {
		live, err := t.Live(ctx, b)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Name(), err)
		}
		n := 0
		for _, f := range m.Files {
			if strings.HasPrefix(f.Key, t.Prefix()) {
				n++
			}
		}
		for _, f := range live {
			if w, ok := want[f.Key]; !ok || w != f {
				return fmt.Errorf("%s: %s is %+v, the manifest has %+v", t.Name(), f.Key, f, w)
			}
		}
		if len(live) != n {
			return fmt.Errorf("%s: found %d metadata files, the manifest has %d", t.Name(), len(live), n)
		}
	}
	return nil
}

// checkAsset checks an asset object by its stored metadata only: size,
// SHA256 and, when the bucket keeps it, CRC64NVME; and the stamp tag when it
// is not empty. An object without SHA256 metadata was not written by this
// tool, and nothing proves its content.
func checkAsset(obj store.Object, sums hashes.Sums, tag string) error {
	switch {
	case obj.SHA256 == "":
		return fmt.Errorf("%s exists without sha256 metadata; its content is unknown", obj.Key)
	case obj.Size != sums.Size || obj.SHA256 != sums.SHA256 || (obj.CRC64NVME != "" && obj.CRC64NVME != sums.CRC64NVME):
		return fmt.Errorf("%s exists with other content (size %d, sha256 %s, crc64nvme %s; want %d, %s, %s)",
			obj.Key, obj.Size, obj.SHA256, obj.CRC64NVME, sums.Size, sums.SHA256, sums.CRC64NVME)
	case tag != "" && obj.Release != tag:
		return fmt.Errorf("%s was just written but carries the stamp %q, not %s", obj.Key, obj.Release, tag)
	}
	return nil
}

func (r *Releaser) concurrency() int {
	if r.Concurrency > 0 {
		return r.Concurrency
	}
	return store.DefaultUploadConcurrency
}

// each calls fn for every index below n, at most Concurrency at once and
// started in index order. After the first error it starts no more calls and
// returns that error once the running ones are done.
func (r *Releaser) each(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(r.concurrency(), n) {
		wg.Go(func() {
			for i := range next {
				if ctx.Err() != nil {
					continue
				}
				if err := fn(ctx, i); err != nil {
					cancel(err)
				}
			}
		})
	}
feed:
	for i := range n {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	return context.Cause(ctx)
}

// heads returns the live objects of files; found is false for a missing key.
func (r *Releaser) heads(ctx context.Context, files []File) ([]store.Object, []bool, error) {
	objs := make([]store.Object, len(files))
	found := make([]bool, len(files))
	err := r.each(ctx, len(files), func(ctx context.Context, i int) error {
		obj, err := r.Bucket.Head(ctx, files[i].Key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		objs[i], found[i] = obj, err == nil
		return err
	})
	return objs, found, err
}

// view reads a snapshot directory as if it were the live bucket. It refuses writes.
type view struct {
	b   store.Bucket
	dir string
}

var _ store.Bucket = view{}

var errView = errors.New("snapshot view is read only")

func (v view) Get(ctx context.Context, key string) ([]byte, store.Object, error) {
	body, obj, err := v.b.Get(ctx, v.dir+key)
	obj.Key = key
	return body, obj, err
}

func (v view) Head(ctx context.Context, key string) (store.Object, error) {
	obj, err := v.b.Head(ctx, v.dir+key)
	obj.Key = key
	return obj, err
}

func (v view) List(ctx context.Context, prefix string, fn func(store.Object) error) error {
	return v.b.List(ctx, v.dir+prefix, func(o store.Object) error {
		o.Key = strings.TrimPrefix(o.Key, v.dir)
		return fn(o)
	})
}

func (view) PutIfAbsent(context.Context, string, []byte, store.PutOptions) (store.Object, error) {
	return store.Object{}, errView
}

func (view) PutIfMatch(context.Context, string, string, []byte, store.PutOptions) (store.Object, error) {
	return store.Object{}, errView
}

func (view) Put(context.Context, string, []byte, store.PutOptions) (store.Object, error) {
	return store.Object{}, errView
}

func (view) PutFileIfAbsent(context.Context, string, string, hashes.Sums, store.PutOptions) (store.Object, error) {
	return store.Object{}, errView
}

func (view) CopyIfSourceMatch(context.Context, string, string, string) (store.Object, error) {
	return store.Object{}, errView
}

func (view) CopyIfAbsent(context.Context, string, string, string, hashes.Sums, store.PutOptions) (store.Object, error) {
	return store.Object{}, errView
}

func (view) AbortUploads(context.Context, string) (int, error) { return 0, errView }

func (view) Delete(context.Context, string) error { return errView }

func (view) DeleteOwned(context.Context, string, string, string) (store.DeleteResult, error) {
	return 0, errView
}
