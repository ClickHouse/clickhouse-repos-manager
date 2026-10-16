package store

import (
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// SetAfterPart installs a hook that runs after each uploaded part.
func (s *Store) SetAfterPart(fn func(part int32) error) { s.afterPart = fn }

// SetPartSums installs a hook that may change the checksums sent with each part.
func (s *Store) SetPartSums(fn func(part int32, sums *hashes.Part)) { s.partSums = fn }

// OpenUpload starts a multipart upload of path in parts of partSize with the
// metadata of opts and sha256, uploads only the given part numbers and leaves
// the upload open, like a killed run.
func (s *Store) OpenUpload(ctx context.Context, key, path string, partSize int64, sha256 string, opts PutOptions, numbers ...int32) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: s.full(key), Metadata: opts.metadata(sha256)})
	if err != nil {
		return s.wrap("create multipart upload", key, err)
	}
	saved := s.partSize
	s.partSize = partSize
	defer func() { s.partSize = saved }()
	for _, n := range numbers {
		if _, err := s.uploadPart(ctx, key, created.UploadId, f, st.Size(), int(n-1)); err != nil {
			return err
		}
	}
	return nil
}
