package store

import (
	"context"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// Bucket is what the release protocol needs from a store. Tests wrap it to inject faults.
// It has no streaming download: the protocol checks objects by their metadata.
type Bucket interface {
	Get(ctx context.Context, key string) ([]byte, Object, error)
	Head(ctx context.Context, key string) (Object, error)
	List(ctx context.Context, prefix string, fn func(Object) error) error

	PutIfAbsent(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error)
	PutIfMatch(ctx context.Context, key, etag string, body []byte, opts PutOptions) (Object, error)
	Put(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error)
	PutFileIfAbsent(ctx context.Context, key, path string, sums hashes.Sums, opts PutOptions) (Object, error)
	CopyIfSourceMatch(ctx context.Context, src, srcETag, dst string) (Object, error)
	CopyIfAbsent(ctx context.Context, src, srcETag, dst string, sums hashes.Sums, opts PutOptions) (Object, error)
	AbortUploads(ctx context.Context, key string) (int, error)
	Delete(ctx context.Context, key string) error
	DeleteOwned(ctx context.Context, key, release, sha256 string) (DeleteResult, error)
}

var _ Bucket = (*Store)(nil)
