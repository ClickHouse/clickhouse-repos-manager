package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
)

// binaryFile reports whether a metadata file is shown as changed rather than diffed.
func binaryFile(key string) bool {
	for _, s := range []string{".gz", ".xz", ".gpg"} {
		if strings.HasSuffix(key, s) {
			return true
		}
	}
	return false
}

// printDryRun writes the metadata and the content named metadata of a dry run
// under out at their live keys, and prints what a publish would add and change.
func printDryRun(w io.Writer, out string, planned []release.Planned, unsigned bool) error {
	write := func(key string, body []byte) error {
		if strings.Contains(key, "..") {
			return fmt.Errorf("refused key %q", key)
		}
		path := filepath.Join(out, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o644)
	}
	for _, p := range planned {
		if p.Type == "deb" && unsigned {
			fmt.Fprintln(w, "deb: no signing key: Release is built, InRelease, Release.gpg and Public.key are not")
		}
		for _, a := range p.Plan.Assets {
			if a.Path != "" || a.CopyOf != "" {
				fmt.Fprintf(w, "%s: add %s (%d bytes)\n", p.Type, a.Key, a.Sums.Size)
				continue
			}
			if err := write(a.Key, a.Body); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s: add content named %s (%d bytes)\n", p.Type, a.Key, a.Sums.Size)
		}
		for _, m := range p.Plan.Metadata {
			if err := write(m.Key, m.Body); err != nil {
				return err
			}
			old, found := p.Old[m.Key]
			switch {
			case found && bytes.Equal(old, m.Body):
			case !binaryFile(m.Key):
				fmt.Fprint(w, unifiedDiff(m.Key, old, m.Body))
			case found:
				fmt.Fprintf(w, "%s: binary changed %s (%d bytes, was %d)\n", p.Type, m.Key, len(m.Body), len(old))
			default:
				fmt.Fprintf(w, "%s: binary new %s (%d bytes)\n", p.Type, m.Key, len(m.Body))
			}
		}
	}
	fmt.Fprintf(w, "dry run: files written to %s, nothing written remote\n", out)
	return nil
}
