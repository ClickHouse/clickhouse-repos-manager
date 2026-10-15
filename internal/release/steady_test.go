package release

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

// The steady state after cutover: releases W and then X are published and the
// lock is free. The adopted snapshot is gone (pruned), and W and X differ in
// content, so code that mixes up snapshot and previous fails.
var (
	tagX  = "v26.8.2.1-stable"
	dateX = "2026-09-15T00:00:00Z"
	snapW = "20260901T000000Z-v26.8.1.1-stable"
	snapX = "20260915T000000Z-v26.8.2.1-stable"
	liveW = map[string]string{
		"deb/index":             "deb/dists/lts/Release deb/dists/lts/main/binary-amd64/Packages",
		"deb/dists/lts/Release": "release W",
		"deb/dists/lts/main/binary-amd64/Packages": "packages W",
	}
	liveX = map[string]string{
		"deb/index":             "deb/dists/lts/Release deb/dists/lts/main/binary-amd64/Packages",
		"deb/dists/lts/Release": "release X",
		"deb/dists/lts/main/binary-amd64/Packages": "packages X",
	}
)

// writeSnapshot writes a complete snapshot as M1c will: files, then the manifest.
// With publish, the files are also copied to their live keys, as the commit does.
func writeSnapshot(t *testing.T, b store.Bucket, name string, files map[string]string, publish bool) {
	t.Helper()
	ctx := context.Background()
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := Manifest{Schema: Schema, Tag: name[len("20060102T150405Z-"):], TagDate: "-"}
	for _, k := range keys {
		obj, err := b.Put(ctx, SnapshotDir(name)+k, []byte(files[k]), store.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if publish {
			if _, err := b.CopyIfSourceMatch(ctx, SnapshotDir(name)+k, obj.ETag, k); err != nil {
				t.Fatal(err)
			}
		}
		m.Files = append(m.Files, File{Key: k, Size: obj.Size, ETag: obj.ETag, SHA256: hashes.Bytes([]byte(files[k])).SHA256})
	}
	if _, err := b.Put(ctx, ManifestKey(name), encode(m), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

// steady seeds the steady state and returns the bucket wrapped so that any
// call touching the adopted snapshot fails the test.
func steady(t *testing.T, b store.Bucket) store.Bucket {
	t.Helper()
	writeSnapshot(t, b, snapW, liveW, false)
	writeSnapshot(t, b, snapX, liveX, true)
	l := Lock{Schema: Schema, Tag: tagX, TagDate: dateX, Snapshot: snapX, Previous: snapW, State: Free,
		Attempts: []Attempt{{RunID: "x", Started: dateX, Version: "old"}}}
	if _, err := b.Put(context.Background(), LockKey, encode(l), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	return noAdopt(t, b)
}

func noAdopt(t *testing.T, b store.Bucket) store.Bucket {
	return &storetest.Faults{B: b, Log: adoptGuard(t)}
}

// adoptGuard is a Faults log that fails the test on any call touching the adopted snapshot.
func adoptGuard(t *testing.T) func(string, ...any) {
	return func(format string, args ...any) {
		if line := fmt.Sprintf(format, args...); strings.Contains(line, AdoptedSnapshot) {
			t.Errorf("adopted snapshot touched: %s", line)
		}
	}
}

// setLive overwrites live files by hand, outside the tool.
func setLive(t *testing.T, b store.Bucket, files map[string]string) {
	t.Helper()
	for k, v := range files {
		if _, err := b.Put(context.Background(), k, []byte(v), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLockTable(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, b := context.Background(), steady(t, b)
		r := releaser(b)

		// Free: previous is the last published snapshot, X, not the one before it.
		first := mustLock(t, r, reqA)
		want := Lock{Schema: 1, Tag: reqA.Tag, TagDate: reqA.Date, Snapshot: snapA, Previous: snapX, State: Locked,
			Attempts: []Attempt{{RunID: "1", RunURL: "https://ci/1", Started: "2026-10-13T10:00:00Z", Version: "test"}}}
		if !reflect.DeepEqual(stored(t, b, first), want) {
			t.Fatalf("lock from free %+v, want %+v", first.Lock, want)
		}

		// Same tag and date: a new attempt, the rest stays.
		r.Now = func() time.Time { return started.Add(time.Hour) }
		again := mustLock(t, r, LockRequest{Tag: reqA.Tag, Date: reqA.Date, RunID: "2"})
		if !reflect.DeepEqual(withoutAttempts(stored(t, b, again)), withoutAttempts(first.Lock)) ||
			len(again.Lock.Attempts) != 2 || again.Lock.Attempts[1] != (Attempt{RunID: "2", Started: "2026-10-13T11:00:00Z", Version: "test"}) {
			t.Fatalf("resume: %+v", again.Lock)
		}

		_, err := r.Lock(ctx, LockRequest{Tag: reqA.Tag, Date: "2026-09-29T12:29:03Z"})
		wantErr(t, err, "with tag date 2026-09-29T12:29:02Z, not 2026-09-29T12:29:03Z")
		_, err = r.Lock(ctx, reqB)
		wantErr(t, err, `held by another release`)
		wantErr(t, err, `"tag": "v26.3.36.6-lts"`)
		for _, bad := range []LockRequest{{Tag: "v1.2.3.4-testing", Date: reqA.Date}, {Tag: reqA.Tag, Date: "2026-09-29"}} {
			_, err = r.Lock(ctx, bad)
			wantErr(t, err, "want")
		}
		if l := stored(t, b, again); !reflect.DeepEqual(l, again.Lock) {
			t.Fatal("a refused lock changed the lock")
		}

		// Break frees it back to X; the next release then gets previous X too.
		if _, err := r.Break(ctx, reqA.Tag, false); err != nil {
			t.Fatal(err)
		}
		h := mustLock(t, r, reqB)
		if l := stored(t, b, h); l.Tag != reqB.Tag || l.Snapshot != snapB || l.Previous != snapX || len(l.Attempts) != 1 {
			t.Fatalf("lock from free after break %+v", l)
		}
	})
}

// Lock from free compares the live files with snapshot, not with previous.
func TestLockFromFreeLiveChanged(t *testing.T) {
	for name, live := range map[string]map[string]string{
		"by hand":        {"deb/dists/lts/Release": "changed by hand"},
		"back to W":      {"deb/dists/lts/Release": liveW["deb/dists/lts/Release"]},
		"index deleted?": nil,
	} {
		t.Run(name, func(t *testing.T) {
			b := steady(t, storetest.NewMem())
			if live != nil {
				setLive(t, b, live)
			} else if err := b.Delete(context.Background(), "deb/index"); err != nil {
				t.Fatal(err)
			}
			before := stored(t, b, Held{})
			_, err := releaser(b).Lock(context.Background(), reqA)
			wantErr(t, err, "live metadata differs from snapshot "+snapX)
			if live == nil {
				wantErr(t, err, "deb/index: missing")
			} else {
				wantErr(t, err, "deb/dists/lts/Release: etag")
			}
			if !reflect.DeepEqual(stored(t, b, Held{}), before) {
				t.Fatal("the lock changed")
			}
		})
	}
	backends(t, func(t *testing.T, b store.Bucket) {
		b = steady(t, b)
		setLive(t, b, map[string]string{"deb/dists/lts/Release": "changed by hand"})
		_, err := releaser(b).Lock(context.Background(), reqA)
		wantErr(t, err, "live metadata differs from snapshot "+snapX)
	})
}

func TestLockFromFreeUnfinishedSnapshot(t *testing.T) {
	b := steady(t, storetest.NewMem())
	if err := b.Delete(context.Background(), ManifestKey(snapX)); err != nil {
		t.Fatal(err)
	}
	_, err := releaser(b).Lock(context.Background(), reqA)
	wantErr(t, err, "snapshot "+snapX+" is unfinished")
}

func TestBreak(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx := context.Background()
		_, err := releaser(b).Break(ctx, reqA.Tag, false)
		wantErr(t, err, "no lock")
		b = steady(t, b)
		r := releaser(b)
		_, err = r.Break(ctx, tagX, false)
		wantErr(t, err, "the lock is free")
		mustLock(t, r, reqA)
		_, err = r.Break(ctx, reqB.Tag, true)
		wantErr(t, err, "held by v26.3.36.6-lts, not v26.7.19.5-stable")

		// Live equal to W, the snapshot before previous: still a mismatch.
		setLive(t, b, map[string]string{"deb/dists/lts/Release": liveW["deb/dists/lts/Release"]})
		before := stored(t, b, Held{})
		_, err = r.Break(ctx, reqA.Tag, false)
		wantErr(t, err, "live metadata differs from snapshot "+snapX+"; refused without --force")
		if !reflect.DeepEqual(stored(t, b, Held{}), before) {
			t.Fatal("a refused break changed the lock")
		}
		res, err := r.Break(ctx, reqA.Tag, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Differs) != 1 || !strings.HasPrefix(res.Differs[0], "deb/dists/lts/Release:") {
			t.Fatalf("differs %q", res.Differs)
		}
		want := before
		want.State, want.Snapshot = Free, snapX
		if l := stored(t, b, res.Held); !reflect.DeepEqual(l, want) {
			t.Fatalf("forced break %+v, want %+v", l, want)
		}
		// The next lock sees the mismatch.
		_, err = r.Lock(ctx, reqB)
		wantErr(t, err, "live metadata differs from snapshot "+snapX)
	})
}

func TestBreakClean(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		b = steady(t, b)
		r := releaser(b)
		h := mustLock(t, r, reqA)
		res, err := r.Break(context.Background(), reqA.Tag, false)
		if err != nil || len(res.Differs) != 0 {
			t.Fatal(err, res.Differs)
		}
		want := h.Lock
		want.State, want.Snapshot = Free, snapX
		if l := stored(t, b, res.Held); !reflect.DeepEqual(l, want) {
			t.Fatalf("break %+v, want %+v", l, want)
		}
	})
}

func TestLockRaceFromFree(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		b = steady(t, b)
		var mu sync.Mutex
		var wg sync.WaitGroup
		var won []string
		for _, req := range []LockRequest{reqA, reqB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := releaser(b).Lock(context.Background(), req)
				switch {
				case err == nil:
					mu.Lock()
					won = append(won, req.Tag)
					mu.Unlock()
				case !errors.Is(err, store.ErrPrecondition) && !strings.Contains(err.Error(), "held by another release"):
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if len(won) != 1 {
			t.Fatalf("winners %q", won)
		}
		if l := stored(t, b, Held{}); l.Tag != won[0] || l.Previous != snapX {
			t.Fatalf("lock %+v, winner %s", l, won[0])
		}
	})
}

func TestLockFromFreeFaults(t *testing.T) {
	storetest.ForEachFault(t, memBucket, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		steady(t, b)
		f.Log = adoptGuard(t)
		faultyRun(t, f, func(r *Releaser) error { _, err := r.Lock(context.Background(), reqA); return err })
		b = noAdopt(t, b)
		h := mustLock(t, releaser(b), reqA)
		want := Lock{Schema: 1, Tag: reqA.Tag, TagDate: reqA.Date, Snapshot: snapA, Previous: snapX, State: Locked}
		if got := withoutAttempts(stored(t, b, h)); !reflect.DeepEqual(got, want) {
			t.Fatalf("lock %+v, want %+v", got, want)
		}
	})
}

func TestBreakFaults(t *testing.T) {
	storetest.ForEachFault(t, memBucket, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		steady(t, b)
		held := mustLock(t, releaser(b), reqA)
		f.Log = adoptGuard(t)
		faultyRun(t, f, func(r *Releaser) error { _, err := r.Break(context.Background(), reqA.Tag, false); return err })
		if _, err := releaser(b).Break(context.Background(), reqA.Tag, false); err != nil && !strings.Contains(err.Error(), "the lock is free") {
			t.Fatal(err)
		}
		want := held.Lock
		want.State, want.Snapshot = Free, snapX
		if l := stored(t, b, Held{}); !reflect.DeepEqual(l, want) {
			t.Fatalf("lock %+v, want %+v", l, want)
		}
	})
}
