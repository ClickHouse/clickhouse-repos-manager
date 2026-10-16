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
// as a multipart upload. A failed one stays open, and the next call for the
// key continues it with the parts that match the file (NoResumeUploads aborts
// it instead). A continued upload keeps the metadata it was created with, which
// the bucket does not list. R2 cannot verify a multipart object as a whole, so
// the caller checks content and metadata after the upload.
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
	var id *string
	var have map[int32]types.CompletedPart
	if s.resume {
		var err error
		if id, have, err = s.resumable(ctx, key, f, sums.Size); err != nil {
			return Object{}, err
		}
	}
	if id == nil {
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
		id = created.UploadId
	}

	done := false
	defer func() {
		if !done && !s.resume {
			s.abort(ctx, key, id)
		}
	}()

	parts, err := s.uploadParts(ctx, key, id, f, sums.Size, have)
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

// resumable picks the open upload of key with the most parts that match the
// file and returns them; it aborts the other uploads, and every upload whose
// parts have another size than the current part size.
func (s *Store) resumable(ctx context.Context, key string, f io.ReaderAt, size int64) (*string, map[int32]types.CompletedPart, error) {
	ids, err := s.uploads(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	var best *string
	var bestParts map[int32]types.CompletedPart
	for _, id := range ids {
		parts, ok, err := s.matchingParts(ctx, key, id, f, size)
		if err != nil {
			return nil, nil, err
		}
		loser := id
		if ok && (best == nil || len(parts) > len(bestParts)) {
			loser, best, bestParts = best, id, parts
		}
		if loser != nil {
			if err := s.abortUpload(ctx, key, loser); err != nil {
				return nil, nil, err
			}
		}
	}
	return best, bestParts, nil
}

// matchingParts lists the parts of an upload and keeps those whose MD5 (the
// part ETag on R2) matches the file section. ok is false when a part has
// another size than the current part size gives.
func (s *Store) matchingParts(ctx context.Context, key string, id *string, f io.ReaderAt, size int64) (map[int32]types.CompletedPart, bool, error) {
	n := int32((size + s.partSize - 1) / s.partSize)
	parts := map[int32]types.CompletedPart{}
	in := &s3.ListPartsInput{Bucket: &s.bucket, Key: s.full(key), UploadId: id}
	for {
		out, err := s.client.ListParts(ctx, in)
		if err != nil {
			return nil, false, s.wrap("list parts of", key, err)
		}
		for _, p := range out.Parts {
			number := aws.ToInt32(p.PartNumber)
			if number < 1 || number > n {
				return nil, false, nil
			}
			off := int64(number-1) * s.partSize
			if aws.ToInt64(p.Size) != min(s.partSize, size-off) {
				return nil, false, nil
			}
			local, err := hashes.SumPart(io.NewSectionReader(f, off, aws.ToInt64(p.Size)))
			if err != nil {
				return nil, false, fmt.Errorf("resume %s: %w", key, err)
			}
			if aws.ToString(p.ETag) == `"`+local.MD5+`"` {
				parts[number] = types.CompletedPart{ETag: p.ETag, PartNumber: aws.Int32(number)}
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return parts, true, nil
		}
		in.PartNumberMarker = out.NextPartNumberMarker
	}
}

// uploads lists the ids of the open multipart uploads of exactly key.
func (s *Store) uploads(ctx context.Context, key string) ([]*string, error) {
	full := aws.ToString(s.full(key))
	var ids []*string
	in := &s3.ListMultipartUploadsInput{Bucket: &s.bucket, Prefix: aws.String(full)}
	for {
		out, err := s.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return nil, s.wrap("list multipart uploads", key, err)
		}
		for _, u := range out.Uploads {
			if aws.ToString(u.Key) == full {
				ids = append(ids, u.UploadId)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return ids, nil
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// AbortUploads aborts every open multipart upload of key and returns their
// number. With NoResumeUploads no upload is left open, and it does nothing.
func (s *Store) AbortUploads(ctx context.Context, key string) (int, error) {
	if !s.resume {
		return 0, nil
	}
	ids, err := s.uploads(ctx, key)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.abortUpload(ctx, key, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func (s *Store) abortUpload(ctx context.Context, key string, id *string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: s.full(key), UploadId: id})
	if err != nil {
		return s.wrap("abort multipart upload", key, err)
	}
	return nil
}

// abort runs even when ctx is cancelled. An upload consumed by a failed
// completion (R2 412) is already gone, and the abort succeeds or finds nothing.
func (s *Store) abort(ctx context.Context, key string, id *string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_ = s.abortUpload(ctx, key, id)
}

// uploadParts uploads every part that have lacks and returns all parts in order.
func (s *Store) uploadParts(ctx context.Context, key string, id *string, f io.ReaderAt, size int64, have map[int32]types.CompletedPart) ([]types.CompletedPart, error) {
	n := int((size + s.partSize - 1) / s.partSize)
	parts := make([]types.CompletedPart, n)
	var todo []int
	for i := range n {
		if p, ok := have[int32(i+1)]; ok {
			parts[i] = p
		} else {
			todo = append(todo, i)
		}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(s.uploadConcurrency, len(todo)) {
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
	for _, i := range todo {
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
	sums, err := hashes.SumPart(io.NewSectionReader(f, off, length))
	if err != nil {
		return types.CompletedPart{}, fmt.Errorf("upload part %d of %s: %w", number, key, err)
	}
	if s.partSums != nil {
		s.partSums(number, &sums)
	}
	md5, err := hashes.Base64(sums.MD5)
	if err != nil {
		return types.CompletedPart{}, err
	}
	crc, err := hashes.Base64(sums.CRC64NVME)
	if err != nil {
		return types.CompletedPart{}, err
	}
	var out *s3.UploadPartOutput
	err = s.send(ctx, func() (e error) {
		out, e = s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:            &s.bucket,
			Key:               s.full(key),
			UploadId:          id,
			PartNumber:        aws.Int32(number),
			Body:              io.NewSectionReader(f, off, length),
			ContentLength:     aws.Int64(length),
			ContentMD5:        aws.String(md5),
			ChecksumCRC64NVME: aws.String(crc),
		})
		return
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
