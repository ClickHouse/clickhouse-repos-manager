// Package store is the only way the tool talks to the bucket.
//
// It knows the R2 quirks that shape the release protocol: conditional PUT works,
// conditions on the CopyObject destination and on DeleteObject are ignored,
// and only the CopyObject source condition is honored.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
)

// MetaSHA256 is the custom metadata key holding the hex SHA256 of the object content.
const MetaSHA256 = "sha256"

// Config holds the connection parameters.
type Config struct {
	Endpoint        string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	// Deletable lists key prefixes (relative to Prefix) where Delete is allowed.
	Deletable []string
}

// Object describes a stored object.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	SHA256       string // hex, from MetaSHA256; empty for objects not written by this tool
	ContentType  string
	CacheControl string
}

// PutOptions are the HTTP headers stored with an object.
type PutOptions struct {
	ContentType  string
	CacheControl string
}

// Store is a bucket, optionally narrowed to a key prefix.
type Store struct {
	client    *s3.Client
	bucket    string
	prefix    string
	deletable []string
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
	return &Store{client: client, bucket: cfg.Bucket, prefix: prefix, deletable: deletable}, nil
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
	obj := Object{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		SHA256:       out.Metadata[MetaSHA256],
		ContentType:  aws.ToString(out.ContentType),
		CacheControl: aws.ToString(out.CacheControl),
	}
	if obj.Size != int64(len(body)) {
		return nil, Object{}, fmt.Errorf("get %s: read %d bytes, expected %d", key, len(body), obj.Size)
	}
	return body, obj, nil
}

// Head returns the attributes of an object.
func (s *Store) Head(ctx context.Context, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: s.full(key)})
	if err != nil {
		return Object{}, s.wrap("head", key, err)
	}
	return Object{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		SHA256:       out.Metadata[MetaSHA256],
		ContentType:  aws.ToString(out.ContentType),
		CacheControl: aws.ToString(out.CacheControl),
	}, nil
}

// PutIfAbsent creates an object, failing with ErrPrecondition if the key exists.
func (s *Store) PutIfAbsent(ctx context.Context, key string, body []byte, opts PutOptions) (Object, error) {
	return s.put(ctx, key, body, opts, func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") })
}

// PutIfMatch replaces an object only if its ETag is still etag, failing with ErrPrecondition otherwise.
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
	sum := sha256.Sum256(body)
	in := &s3.PutObjectInput{
		Bucket:         &s.bucket,
		Key:            s.full(key),
		Body:           bytes.NewReader(body),
		ContentLength:  aws.Int64(int64(len(body))),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
		Metadata:       map[string]string{MetaSHA256: hex.EncodeToString(sum[:])},
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
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return Object{}, s.wrap("put", key, err)
	}
	return Object{
		Key:          key,
		Size:         int64(len(body)),
		ETag:         aws.ToString(out.ETag),
		SHA256:       hex.EncodeToString(sum[:]),
		ContentType:  opts.ContentType,
		CacheControl: opts.CacheControl,
	}, nil
}

// CopyIfSourceMatch copies src to dst server side, only if src still has srcETag.
//
// R2 ignores conditions on the destination, so dst is always overwritten.
// The copy keeps content, ETag, metadata, Content-Type and Cache-Control of src.
func (s *Store) CopyIfSourceMatch(ctx context.Context, src, srcETag, dst string) (Object, error) {
	if srcETag == "" {
		return Object{}, errors.New("copy: empty source etag")
	}
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            &s.bucket,
		Key:               s.full(dst),
		CopySource:        aws.String(copySource(s.bucket, *s.full(src))),
		CopySourceIfMatch: aws.String(srcETag),
		MetadataDirective: types.MetadataDirectiveCopy,
	})
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
