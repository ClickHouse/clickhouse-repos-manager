// Command ch-deploy publishes ClickHouse packages to packages.clickhouse.com.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/config"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/release"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/tgz"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type command struct {
	summary string
	run     func(args []string, stdout io.Writer) error
}

var commands = map[string]command{
	"version":  {"print the version", runVersion},
	"check":    {"check bucket access and the signing key", runCheck},
	"lock":     {"take the release lock; 'lock status', 'lock break' (DANGEROUS, break glass only)", runLock},
	"publish":  {"publish the packages of the release that holds the lock, or continue it", runPublish},
	"unlock":   {"free the lock after the client tests passed; the release is frozen", runUnlock},
	"rollback": {"undo the release that holds the lock", runRollback},
}

// assetsTail is how many assets lines lock status prints.
const assetsTail = 20

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

// packageTypes are the package types this build knows; adopt covers only them.
func packageTypes() []release.PackageType { return []release.PackageType{tgz.Type{}} }

// testStoreConfig is set only by tests, for buckets the store must treat
// differently (Miniflare cannot list multipart uploads).
var testStoreConfig func(*store.Config)

func releaser(l *config.Launch) (*release.Releaser, error) {
	l.Resolve()
	cfg, err := l.StoreConfig(config.SnapshotsPrefix)
	if err != nil {
		return nil, err
	}
	if testStoreConfig != nil {
		testStoreConfig(&cfg)
	}
	s, err := store.New(cfg)
	if err != nil {
		return nil, err
	}
	concurrency, err := l.Concurrency()
	if err != nil {
		return nil, err
	}
	return &release.Releaser{
		Bucket: s, Types: packageTypes(), Version: version + revision(),
		Concurrency: concurrency, PublicURL: l.PublicBase(),
	}, nil
}

// signalContext stops at SIGINT or SIGTERM; conditional writes make a stop at any point safe to rerun.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func printLock(w io.Writer, h release.Held) error {
	b, err := json.MarshalIndent(h.Lock, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\netag: %s\n", b, h.ETag)
	return err
}

func runLock(args []string, stdout io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return runLockStatus(args[1:], stdout)
		case "break":
			return runLockBreak(args[1:], stdout)
		}
	}
	var l config.Launch
	var req release.LockRequest
	fs := newFlagSet("lock", stdout)
	l.Register(fs)
	fs.StringVar(&req.Tag, "tag", "", "release tag, v<major>.<minor>.<patch>.<build>-lts or -stable (required)")
	fs.StringVar(&req.Date, "date", "", "creation time of the annotated tag, YYYY-MM-DDTHH:MM:SSZ (required)")
	fs.StringVar(&req.RunID, "run-id", "", "CI run id, recorded in the lock")
	fs.StringVar(&req.RunURL, "run-url", "", "CI run URL, recorded in the lock")
	if err := parse(fs, args); err != nil {
		return err
	}
	if req.Tag == "" || req.Date == "" {
		fmt.Fprintln(stdout, "--tag and --date are required")
		return errUsage
	}
	r, err := releaser(&l)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	h, err := r.Lock(ctx, req)
	if err != nil {
		return err
	}
	return printLock(stdout, h)
}

func runLockStatus(args []string, stdout io.Writer) error {
	var l config.Launch
	fs := newFlagSet("lock status", stdout)
	l.Register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	r, err := releaser(&l)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	h, found, err := r.Status(ctx)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintln(stdout, "no lock (not adopted)")
		return nil
	}
	if err := printLock(stdout, h); err != nil {
		return err
	}
	lines, total, err := r.AssetsTail(ctx, assetsTail)
	if err != nil || total == 0 {
		return err
	}
	fmt.Fprintf(stdout, "assets, last %d of %d:\n", len(lines), total)
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func runPublish(args []string, stdout io.Writer) error {
	var l config.Launch
	fs := newFlagSet("publish", stdout)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: ch-deploy publish [flags] <files...>\n\nFlags come before the files.")
		fs.PrintDefaults()
	}
	l.Register(fs)
	only := map[string]*bool{}
	for _, name := range []string{"deb", "rpm", "tgz"} {
		only[name] = fs.Bool("only-"+name, false, "publish only "+name+" packages; combinable, narrows the file set check")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return errUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stdout, "no files given")
		return errUsage
	}
	var types []string
	for _, name := range []string{"deb", "rpm", "tgz"} {
		if *only[name] {
			types = append(types, name)
		}
	}
	r, err := releaser(&l)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	p, err := r.Publish(ctx, fs.Args(), types)
	if err != nil {
		return err
	}
	if err := printLock(stdout, p.Held); err != nil {
		return err
	}
	if r.PublicURL == "" {
		_, err = fmt.Fprintln(stdout, "public URL check: skipped, no --public-url")
	} else {
		_, err = fmt.Fprintf(stdout, "public URL check: %d objects at %s\n", p.PublicChecked, r.PublicURL)
	}
	return err
}

func runUnlock(args []string, stdout io.Writer) error {
	return runTagged("unlock", args, stdout, func(r *release.Releaser, ctx context.Context, tag string) (release.Held, error) {
		u, err := r.Unlock(ctx, tag)
		if err != nil {
			return release.Held{}, err
		}
		for _, p := range u.Pruned {
			fmt.Fprintf(stdout, "pruned %s: %d deleted\n", p.Type, len(p.Deleted))
			for _, key := range p.NotStamped {
				fmt.Fprintf(stdout, "pruned %s: %s is unreferenced but carries no stamp, kept\n", p.Type, key)
			}
		}
		return u.Held, nil
	})
}

func runRollback(args []string, stdout io.Writer) error {
	return runTagged("rollback", args, stdout, (*release.Releaser).Rollback)
}

// runTagged runs a command that takes only --tag of the release holding the lock.
func runTagged(name string, args []string, stdout io.Writer, do func(*release.Releaser, context.Context, string) (release.Held, error)) error {
	var l config.Launch
	var tag string
	fs := newFlagSet(name, stdout)
	l.Register(fs)
	fs.StringVar(&tag, "tag", "", "tag of the release that holds the lock (required)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if tag == "" {
		fmt.Fprintln(stdout, "--tag is required")
		return errUsage
	}
	r, err := releaser(&l)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	h, err := do(r, ctx, tag)
	if err != nil {
		return err
	}
	return printLock(stdout, h)
}

func runLockBreak(args []string, stdout io.Writer) error {
	var l config.Launch
	var tag string
	var force bool
	fs := newFlagSet("lock break", stdout)
	l.Register(fs)
	fs.StringVar(&tag, "tag", "", "tag of the release that holds the lock (required)")
	fs.BoolVar(&force, "force", false, "DANGEROUS: free the lock even if the live metadata matches neither snapshot")
	if err := parse(fs, args); err != nil {
		return err
	}
	if tag == "" {
		fmt.Fprintln(stdout, "--tag is required")
		return errUsage
	}
	r, err := releaser(&l)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	res, err := r.Break(ctx, tag, force)
	if err != nil {
		return err
	}
	for _, d := range res.Differs {
		fmt.Fprintf(stdout, "differs: %s\n", d)
	}
	return printLock(stdout, res.Held)
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
