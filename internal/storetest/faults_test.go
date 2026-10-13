package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// memBucket is just enough of a bucket to test Faults; it is not an R2 model.
type memBucket struct {
	mu   sync.Mutex
	objs map[string]store.Object
	body map[string][]byte
	n    int
}

func newMem() *memBucket {
	return &memBucket{objs: map[string]store.Object{}, body: map[string][]byte{}}
}

func (m *memBucket) Get(_ context.Context, key string) ([]byte, store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, store.Object{}, store.ErrNotFound
	}
	return m.body[key], o, nil
}

func (m *memBucket) Open(ctx context.Context, key string) (io.ReadCloser, store.Object, error) {
	b, o, err := m.Get(ctx, key)
	return io.NopCloser(bytes.NewReader(b)), o, err
}

func (m *memBucket) Head(ctx context.Context, key string) (store.Object, error) {
	_, o, err := m.Get(ctx, key)
	return o, err
}

func (m *memBucket) List(context.Context, string, func(store.Object) error) error { return nil }

// put stores body when cond accepts the current object (nil when absent).
func (m *memBucket) put(key string, body []byte, cond func(*store.Object) bool) (store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cur *store.Object
	if o, ok := m.objs[key]; ok {
		cur = &o
	}
	if cond != nil && !cond(cur) {
		return store.Object{}, store.ErrPrecondition
	}
	m.n++
	o := store.Object{Key: key, Size: int64(len(body)), ETag: fmt.Sprintf("%q", fmt.Sprint(m.n))}
	m.objs[key], m.body[key] = o, body
	return o, nil
}

func (m *memBucket) PutIfAbsent(_ context.Context, key string, body []byte, _ store.PutOptions) (store.Object, error) {
	return m.put(key, body, func(o *store.Object) bool { return o == nil })
}

func (m *memBucket) PutIfMatch(_ context.Context, key, etag string, body []byte, _ store.PutOptions) (store.Object, error) {
	return m.put(key, body, func(o *store.Object) bool { return o != nil && o.ETag == etag })
}

func (m *memBucket) Put(_ context.Context, key string, body []byte, _ store.PutOptions) (store.Object, error) {
	return m.put(key, body, nil)
}

func (m *memBucket) PutFileIfAbsent(context.Context, string, string, hashes.Sums, store.PutOptions) (store.Object, error) {
	return store.Object{}, errors.New("not implemented")
}

func (m *memBucket) CopyIfSourceMatch(context.Context, string, string, string) (store.Object, error) {
	return store.Object{}, errors.New("not implemented")
}

func (m *memBucket) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	delete(m.body, key)
	return nil
}

func (m *memBucket) DeleteOwned(ctx context.Context, key, _, _ string) (store.DeleteResult, error) {
	return store.Deleted, m.Delete(ctx, key)
}

func TestFaultsBefore(t *testing.T) {
	ctx, m := context.Background(), newMem()
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
	ctx, m := context.Background(), newMem()
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
	ctx, m := context.Background(), newMem()
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
	ctx, m := context.Background(), newMem()
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
	ForEachFault(t, func(*testing.T) store.Bucket { return newMem() }, func(t *testing.T, b store.Bucket, f *Faults) {
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
