// Package tgz is the TGZ package type: .tgz archives, each with its
// .tgz.sha512, in tgz/<channel>/. It has no index metadata.
package tgz

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

const (
	archive  = ".tgz"
	checksum = ".tgz.sha512"
)

// Type is the TGZ package type.
type Type struct{}

var _ release.PackageType = Type{}

func (Type) Name() string   { return "tgz" }
func (Type) Prefix() string { return "tgz/" }
func (Type) Indexed() bool  { return false }

// Live finds nothing: TGZ has no metadata.
func (Type) Live(context.Context, store.Bucket) ([]release.File, error) { return nil, nil }

// Parse takes <package>-<version>-<arch>.tgz and the same name with .tgz.sha512.
func (Type) Parse(name, version string) (pkg, arch string, ok bool, err error) {
	base, ok := strings.CutSuffix(name, checksum)
	if !ok {
		if base, ok = strings.CutSuffix(name, archive); !ok {
			return "", "", false, nil
		}
	}
	i := strings.LastIndexByte(base, '-')
	if i < 0 {
		return "", "", true, fmt.Errorf("%s: want <package>-%s-<arch>%s", name, version, archive)
	}
	if arch = base[i+1:]; arch != "amd64" && arch != "arm64" {
		return "", "", true, fmt.Errorf("%s: unknown architecture %q, want amd64 or arm64", name, arch)
	}
	if pkg, ok = strings.CutSuffix(base[:i], "-"+version); !ok || pkg == "" {
		return "", "", true, fmt.Errorf("%s: want <package>-%s-<arch>%s, the version of the release", name, version, archive)
	}
	return pkg, arch, true, nil
}

// Check pairs every .tgz with its .tgz.sha512 and checks that the checksum
// file holds "<sha512>  <name>\n" of the archive, as sha512sum writes it.
func (Type) Check(pkgs []release.Package) error {
	archives := map[string]release.Package{}
	checksums := map[string]release.Package{}
	for _, p := range pkgs {
		if base, ok := strings.CutSuffix(p.Name, checksum); ok {
			checksums[base+archive] = p
		} else {
			archives[p.Name] = p
		}
	}
	for name := range checksums {
		if _, ok := archives[name]; !ok {
			return fmt.Errorf("%s has no %s", checksums[name].Name, name)
		}
	}
	for name, a := range archives {
		c, ok := checksums[name]
		if !ok {
			return fmt.Errorf("%s has no %s", name, name+".sha512")
		}
		if err := checkSum(c, a); err != nil {
			return err
		}
	}
	return nil
}

// maxChecksumSize is far above the 128 hex digits, two spaces, name and newline.
const maxChecksumSize = 4096

func checkSum(c, a release.Package) error {
	if c.Sums.Size > maxChecksumSize {
		return fmt.Errorf("%s: %d bytes, too large for a checksum file", c.Name, c.Sums.Size)
	}
	f, err := os.Open(c.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxChecksumSize+1))
	if err != nil {
		return fmt.Errorf("%s: %w", c.Name, err)
	}
	sum, name, ok := strings.Cut(strings.TrimSuffix(string(body), "\n"), "  ")
	switch {
	case !ok || !strings.HasSuffix(string(body), "\n") || strings.Contains(name, "\n"):
		return fmt.Errorf("%s: want \"<sha512>  <name>\\n\"", c.Name)
	case name != a.Name:
		return fmt.Errorf("%s names %s, not %s", c.Name, name, a.Name)
	case sum != a.Sums.SHA512:
		return fmt.Errorf("%s: sha512 %s, but %s has %s", c.Name, sum, a.Name, a.Sums.SHA512)
	}
	return nil
}

// Stage uploads every package to the first channel of the tag and copies it to
// the others. TGZ builds no metadata.
func (t Type) Stage(_ context.Context, _ *release.Previous, tag release.Tag, pkgs []release.Package) (release.Plan, error) {
	var plan release.Plan
	for _, p := range pkgs {
		opts := store.PutOptions{ContentType: "application/gzip", SHA512: p.Sums.SHA512}
		if strings.HasSuffix(p.Name, checksum) {
			opts.ContentType = "text/plain; charset=utf-8"
		}
		first := t.Prefix() + tag.Channels[0] + "/" + p.Name
		plan.Assets = append(plan.Assets, release.Asset{Key: first, Path: p.Path, Sums: p.Sums, Opts: opts})
		for _, ch := range tag.Channels[1:] {
			plan.Assets = append(plan.Assets, release.Asset{Key: t.Prefix() + ch + "/" + p.Name, CopyOf: first, Opts: opts})
		}
	}
	return plan, nil
}
