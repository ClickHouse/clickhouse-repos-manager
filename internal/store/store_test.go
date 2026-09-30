package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestCanDelete(t *testing.T) {
	s := &Store{deletable: []string{"configs/ch-deploy/runs/"}}
	for key, want := range map[string]bool{
		"configs/ch-deploy/runs/20260929T122902Z-v26.3.36.6-lts/manifest.json": true,
		"configs/ch-deploy/runs/":          false,
		"configs/ch-deploy/lock":           false,
		"deb/pool/main/c/clickhouse/x.deb": false,
		"configs/ch-deploy/runs/../lock":   false,
		"configs/ch-deploy/runsX/a":        false,
	} {
		if got := s.canDelete(key); got != want {
			t.Errorf("canDelete(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestNewRejectsEmptyDeletable(t *testing.T) {
	_, err := New(Config{Endpoint: "https://example.com", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", Deletable: []string{"/"}})
	if err == nil {
		t.Fatal("an empty deletable prefix was accepted")
	}
}

// testStore connects to a real bucket, isolated under a random prefix:
// CH_DEPLOY_TEST_ENDPOINT, CH_DEPLOY_TEST_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
func testStore(t *testing.T) *Store {
	t.Helper()
	endpoint, bucket := os.Getenv("CH_DEPLOY_TEST_ENDPOINT"), os.Getenv("CH_DEPLOY_TEST_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("CH_DEPLOY_TEST_ENDPOINT and CH_DEPLOY_TEST_BUCKET are not set")
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	prefix := "ch-deploy-test/" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(id)
	s, err := New(Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Prefix:          prefix,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Deletable:       []string{"tmp/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleaner := *s
		cleaner.deletable = []string{""} // the whole random test prefix
		ctx := context.Background()
		var keys []string
		if err := s.List(ctx, "", func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil {
			t.Logf("cleanup list: %v", err)
		}
		for _, k := range keys {
			if err := cleaner.Delete(ctx, k); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})
	return s
}

func TestPutIfAbsent(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	obj, err := s.PutIfAbsent(ctx, "a", []byte("one"), PutOptions{ContentType: "text/plain", CacheControl: "no-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "a", []byte("two"), PutOptions{}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second PutIfAbsent: %v, want ErrPrecondition", err)
	}
	body, got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one" || got.ETag != obj.ETag {
		t.Fatalf("got %q %s, want %q %s", body, got.ETag, "one", obj.ETag)
	}
	if got.SHA256 != obj.SHA256 || got.ContentType != "text/plain" || got.CacheControl != "no-cache" {
		t.Fatalf("attributes not stored: %+v", got)
	}
}

func TestPutIfAbsentRace(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutIfAbsent(ctx, "race", []byte{byte(i)}, PutOptions{})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, ErrPrecondition) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d writers won, want 1", won)
	}
}

func TestPutIfMatch(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	first, err := s.PutIfAbsent(ctx, "lock", []byte("v1"), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutIfMatch(ctx, "lock", first.ETag, []byte("v2"), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfMatch(ctx, "lock", first.ETag, []byte("v3"), PutOptions{}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale PutIfMatch: %v, want ErrPrecondition", err)
	}
	if _, err := s.PutIfMatch(ctx, "missing", second.ETag, []byte("x"), PutOptions{}); err == nil {
		t.Fatal("PutIfMatch created a missing object")
	}
	body, _, err := s.Get(ctx, "lock")
	if err != nil || string(body) != "v2" {
		t.Fatalf("got %q, %v, want v2", body, err)
	}
}

func TestCopyIfSourceMatch(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	src, err := s.PutIfAbsent(ctx, "runs/1/Packages.gz", []byte("staged"), PutOptions{ContentType: "application/gzip", CacheControl: "no-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "live/Packages.gz", []byte("old"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	dst, err := s.CopyIfSourceMatch(ctx, "runs/1/Packages.gz", src.ETag, "live/Packages.gz")
	if err != nil {
		t.Fatal(err)
	}
	if dst.ETag != src.ETag || dst.SHA256 != src.SHA256 || dst.ContentType != "application/gzip" || dst.CacheControl != "no-cache" {
		t.Fatalf("copy changed attributes: src %+v, dst %+v", src, dst)
	}
	if _, err := s.CopyIfSourceMatch(ctx, "runs/1/Packages.gz", `"00000000000000000000000000000000"`, "live/Packages.gz"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale source copy: %v, want ErrPrecondition", err)
	}
}

func TestNotFound(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	if _, _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: %v, want ErrNotFound", err)
	}
	if _, err := s.Head(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head: %v, want ErrNotFound", err)
	}
}

func TestListAndDelete(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	for _, k := range []string{"tmp/b", "tmp/a", "keep"} {
		if _, err := s.PutIfAbsent(ctx, k, []byte(k), PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	if err := s.List(ctx, "", func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if !sort.StringsAreSorted(keys) || len(keys) != 3 || keys[0] != "keep" {
		t.Fatalf("List = %q", keys)
	}
	if err := s.Delete(ctx, "keep"); err == nil {
		t.Fatal("Delete outside the deletable prefixes succeeded")
	}
	if err := s.Delete(ctx, "tmp/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Head(ctx, "tmp/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted object still there: %v", err)
	}
}

func TestLargeBody(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	body := bytes.Repeat([]byte("Package: clickhouse-client\n"), 200_000) // ~5 MB, like a big Packages
	if _, err := s.PutIfAbsent(ctx, "big", body, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(ctx, "big")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestCopySource(t *testing.T) {
	for key, want := range map[string]string{
		"deb/pool/main/c/clickhouse/clickhouse-client_26.3.36.6_amd64.deb": "b/deb/pool/main/c/clickhouse/clickhouse-client_26.3.36.6_amd64.deb",
		"deb/pool/main/f/foo/foo_1.0+dfsg~1_amd64.deb":                     "b/deb/pool/main/f/foo/foo_1.0+dfsg~1_amd64.deb",
		"a b/c%d?e#f": "b/a%20b/c%25d%3Fe%23f",
	} {
		if got := copySource("b", key); got != want {
			t.Errorf("copySource(%q) = %q, want %q", key, got, want)
		}
	}
}
