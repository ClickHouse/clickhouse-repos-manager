package store_test

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

func TestPutIfAbsent(t *testing.T) {
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	obj, err := s.PutIfAbsent(ctx, "a", []byte("one"), store.PutOptions{ContentType: "text/plain", CacheControl: "no-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "a", []byte("two"), store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) {
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
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutIfAbsent(ctx, "race", []byte{byte(i)}, store.PutOptions{})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, store.ErrPrecondition) {
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
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	first, err := s.PutIfAbsent(ctx, "lock", []byte("v1"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutIfMatch(ctx, "lock", first.ETag, []byte("v2"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfMatch(ctx, "lock", first.ETag, []byte("v3"), store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale PutIfMatch: %v, want ErrPrecondition", err)
	}
	if _, err := s.PutIfMatch(ctx, "missing", second.ETag, []byte("x"), store.PutOptions{}); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("PutIfMatch of a missing key: %v, want ErrPrecondition", err)
	}
	body, _, err := s.Get(ctx, "lock")
	if err != nil || string(body) != "v2" {
		t.Fatalf("got %q, %v, want v2", body, err)
	}
}

func TestCopyIfSourceMatch(t *testing.T) {
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	src, err := s.PutIfAbsent(ctx, "runs/1/Packages.gz", []byte("staged"), store.PutOptions{ContentType: "application/gzip", CacheControl: "no-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutIfAbsent(ctx, "live/Packages.gz", []byte("old"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	dst, err := s.CopyIfSourceMatch(ctx, "runs/1/Packages.gz", src.ETag, "live/Packages.gz")
	if err != nil {
		t.Fatal(err)
	}
	if dst.ETag != src.ETag || dst.SHA256 != src.SHA256 || dst.ContentType != "application/gzip" || dst.CacheControl != "no-cache" {
		t.Fatalf("copy changed attributes: src %+v, dst %+v", src, dst)
	}
	if _, err := s.CopyIfSourceMatch(ctx, "runs/1/Packages.gz", `"00000000000000000000000000000000"`, "live/Packages.gz"); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale source copy: %v, want ErrPrecondition", err)
	}
}

func TestNotFound(t *testing.T) {
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	if _, _, err := s.Get(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get: %v, want ErrNotFound", err)
	}
	if _, err := s.Head(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Head: %v, want ErrNotFound", err)
	}
}

func TestListAndDelete(t *testing.T) {
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	for _, k := range []string{"tmp/b", "tmp/a", "keep"} {
		if _, err := s.PutIfAbsent(ctx, k, []byte(k), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	if err := s.List(ctx, "", func(o store.Object) error { keys = append(keys, o.Key); return nil }); err != nil {
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
	if _, err := s.Head(ctx, "tmp/a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted object still there: %v", err)
	}
}

func TestLargeBody(t *testing.T) {
	s, ctx := storetest.New(t, "tmp/"), context.Background()
	body := bytes.Repeat([]byte("Package: clickhouse-client\n"), 200_000) // ~5 MB, like a big Packages
	if _, err := s.PutIfAbsent(ctx, "big", body, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(ctx, "big")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestCopySpecialKey(t *testing.T) {
	s, ctx := storetest.New(t), context.Background()
	for _, key := range []string{"pool/foo_1.0+dfsg~1_amd64.deb", "a b/c%d?e#f"} {
		src, err := s.PutIfAbsent(ctx, key, []byte(key), store.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		dst, err := s.CopyIfSourceMatch(ctx, key, src.ETag, "copy/"+key)
		if err != nil {
			t.Fatalf("copy %q: %v", key, err)
		}
		body, _, err := s.Get(ctx, "copy/"+key)
		if err != nil || string(body) != key || dst.ETag != src.ETag {
			t.Fatalf("copy %q: got %q, %v", key, body, err)
		}
	}
}

func TestStampRoundTrip(t *testing.T) {
	s, ctx := storetest.New(t), context.Background()
	opts := store.PutOptions{Release: "v26.3.36.6-lts", SHA512: "abc"}
	put, err := s.PutIfAbsent(ctx, "a", []byte("x"), opts)
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.Head(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []store.Object{put, head, got} {
		if o.Release != opts.Release || o.SHA512 != opts.SHA512 || o.SHA256 != put.SHA256 {
			t.Fatalf("stamp lost: %+v", o)
		}
	}
	if _, err := s.PutIfAbsent(ctx, "b", []byte("x"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if o, err := s.Head(ctx, "b"); err != nil || o.Release != "" || o.SHA512 != "" {
		t.Fatalf("unstamped object: %+v, %v", o, err)
	}
}

func TestDeleteOwned(t *testing.T) {
	s, ctx := storetest.New(t), context.Background()
	const tag = "v26.3.36.6-lts"
	mine, err := s.PutIfAbsent(ctx, "deb/pool/mine.deb", []byte("mine"), store.PutOptions{Release: tag})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.PutIfAbsent(ctx, "deb/pool/other.deb", []byte("other"), store.PutOptions{Release: "v26.2.1.1-stable"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.PutIfAbsent(ctx, "deb/pool/plain.deb", []byte("plain"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeleteOwned(ctx, mine.Key, tag, other.SHA256); !errors.Is(err, store.ErrHashMismatch) {
		t.Fatalf("wrong sha256: %v, want ErrHashMismatch", err)
	}
	for _, tc := range []struct {
		key, sha256 string
		want        store.DeleteResult
	}{
		{other.Key, other.SHA256, store.NotOwned},
		{plain.Key, plain.SHA256, store.NotOwned},
		{"deb/pool/missing.deb", mine.SHA256, store.Absent},
		{mine.Key, mine.SHA256, store.Deleted},
		{mine.Key, mine.SHA256, store.Absent},
	} {
		if got, err := s.DeleteOwned(ctx, tc.key, tag, tc.sha256); err != nil || got != tc.want {
			t.Errorf("DeleteOwned(%s) = %v, %v, want %v", tc.key, got, err, tc.want)
		}
	}
	for _, o := range []store.Object{other, plain} {
		if _, err := s.Head(ctx, o.Key); err != nil {
			t.Errorf("%s: %v, must stay", o.Key, err)
		}
	}
	for _, bad := range [][3]string{{"k", "", "x"}, {"k", tag, ""}, {"", tag, "x"}, {"a/../b", tag, "x"}} {
		if _, err := s.DeleteOwned(ctx, bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("DeleteOwned(%q) accepted", bad)
		}
	}
}

func TestDeleteStamped(t *testing.T) {
	s, ctx := storetest.New(t), context.Background()
	stamped, err := s.PutIfAbsent(ctx, "deb/dists/lts/main/binary-amd64/by-hash/SHA512/a", []byte("a"), store.PutOptions{Release: "v26.2.1.1-stable"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.PutIfAbsent(ctx, "deb/dists/lts/main/binary-amd64/by-hash/SHA512/b", []byte("b"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want store.DeleteResult
	}{
		{plain.Key, store.NotStamped},
		{stamped.Key, store.Deleted},
		{stamped.Key, store.Absent},
	} {
		if got, err := s.DeleteStamped(ctx, tc.key); err != nil || got != tc.want {
			t.Errorf("DeleteStamped(%s) = %v, %v, want %v", tc.key, got, err, tc.want)
		}
	}
	if _, err := s.Head(ctx, plain.Key); err != nil {
		t.Errorf("%s: %v, must stay", plain.Key, err)
	}
	for _, bad := range []string{"", "a/../b"} {
		if _, err := s.DeleteStamped(ctx, bad); err == nil {
			t.Errorf("DeleteStamped(%q) accepted", bad)
		}
	}
}

// An object written without a Content-Type has none, like the production
// indexes; the SDK default application/octet-stream is not sent.
func TestNoDefaultContentType(t *testing.T) {
	storetest.SkipOnMiniflare(t, "it stores application/octet-stream when no Content-Type was sent")
	s, ctx := storetest.New(t), context.Background()
	none, err := s.PutIfAbsent(ctx, "none", []byte("x"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "gz", []byte("x"), store.PutOptions{ContentType: "application/gzip"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyIfSourceMatch(ctx, "none", none.ETag, "copy"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"none": "", "gz": "application/gzip", "copy": ""} {
		if obj, err := s.Head(ctx, key); err != nil || obj.ContentType != want {
			t.Errorf("%s: content type %q, want %q: %v", key, obj.ContentType, want, err)
		}
	}
}
