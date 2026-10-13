package store_test

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
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
	storetest.SkipOnMiniflare(t, "does not verify x-amz-checksum-sha256")
	s, ctx := storetest.New(t), context.Background()
	path, sums := writeFile(t, 100, 1)
	sums.SHA256 = hashes.Bytes([]byte("other")).SHA256
	_, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "BadDigest") {
		t.Fatalf("upload with a wrong SHA256: %v, want BadDigest", err)
	}
	if _, err := s.Head(ctx, "f"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("object written: %v", err)
	}
}

func TestMultipartFailureAborts(t *testing.T) {
	s, ctx := multipartStore(t), context.Background()
	path, sums := writeFile(t, 3*partSize, 2)
	boom := errors.New("boom")
	s.SetAfterPart(func(part int32) error {
		if part == 2 {
			return boom
		}
		return nil
	})
	if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{}); !errors.Is(err, boom) {
		t.Fatalf("upload: %v, want the part failure", err)
	}
	if _, err := s.Head(ctx, "f"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("object written: %v", err)
	}
	storetest.SkipOnMiniflare(t, "ListMultipartUploads is not implemented")
	noOpenUploads(t, s)
}

func TestMultipartCancelAborts(t *testing.T) {
	s := multipartStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path, sums := writeFile(t, 3*partSize, 3)
	s.SetAfterPart(func(int32) error { cancel(); return nil })
	if _, err := s.PutFileIfAbsent(ctx, "f", path, sums, store.PutOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload: %v, want context.Canceled", err)
	}
	if _, err := s.Head(context.Background(), "f"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("object written: %v", err)
	}
	storetest.SkipOnMiniflare(t, "ListMultipartUploads is not implemented")
	noOpenUploads(t, s)
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
