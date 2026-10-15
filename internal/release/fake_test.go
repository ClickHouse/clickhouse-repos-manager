package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// fakeType keeps its metadata in <prefix>index, which lists the other metadata
// keys; the index is metadata too. Per channel it builds Packages ("<key>
// <sha256>" lines, append only), a content named by-hash copy of it, and a
// Release that changes on every build, as real signed metadata does.
type fakeType struct{ name string }

// generation makes every build differ, so no test can rely on a deterministic rebuild.
var generation atomic.Int64

func (f fakeType) Name() string           { return f.name }
func (f fakeType) Prefix() string         { return f.name + "/" }
func (f fakeType) Match(name string) bool { return strings.HasSuffix(name, "."+f.name) }

func (f fakeType) Live(ctx context.Context, b store.Bucket) ([]File, error) {
	index, _, err := b.Get(ctx, f.Prefix()+"index")
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []File
	for _, key := range append([]string{f.Prefix() + "index"}, strings.Fields(string(index))...) {
		body, obj, err := b.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Key: key, Size: obj.Size, ETag: obj.ETag, SHA256: hashes.Bytes(body).SHA256})
	}
	return files, nil
}

func (f fakeType) Stage(ctx context.Context, prev *Previous, tag Tag, pkgs []Package) (Plan, error) {
	p := f.Prefix()
	var plan Plan
	for _, pkg := range pkgs {
		for _, ch := range tag.Channels {
			plan.Assets = append(plan.Assets, Asset{Key: p + ch + "/" + pkg.Name, Path: pkg.Path, Sums: pkg.Sums})
		}
	}
	oldIndex, _, err := prev.Get(ctx, p+"index")
	if err != nil {
		return Plan{}, err
	}
	keys := map[string]bool{}
	for _, k := range strings.Fields(string(oldIndex)) {
		keys[k] = true
	}
	for _, ch := range tag.Channels {
		pk := p + "dists/" + ch + "/Packages"
		body, _, err := prev.Get(ctx, pk)
		if err != nil {
			return Plan{}, err
		}
		have := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			if k, sum, ok := strings.Cut(line, " "); ok {
				have[k] = sum
			}
		}
		for _, pkg := range pkgs {
			key := p + ch + "/" + pkg.Name
			if sum, ok := have[key]; ok {
				if sum != pkg.Sums.SHA256 {
					return Plan{}, fmt.Errorf("%s is in the index with other content", key)
				}
				continue
			}
			body = append(body, key+" "+pkg.Sums.SHA256+"\n"...)
		}
		sum := hashes.Bytes(body).SHA256
		plan.Assets = append(plan.Assets, Asset{Key: p + "dists/" + ch + "/by-hash/" + sum, Body: body})
		release := fmt.Sprintf("packages %s\ngeneration %d\n", sum, generation.Add(1))
		rk := p + "dists/" + ch + "/Release"
		plan.Metadata = append(plan.Metadata, Metadata{Key: pk, Body: body}, Metadata{Key: rk, Body: []byte(release)})
		keys[pk], keys[rk] = true, true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	if index := strings.Join(sorted, " "); index != string(oldIndex) {
		plan.Metadata = append(plan.Metadata, Metadata{Key: p + "index", Body: []byte(index)})
	}
	return plan, nil
}

// packageFiles writes one fake package per package type for the tag's version.
func packageFiles(t *testing.T, tag string) []string {
	t.Helper()
	tg, err := ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var paths []string
	for _, pt := range types {
		path := filepath.Join(dir, "clickhouse-"+tg.Version()+"."+pt.Name())
		if err := os.WriteFile(path, []byte(pt.Name()+" "+tg.Version()+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}
