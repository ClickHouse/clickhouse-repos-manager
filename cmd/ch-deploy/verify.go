package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/deb"
)

// errProblems makes verify exit with 1 after it printed the problems.
var errProblems = errors.New("the repository has problems")

func runVerify(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "deb" {
		if len(args) > 0 && (args[0] == "rpm" || args[0] == "tgz") {
			return fmt.Errorf("verify %s: package type not in this build", args[0])
		}
		fmt.Fprintln(stdout, "Usage: ch-deploy verify deb [--channel <codename>] [flags]")
		return errUsage
	}
	var l config.Launch
	var channel string
	fs := newFlagSet("verify deb", stdout)
	l.Register(fs)
	fs.StringVar(&channel, "channel", "", "verify only this codename; without it every pool file must be referenced")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	r, err := releaser(&l)
	if err != nil {
		return err
	}
	var typ deb.Type
	for _, t := range r.Types {
		if d, ok := t.(deb.Type); ok {
			typ = d
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	rep, err := typ.Verify(ctx, r.Bucket, channel)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "deb: %d codenames, %d index entries, %d pool files, %d by-hash files\n", rep.Codenames, rep.Stanzas, rep.PoolFiles, rep.ByHash)
	for _, s := range rep.Signers {
		fmt.Fprintf(stdout, "deb %s: signed by %s\n", s.Codename, strings.Join(s.Keys, ", "))
	}
	if r.PublicURL == "" {
		fmt.Fprintln(stdout, "public URL check: skipped, no --public-url")
	} else {
		n, err := r.CheckPublic(ctx, rep.Files, rep.Named)
		if err != nil {
			rep.Problems = append(rep.Problems, "public URL check: "+err.Error())
		} else {
			fmt.Fprintf(stdout, "public URL check: %d objects at %s\n", n, r.PublicURL)
		}
	}
	for _, p := range rep.Problems {
		fmt.Fprintln(stdout, "problem:", p)
	}
	if len(rep.Problems) > 0 {
		return fmt.Errorf("%w: %d", errProblems, len(rep.Problems))
	}
	return nil
}
