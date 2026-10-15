package release

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Unlock ends a committed release after the client tests passed: it prunes old
// snapshots, freezes the tag and frees the lock.
func (r *Releaser) Unlock(ctx context.Context, tag string) (Held, error) {
	cur, err := r.held(ctx, tag)
	if err != nil {
		return Held{}, err
	}
	l := cur.Lock
	if l.State != Committed {
		return Held{}, fmt.Errorf("unlock needs state %s, the state is %s; end this release with rollback", Committed, l.State)
	}
	m, err := r.manifest(ctx, l.Snapshot)
	if err != nil {
		return Held{}, err
	}
	if diff, err := r.diffLive(ctx, m); err != nil {
		return Held{}, err
	} else if len(diff) > 0 {
		return Held{}, fmt.Errorf("live metadata differs from snapshot %s; run publish again to recover:\n  %s", l.Snapshot, strings.Join(diff, "\n  "))
	}
	names, err := r.snapshots(ctx)
	if err != nil {
		return Held{}, err
	}
	for i, name := range names {
		if i >= len(names)-config.SnapshotRetention || name == l.Snapshot || name == l.Previous {
			continue
		}
		if err := r.deleteSnapshot(ctx, name, false); err != nil {
			return Held{}, fmt.Errorf("prune %s: %w", name, err)
		}
	}
	obj, err := r.Bucket.Head(ctx, ManifestKey(l.Snapshot))
	if err != nil {
		return Held{}, err
	}
	mf := File{Key: ManifestKey(l.Snapshot), Size: obj.Size, ETag: obj.ETag, SHA256: obj.SHA256}
	if err := r.copyKnown(ctx, mf.Key, ReleasedKey(tag), mf); err != nil {
		return Held{}, err
	}
	return r.setState(ctx, cur, Free)
}

// Rollback undoes the release that holds the lock: the index metadata goes back
// to previous first, then every asset with this release's stamp is deleted,
// then the release snapshot. It frees the lock with snapshot previous.
func (r *Releaser) Rollback(ctx context.Context, tag string) (Held, error) {
	cur, err := r.held(ctx, tag)
	if err != nil {
		return Held{}, err
	}
	if frozen, err := r.released(ctx, tag); err != nil {
		return Held{}, err
	} else if frozen {
		return Held{}, fmt.Errorf("%s is released and frozen; rollback is refused", tag)
	}
	if cur.Lock.State != RollingBack {
		if cur, err = r.setState(ctx, cur, RollingBack); err != nil {
			return Held{}, err
		}
	}
	l := cur.Lock
	prev, err := r.manifest(ctx, l.Previous)
	if err != nil {
		return Held{}, err
	}
	m, err := r.manifest(ctx, l.Snapshot)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The commit never ran, or an earlier rollback already restored everything.
		if diff, err := r.diffLive(ctx, prev); err != nil {
			return Held{}, err
		} else if len(diff) > 0 {
			return Held{}, fmt.Errorf("no manifest for %s and live metadata differs from %s, clean up by hand:\n  %s", l.Snapshot, l.Previous, strings.Join(diff, "\n  "))
		}
	case err != nil:
		return Held{}, err
	default:
		if err := r.restore(ctx, l, prev, m); err != nil {
			return Held{}, err
		}
	}
	_, lines, _, err := r.readAssets(ctx, l.Snapshot)
	if err != nil {
		return Held{}, err
	}
	for _, line := range lines {
		if _, err := r.Bucket.DeleteOwned(ctx, line.Key, tag, line.SHA256); err != nil {
			return Held{}, err
		}
	}
	if err := r.deleteSnapshot(ctx, l.Snapshot, true); err != nil {
		return Held{}, err
	}
	l.State, l.Snapshot = Free, l.Previous
	return r.write(ctx, l, cur.ETag)
}

// restore puts the index metadata back to previous, in reverse commit order.
func (r *Releaser) restore(ctx context.Context, l Lock, prev, m Manifest) error {
	prevBy := byKey(prev.Files)
	for i := len(m.Files) - 1; i >= 0; i-- {
		f := m.Files[i]
		p, inPrev := prevBy[f.Key]
		obj, err := r.Bucket.Head(ctx, f.Key)
		live := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		switch {
		case inPrev && live && same(obj, p), !inPrev && !live:
		case live && same(obj, f) && inPrev:
			if err := r.copyKnown(ctx, SnapshotDir(l.Previous)+f.Key, f.Key, p); err != nil {
				return err
			}
		case live && same(obj, f):
			res, err := r.Bucket.DeleteOwned(ctx, f.Key, l.Tag, f.SHA256)
			if err != nil {
				return err
			}
			if res == store.NotOwned {
				return fmt.Errorf("%s matches %s but carries no stamp %s", f.Key, l.Snapshot, l.Tag)
			}
		default:
			return fmt.Errorf("live %s matches neither %s nor %s, clean up by hand", f.Key, l.Previous, l.Snapshot)
		}
	}
	return nil
}
