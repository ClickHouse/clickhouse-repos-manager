// Package store is the only way the tool talks to the bucket.
//
// It knows the R2 quirks that shape the release protocol: conditional PUT works,
// conditions on the CopyObject destination and on DeleteObject are ignored,
// and only the CopyObject source condition is honored.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

var (
	// ErrNotFound means the object does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrPrecondition means a conditional request failed (HTTP 412).
	//
	// A retried request may fail with it even though the first attempt succeeded
	// and only its response was lost, so callers must read the object and compare
	// before treating it as a conflict.
	ErrPrecondition = errors.New("precondition failed")
	// ErrHashMismatch means an object carries the expected release stamp but other content.
	ErrHashMismatch = errors.New("hash mismatch")
)

// Custom metadata keys (x-amz-meta-*).
const (
	// MetaSHA256 holds the hex SHA256 of the object content.
	MetaSHA256 = "sha256"
	// MetaSHA512 holds the hex SHA512 of the object content, set for TGZ files.
	MetaSHA512 = "sha512"
	// MetaRelease is the stamp: the tag of the release that created the object.
	MetaRelease = "release"
)

// Multipart upload limits.
const (
	DefaultPartSize          = 64 << 20
	MinPartSize              = 5 << 20
	MaxParts                 = 10000
	DefaultUploadConcurrency = 4
)

// TestPrefix is the only key prefix where Config.DeleteAnyKey is accepted.
const TestPrefix = "ch-deploy-test/"

// Config holds the connection parameters.
type Config struct {
	Endpoint        string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	// Deletable lists key prefixes (relative to Prefix) where Delete is allowed.
	Deletable []string
	// DeleteAnyKey allows Delete of every key under Prefix, which must be below TestPrefix.
	DeleteAnyKey bool
	// PartSize is the multipart part size; files up to it go in one PUT. Default DefaultPartSize.
	PartSize int64
	// UploadConcurrency limits the requests that send data (parts, single PUTs,
	// copies) in flight at once. Default DefaultUploadConcurrency.
	UploadConcurrency int
	// NoResumeUploads is for buckets without ListMultipartUploads and ListParts
	// (Miniflare): a failed multipart upload is aborted instead of kept for a rerun.
	NoResumeUploads bool
}

// Object describes a stored object.
type Object struct {
	Key     string
	Size    int64
	ETag    string
	SHA256  string // hex, from MetaSHA256; empty for objects not written by this tool
	SHA512  string // hex, from MetaSHA512
	Release string // the stamp, from MetaRelease
	// CRC64NVME is the hex full object checksum from Head; R2 keeps one for
	// every object, Miniflare none.
	CRC64NVME    string
	ContentType  string
	CacheControl string
}

// PutOptions are the HTTP headers and metadata stored with an object.
type PutOptions struct {
	ContentType  string
	CacheControl string
	// Release is the stamp; empty for private state.
	Release string
	// SHA512 is stored as MetaSHA512 when set.
	SHA512 string
}

func (o PutOptions) metadata(sha256 string) map[string]string {
	m := map[string]string{MetaSHA256: sha256}
	if o.Release != "" {
		m[MetaRelease] = o.Release
	}
	if o.SHA512 != "" {
		m[MetaSHA512] = o.SHA512
	}
	return m
}

func (o PutOptions) object(key string, size int64, etag, sha256 string) Object {
	return Object{
		Key:          key,
		Size:         size,
		ETag:         etag,
		SHA256:       sha256,
		SHA512:       o.SHA512,
		Release:      o.Release,
		ContentType:  o.ContentType,
		CacheControl: o.CacheControl,
	}
}

// DeleteResult is the outcome of DeleteOwned and DeleteStamped.
type DeleteResult int

const (
	// Deleted means the object carried the stamp and hash and is gone now.
	Deleted DeleteResult = iota + 1
	// Absent means there was no object.
	Absent
	// NotOwned means the object carries no stamp or another one, and stays.
	NotOwned
	// NotStamped means the object carries no stamp at all, and stays.
	NotStamped
)

func (r DeleteResult) String() string {
	switch r {
	case Deleted:
		return "deleted"
	case Absent:
		return "absent"
	case NotOwned:
		return "not owned"
	case NotStamped:
		return "not stamped"
	}
	return fmt.Sprintf("DeleteResult(%d)", int(r))
}

// Store is a bucket, optionally narrowed to a key prefix.
type Store struct {
	client            *s3.Client
	bucket            string
	prefix            string
	deletable         []string
	partSize          int64
	uploadConcurrency int
	resume            bool
	// sending holds a token for every request in flight that sends data.
	sending chan struct{}
	// afterPart, set only by tests, runs after each uploaded part and can fail the upload.
	afterPart func(part int32) error
	// partSums, set only by tests, may change the checksums sent with a part.
	partSums func(part int32, sums *hashes.Part)
	// sendHook, set only by tests, runs inside send with the number of requests in flight.
	sendHook func(inFlight int)
}

// New creates a Store. Credentials come only from cfg; shared AWS config files are never read.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("endpoint and bucket are required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("access key id and secret access key are required")
	}
	prefix := normalizePrefix(cfg.Prefix)
	var deletable []string
	for _, d := range cfg.Deletable {
		d = normalizePrefix(d)
		if d == "" {
			return nil, errors.New("an empty deletable prefix would allow deleting everything")
		}
		deletable = append(deletable, d)
	}
	if cfg.DeleteAnyKey {
		if !strings.HasPrefix(prefix, TestPrefix) || prefix == TestPrefix || strings.Contains(prefix, "..") {
			return nil, fmt.Errorf("delete any key is allowed only below %s, not under prefix %q", TestPrefix, prefix)
		}
		deletable = []string{""}
	}
	partSize := cfg.PartSize
	if partSize == 0 {
		partSize = DefaultPartSize
	}
	if partSize < MinPartSize {
		return nil, fmt.Errorf("part size %d is below the minimum %d", partSize, MinPartSize)
	}
	concurrency := cfg.UploadConcurrency
	if concurrency == 0 {
		concurrency = DefaultUploadConcurrency
	}
	if concurrency < 1 {
		return nil, fmt.Errorf("upload concurrency %d is below 1", concurrency)
	}

	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		// Checksums are set explicitly per request; SDK defaults (CRC32 trailers) are not relied on.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 awshttp.NewBuildableClient().WithTimeout(10 * time.Minute),
		// workerd (the local R2 simulator) never answers Expect: 100-continue.
		ContinueHeaderThresholdBytes: -1,
		APIOptions:                   []func(*middleware.Stack) error{unsignedAcceptEncoding},
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 8
			o.MaxBackoff = 30 * time.Second
			// R2 answers ServiceUnavailable "Reduce your concurrent request rate" under load.
			o.Retryables = append(o.Retryables, retry.RetryableErrorCode{Codes: map[string]struct{}{"ServiceUnavailable": {}}})
		}),
	})
	return &Store{
		client: client, bucket: cfg.Bucket, prefix: prefix, deletable: deletable,
		partSize: partSize, uploadConcurrency: concurrency, resume: !cfg.NoResumeUploads,
		sending: make(chan struct{}, concurrency),
	}, nil
}

// send runs a request that sends data within the UploadConcurrency limit.
func (s *Store) send(ctx context.Context, do func() error) error {
	select {
	case s.sending <- struct{}{}:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	defer func() { <-s.sending }()
	if s.sendHook != nil {
		s.sendHook(len(s.sending))
	}
	return do()
}

// Ping checks that the bucket is reachable with the configured credentials.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: aws.String(s.prefix), MaxKeys: aws.Int32(1)})
	if err != nil {
		return s.wrap("list", s.prefix, err)
	}
	return nil
}

// Get returns the content and attributes of an object.
func (s *Store) Get(ctx context.Context, key string) ([]byte, Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: s.full(key)})
	if err != nil {
		return nil, Object{}, s.wrap("get", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, Object{}, fmt.Errorf("get %s: %w", key, err)
	}
	obj := object(key, out.ContentLength, out.ETag, out.Metadata, out.ContentType, out.CacheControl)
	if obj.Size != int64(len(body)) {
		return nil, Object{}, fmt.Errorf("get %s: read %d bytes, expected %d", key, len(body), obj.Size)
	}
	return body, obj, nil
}

// Open streams an object. The caller closes the body and checks the length it read.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: s.full(key)})
	if err != nil {
		return nil, Object{}, s.wrap("get", key, err)
	}
	return out.Body, object(key, out.ContentLength, out.ETag, out.Metadata, out.ContentType, out.CacheControl), nil
}

// Head returns the attributes of an object, with its CRC64NVME when the bucket keeps one.
func (s *Store) Head(ctx context.Context, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: s.full(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return Object{}, s.wrap("head", key, err)
	}
	obj := object(key, out.ContentLength, out.ETag, out.Metadata, out.ContentType, out.CacheControl)
	if c := aws.ToString(out.ChecksumCRC64NVME); c != "" {
		if obj.CRC64NVME, err = hashes.Hex(c); err != nil {
			return Object{}, fmt.Errorf("head %s: %w", key, err)
		}
	}
	return obj, nil
}

func object(key string, size *int64, etag *string, meta map[string]string, contentType, cacheControl *string) Object {
	return Object{
		Key:          key,
		Size:         aws.ToInt64(size),
		ETag:         aws.ToString(etag),
		SHA256:       meta[MetaSHA256],
		SHA512:       meta[MetaSHA512],
		Release:      meta[MetaRelease],
		ContentType:  aws.ToString(contentType),
		CacheControl: aws.ToString(cacheControl),
	}
}

// PutIfAbsent creates an object, failing with ErrPrecondition if the key exists.
func (s *Store) PutIfAbsent(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error) {
	return s.put(ctx, key, body, opts, func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
}

// PutIfMatch replaces an object only if its ETag is still etag, failing with ErrPrecondition otherwise.
// A missing key is ErrPrecondition too (R2 and Miniflare), not ErrNotFound.
func (s *Store) PutIfMatch(ctx context.Context, key, etag string, body []byte, opts PutOptions) (Object, error) {
	if etag == "" {
		return Object{}, errors.New("put if match: empty etag")
	}
	return s.put(ctx, key, body, opts, func(in *s3.PutObjectInput) { in.IfMatch = aws.String(etag) })
}

// Put writes an object unconditionally. Use only for private state that the tool alone owns.
func (s *Store) Put(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error) {
	return s.put(ctx, key, body, opts, nil)
}

func (s *Store) put(ctx context.Context, key string, body []byte, opts PutOptions, cond func(*s3.PutObjectInput)) (Object, error) {
	return s.putObject(ctx, key, bytes.NewReader(body), hashes.Bytes(body), opts, cond)
}

// putObject sends one PutObject with Content-MD5 and CRC64NVME from sums.
// R2 verifies both, Miniflare only the MD5.
func (s *Store) putObject(ctx context.Context, key string, body io.ReadSeeker, sums hashes.Sums, opts PutOptions, cond func(*s3.PutObjectInput)) (Object, error) {
	md5, err := hashes.Base64(sums.MD5)
	if err != nil {
		return Object{}, fmt.Errorf("put %s: %w", key, err)
	}
	crc, err := hashes.Base64(sums.CRC64NVME)
	if err != nil {
		return Object{}, fmt.Errorf("put %s: %w", key, err)
	}
	in := &s3.PutObjectInput{
		Bucket:            &s.bucket,
		Key:               s.full(key),
		Body:              body,
		ContentLength:     aws.Int64(sums.Size),
		ContentMD5:        aws.String(md5),
		ChecksumCRC64NVME: aws.String(crc),
		Metadata:          opts.metadata(sums.SHA256),
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if opts.CacheControl != "" {
		in.CacheControl = aws.String(opts.CacheControl)
	}
	if cond != nil {
		cond(in)
	}
	var out *s3.PutObjectOutput
	err = s.send(ctx, func() (e error) { out, e = s.client.PutObject(ctx, in); return })
	if err != nil {
		return Object{}, s.wrap("put", key, err)
	}
	return opts.object(key, sums.Size, aws.ToString(out.ETag), sums.SHA256), nil
}

// CopyIfSourceMatch copies src to dst server side, only if src still has srcETag.
//
// R2 ignores conditions on the destination, so dst is always overwritten.
// The copy keeps content, ETag, metadata, Content-Type and Cache-Control of src.
func (s *Store) CopyIfSourceMatch(ctx context.Context, src, srcETag, dst string) (Object, error) {
	if srcETag == "" {
		return Object{}, errors.New("copy: empty source etag")
	}
	return s.copy(ctx, src, dst, &s3.CopyObjectInput{
		CopySourceIfMatch: aws.String(srcETag),
		MetadataDirective: types.MetadataDirectiveCopy,
	})
}

// CopyIfAbsent copies src to a new key dst server side, only if src still has
// srcETag, with the metadata of opts and sums instead of those of src.
//
// R2 ignores conditions on the destination, so a HEAD decides that dst is
// absent; ErrPrecondition when it exists. The global lock excludes other
// writers in between. A copy to another key turns a multipart object into a
// single part one, so the ETag of dst differs from srcETag.
func (s *Store) CopyIfAbsent(ctx context.Context, src, srcETag, dst string, sums hashes.Sums, opts PutOptions) (Object, error) {
	if srcETag == "" {
		return Object{}, errors.New("copy: empty source etag")
	}
	switch _, err := s.Head(ctx, dst); {
	case err == nil:
		return Object{}, fmt.Errorf("copy %s to %s: exists: %w", src, dst, ErrPrecondition)
	case !errors.Is(err, ErrNotFound):
		return Object{}, err
	}
	in := &s3.CopyObjectInput{
		CopySourceIfMatch: aws.String(srcETag),
		MetadataDirective: types.MetadataDirectiveReplace,
		Metadata:          opts.metadata(sums.SHA256),
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if opts.CacheControl != "" {
		in.CacheControl = aws.String(opts.CacheControl)
	}
	return s.copy(ctx, src, dst, in)
}

func (s *Store) copy(ctx context.Context, src, dst string, in *s3.CopyObjectInput) (Object, error) {
	in.Bucket = &s.bucket
	in.Key = s.full(dst)
	in.CopySource = aws.String(copySource(s.bucket, *s.full(src)))
	err := s.send(ctx, func() error { _, e := s.client.CopyObject(ctx, in); return e })
	if err != nil {
		return Object{}, s.wrap("copy "+src+" to", dst, err)
	}
	return s.Head(ctx, dst)
}

// List calls fn for every object under prefix, in key order.
func (s *Store) List(ctx context.Context, prefix string, fn func(Object) error) error {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
		Prefix: aws.String(s.prefix + prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return s.wrap("list", prefix, err)
		}
		for _, o := range page.Contents {
			obj := Object{
				Key:  strings.TrimPrefix(aws.ToString(o.Key), s.prefix),
				Size: aws.ToInt64(o.Size),
				ETag: aws.ToString(o.ETag),
			}
			if err := fn(obj); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete removes an object under one of the configured deletable prefixes and refuses anything else.
func (s *Store) Delete(ctx context.Context, key string) error {
	if !s.canDelete(key) {
		return fmt.Errorf("delete %s: refused, the key is outside the deletable prefixes %q", key, s.deletable)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: s.full(key)}); err != nil {
		return s.wrap("delete", key, err)
	}
	return nil
}

// DeleteOwned deletes an object only if it carries the stamp release and the hex SHA256 sha256.
//
// It is the only delete that reaches public package paths. A matching stamp with
// other content fails with ErrHashMismatch and deletes nothing. R2 ignores
// conditions on DeleteObject, so the global lock must exclude other writers.
func (s *Store) DeleteOwned(ctx context.Context, key, release, sha256 string) (DeleteResult, error) {
	if release == "" || sha256 == "" {
		return 0, fmt.Errorf("delete owned %s: release and sha256 are required", key)
	}
	if key == "" || strings.Contains(key, "..") {
		return 0, fmt.Errorf("delete owned %q: refused", key)
	}
	obj, err := s.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return Absent, nil
	}
	if err != nil {
		return 0, err
	}
	if obj.Release != release {
		return NotOwned, nil
	}
	if obj.SHA256 != sha256 {
		return 0, fmt.Errorf("delete owned %s: stamp %s, sha256 %s, want %s: %w", key, release, obj.SHA256, sha256, ErrHashMismatch)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: s.full(key)}); err != nil {
		return 0, s.wrap("delete", key, err)
	}
	return Deleted, nil
}

// DeleteStamped deletes an object only if it carries the stamp of any release,
// so it never removes an object this tool did not write. Only the by-hash
// pruning calls it; like DeleteOwned it is not limited to the deletable prefixes.
func (s *Store) DeleteStamped(ctx context.Context, key string) (DeleteResult, error) {
	if key == "" || strings.Contains(key, "..") {
		return 0, fmt.Errorf("delete stamped %q: refused", key)
	}
	obj, err := s.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return Absent, nil
	}
	if err != nil {
		return 0, err
	}
	if obj.Release == "" {
		return NotStamped, nil
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: s.full(key)}); err != nil {
		return 0, s.wrap("delete", key, err)
	}
	return Deleted, nil
}

func (s *Store) canDelete(key string) bool {
	if strings.Contains(key, "..") {
		return false
	}
	for _, d := range s.deletable {
		if strings.HasPrefix(key, d) && len(key) > len(d) {
			return true
		}
	}
	return false
}

func (s *Store) full(key string) *string {
	return aws.String(s.prefix + key)
}

func (s *Store) wrap(op, key string, err error) error {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%s %s: %w", op, key, ErrPrecondition)
		case http.StatusNotFound:
			return fmt.Errorf("%s %s: %w", op, key, ErrNotFound)
		}
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "PreconditionFailed":
			return fmt.Errorf("%s %s: %w", op, key, ErrPrecondition)
		case "NoSuchKey", "NotFound":
			return fmt.Errorf("%s %s: %w", op, key, ErrNotFound)
		}
	}
	return fmt.Errorf("%s %s: %w", op, key, err)
}

// unsignedAcceptEncoding keeps "Accept-Encoding: identity" out of the signature.
// workerd rewrites the header, so a signature covering it fails in the local R2 simulator.
// The header itself stays: without it Go asks for gzip and may transparently decompress.
func unsignedAcceptEncoding(stack *middleware.Stack) error {
	const name = "Accept-Encoding"
	if err := stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("ChDeployDropAcceptEncoding",
		func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
			if r, ok := in.Request.(*smithyhttp.Request); ok {
				r.Header.Del(name)
			}
			return next.HandleFinalize(ctx, in)
		}), "Signing", middleware.Before); err != nil {
		return err
	}
	return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("ChDeployRestoreAcceptEncoding",
		func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
			if r, ok := in.Request.(*smithyhttp.Request); ok {
				r.Header.Set(name, "identity")
			}
			return next.HandleFinalize(ctx, in)
		}), "Signing", middleware.After)
}

// copySource is the URL-encoded "bucket/key" that CopyObject requires.
func copySource(bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return url.PathEscape(bucket) + "/" + strings.Join(segments, "/")
}

func normalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}
