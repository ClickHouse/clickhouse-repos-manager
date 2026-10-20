package release

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/hashes"
)

// Planned is what a dry run would publish for one package type.
type Planned struct {
	Type string
	Plan Plan
	// Old maps the metadata keys of the plan that exist live to their bytes.
	Old map[string][]byte
}

// DryRun builds what publish would write for tag on top of the live
// repository, without a lock and without any write. Previous is the verified
// live metadata.
func (r *Releaser) DryRun(ctx context.Context, tagName string, paths, only []string) ([]Planned, error) {
	tag, err := ParseTag(tagName)
	if err != nil {
		return nil, err
	}
	byType, _, err := r.packages(ctx, paths, tag.Version(), only)
	if err != nil {
		return nil, err
	}
	var live []File
	for _, t := range r.Types {
		files, err := t.Live(ctx, r.Bucket)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name(), err)
		}
		live = append(live, files...)
	}
	prev := LivePrevious(r.Bucket, live)
	var out []Planned
	for _, t := range r.Types {
		pkgs := byType[t.Name()]
		if len(pkgs) == 0 {
			continue
		}
		plan, err := t.Stage(ctx, prev, tag, pkgs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name(), err)
		}
		p := Planned{Type: t.Name(), Plan: plan, Old: map[string][]byte{}}
		sums := map[string]hashes.Sums{}
		for i, a := range plan.Assets {
			switch {
			case a.CopyOf != "":
				plan.Assets[i].Sums = sums[a.CopyOf]
			case a.Path == "":
				plan.Assets[i].Sums = hashes.Bytes(a.Body)
			}
			sums[a.Key] = plan.Assets[i].Sums
		}
		for _, m := range plan.Metadata {
			body, found, err := prev.Get(ctx, m.Key)
			if err != nil {
				return nil, err
			}
			if found {
				p.Old[m.Key] = body
			}
		}
		out = append(out, p)
	}
	return out, nil
}
