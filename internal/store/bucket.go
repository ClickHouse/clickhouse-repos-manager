package store

import (
	"context"
	"io"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// Bucket is what the release protocol needs from a store. Tests wrap it to inject faults.
type Bucket interface {
	Get(ctx context.Context, key string) ([]byte, Object, error)
	Open(ctx context.Context, key string) (io.ReadCloser, Object, error)
	Head(ctx context.Context, key string) (Object, error)
	List(ctx context.Context, prefix string, fn func(Object) error) error

	PutIfAbsent(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error)
	PutIfMatch(ctx context.Context, key, etag string, body []byte, opts PutOptions) (Object, error)
	Put(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error)
	PutFileIfAbsent(ctx context.Context, key, path string, sums hashes.Sums, opts PutOptions) (Object, error)
	CopyIfSourceMatch(ctx context.Context, src, srcETag, dst string) (Object, error)
	Delete(ctx context.Context, key string) error
	DeleteOwned(ctx context.Context, key, release, sha256 string) (DeleteResult, error)
}

var _ Bucket = (*Store)(nil)
