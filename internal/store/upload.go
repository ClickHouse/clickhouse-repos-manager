package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// PutFileIfAbsent uploads the file at path, failing with ErrPrecondition if the key exists.
//
// sums must be the hashes of the file; its size is checked before the upload.
// The file is streamed and never held in memory. Files above the part size go
// as a multipart upload, which is aborted on any failure. R2 cannot verify a
// multipart object as a whole, so the caller checks it after the upload.
func (s *Store) PutFileIfAbsent(ctx context.Context, key, path string, sums hashes.Sums, opts PutOptions) (Object, error) {
	f, err := os.Open(path)
	if err != nil {
		return Object{}, fmt.Errorf("put %s: %w", key, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Object{}, fmt.Errorf("put %s: %w", key, err)
	}
	if st.Size() != sums.Size {
		return Object{}, fmt.Errorf("put %s: %s has %d bytes, the hashes are for %d", key, path, st.Size(), sums.Size)
	}
	if parts := (sums.Size + s.partSize - 1) / s.partSize; parts > MaxParts {
		return Object{}, fmt.Errorf("put %s: %d bytes need %d parts of %d, more than %d", key, sums.Size, parts, s.partSize, MaxParts)
	}

	// Saves an upload; the conditional write below still decides.
	switch _, err := s.Head(ctx, key); {
	case err == nil:
		return Object{}, fmt.Errorf("put %s: exists: %w", key, ErrPrecondition)
	case !errors.Is(err, ErrNotFound):
		return Object{}, err
	}

	if sums.Size <= s.partSize {
		return s.putObject(ctx, key, io.NewSectionReader(f, 0, sums.Size), sums, opts,
			func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
	}
	return s.putMultipart(ctx, key, f, sums, opts)
}

func (s *Store) putMultipart(ctx context.Context, key string, f io.ReaderAt, sums hashes.Sums, opts PutOptions) (Object, error) {
	in := &s3.CreateMultipartUploadInput{
		Bucket:   &s.bucket,
		Key:      s.full(key),
		Metadata: opts.metadata(sums.SHA256),
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if opts.CacheControl != "" {
		in.CacheControl = aws.String(opts.CacheControl)
	}
	created, err := s.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return Object{}, s.wrap("create multipart upload", key, err)
	}
	id := created.UploadId

	done := false
	defer func() {
		if !done {
			s.abort(ctx, key, id)
		}
	}()

	parts, err := s.uploadParts(ctx, key, id, f, sums.Size)
	if err != nil {
		return Object{}, err
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &s.bucket,
		Key:             s.full(key),
		UploadId:        id,
		IfNoneMatch:     aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return Object{}, s.wrap("complete multipart upload", key, err)
	}
	done = true
	return opts.object(key, sums.Size, aws.ToString(out.ETag), sums.SHA256), nil
}

// abort runs even when ctx is cancelled. An upload consumed by a failed
// completion (R2 412) is already gone, and the abort succeeds or finds nothing.
func (s *Store) abort(ctx context.Context, key string, id *string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_, _ = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: s.full(key), UploadId: id})
}

func (s *Store) uploadParts(ctx context.Context, key string, id *string, f io.ReaderAt, size int64) ([]types.CompletedPart, error) {
	n := int((size + s.partSize - 1) / s.partSize)
	parts := make([]types.CompletedPart, n)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(s.uploadConcurrency, n) {
		wg.Go(func() {
			for i := range jobs {
				part, err := s.uploadPart(ctx, key, id, f, size, i)
				if err != nil {
					cancel(err)
					continue
				}
				parts[i] = part
			}
		})
	}
feed:
	for i := range n {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return parts, nil
}

func (s *Store) uploadPart(ctx context.Context, key string, id *string, f io.ReaderAt, size int64, i int) (types.CompletedPart, error) {
	if err := ctx.Err(); err != nil {
		return types.CompletedPart{}, context.Cause(ctx)
	}
	off := int64(i) * s.partSize
	length := min(s.partSize, size-off)
	number := int32(i + 1)
	section := io.NewSectionReader(f, off, length)
	md5, err := hashes.ContentMD5(section)
	if err != nil {
		return types.CompletedPart{}, fmt.Errorf("upload part %d of %s: %w", number, key, err)
	}
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        &s.bucket,
		Key:           s.full(key),
		UploadId:      id,
		PartNumber:    aws.Int32(number),
		Body:          io.NewSectionReader(f, off, length),
		ContentLength: aws.Int64(length),
		ContentMD5:    aws.String(md5),
	})
	if err != nil {
		return types.CompletedPart{}, s.wrap(fmt.Sprintf("upload part %d of", number), key, err)
	}
	if s.afterPart != nil {
		if err := s.afterPart(number); err != nil {
			return types.CompletedPart{}, err
		}
	}
	return types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)}, nil
}

// OpenUploads lists the keys of unfinished multipart uploads under prefix.
// Miniflare does not implement it.
func (s *Store) OpenUploads(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	in := &s3.ListMultipartUploadsInput{Bucket: &s.bucket, Prefix: aws.String(s.prefix + prefix)}
	for {
		out, err := s.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return nil, s.wrap("list multipart uploads", prefix, err)
		}
		for _, u := range out.Uploads {
			keys = append(keys, aws.ToString(u.Key)[len(s.prefix):])
		}
		if !aws.ToBool(out.IsTruncated) {
			return keys, nil
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}
