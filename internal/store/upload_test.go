package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

const partSize = store.MinPartSize

// multipartStore uploads in 5 MiB parts, so small files exercise the multipart path.
func multipartStore(t *testing.T) *store.Store {
	return storetest.NewConfig(t, func(c *store.Config) {
		c.PartSize = partSize
		c.UploadConcurrency = 2
	})
}

// writeFile writes size pseudo random bytes and returns the path and hashes.
func writeFile(t *testing.T, size int64, seed uint64) (string, hashes.Sums) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)})
	if _, err := io.CopyN(f, r, size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	sums, err := hashes.File(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, sums
}

// readBack streams the object and checks its content and metadata against sums.
func readBack(t *testing.T, s store.Bucket, key string, sums hashes.Sums, release string) store.Object {
	t.Helper()
	body, obj, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := hashes.Sum(body)
	if err != nil {
		t.Fatal(err)
	}
	if got != sums {
		t.Fatalf("%s: read back %+v, want %+v", key, got, sums)
	}
	if obj.Size != sums.Size || obj.SHA256 != sums.SHA256 || obj.Release != release {
		t.Fatalf("%s: attributes %+v, want size %d, sha256 %s, release %q", key, obj, sums.Size, sums.SHA256, release)
	}
	return obj
}

func noOpenUploads(t *testing.T, s *store.Store) {
	t.Helper()
	keys, err := s.OpenUploads(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) > 0 {
		t.Fatalf("open multipart uploads left: %q", keys)
	}
}

func TestPutFileIfAbsent(t *testing.T) {
	s, ctx := multipartStore(t), context.Background()
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"empty", 0},
		{"single", 1000},
		{"one-part", partSize},
		{"multipart", 2*partSize + 12345}, // short last part
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, sums := writeFile(t, tc.size, uint64(tc.size))
			key := "pool/" + tc.name + ".deb"
			opts := store.PutOptions{ContentType: "application/vnd.debian.binary-package", Release: "v26.3.36.6-lts", SHA512: sums.SHA512}
			put, err := s.PutFileIfAbsent(ctx, key, path, sums, opts)
			if err != nil {
				t.Fatal(err)
			}
			got := readBack(t, s, key, sums, opts.Release)
			if got.ETag != put.ETag || got.SHA512 != sums.SHA512 || got.ContentType != opts.ContentType {
				t.Fatalf("attributes %+v, put returned %+v", got, put)
			}

			// A conflict leaves the first content in place, on both paths.
			other, otherSums := writeFile(t, tc.size+1, 99)
			if _, err := s.PutFileIfAbsent(ctx, key, other, otherSums, opts); !errors.Is(err, store.ErrPrecondition) {
				t.Fatalf("second upload: %v, want ErrPrecondition", err)
			}
			readBack(t, s, key, sums, opts.Release)
		})
	}
}

func TestPutFileIfAbsentChecksSize(t *testing.T) {
	s, ctx := storetest.New(t), context.Background()
	path, sums := writeFile(t, 100, 1)
	sums.Size++
	if _, err := s.PutFileIfAbsent(ctx, "x", path, sums, store.PutOptions{}); err == nil {
		t.Fatal("upload with a wrong size succeeded")
	}
	if _, err := s.Head(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("object written: %v", err)
	}
}

func TestPutFileIfAbsentBadDigest(t *testing.T) {
	other := hashes.Bytes([]byte("other"))
	for _, tc := range []struct {
		name   string
		size   int64
		r2Only bool
		sums   func(*hashes.Sums)
		part   func(*hashes.Part)
	}{
		{name: "single-md5", size: 100, sums: func(s *hashes.Sums) { s.MD5 = other.MD5 }},
		{name: "single-crc", size: 100, r2Only: true, sums: func(s *hashes.Sums) { s.CRC64NVME = other.CRC64NVME }},
		{name: "part-md5", size: 2 * partSize, part: func(p *hashes.Part) { p.MD5 = other.MD5 }},
		{name: "part-crc", size: 2 * partSize, r2Only: true, part: func(p *hashes.Part) { p.CRC64NVME = other.CRC64NVME }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.r2Only {
				storetest.SkipOnMiniflare(t, "does not verify x-amz-checksum-crc64nvme")
			}
			s, ctx := multipartStore(t), context.Background()
			path, sums := writeFile(t, tc.size, 1)
			if tc.sums != nil {
				tc.sums(&sums)
			}
			if tc.part != nil {
				s.SetPartSums(func(n int32, p *hashes.Part) {
					if n == 2 {
						tc.part(p)
					}
				})
			}
			_, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{})
			if err == nil || !strings.Contains(err.Error(), "BadDigest") {
				t.Fatalf("upload with a wrong checksum: %v, want BadDigest", err)
			}
			if _, err := s.Head(ctx, "f"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("object written: %v", err)
			}
		})
	}
}

// R2 keeps a full object CRC64NVME for every object, single PUT or multipart.
func TestHeadCRC64NVME(t *testing.T) {
	s, ctx := multipartStore(t), context.Background()
	for i, size := range []int64{100, 2*partSize + 1} {
		path, sums := writeFile(t, size, uint64(i))
		key := fmt.Sprint("f", i)
		if _, err := s.PutFileIfAbsent(ctx, key, path, sums, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		obj, err := s.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		want := sums.CRC64NVME
		if storetest.Backend() == "miniflare" {
			want = ""
		}
		if obj.CRC64NVME != want {
			t.Fatalf("%d bytes: CRC64NVME %q, want %q", size, obj.CRC64NVME, want)
		}
	}
}

// countParts makes the store count its uploaded parts and fail after part failAfter (0 never).
func countParts(s *store.Store, failAfter int32, fail error) *atomic.Int32 {
	var n atomic.Int32
	s.SetAfterPart(func(part int32) error {
		n.Add(1)
		if part == failAfter {
			return fail
		}
		return nil
	})
	return &n
}

// serialStore uploads one part at a time, so the parts sent before a failure are known.
func serialStore(t *testing.T) *store.Store {
	return storetest.NewConfig(t, func(c *store.Config) {
		c.PartSize = partSize
		c.UploadConcurrency = 1
	})
}

// A failed upload stays open on R2, and the rerun sends only the missing parts.
// Miniflare aborts it, and the rerun sends every part.
func TestMultipartResumeAfterFailure(t *testing.T) {
	s, ctx := serialStore(t), context.Background()
	path, sums := writeFile(t, 3*partSize, 2)
	boom := errors.New("boom")
	countParts(s, 2, boom)
	if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{Release: "r"}); !errors.Is(err, boom) {
		t.Fatalf("upload: %v, want the part failure", err)
	}
	if _, err := s.Head(ctx, "f"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("object written: %v", err)
	}
	if storetest.Backend() != "miniflare" {
		if keys, err := s.OpenUploads(ctx, ""); err != nil || len(keys) != 1 {
			t.Fatalf("open uploads %q, %v, want one", keys, err)
		}
	}
	sent := countParts(s, 0, nil)
	if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{Release: "r"}); err != nil {
		t.Fatal(err)
	}
	readBack(t, s, "f", sums, "r")
	want := int32(1)
	if storetest.Backend() == "miniflare" {
		want = 3
	}
	if sent.Load() != want {
		t.Fatalf("rerun sent %d parts, want %d", sent.Load(), want)
	}
	if storetest.Backend() != "miniflare" {
		noOpenUploads(t, s)
	}
}

func TestMultipartResumeAfterCancel(t *testing.T) {
	storetest.SkipOnMiniflare(t, "ListMultipartUploads is not implemented")
	s := serialStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path, sums := writeFile(t, 3*partSize, 3)
	s.SetAfterPart(func(int32) error { cancel(); return nil })
	if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload: %v, want context.Canceled", err)
	}
	sent := countParts(s, 0, nil)
	if _, err := s.PutFileIfAbsent(context.Background(), "f", path, sums, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	readBack(t, s, "f", sums, "")
	if sent.Load() != 2 {
		t.Fatalf("rerun sent %d parts, want 2", sent.Load())
	}
	noOpenUploads(t, s)
}

// Open uploads left by earlier runs: the one with the most matching parts goes on,
// uploads with another part size or other content give nothing, and the rest is aborted.
func TestMultipartResumeChoice(t *testing.T) {
	storetest.SkipOnMiniflare(t, "ListMultipartUploads is not implemented")
	for _, tc := range []struct {
		name string
		open func(t *testing.T, s *store.Store, path, sha256 string)
		sent int32
	}{
		{"most-parts", func(t *testing.T, s *store.Store, path, sha256 string) {
			openUpload(t, s, "f", path, sha256, partSize, 1)
			openUpload(t, s, "f", path, sha256, partSize, 1, 2)
			openUpload(t, s, "f", path, sha256, partSize, 3)
		}, 1},
		{"other-part-size", func(t *testing.T, s *store.Store, path, sha256 string) {
			openUpload(t, s, "f", path, sha256, partSize+1<<20, 1)
		}, 3},
		{"other-content", func(t *testing.T, s *store.Store, _, sha256 string) {
			other, _ := writeFile(t, 3*partSize, 99)
			openUpload(t, s, "f", other, sha256, partSize, 1, 2, 3)
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := serialStore(t), context.Background()
			path, sums := writeFile(t, 3*partSize, 4)
			tc.open(t, s, path, sums.SHA256)
			sent := countParts(s, 0, nil)
			if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{Release: "r"}); err != nil {
				t.Fatal(err)
			}
			readBack(t, s, "f", sums, "r")
			if sent.Load() != tc.sent {
				t.Fatalf("sent %d parts, want %d", sent.Load(), tc.sent)
			}
			noOpenUploads(t, s)
		})
	}
}

// The LTS copy of a package: a multipart source becomes a single part object
// with the metadata given, and an existing destination stays.
func TestCopyIfAbsent(t *testing.T) {
	s, ctx := multipartStore(t), context.Background()
	path, sums := writeFile(t, 2*partSize+1, 6)
	src, err := s.PutFileIfAbsent(ctx, "tgz/lts/f.tgz", path, sums, store.PutOptions{Release: "other", ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	opts := store.PutOptions{Release: "r", ContentType: "application/gzip", SHA512: sums.SHA512}
	dst, err := s.CopyIfAbsent(ctx, "tgz/lts/f.tgz", src.ETag, "tgz/stable/f.tgz", sums, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := readBack(t, s, "tgz/stable/f.tgz", sums, "r")
	if dst.Size != sums.Size || dst.Release != "r" || dst.SHA256 != sums.SHA256 || dst.SHA512 != sums.SHA512 || got.ContentType != opts.ContentType {
		t.Fatalf("copy attributes %+v", dst)
	}
	if storetest.Backend() != "miniflare" {
		if dst.ETag != `"`+sums.MD5+`"` || dst.CRC64NVME != sums.CRC64NVME {
			t.Fatalf("copy etag %s crc %s, want the MD5 %s and crc %s (source etag %s)", dst.ETag, dst.CRC64NVME, sums.MD5, sums.CRC64NVME, src.ETag)
		}
	}

	if _, err := s.PutIfAbsent(ctx, "tgz/stable/g.tgz", []byte("g"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyIfAbsent(ctx, "tgz/lts/f.tgz", src.ETag, "tgz/stable/g.tgz", sums, opts); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("copy over an existing key: %v, want ErrPrecondition", err)
	}
	if body, _, err := s.Get(ctx, "tgz/stable/g.tgz"); err != nil || string(body) != "g" {
		t.Fatalf("existing destination changed: %q, %v", body, err)
	}
	if _, err := s.CopyIfAbsent(ctx, "tgz/lts/f.tgz", `"00000000000000000000000000000000"`, "tgz/stable/h.tgz", sums, opts); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale source copy: %v, want ErrPrecondition", err)
	}
}

// openUpload leaves an upload of key open with the metadata PutOptions{Release: "r"} and sha256.
func openUpload(t *testing.T, s *store.Store, key, path, sha256 string, partSize int64, parts ...int32) {
	t.Helper()
	if err := s.OpenUpload(context.Background(), key, path, partSize, sha256, store.PutOptions{Release: "r"}, parts...); err != nil {
		t.Fatal(err)
	}
}

func TestAbortUploads(t *testing.T) {
	s, ctx := serialStore(t), context.Background()
	if n, err := s.AbortUploads(ctx, "f"); err != nil || n != 0 {
		t.Fatalf("AbortUploads without uploads = %d, %v", n, err)
	}
	storetest.SkipOnMiniflare(t, "ListMultipartUploads is not implemented")
	path, _ := writeFile(t, 2*partSize, 5)
	for _, key := range []string{"f", "f", "f2"} {
		openUpload(t, s, key, path, "", partSize, 1)
	}
	if n, err := s.AbortUploads(ctx, "f"); err != nil || n != 2 {
		t.Fatalf("AbortUploads = %d, %v, want 2", n, err)
	}
	if keys, err := s.OpenUploads(ctx, ""); err != nil || len(keys) != 1 || keys[0] != "f2" {
		t.Fatalf("open uploads %q, %v, want only f2", keys, err)
	}
}

// A writer that creates the key between the HEAD check and the completion loses nothing.
func TestMultipartCompleteOverExisting(t *testing.T) {
	storetest.SkipOnMiniflare(t, "multipart completion overwrites an existing key")
	s, ctx := multipartStore(t), context.Background()
	path, sums := writeFile(t, 2*partSize, 4)
	s.SetAfterPart(func(part int32) error {
		if part != 1 {
			return nil
		}
		_, err := s.PutIfAbsent(ctx, "f", []byte("winner"), store.PutOptions{})
		return err
	})
	if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("upload: %v, want ErrPrecondition", err)
	}
	body, _, err := s.Get(ctx, "f")
	if err != nil || string(body) != "winner" {
		t.Fatalf("existing object changed: %q, %v", body, err)
	}
	noOpenUploads(t, s)
}

func TestPutFileSpecialKeys(t *testing.T) {
	s, ctx := multipartStore(t), context.Background()
	for i, key := range []string{"pool/foo_1.0+dfsg~1_amd64.deb", "a b/c%d?e#f.tgz"} {
		path, sums := writeFile(t, int64(i)*2*partSize+10, uint64(i))
		if _, err := s.PutFileIfAbsent(ctx, key, path, sums, store.PutOptions{Release: "r"}); err != nil {
			t.Fatalf("%q: %v", key, err)
		}
		readBack(t, s, key, sums, "r")
		if res, err := s.DeleteOwned(ctx, key, "r", sums.SHA256); err != nil || res != store.Deleted {
			t.Fatalf("%q: DeleteOwned = %v, %v", key, res, err)
		}
	}
}

func TestPartLimits(t *testing.T) {
	cfg := store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}
	for _, bad := range []store.Config{
		func() store.Config { c := cfg; c.PartSize = store.MinPartSize - 1; return c }(),
		func() store.Config { c := cfg; c.UploadConcurrency = -1; return c }(),
	} {
		if _, err := store.New(bad); err == nil {
			t.Errorf("New(%+v) succeeded", bad)
		}
	}

	cfg.PartSize = store.MinPartSize
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sparse")
	size := int64(store.MaxParts)*store.MinPartSize + 1
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	// Refused before any request: the endpoint does not exist.
	_, err = s.PutFileIfAbsent(context.Background(), "x", path, hashes.Sums{Size: size}, store.PutOptions{})
	if err == nil || errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("upload of %d parts: %v, want a part count error", store.MaxParts+1, err)
	}
}
