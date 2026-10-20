package deb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// Report is the result of Verify. Problems are defects of the repository;
// Verify returns an error only when it cannot read the bucket.
type Report struct {
	Codenames, Stanzas, PoolFiles, ByHash int
	Problems                              []string
	// Files are the verified index metadata, Named the by-hash keys, for the
	// public URL check.
	Files, Named []release.File
	// Signers are the keys that signed each codename with valid signatures.
	Signers []Signed
}

// Signed names the keys that signed a codename.
type Signed struct {
	Codename string
	Keys     []string
}

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Verify checks every codename, or only channel: everything Read checks, every
// by-hash key Release names, and every pool file an index references, by size
// and, for a single part object, by ETag against MD5sum. Without channel every
// .deb in the pool must be referenced by an index. A bad signature is a
// problem, and the other checks go on.
func (t Type) Verify(ctx context.Context, b store.Bucket, channel string) (Report, error) {
	codenames := config.Codenames
	if channel != "" {
		if !slices.Contains(config.Codenames, channel) {
			return Report{}, fmt.Errorf("unknown channel %q", channel)
		}
		codenames = []string{channel}
	}
	pool := map[string]store.Object{}
	if err := b.List(ctx, Prefix+"pool/", func(o store.Object) error { pool[o.Key] = o; return nil }); err != nil {
		return Report{}, err
	}
	var rep Report
	referenced := map[string]bool{}
	// Without the indexes of a codename nothing is known to be unreferenced;
	// a bad signature alone does not hide them.
	unverified := false
	for _, cn := range codenames {
		empty, err := emptyPrefix(ctx, b, Dists(cn))
		if err != nil {
			return Report{}, err
		}
		if empty {
			if channel != "" {
				rep.problem("%s: no metadata", cn)
			}
			continue
		}
		c, err := t.Read(ctx, b, cn)
		if err != nil {
			rep.problem("%s: %v", cn, err)
			unverified = true
			continue
		}
		if c.Signature != nil {
			rep.problem("%s: signature: %v", cn, c.Signature)
		} else {
			rep.Signers = append(rep.Signers, Signed{Codename: cn, Keys: c.Signers})
		}
		rep.Codenames++
		rep.Files = append(rep.Files, c.Files...)
		for _, f := range ByHashFiles(cn, c.Release) {
			rep.ByHash++
			rep.Named = append(rep.Named, f)
			obj, err := b.Head(ctx, f.Key)
			switch {
			case errors.Is(err, store.ErrNotFound):
				rep.problem("%s: missing", f.Key)
			case err != nil:
				return Report{}, err
			case obj.Size != f.Size:
				rep.problem("%s: size %d, Release has %d", f.Key, obj.Size, f.Size)
			}
		}
		for _, arch := range config.Architectures {
			for _, p := range c.Indexes[arch].Stanzas() {
				rep.Stanzas++
				key := Prefix + p.Value("Filename")
				referenced[key] = true
				obj, ok := pool[key]
				if !ok {
					rep.problem("%s %s: %s is missing", cn, KeyOf(p), key)
					continue
				}
				if size := strconv.FormatInt(obj.Size, 10); size != p.Value("Size") {
					rep.problem("%s: size %s, the index has %s", key, size, p.Value("Size"))
					continue
				}
				if etag := strings.Trim(obj.ETag, `"`); !strings.Contains(etag, "-") && etag != p.Value("MD5sum") {
					rep.problem("%s: MD5 %s, the index has %s", key, etag, p.Value("MD5sum"))
				}
			}
		}
	}
	checkReferenced := channel == "" && !unverified
	if channel == "" && unverified {
		rep.problem("unreferenced pool files not checked: a codename failed verification")
	}
	for _, key := range slices.Sorted(maps.Keys(pool)) {
		obj := pool[key]
		switch {
		case strings.HasSuffix(key, "/") && obj.Size == 0:
			// geesefs directory markers.
		case !strings.HasSuffix(key, suffix):
			rep.problem("%s: not a .deb", key)
		default:
			rep.PoolFiles++
			if checkReferenced && !referenced[key] {
				rep.problem("%s: no index references it", key)
			}
		}
	}
	return rep, nil
}
