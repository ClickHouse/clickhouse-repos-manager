package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

// objects is every object of the bucket: key to ETag and stamp.
func objects(t *testing.T, b store.Bucket) map[string]string {
	t.Helper()
	ctx := context.Background()
	m := map[string]string{}
	err := b.List(ctx, "", func(o store.Object) error {
		h, err := b.Head(ctx, o.Key)
		m[o.Key] = h.ETag + " " + h.Release
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// checkPublished checks that the release holds the lock in state committed and
// that every live index metadata file is byte for byte its snapshot file.
func checkPublished(t *testing.T, b store.Bucket, snap string) {
	t.Helper()
	ctx := context.Background()
	l := stored(t, b, Held{})
	if l.State != Committed || l.Snapshot != snap {
		t.Fatalf("lock %+v, want %s committed", l, snap)
	}
	m, err := releaser(b).manifest(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		live, _, err := b.Get(ctx, f.Key)
		if err != nil {
			t.Fatal(err)
		}
		if want := snapshotBody(t, b, snap, f.Key); string(live) != want {
			t.Fatalf("%s: live %q, snapshot %q", f.Key, live, want)
		}
	}
	if err := releaser(b).checkTypes(ctx, b, m); err != nil {
		t.Fatal(err)
	}
}

// checkRolledBack checks that the bucket is back to before, apart from the
// lock and the assets list, which stays.
func checkRolledBack(t *testing.T, b store.Bucket, before map[string]string) {
	t.Helper()
	if l := stored(t, b, Held{}); l.State != Free || l.Snapshot != snapX {
		t.Fatalf("lock %+v, want free with snapshot %s", l, snapX)
	}
	after := objects(t, b)
	for _, k := range []string{LockKey, AssetsKey(snapA)} {
		delete(after, k)
	}
	delete(before, LockKey)
	if !reflect.DeepEqual(after, before) {
		for k, v := range after {
			if before[k] != v {
				t.Errorf("%s: %q, before %q", k, v, before[k])
			}
		}
		for k := range before {
			if _, ok := after[k]; !ok {
				t.Errorf("%s: gone", k)
			}
		}
		t.FailNow()
	}
}

func TestSteady(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		b = steady(t, b)
		l := stored(t, b, Held{})
		if l.State != Free || l.Snapshot != snapX || l.Previous != snapW {
			t.Fatalf("lock %+v", l)
		}
		// X is -stable: its snapshot keeps the lts metadata of W byte for byte.
		for _, key := range []string{"deb/dists/lts/Packages", "deb/dists/lts/Release", "tgz/dists/lts/Release"} {
			if w, x := snapshotBody(t, b, snapW, key), snapshotBody(t, b, snapX, key); w != x {
				t.Fatalf("%s: W %q, X %q", key, w, x)
			}
		}
		if snapshotBody(t, b, snapW, stableRelease) == snapshotBody(t, b, snapX, stableRelease) {
			t.Fatal("W and X have the same stable Release")
		}
		for _, tag := range []string{reqW.Tag, reqX.Tag} {
			if _, err := b.Head(context.Background(), ReleasedKey(tag)); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestPublish(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, b := context.Background(), steady(t, b)
		r := releaser(b)
		mustLock(t, r, reqA)
		files := packageFiles(t, reqA.Tag)
		mustPublish(t, r, files)
		checkPublished(t, b, snapA)

		// Every asset exists with the stamp; a rerun in committed changes nothing.
		_, lines, _, err := r.readAssets(ctx, snapA)
		if err != nil {
			t.Fatal(err)
		}
		// Two packages in two channels, and a by-hash file per package type and channel.
		if len(lines) != 8 {
			t.Fatalf("assets %+v", lines)
		}
		for _, line := range lines {
			obj, err := b.Head(ctx, line.Key)
			if err != nil || obj.Release != reqA.Tag || obj.SHA256 != line.SHA256 {
				t.Fatalf("%s: %+v %v", line.Key, obj, err)
			}
		}
		before := objects(t, b)
		mustPublish(t, r, files)
		if after := objects(t, b); !reflect.DeepEqual(after, before) {
			t.Fatal("a rerun in committed changed the bucket")
		}
		tail, total, err := r.AssetsTail(ctx, 3)
		if err != nil || total != 8 || len(tail) != 3 || !strings.Contains(tail[2], `"key":"`) {
			t.Fatalf("tail %q of %d: %v", tail, total, err)
		}

		if _, err := r.Unlock(ctx, reqA.Tag); err != nil {
			t.Fatal(err)
		}
		released, _, err := b.Get(ctx, ReleasedKey(reqA.Tag))
		if err != nil {
			t.Fatal(err)
		}
		manifest, _, err := b.Get(ctx, ManifestKey(snapA))
		if err != nil || !bytes.Equal(released, manifest) {
			t.Fatalf("released %q, manifest %q: %v", released, manifest, err)
		}
		_, err = r.Lock(ctx, reqA)
		wantErr(t, err, "released and frozen")
		if tail, total, err := r.AssetsTail(ctx, 3); err != nil || total != 0 || tail != nil {
			t.Fatalf("tail of a free lock %q %d %v", tail, total, err)
		}
	})
}

// A kill or a lost response at every write of publish: the rerun reaches the
// snapshot byte for byte, although the rebuild differs from the first attempt.
func TestPublishFaults(t *testing.T) {
	storetest.ForEachFault(t, memBucket, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		steady(t, b)
		mustLock(t, releaser(b), reqA)
		files := packageFiles(t, reqA.Tag)
		f.Log = adoptGuard(t)
		faultyRun(t, f, func(r *Releaser) error { _, err := r.Publish(context.Background(), files, nil); return err })
		b = noAdopt(t, b)
		mustPublish(t, releaser(b), files)
		checkPublished(t, b, snapA)
	})
}

// Rollback after a publish that stopped at any write.
func TestRollbackAfterPublishFaults(t *testing.T) {
	storetest.ForEachFault(t, memBucket, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		steady(t, b)
		before := objects(t, b)
		mustLock(t, releaser(b), reqA)
		files := packageFiles(t, reqA.Tag)
		f.Log = adoptGuard(t)
		faultyRun(t, f, func(r *Releaser) error { _, err := r.Publish(context.Background(), files, nil); return err })
		b = noAdopt(t, b)
		if _, err := releaser(b).Rollback(context.Background(), reqA.Tag); err != nil {
			t.Fatal(err)
		}
		checkRolledBack(t, b, before)
	})
}

// A rollback that stops at any write is finished by the next one.
func TestRollbackFaults(t *testing.T) {
	storetest.ForEachFault(t, memBucket, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		steady(t, b)
		before := objects(t, b)
		mustLock(t, releaser(b), reqA)
		mustPublish(t, releaser(b), packageFiles(t, reqA.Tag))
		f.Log = adoptGuard(t)
		faultyRun(t, f, func(r *Releaser) error { _, err := r.Rollback(context.Background(), reqA.Tag); return err })
		b = noAdopt(t, b)
		if _, err := releaser(b).Rollback(context.Background(), reqA.Tag); err != nil && !strings.Contains(err.Error(), "the lock is free") {
			t.Fatal(err)
		}
		checkRolledBack(t, b, before)
	})
}

// Rollback deletes only what carries the release stamp.
func TestRollbackKeepsForeign(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, b := context.Background(), steady(t, b)
		files := packageFiles(t, reqA.Tag)
		body, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(files[0])
		if _, err := b.Put(ctx, "deb/lts/"+name, body, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Put(ctx, "deb/stable/"+name, body, store.PutOptions{Release: "v1.1.1.1-stable"}); err != nil {
			t.Fatal(err)
		}
		before := objects(t, b)
		r := releaser(b)
		mustLock(t, r, reqA)
		mustPublish(t, r, files)
		if _, err := r.Rollback(ctx, reqA.Tag); err != nil {
			t.Fatal(err)
		}
		checkRolledBack(t, b, before)
	})
}

func TestPublishConflict(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, b := context.Background(), steady(t, b)
		files := packageFiles(t, reqA.Tag)
		if _, err := b.Put(ctx, "tgz/lts/"+filepath.Base(files[1]), []byte("other"), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		before := objects(t, b)
		r := releaser(b)
		mustLock(t, r, reqA)
		_, err := r.Publish(ctx, files, nil)
		wantErr(t, err, "exists with other content")
		if _, err := r.Rollback(ctx, reqA.Tag); err != nil {
			t.Fatal(err)
		}
		checkRolledBack(t, b, before)
	})
}

func TestPublishRefused(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	files := packageFiles(t, reqA.Tag)
	_, err := r.Publish(ctx, files, nil)
	wantErr(t, err, "the state is free")
	mustLock(t, r, reqA)
	for _, tc := range []struct {
		paths []string
		only  []string
		want  string
	}{
		{append(files, filepath.Join(t.TempDir(), "x.rpm")), nil, "not a package of any known package type"},
		{append(files, files[0]), nil, "given twice"},
		{append(files, "clickhouse-26.3.36.6-amd64.deb"), nil, "given twice"},
		{[]string{"clickhouse-26.3.36.6-x86_64.deb"}, nil, `unknown architecture "x86_64"`},
		{[]string{"clickhouse-26.3.36.7-amd64.deb"}, nil, "not version 26.3.36.6"},
		{[]string{"amd64.deb"}, nil, "no architecture"},
		{[]string{"-26.3.36.6-amd64.deb"}, nil, "not version"},
		{files[:1], nil, "no tgz packages"},
		{packageSet(t, reqA.Tag, []string{"a", "b"}, []string{"amd64", "arm64"})[1:], nil, "deb lacks a amd64"},
		{nil, nil, "no deb packages"},
		{files, []string{"deb"}, "a tgz package, but only deb are published"},
		{files, []string{"rpm"}, "package type rpm not in this build"},
		{files[1:], []string{"deb", "tgz"}, "no deb packages"},
	} {
		_, err = r.Publish(ctx, tc.paths, tc.only)
		wantErr(t, err, tc.want)
	}
	if l := stored2(t, b).Lock; l.State != Locked {
		t.Fatalf("a refused publish changed the lock: %+v", l)
	}

	// Live changed by hand before the commit: nothing is copied.
	setLive(t, b, map[string]string{stableRelease: "changed by hand"})
	_, err = r.Publish(ctx, files, nil)
	wantErr(t, err, "matches neither "+snapX+" nor "+snapA)
	if got := string(must(b.Get(ctx, "deb/dists/lts/Release"))); got != snapshotBody(t, b, snapX, "deb/dists/lts/Release") {
		t.Fatalf("lts Release changed: %q", got)
	}
}

func must(body []byte, _ store.Object, err error) []byte {
	if err != nil {
		panic(err)
	}
	return body
}

// Once the commit started, the packages must be the ones in assets.
func TestPublishAfterCommitStarted(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	mustLock(t, r, reqA)
	files := packageFiles(t, reqA.Tag)
	h := mustPublish(t, r, files)
	h, err := r.setState(ctx, h, Committing)
	if err != nil {
		t.Fatal(err)
	}
	extra := packageSet(t, reqA.Tag, []string{"extra"}, []string{"amd64"})
	_, err = r.Publish(ctx, append(files, extra...), nil)
	wantErr(t, err, "extra-26.3.36.6-amd64.deb has no line in assets")
	changed := filepath.Join(t.TempDir(), filepath.Base(files[0]))
	if err := os.WriteFile(changed, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = r.Publish(ctx, []string{changed, files[1]}, nil)
	wantErr(t, err, "differs from deb/")

	// In committing a live file that is neither previous nor the snapshot stops the run.
	setLive(t, b, map[string]string{stableRelease: "changed by hand"})
	_, err = r.Publish(ctx, files, nil)
	wantErr(t, err, "matches neither")

	// In committed publish recovers whatever differs from the snapshot.
	if _, err := r.setState(ctx, stored2(t, b), Committed); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "tgz/index"); err != nil {
		t.Fatal(err)
	}
	mustPublish(t, r, files)
	checkPublished(t, b, snapA)
}

func stored2(t *testing.T, b store.Bucket) Held {
	t.Helper()
	h, found, err := releaser(b).read(context.Background())
	if err != nil || !found {
		t.Fatal(err)
	}
	return h
}

// The first release into an empty repository, and its rollback.
func TestFirstRelease(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, r := context.Background(), releaser(b)
		mustLock(t, r, reqA)
		before := objects(t, b)
		files := packageFiles(t, reqA.Tag)
		mustPublish(t, r, files)
		checkPublished(t, b, snapA)
		if _, err := r.Rollback(ctx, reqA.Tag); err != nil {
			t.Fatal(err)
		}
		after := objects(t, b)
		delete(after, AssetsKey(snapA))
		delete(after, LockKey)
		delete(before, LockKey)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("after rollback %v, before %v", after, before)
		}
		if l := stored(t, b, Held{}); l.State != Free || l.Snapshot != AdoptedSnapshot {
			t.Fatalf("lock %+v", l)
		}
		// A fresh attempt of the same release: the assets lines stay, new ones are appended.
		mustLock(t, r, reqA)
		mustPublish(t, r, files)
		checkPublished(t, b, snapA)
		if _, err := r.Unlock(ctx, reqA.Tag); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUnlock(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	fakes := seedSnapshots(t, b)
	_, err := r.Unlock(ctx, reqA.Tag)
	wantErr(t, err, "the lock is free")
	mustLock(t, r, reqA)
	_, err = r.Unlock(ctx, reqA.Tag)
	wantErr(t, err, "unlock needs state committed, the state is locked; end this release with rollback")
	files := packageFiles(t, reqA.Tag)
	mustPublish(t, r, files)
	_, err = r.Unlock(ctx, reqX.Tag)
	wantErr(t, err, "held by v26.3.36.6-lts, not v26.8.2.1-stable")
	setLive(t, b, map[string]string{stableRelease: "changed by hand"})
	_, err = r.Unlock(ctx, reqA.Tag)
	wantErr(t, err, "live metadata differs from snapshot "+snapA)
	mustPublish(t, r, files)
	h, err := r.Unlock(ctx, reqA.Tag)
	if err != nil {
		t.Fatal(err)
	}
	if l := stored(t, b, h); l.State != Free || l.Snapshot != snapA || l.Previous != snapX {
		t.Fatalf("lock %+v", l)
	}
	checkPruned(t, b, fakes)
	_, err = r.Rollback(ctx, reqA.Tag)
	wantErr(t, err, "the lock is free")
}

// seedSnapshots adds 30 snapshots newer than every release of the tests.
func seedSnapshots(t *testing.T, b store.Bucket) []string {
	t.Helper()
	var names []string
	for i := range 30 {
		name := fmt.Sprintf("20270101T0000%02dZ-v27.1.1.%d-stable", i, i+1)
		for key, body := range map[string][]byte{SnapshotDir(name) + "deb/index": []byte("x"), ManifestKey(name): encode(Manifest{Schema: Schema})} {
			if _, err := b.Put(context.Background(), key, body, store.PutOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		names = append(names, name)
	}
	return names
}

// checkPruned: the 30 newest snapshots stay, plus the snapshot and previous of the lock.
func checkPruned(t *testing.T, b store.Bucket, fakes []string) {
	t.Helper()
	names, err := releaser(b).snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{snapX, snapA}, fakes...)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("snapshots %q, want %q", names, want)
	}
}

func TestUnlockFaults(t *testing.T) {
	storetest.ForEachFault(t, memBucket, func(t *testing.T, b store.Bucket, f *storetest.Faults) {
		steady(t, b)
		fakes := seedSnapshots(t, b)
		mustLock(t, releaser(b), reqA)
		mustPublish(t, releaser(b), packageFiles(t, reqA.Tag))
		f.Log = adoptGuard(t)
		faultyRun(t, f, func(r *Releaser) error { _, err := r.Unlock(context.Background(), reqA.Tag); return err })
		b = noAdopt(t, b)
		if _, err := releaser(b).Unlock(context.Background(), reqA.Tag); err != nil && !strings.Contains(err.Error(), "the lock is free") {
			t.Fatal(err)
		}
		if l := stored(t, b, Held{}); l.State != Free || l.Snapshot != snapA {
			t.Fatalf("lock %+v", l)
		}
		if _, err := b.Head(context.Background(), ReleasedKey(reqA.Tag)); err != nil {
			t.Fatal(err)
		}
		checkPruned(t, b, fakes)
	})
}

// Break after a commit: live equal to the release snapshot makes it the published one.
func TestBreakCommitted(t *testing.T) {
	backends(t, func(t *testing.T, b store.Bucket) {
		ctx, b := context.Background(), steady(t, b)
		r := releaser(b)
		mustLock(t, r, reqA)
		mustPublish(t, r, packageFiles(t, reqA.Tag))
		res, err := r.Break(ctx, reqA.Tag, false)
		if err != nil || len(res.Differs) != 0 {
			t.Fatal(err, res.Differs)
		}
		if l := stored(t, b, res.Held); l.State != Free || l.Snapshot != snapA || l.Previous != snapX {
			t.Fatalf("lock %+v", l)
		}
		if h := mustLock(t, r, reqB); h.Lock.Previous != snapA {
			t.Fatalf("lock %+v", h.Lock)
		}
	})
}

// Break on a mix of both snapshots: refused, and with force previous is recorded.
func TestBreakMix(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	mustLock(t, r, reqA)
	mustPublish(t, r, packageFiles(t, reqA.Tag))
	setLive(t, b, map[string]string{stableRelease: snapshotBody(t, b, snapX, stableRelease)})
	_, err := r.Break(ctx, reqA.Tag, false)
	wantErr(t, err, "refused without --force")
	res, err := r.Break(ctx, reqA.Tag, true)
	if err != nil || len(res.Differs) == 0 {
		t.Fatal(err, res.Differs)
	}
	if l := stored(t, b, res.Held); l.State != Free || l.Snapshot != snapX {
		t.Fatalf("lock %+v", l)
	}
}

// strayType builds a metadata file its own index does not list, so Live finds less than the snapshot holds.
type strayType struct{ fakeType }

func (s strayType) Stage(ctx context.Context, prev *Previous, tag Tag, pkgs []Package) (Plan, error) {
	plan, err := s.fakeType.Stage(ctx, prev, tag, pkgs)
	plan.Metadata = append(plan.Metadata, Metadata{Key: s.Prefix() + "stray", Body: []byte("stray")})
	return plan, err
}

func TestPublishBrokenSnapshot(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	live := objects(t, b)
	r := releaser(b)
	r.Types = []PackageType{strayType{fakeType{"deb"}}, fakeType{"tgz"}}
	mustLock(t, r, reqA)
	_, err := r.Publish(ctx, packageFiles(t, reqA.Tag), nil)
	wantErr(t, err, "snapshot "+snapA+": deb: found 5 metadata files, the manifest has 6")
	after := objects(t, b)
	for k, v := range live {
		if k != LockKey && after[k] != v {
			t.Fatalf("%s changed", k)
		}
	}
	if _, err := b.Head(ctx, ManifestKey(snapA)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("manifest written: %v", err)
	}
}

// snapshotPutFails fails every Put into a snapshot file other than the manifest.
type snapshotPutFails struct {
	store.Bucket
	name string
}

func (s snapshotPutFails) Put(ctx context.Context, key string, body []byte, opts store.PutOptions) (store.Object, error) {
	if strings.HasPrefix(key, SnapshotDir(s.name)) && key != ManifestKey(s.name) {
		return store.Object{}, errors.New("injected")
	}
	return s.Bucket.Put(ctx, key, body, opts)
}

// Staging again removes the manifest before it rewrites the snapshot.
func TestRestageDropsManifest(t *testing.T) {
	ctx, b := context.Background(), steady(t, newMem())
	r := releaser(b)
	mustLock(t, r, reqA)
	files := packageFiles(t, reqA.Tag)
	h := mustPublish(t, r, files)
	if _, err := r.setState(ctx, h, Staging); err != nil {
		t.Fatal(err)
	}
	_, err := releaser(snapshotPutFails{b, snapA}).Publish(ctx, files, nil)
	wantErr(t, err, "injected")
	if _, err := b.Head(ctx, ManifestKey(snapA)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale manifest kept: %v", err)
	}
}

func TestCopyKnown(t *testing.T) {
	ctx, b := context.Background(), newMem()
	r := releaser(b)
	obj, err := b.Put(ctx, "src", []byte("body"), store.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := File{Key: "src", Size: obj.Size, ETag: obj.ETag, SHA256: hashes.Bytes([]byte("body")).SHA256}
	if err := r.copyKnown(ctx, "src", "dst", want); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "src", []byte("changed"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.copyKnown(ctx, "src", "dst2", want); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("changed source: %v", err)
	}
	if _, err := b.Put(ctx, "src", []byte("body"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	other := want
	other.SHA256 = hashes.Bytes([]byte("other")).SHA256
	wantErr(t, r.copyKnown(ctx, "src", "dst3", other), "want")
}

func TestParseAssets(t *testing.T) {
	line := `{"key":"a","size":1,"md5":"","sha1":"","sha256":"x","sha512":""}`
	if l, err := ParseAssets([]byte(line + "\n" + line + "\n")); err != nil || len(l) != 2 {
		t.Fatal(l, err)
	}
	for _, bad := range []string{line, `{"key":"a","extra":1}` + "\n", `{"size":1}` + "\n", "{} {}\n", "x\n"} {
		if _, err := ParseAssets([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
