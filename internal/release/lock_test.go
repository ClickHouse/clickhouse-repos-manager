package release

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

var (
	types   = []PackageType{fakeType{"deb"}, fakeType{"tgz"}}
	started = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	reqA    = LockRequest{Tag: "v26.3.36.6-lts", Date: "2026-09-29T12:29:02Z", RunID: "1", RunURL: "https://ci/1"}
	reqB    = LockRequest{Tag: "v26.7.19.5-stable", Date: "2026-10-01T08:00:00Z"}
	snapA   = "20260929T122902Z-v26.3.36.6-lts"
	snapB   = "20261001T080000Z-v26.7.19.5-stable"
	// The live deb metadata of an existing repository; tgz stays empty.
	seedDeb = map[string]string{
		"deb/index":             "deb/dists/lts/Release deb/dists/lts/main/binary-amd64/Packages",
		"deb/dists/lts/Release": "release",
		"deb/dists/lts/main/binary-amd64/Packages": "packages",
	}
)

func releaser(b store.Bucket) *Releaser {
	return &Releaser{Bucket: b, Types: types, Version: "test", Now: func() time.Time { return started }}
}

func seed(t *testing.T, b store.Bucket, objs map[string]string) {
	t.Helper()
	for k, v := range objs {
		if _, err := b.Put(context.Background(), k, []byte(v), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A package is not metadata and is not adopted.
	if _, ok := objs["deb/index"]; ok {
		if _, err := b.Put(context.Background(), "deb/pool/main/c/clickhouse/a.deb", []byte("deb"), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// backends runs fn on the in-memory bucket and, when configured, on the test bucket.
func backends(t *testing.T, fn func(t *testing.T, b store.Bucket)) {
	t.Run("mem", func(t *testing.T) { fn(t, storetest.NewMem()) })
	t.Run("bucket", func(t *testing.T) { fn(t, realBucket(t)) })
}

func mustLock(t *testing.T, r *Releaser, req LockRequest) Held {
	t.Helper()
	h, err := r.Lock(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func wantErr(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("error %v, want %q", err, msg)
	}
}

// stored reads the lock and checks it against h.
func stored(t *testing.T, b store.Bucket, h Held) Lock {
	t.Helper()
	body, obj, err := b.Get(context.Background(), LockKey)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ParseLock(body)
	if err != nil {
		t.Fatal(err)
	}
	if h.ETag != "" && (obj.ETag != h.ETag || !reflect.DeepEqual(l, h.Lock)) {
		t.Fatalf("stored lock %+v %s, returned %+v %s", l, obj.ETag, h.Lock, h.ETag)
	}
	return l
}

func withoutAttempts(l Lock) Lock { l.Attempts = nil; return l }

// checkAdopted checks the adopted manifest and that the snapshot holds the live bytes.
func checkAdopted(t *testing.T, b store.Bucket, live map[string]string) {
	t.Helper()
	ctx := context.Background()
	body, _, err := b.Get(ctx, ManifestKey(AdoptedSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tag != "" || m.TagDate != "" || len(m.Files) != len(live) {
		t.Fatalf("manifest %+v, want %d files", m, len(live))
	}
	for i, f := range m.Files {
		if i > 0 && m.Files[i-1].Key >= f.Key {
			t.Fatalf("files not sorted: %+v", m.Files)
		}
		want := []byte(live[f.Key])
		got, obj, err := b.Get(ctx, SnapshotDir(AdoptedSnapshot)+f.Key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) || obj.ETag != f.ETag || f.Size != int64(len(want)) || f.SHA256 != hashes.Bytes(want).SHA256 {
			t.Fatalf("%s: snapshot %q %s, manifest %+v", f.Key, got, obj.ETag, f)
		}
	}
}

func TestLockAdoptEmpty(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		h := mustLock(t, releaser(b), reqA)
		want := Lock{Schema: 1, Tag: reqA.Tag, TagDate: reqA.Date, Snapshot: snapA, Previous: AdoptedSnapshot, State: Locked,
			Attempts: []Attempt{{RunID: "1", RunURL: "https://ci/1", Started: "2026-10-13T10:00:00Z", Version: "test"}}}
		if !reflect.DeepEqual(stored(t, b, h), want) {
			t.Fatalf("lock %+v, want %+v", h.Lock, want)
		}
		checkAdopted(t, b, nil)
	})
}

func TestLockAdoptExisting(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		seed(t, b, seedDeb)
		mustLock(t, releaser(b), reqA)
		checkAdopted(t, b, seedDeb)
	})
}

func TestLockAdoptWithoutMetadata(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, r := context.Background(), releaser(b)
		seed(t, b, map[string]string{"deb/pool/main/c/clickhouse/old.deb": "x"})
		_, err := r.Lock(ctx, reqA)
		wantErr(t, err, "holds objects (deb/pool/main/c/clickhouse/old.deb) but no metadata")
		// The lock is taken and the adopt is unfinished: break refuses, the next lock finishes it.
		stored(t, b, Held{})
		_, err = r.Break(ctx, reqA.Tag, true)
		wantErr(t, err, "unfinished")
		seed(t, b, seedDeb)
		if h := mustLock(t, r, reqA); len(h.Lock.Attempts) != 2 {
			t.Fatalf("attempts %+v", h.Lock.Attempts)
		}
		checkAdopted(t, b, seedDeb)
	})
}

func TestLockMissingWithSnapshots(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		seed(t, b, map[string]string{"configs/ch-deploy/runs/" + snapA + "/x": "x"})
		_, err := releaser(b).Lock(context.Background(), reqA)
		wantErr(t, err, "the lock was deleted")
		if _, err := b.Head(context.Background(), LockKey); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lock created: %v", err)
		}
	})
}

func TestLockUnknownSchema(t *testing.T) {
	b := storetest.NewMem()
	seed(t, b, map[string]string{LockKey: `{"schema":2}`})
	_, err := releaser(b).Lock(context.Background(), reqA)
	wantErr(t, err, "schema 2")
}

// The first lock: two releases race to create it, one wins and adopts.
func TestLockRaceFromMissing(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		seed(t, b, seedDeb)
		var mu sync.Mutex
		var wg sync.WaitGroup
		var won []string
		for _, req := range []LockRequest{reqA, reqB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := releaser(b).Lock(context.Background(), req); err == nil {
					mu.Lock()
					won = append(won, req.Tag)
					mu.Unlock()
				} else if !strings.Contains(err.Error(), "held by another release") {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if len(won) != 1 {
			t.Fatalf("winners %q", won)
		}
		if l := stored(t, b, Held{}); l.Tag != won[0] {
			t.Fatalf("lock holds %s, winner %s", l.Tag, won[0])
		}
		checkAdopted(t, b, seedDeb)
	})
}

// Two runs of the same release both go on: the loser resumes.
func TestLockRaceSameTag(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		seed(t, b, seedDeb)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := reqA
				req.RunID = string(rune('1' + i))
				if _, err := releaser(b).Lock(context.Background(), req); err != nil && !strings.Contains(err.Error(), "changed under this run") {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if l := stored(t, b, Held{}); l.Tag != reqA.Tag || l.State != Locked || len(l.Attempts) == 0 {
			t.Fatalf("lock %+v", l)
		}
		checkAdopted(t, b, seedDeb)
	})
}
