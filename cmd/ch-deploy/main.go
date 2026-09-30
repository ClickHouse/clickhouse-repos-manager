// Command ch-deploy publishes ClickHouse packages to packages.clickhouse.com.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type command struct {
	summary string
	run     func(args []string, stdout io.Writer) error
}

var commands = map[string]command{
	"version": {"print the version", runVersion},
	"check":   {"check bucket access and the signing key", runCheck},
}

var (
	// errUsage marks errors caused by the command line; the flag package has already printed them.
	errUsage = errors.New("usage")
	// errHelp stops a command after its help was printed.
	errHelp = errors.New("help")
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "ch-deploy: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	if err := cmd.run(args[1:], stdout); err != nil {
		if errors.Is(err, errHelp) {
			return 0
		}
		if errors.Is(err, errUsage) {
			return 2
		}
		fmt.Fprintf(stderr, "ch-deploy %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: ch-deploy <command> [flags]\n\nCommands:")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-10s %s\n", name, commands[name].summary)
	}
	fmt.Fprintln(w, "\nRun 'ch-deploy <command> -h' for the flags of a command.")
}

func newFlagSet(name string, stdout io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("ch-deploy "+name, flag.ContinueOnError)
	fs.SetOutput(stdout)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected arguments: %q\n", fs.Args())
		return errUsage
	}
	return nil
}

func runVersion(args []string, stdout io.Writer) error {
	if err := parse(newFlagSet("version", stdout), args); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ch-deploy %s%s\n", version, revision())
	return nil
}

func revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return ""
	}
	return " (" + rev + dirty + ")"
}

func runCheck(args []string, stdout io.Writer) error {
	var l config.Launch
	fs := newFlagSet("check", stdout)
	l.Register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	l.Resolve()
	if !l.HasStore() && !l.HasSigner() {
		return errors.New("nothing to check: configure the bucket, the signing key, or both")
	}
	if l.HasStore() {
		s, err := l.Store()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.Ping(ctx); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "bucket: ok (%s, bucket %s, prefix %q)\n", l.Endpoint, l.Bucket, l.Prefix)
	}
	if l.HasSigner() {
		s, err := l.Signer()
		if err != nil {
			return err
		}
		probe := []byte("ch-deploy check\n")
		sig, err := s.DetachSign(probe)
		if err != nil {
			return err
		}
		if err := s.Verifier().VerifyDetached(probe, sig); err != nil {
			return fmt.Errorf("signature does not verify: %w", err)
		}
		fmt.Fprintf(stdout, "signing: ok (key %s, primary %s)\n", s.Fingerprint(), s.PrimaryFingerprint())
	}
	return nil
}
