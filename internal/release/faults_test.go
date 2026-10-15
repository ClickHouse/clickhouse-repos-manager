package release

import (
	"context"
	"reflect"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

func memBucket(*testing.T) store.Bucket { return storetest.NewMem() }

func realBucket(t *testing.T) store.Bucket { return storetest.New(t) }

// faultyRun runs step through the faulty bucket: it must succeed unless the
// fault killed the run, and a Retried fault never kills it.
func faultyRun(t *testing.T, f *storetest.Faults, step func(r *Releaser) error) {
	t.Helper()
	err := step(releaser(f))
	if err != nil && (!f.Injected() || f.Mode == storetest.Retried) {
		t.Fatalf("faulty run (%s): %v", f.Mode, err)
	}
}

// The first lock with adopt; the rerun reaches the same lock and a complete snapshot.
func lockFromMissing(t *testing.T, b store.Bucket, f *storetest.Faults) {
	seed(t, b, seedDeb)
	faultyRun(t, f, func(r *Releaser) error { _, err := r.Lock(context.Background(), reqA); return err })
	h := mustLock(t, releaser(b), reqA)
	want := Lock{Schema: 1, Tag: reqA.Tag, TagDate: reqA.Date, Snapshot: snapA, Previous: AdoptedSnapshot, State: Locked}
	if got := withoutAttempts(stored(t, b, h)); !reflect.DeepEqual(got, want) {
		t.Fatalf("lock %+v, want %+v", got, want)
	}
	checkAdopted(t, b, seedDeb)
}

func TestLockFaults(t *testing.T) {
	t.Run("mem", func(t *testing.T) { storetest.ForEachFault(t, memBucket, lockFromMissing) })
	t.Run("bucket", func(t *testing.T) { storetest.ForEachFault(t, realBucket, lockFromMissing) })
}
