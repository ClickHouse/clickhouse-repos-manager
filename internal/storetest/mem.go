package storetest

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Mem is an in-memory Bucket for fast protocol tests. It is not an R2 model:
// it keeps only what the protocol relies on (conditional writes, ETag = MD5,
// the copy source condition, stamps, CRC64NVME), and every behaviour must also
// be tested on a real bucket.
//
// Files above PartSize get a multipart ETag, which a copy to another key
// replaces with the MD5, as on R2.
type Mem struct {
	PartSize int64

	mu   sync.Mutex
	objs map[string]memObject
}

type memObject struct {
	obj  store.Object
	body []byte
}

var _ store.Bucket = (*Mem)(nil)

// NewMem returns an empty bucket.
func NewMem() *Mem { return &Mem{PartSize: store.DefaultPartSize, objs: map[string]memObject{}} }

func (m *Mem) Get(_ context.Context, key string) ([]byte, store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, store.Object{}, fmt.Errorf("get %s: %w", key, store.ErrNotFound)
	}
	return bytes.Clone(o.body), o.obj, nil
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
// A non empty etag replaces the single part ETag.
func (m *Mem) put(key string, body []byte, opts store.PutOptions, etag string, cond func(*store.Object) bool) (store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cur *store.Object
	if o, ok := m.objs[key]; ok {
		cur = &o.obj
	}
	if cond != nil && !cond(cur) {
		return store.Object{}, fmt.Errorf("put %s: %w", key, store.ErrPrecondition)
	}
	o := memObj(key, body, opts)
	if etag != "" {
		o.ETag = etag
	}
	m.objs[key] = memObject{obj: o, body: bytes.Clone(body)}
	return o, nil
}

// memObj is the object a single part upload of body creates on R2.
func memObj(key string, body []byte, opts store.PutOptions) store.Object {
	sums := hashes.Bytes(body)
	return store.Object{
		Key:          key,
		Size:         sums.Size,
		ETag:         `"` + sums.MD5 + `"`,
		SHA256:       sums.SHA256,
		SHA512:       opts.SHA512,
		Release:      opts.Release,
		ContentType:  opts.ContentType,
		CacheControl: opts.CacheControl,
		CRC64NVME:    sums.CRC64NVME,
	}
}

// multipartETag is the S3 ETag of body uploaded in parts of size part.
func multipartETag(body []byte, part int64) string {
	var md5s []byte
	n := 0
	for off := int64(0); off < int64(len(body)); off += part {
		sum := md5.Sum(body[off:min(off+part, int64(len(body)))])
		md5s = append(md5s, sum[:]...)
		n++
	}
	sum := md5.Sum(md5s)
	return fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sum[:]), n)
}

func (m *Mem) PutIfAbsent(_ context.Context, key string, body []byte, opts store.PutOptions) (store.Object, error) {
	return m.put(key, body, opts, "", func(o *store.Object) bool { return o == nil })
}

// PutIfMatch fails with ErrPrecondition on a missing key, as R2 does.
func (m *Mem) PutIfMatch(_ context.Context, key, etag string, body []byte, opts store.PutOptions) (store.Object, error) {
	return m.put(key, body, opts, "", func(o *store.Object) bool { return o != nil && o.ETag == etag })
}

func (m *Mem) Put(_ context.Context, key string, body []byte, opts store.PutOptions) (store.Object, error) {
	return m.put(key, body, opts, "", nil)
}

// PutForeign writes an object as another tool did: without the SHA256
// metadata and the stamp, as every production object before ch-deploy.
func (m *Mem) PutForeign(key string, body []byte) {
	o, _ := m.put(key, body, store.PutOptions{}, "", nil)
	o.SHA256 = ""
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObject{obj: o, body: bytes.Clone(body)}
}

func (m *Mem) PutFileIfAbsent(_ context.Context, key, path string, _ hashes.Sums, opts store.PutOptions) (store.Object, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return store.Object{}, err
	}
	etag := ""
	if m.PartSize > 0 && int64(len(body)) > m.PartSize {
		etag = multipartETag(body, m.PartSize)
	}
	return m.put(key, body, opts, etag, func(o *store.Object) bool { return o == nil })
}

// CopyIfSourceMatch overwrites dst, as R2 does, and keeps the metadata of src.
// The ETag stays on a copy onto the same key; a multipart ETag becomes the MD5
// on a copy to another key.
func (m *Mem) CopyIfSourceMatch(_ context.Context, src, srcETag, dst string) (store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, err := m.source(src, srcETag, dst)
	if err != nil {
		return store.Object{}, err
	}
	if src != dst {
		o.obj.ETag = memObj(dst, o.body, store.PutOptions{}).ETag
	}
	o.obj.Key = dst
	m.objs[dst] = o
	return o.obj, nil
}

// CopyIfAbsent fails with ErrPrecondition when dst exists, and gives the copy
// the metadata of opts and sums.
func (m *Mem) CopyIfAbsent(_ context.Context, src, srcETag, dst string, sums hashes.Sums, opts store.PutOptions) (store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[dst]; ok {
		return store.Object{}, fmt.Errorf("copy %s to %s: exists: %w", src, dst, store.ErrPrecondition)
	}
	o, err := m.source(src, srcETag, dst)
	if err != nil {
		return store.Object{}, err
	}
	obj := memObj(dst, o.body, opts)
	obj.SHA256 = sums.SHA256
	m.objs[dst] = memObject{obj: obj, body: o.body}
	return obj, nil
}

func (m *Mem) source(src, srcETag, dst string) (memObject, error) {
	o, ok := m.objs[src]
	if !ok {
		return memObject{}, fmt.Errorf("copy %s to %s: %w", src, dst, store.ErrNotFound)
	}
	if o.obj.ETag != srcETag {
		return memObject{}, fmt.Errorf("copy %s to %s: %w", src, dst, store.ErrPrecondition)
	}
	return o, nil
}

// AbortUploads finds nothing: Mem has no multipart uploads.
func (m *Mem) AbortUploads(context.Context, string) (int, error) { return 0, nil }

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

func (m *Mem) DeleteStamped(ctx context.Context, key string) (store.DeleteResult, error) {
	o, err := m.Head(ctx, key)
	switch {
	case err != nil:
		return store.Absent, nil
	case o.Release == "":
		return store.NotStamped, nil
	}
	return store.Deleted, m.Delete(ctx, key)
}
