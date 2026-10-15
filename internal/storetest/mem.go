package storetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Mem is an in-memory Bucket for fast protocol tests. It is not an R2 model:
// it keeps only what the protocol relies on (conditional writes, ETag = MD5 kept
// by copies, the copy source condition, stamps), and every behaviour must also
// be tested on a real bucket.
type Mem struct {
	mu   sync.Mutex
	objs map[string]memObject
}

type memObject struct {
	obj  store.Object
	body []byte
}

var _ store.Bucket = (*Mem)(nil)

// NewMem returns an empty bucket.
func NewMem() *Mem { return &Mem{objs: map[string]memObject{}} }

func (m *Mem) Get(_ context.Context, key string) ([]byte, store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, store.Object{}, fmt.Errorf("get %s: %w", key, store.ErrNotFound)
	}
	return bytes.Clone(o.body), o.obj, nil
}

func (m *Mem) Open(ctx context.Context, key string) (io.ReadCloser, store.Object, error) {
	b, o, err := m.Get(ctx, key)
	if err != nil {
		return nil, o, err
	}
	return io.NopCloser(bytes.NewReader(b)), o, nil
}

func (m *Mem) Head(ctx context.Context, key string) (store.Object, error) {
	_, o, err := m.Get(ctx, key)
	return o, err
}

func (m *Mem) List(_ context.Context, prefix string, fn func(store.Object) error) error {
	m.mu.Lock()
	var objs []store.Object
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			objs = append(objs, store.Object{Key: k, Size: o.obj.Size, ETag: o.obj.ETag})
		}
	}
	m.mu.Unlock()
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	for _, o := range objs {
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

// put stores body when cond accepts the current object (nil when absent).
func (m *Mem) put(key string, body []byte, opts store.PutOptions, cond func(*store.Object) bool) (store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cur *store.Object
	if o, ok := m.objs[key]; ok {
		cur = &o.obj
	}
	if cond != nil && !cond(cur) {
		return store.Object{}, fmt.Errorf("put %s: %w", key, store.ErrPrecondition)
	}
	sums := hashes.Bytes(body)
	o := store.Object{
		Key:          key,
		Size:         sums.Size,
		ETag:         `"` + sums.MD5 + `"`, // a single part upload on R2
		SHA256:       sums.SHA256,
		SHA512:       opts.SHA512,
		Release:      opts.Release,
		ContentType:  opts.ContentType,
		CacheControl: opts.CacheControl,
	}
	m.objs[key] = memObject{obj: o, body: bytes.Clone(body)}
	return o, nil
}

func (m *Mem) PutIfAbsent(_ context.Context, key string, body []byte, opts store.PutOptions) (store.Object, error) {
	return m.put(key, body, opts, func(o *store.Object) bool { return o == nil })
}

// PutIfMatch fails with ErrPrecondition on a missing key, as R2 does.
func (m *Mem) PutIfMatch(_ context.Context, key, etag string, body []byte, opts store.PutOptions) (store.Object, error) {
	return m.put(key, body, opts, func(o *store.Object) bool { return o != nil && o.ETag == etag })
}

func (m *Mem) Put(_ context.Context, key string, body []byte, opts store.PutOptions) (store.Object, error) {
	return m.put(key, body, opts, nil)
}

func (m *Mem) PutFileIfAbsent(_ context.Context, key, path string, _ hashes.Sums, opts store.PutOptions) (store.Object, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return store.Object{}, err
	}
	return m.put(key, body, opts, func(o *store.Object) bool { return o == nil })
}

// CopyIfSourceMatch overwrites dst, as R2 does, and keeps the ETag and metadata of src.
func (m *Mem) CopyIfSourceMatch(_ context.Context, src, srcETag, dst string) (store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[src]
	if !ok {
		return store.Object{}, fmt.Errorf("copy %s to %s: %w", src, dst, store.ErrNotFound)
	}
	if o.obj.ETag != srcETag {
		return store.Object{}, fmt.Errorf("copy %s to %s: %w", src, dst, store.ErrPrecondition)
	}
	o.obj.Key = dst
	m.objs[dst] = o
	return o.obj, nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *Mem) DeleteOwned(ctx context.Context, key, release, sha256 string) (store.DeleteResult, error) {
	o, err := m.Head(ctx, key)
	switch {
	case err != nil:
		return store.Absent, nil
	case o.Release != release:
		return store.NotOwned, nil
	case o.SHA256 != sha256:
		return 0, fmt.Errorf("delete owned %s: %w", key, store.ErrHashMismatch)
	}
	return store.Deleted, m.Delete(ctx, key)
}
