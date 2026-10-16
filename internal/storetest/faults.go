package storetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// ErrInjected is returned by Faults for the failed call and every call after it
// (modes Before and After).
var ErrInjected = errors.New("injected fault")

// Mode says when the failing call fails.
type Mode int

const (
	// Before fails the call without sending it.
	Before Mode = iota
	// After sends the call and loses its response: the change is applied, the caller sees an error.
	After
	// Retried sends the call twice, as an SDK retry after a lost response does.
	// The caller sees the second result, for example ErrPrecondition from a
	// conditional write that the first attempt applied. Nothing dies.
	Retried
)

// Modes lists every Mode, in the order ForEachFault runs them.
var Modes = []Mode{Before, After, Retried}

func (m Mode) String() string {
	switch m {
	case After:
		return "after"
	case Retried:
		return "retried"
	}
	return "before"
}

// Faults wraps a Bucket and fails its Nth mutating call (counted from 1; 0 never fails).
//
// In modes Before and After every call fails after the fault, reads included,
// as in a killed process; a rerun uses a fresh wrapper, or the bucket itself,
// over the same prefix. In mode Retried the process goes on.
type Faults struct {
	B    store.Bucket
	N    int
	Mode Mode
	// Log, when set, gets one line per call.
	Log func(format string, args ...any)

	mu    sync.Mutex
	calls int
	fired bool
	dead  bool
}

var _ store.Bucket = (*Faults)(nil)

// Calls is the number of mutating calls seen so far, including the failed one.
func (f *Faults) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Injected reports whether the fault fired.
func (f *Faults) Injected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

func (f *Faults) log(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

func (f *Faults) read(op, key string) error {
	f.mu.Lock()
	dead := f.dead
	f.mu.Unlock()
	if dead {
		f.log("%s %s: dead", op, key)
		return fmt.Errorf("%s %s: %w", op, key, ErrInjected)
	}
	f.log("%s %s", op, key)
	return nil
}

func (f *Faults) mutate(op, key string, do func() error) error {
	f.mu.Lock()
	if f.dead {
		f.mu.Unlock()
		f.log("%s %s: dead", op, key)
		return fmt.Errorf("%s %s: %w", op, key, ErrInjected)
	}
	f.calls++
	n := f.calls
	fire := n == f.N
	if fire {
		f.fired = true
		f.dead = f.Mode == Before
	}
	f.mu.Unlock()

	if fire && f.Mode == Before {
		f.log("#%d %s %s: fail before", n, op, key)
		return fmt.Errorf("%s %s: %w", op, key, ErrInjected)
	}
	err := do()
	if fire && f.Mode == Retried {
		first := err
		err = do()
		f.log("#%d %s %s: applied (%v), response lost, retry: %v", n, op, key, first, err)
		return err
	}
	if fire {
		f.mu.Lock()
		f.dead = true
		f.mu.Unlock()
		f.log("#%d %s %s: applied (%v), response lost", n, op, key, err)
		return fmt.Errorf("%s %s: %w", op, key, ErrInjected)
	}
	f.log("#%d %s %s: %v", n, op, key, err)
	return err
}

func (f *Faults) Get(ctx context.Context, key string) ([]byte, store.Object, error) {
	if err := f.read("get", key); err != nil {
		return nil, store.Object{}, err
	}
	return f.B.Get(ctx, key)
}

func (f *Faults) Open(ctx context.Context, key string) (io.ReadCloser, store.Object, error) {
	if err := f.read("open", key); err != nil {
		return nil, store.Object{}, err
	}
	return f.B.Open(ctx, key)
}

func (f *Faults) Head(ctx context.Context, key string) (store.Object, error) {
	if err := f.read("head", key); err != nil {
		return store.Object{}, err
	}
	return f.B.Head(ctx, key)
}

func (f *Faults) List(ctx context.Context, prefix string, fn func(store.Object) error) error {
	if err := f.read("list", prefix); err != nil {
		return err
	}
	return f.B.List(ctx, prefix, fn)
}

func (f *Faults) PutIfAbsent(ctx context.Context, key string, body []byte, opts store.PutOptions) (obj store.Object, err error) {
	err = f.mutate("put if absent", key, func() (e error) { obj, e = f.B.PutIfAbsent(ctx, key, body, opts); return })
	return obj, err
}

func (f *Faults) PutIfMatch(ctx context.Context, key, etag string, body []byte, opts store.PutOptions) (obj store.Object, err error) {
	err = f.mutate("put if match", key, func() (e error) { obj, e = f.B.PutIfMatch(ctx, key, etag, body, opts); return })
	return obj, err
}

func (f *Faults) Put(ctx context.Context, key string, body []byte, opts store.PutOptions) (obj store.Object, err error) {
	err = f.mutate("put", key, func() (e error) { obj, e = f.B.Put(ctx, key, body, opts); return })
	return obj, err
}

func (f *Faults) PutFileIfAbsent(ctx context.Context, key, path string, sums hashes.Sums, opts store.PutOptions) (obj store.Object, err error) {
	err = f.mutate("put file if absent", key, func() (e error) { obj, e = f.B.PutFileIfAbsent(ctx, key, path, sums, opts); return })
	return obj, err
}

func (f *Faults) CopyIfSourceMatch(ctx context.Context, src, srcETag, dst string) (obj store.Object, err error) {
	err = f.mutate("copy "+src+" to", dst, func() (e error) { obj, e = f.B.CopyIfSourceMatch(ctx, src, srcETag, dst); return })
	return obj, err
}

func (f *Faults) CopyIfAbsent(ctx context.Context, src, srcETag, dst string, sums hashes.Sums, opts store.PutOptions) (obj store.Object, err error) {
	err = f.mutate("copy if absent "+src+" to", dst, func() (e error) { obj, e = f.B.CopyIfAbsent(ctx, src, srcETag, dst, sums, opts); return })
	return obj, err
}

func (f *Faults) AbortUploads(ctx context.Context, key string) (n int, err error) {
	err = f.mutate("abort uploads", key, func() (e error) { n, e = f.B.AbortUploads(ctx, key); return })
	return n, err
}

func (f *Faults) Delete(ctx context.Context, key string) error {
	return f.mutate("delete", key, func() error { return f.B.Delete(ctx, key) })
}

func (f *Faults) DeleteOwned(ctx context.Context, key, release, sha256 string) (res store.DeleteResult, err error) {
	err = f.mutate("delete owned", key, func() (e error) { res, e = f.B.DeleteOwned(ctx, key, release, sha256); return })
	return res, err
}

// ForEachFault runs scenario once without a fault to count its mutating calls,
// then as a subtest for every call number and every mode.
//
// Each run gets a fresh bucket from newBucket and a Faults wrapper over it.
// The scenario runs its steps through faulty, then reruns or checks through bucket.
func ForEachFault(t *testing.T, newBucket func(t *testing.T) store.Bucket, scenario func(t *testing.T, bucket store.Bucket, faulty *Faults)) {
	t.Helper()
	count := &Faults{B: newBucket(t), Log: t.Logf}
	scenario(t, count.B, count)
	if t.Failed() {
		return
	}
	n := count.Calls()
	if n == 0 {
		t.Fatal("the scenario made no mutating call")
	}
	for i := 1; i <= n; i++ {
		for _, mode := range Modes {
			t.Run(fmt.Sprintf("call%d-%s", i, mode), func(t *testing.T) {
				b := newBucket(t)
				f := &Faults{B: b, N: i, Mode: mode, Log: t.Logf}
				scenario(t, b, f)
				if !f.Injected() {
					t.Errorf("call %d (%s) was never made; the scenario is not deterministic", i, mode)
				}
			})
		}
	}
}
