package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

func TestFaultsBefore(t *testing.T) {
	ctx, m := context.Background(), NewMem()
	f := &Faults{B: m, N: 2, Mode: Before}
	if _, err := f.Put(ctx, "a", []byte("1"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Head(ctx, "a"); err != nil || f.Calls() != 1 {
		t.Fatalf("a read failed or was counted: %v, calls %d", err, f.Calls())
	}
	if _, err := f.Put(ctx, "b", []byte("2"), store.PutOptions{}); !errors.Is(err, ErrInjected) {
		t.Fatalf("call 2: %v, want ErrInjected", err)
	}
	if _, err := m.Head(ctx, "b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a call failed before was applied")
	}
	if _, err := f.Head(ctx, "a"); !errors.Is(err, ErrInjected) {
		t.Fatalf("read after the fault: %v, want ErrInjected", err)
	}
	if _, err := f.Put(ctx, "c", nil, store.PutOptions{}); !errors.Is(err, ErrInjected) || !f.Injected() {
		t.Fatalf("write after the fault: %v, want ErrInjected", err)
	}
	if f.Calls() != 2 {
		t.Fatalf("calls %d after death, want 2", f.Calls())
	}
}

func TestFaultsAfter(t *testing.T) {
	ctx, m := context.Background(), NewMem()
	f := &Faults{B: m, N: 1, Mode: After}
	if err := f.Delete(ctx, "missing"); !errors.Is(err, ErrInjected) {
		t.Fatalf("call 1: %v, want ErrInjected", err)
	}
	f = &Faults{B: m, N: 1, Mode: After}
	if _, err := f.PutIfAbsent(ctx, "a", []byte("1"), store.PutOptions{}); !errors.Is(err, ErrInjected) {
		t.Fatalf("call 1: %v, want ErrInjected", err)
	}
	if b, _, err := m.Get(ctx, "a"); err != nil || string(b) != "1" {
		t.Fatalf("a call with a lost response was not applied: %q, %v", b, err)
	}
}

func TestFaultsRetried(t *testing.T) {
	ctx, m := context.Background(), NewMem()
	first, err := m.PutIfAbsent(ctx, "lock", []byte("v1"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := &Faults{B: m, N: 1, Mode: Retried}
	if _, err := f.PutIfMatch(ctx, "lock", first.ETag, []byte("v2"), store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("retried PutIfMatch: %v, want ErrPrecondition", err)
	}
	if !f.Injected() {
		t.Fatal("the fault did not fire")
	}
	body, obj, err := f.Get(ctx, "lock")
	if err != nil || string(body) != "v2" {
		t.Fatalf("read after the retry: %q, %v, want v2 and alive", body, err)
	}
	if _, err := f.PutIfMatch(ctx, "lock", obj.ETag, []byte("v3"), store.PutOptions{}); err != nil {
		t.Fatalf("write after the retry: %v", err)
	}
	if _, err := f.Put(ctx, "free", []byte("x"), store.PutOptions{}); err != nil || f.Calls() != 3 {
		t.Fatalf("unconditional write: %v, calls %d, want 3", err, f.Calls())
	}
}

func TestMemBucketPutIfMatch(t *testing.T) {
	ctx, m := context.Background(), NewMem()
	if _, err := m.PutIfMatch(ctx, "missing", `"1"`, nil, store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("missing key: %v, want ErrPrecondition as on R2", err)
	}
	a, err := m.Put(ctx, "a", []byte("1"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.PutIfMatch(ctx, "a", a.ETag, []byte("2"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutIfMatch(ctx, "a", a.ETag, []byte("3"), store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) || a.ETag == b.ETag {
		t.Fatalf("stale etag: %v, etags %s %s", err, a.ETag, b.ETag)
	}
}

func TestForEachFault(t *testing.T) {
	runs := 0
	var subtests []string
	ForEachFault(t, func(*testing.T) store.Bucket { return NewMem() }, func(t *testing.T, b store.Bucket, f *Faults) {
		runs++
		subtests = append(subtests, t.Name())
		ctx := context.Background()
		_, err1 := f.PutIfAbsent(ctx, "a", []byte("1"), store.PutOptions{})
		_, err2 := f.PutIfAbsent(ctx, "b", []byte("2"), store.PutOptions{})
		if !f.Injected() && (err1 != nil || err2 != nil) {
			t.Fatal(err1, err2)
		}
		// Rerun through the bucket itself: an existing key from the failed attempt is fine.
		for _, k := range []string{"a", "b"} {
			if _, err := b.PutIfAbsent(ctx, k, []byte(k), store.PutOptions{}); err != nil && !errors.Is(err, store.ErrPrecondition) {
				t.Fatal(err)
			}
		}
	})
	if runs != 1+2*len(Modes) {
		t.Fatalf("%d runs (%q), want 1 counting run plus 2 calls times %d modes", runs, subtests, len(Modes))
	}
}
