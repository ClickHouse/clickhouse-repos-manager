package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// The LTS copy is a single part object with the MD5 as ETag, and the check
// after the copy goes by metadata, not by the ETag of the source.
func TestPublishLTSCopy(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	mustLock(t, r, reqA)
	files := packageFiles(t, reqA.Tag)
	mustPublish(t, r, files)
	for _, f := range files {
		sums, err := hashes.File(f)
		if err != nil {
			t.Fatal(err)
		}
		name, prefix := filepath.Base(f), filepath.Ext(f)[1:]+"/"
		lts, err := b.Head(ctx, prefix+"lts/"+name)
		if err != nil {
			t.Fatal(err)
		}
		stable, err := b.Head(ctx, prefix+"stable/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(lts.ETag, `-5"`) || stable.ETag != `"`+sums.MD5+`"` || stable.Release != reqA.Tag || stable.SHA256 != sums.SHA256 {
			t.Fatalf("%s: lts %+v, stable %+v", name, lts, stable)
		}
	}
}

// An object without our metadata under a package key proves nothing about its
// content: the publish stops, and rollback leaves the object alone.
func TestPublishForeignObject(t *testing.T) {
	ctx, m := context.Background(), newMem()
	b := steady(t, m)
	files := packageFiles(t, reqA.Tag)
	m.PutForeign("tgz/lts/"+filepath.Base(files[1]), []byte("package "+filepath.Base(files[1])+"\n"))
	before := objects(t, b)
	r := releaser(b)
	mustLock(t, r, reqA)
	_, err := r.Publish(ctx, files, nil)
	wantErr(t, err, "exists without sha256 metadata")
	if _, err := r.Rollback(ctx, reqA.Tag); err != nil {
		t.Fatal(err)
	}
	checkRolledBack(t, b, before)
}

// otherStamp writes packages with another stamp, like an open upload that
// another release created and this run completed.
type otherStamp struct{ store.Bucket }

func (o otherStamp) PutFileIfAbsent(ctx context.Context, key, path string, sums hashes.Sums, opts store.PutOptions) (store.Object, error) {
	opts.Release = "v1.1.1.1-lts"
	return o.Bucket.PutFileIfAbsent(ctx, key, path, sums, opts)
}

// otherCRC reports another CRC64NVME for every package.
type otherCRC struct{ store.Bucket }

func (o otherCRC) Head(ctx context.Context, key string) (store.Object, error) {
	obj, err := o.Bucket.Head(ctx, key)
	if !strings.Contains(key, "/dists/") && !strings.HasPrefix(key, "configs/") {
		obj.CRC64NVME = "0000000000000000"
	}
	return obj, err
}

func TestPublishChecksWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(store.Bucket) store.Bucket
		want string
	}{
		{"stamp", func(b store.Bucket) store.Bucket { return otherStamp{b} }, `carries the stamp "v1.1.1.1-lts", not ` + reqA.Tag},
		{"crc", func(b store.Bucket) store.Bucket { return otherCRC{b} }, "exists with other content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := steady(t, newMem())
			mustLock(t, releaser(b), reqA)
			_, err := releaser(tc.wrap(b)).Publish(context.Background(), packageFiles(t, reqA.Tag), nil)
			wantErr(t, err, tc.want)
		})
	}
}

// flatType is a package type without index metadata, as TGZ.
type flatType struct{ fakeType }

func (flatType) Indexed() bool { return false }

// Adopt of a package type without index metadata finds nothing, and objects under its prefix are fine.
func TestLockAdoptUnindexed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		t    PackageType
		want string
	}{
		{flatType{fakeType{"flat"}}, ""},
		{fakeType{"flat"}, "flat/ holds objects (flat/lts/a.flat) but no metadata"},
	} {
		b := newMem()
		seed(t, b, map[string]string{"flat/lts/a.flat": "a"})
		r := releaser(b)
		r.Types = []PackageType{fakeType{"deb"}, tc.t}
		_, err := r.Lock(ctx, reqA)
		if tc.want != "" {
			wantErr(t, err, tc.want)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		m, err := r.manifest(ctx, AdoptedSnapshot)
		if err != nil || len(m.Files) != 0 {
			t.Fatalf("adopted %+v, %v", m, err)
		}
	}
}

// aborts records the keys of AbortUploads.
type aborts struct {
	store.Bucket
	mu   sync.Mutex
	keys []string
}

func (a *aborts) AbortUploads(ctx context.Context, key string) (int, error) {
	a.mu.Lock()
	a.keys = append(a.keys, key)
	a.mu.Unlock()
	return a.Bucket.AbortUploads(ctx, key)
}

func TestRollbackAbortsUploads(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	mustLock(t, r, reqA)
	mustPublish(t, r, packageFiles(t, reqA.Tag))
	_, lines, _, err := r.readAssets(ctx, snapA)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range lines {
		want = append(want, l.Key)
	}
	a := &aborts{Bucket: b}
	if _, err := releaser(a).Rollback(ctx, reqA.Tag); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	sort.Strings(a.keys)
	if !reflect.DeepEqual(a.keys, want) {
		t.Fatalf("aborted %q, want every assets key %q", a.keys, want)
	}
}

// public serves the objects of b by HEAD, as the public bucket URL does; keys in missing are 404.
func public(t *testing.T, b store.Bucket, missing *sync.Map) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/")
		obj, err := b.Head(req.Context(), key)
		if _, gone := missing.Load(key); err != nil || gone || req.Method != http.MethodHead {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPublishPublicURL(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	var missing sync.Map
	r := releaser(b)
	files := packageSet(t, reqA.Tag, []string{"a b"}, []string{"amd64"})
	mustLock(t, r, reqA)
	p, err := r.Publish(ctx, files, nil)
	if err != nil || p.PublicChecked != 0 {
		t.Fatalf("without a public URL: checked %d, %v", p.PublicChecked, err)
	}
	r.PublicURL = public(t, b, &missing).URL + "/"
	if p, err = r.Publish(ctx, files, nil); err != nil || p.PublicChecked != 4 {
		t.Fatalf("checked %d, want 2 packages in 2 channels: %v", p.PublicChecked, err)
	}
	missing.Store("tgz/stable/"+filepath.Base(files[1]), true)
	_, err = r.Publish(ctx, files, nil)
	wantErr(t, err, "public URL check: "+r.PublicURL+"tgz/stable/a%20b-26.3.36.6-amd64.tgz: status 404")
}

// The same release with 8 requests at once and with one: the same objects.
func TestPublishConcurrent(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, b := context.Background(), steady(t, b)
		before := objects(t, b)
		files := packageSet(t, reqA.Tag, []string{"a", "b", "c"}, []string{"amd64", "arm64"})
		packages := func() map[string]string {
			m := map[string]string{}
			for k, v := range objects(t, b) {
				if !strings.Contains(k, "/dists/") && strings.Contains(k, "-26.3.36.6-") {
					m[k] = v
				}
			}
			return m
		}
		keys := func() []string {
			var ks []string
			for k := range objects(t, b) {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			return ks
		}

		fast := releaser(b)
		fast.Concurrency = 8
		mustLock(t, fast, reqA)
		mustPublish(t, fast, files)
		checkPublished(t, b, snapA)
		fastKeys, fastPkgs := keys(), packages()
		if len(fastPkgs) != 24 {
			t.Fatalf("%d packages, want 6 per type in 2 channels", len(fastPkgs))
		}
		if _, err := fast.Rollback(ctx, reqA.Tag); err != nil {
			t.Fatal(err)
		}
		checkRolledBack(t, b, before)

		slow := releaser(b)
		slow.Concurrency = 1
		mustLock(t, slow, reqA)
		mustPublish(t, slow, files)
		checkPublished(t, b, snapA)
		if got := keys(); !reflect.DeepEqual(got, fastKeys) {
			t.Fatalf("keys differ:\n  serial %q\n  parallel %q", got, fastKeys)
		}
		if got := packages(); !reflect.DeepEqual(got, fastPkgs) {
			t.Fatalf("packages differ:\n  serial %q\n  parallel %q", got, fastPkgs)
		}
	})
}

// each stops starting calls after the first error and returns it.
func TestEach(t *testing.T) {
	for _, n := range []int{1, 3} {
		r := &Releaser{Concurrency: n}
		var calls atomic.Int32
		boom := context.DeadlineExceeded
		err := r.each(context.Background(), 100, func(_ context.Context, i int) error {
			calls.Add(1)
			if i == 5 {
				return boom
			}
			return nil
		})
		// One at a time is exact, as ForEachFault needs; in parallel others may start meanwhile.
		if err != boom || (n == 1 && calls.Load() != 6) || calls.Load() == 100 {
			t.Fatalf("concurrency %d: %v after %d calls", n, err, calls.Load())
		}
		calls.Store(0)
		if err := r.each(context.Background(), 100, func(context.Context, int) error { calls.Add(1); return nil }); err != nil || calls.Load() != 100 {
			t.Fatalf("concurrency %d: %v after %d calls", n, err, calls.Load())
		}
	}
}
