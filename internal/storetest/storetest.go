// Package storetest gives tests an isolated store in a real or simulated bucket.
//
// Every test gets its own prefix below the run prefix and removes it when it
// finishes. The run prefix is CH_DEPLOY_TEST_PREFIX, or a generated one; a
// crashed run leaves it behind, and Purge or PurgeStale removes it later.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// TimeFormat is the timestamp that starts every run prefix name.
const TimeFormat = "20060102T150405Z"

// Env is the test bucket connection, read from the environment.
type Env struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// FromEnv reads CH_DEPLOY_TEST_ENDPOINT, CH_DEPLOY_TEST_BUCKET, AWS_ACCESS_KEY_ID
// and AWS_SECRET_ACCESS_KEY. It reports false when endpoint or bucket are unset.
func FromEnv() (Env, bool) {
	e := Env{
		Endpoint:        os.Getenv("CH_DEPLOY_TEST_ENDPOINT"),
		Bucket:          os.Getenv("CH_DEPLOY_TEST_BUCKET"),
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	return e, e.Endpoint != "" && e.Bucket != ""
}

// Config is the store configuration for prefix.
func (e Env) Config(prefix string) store.Config {
	return store.Config{
		Endpoint:        e.Endpoint,
		Bucket:          e.Bucket,
		Prefix:          prefix,
		AccessKeyID:     e.AccessKeyID,
		SecretAccessKey: e.SecretAccessKey,
	}
}

// Store opens the bucket under prefix.
func (e Env) Store(prefix string, deleteAnyKey bool, deletable ...string) (*store.Store, error) {
	cfg := e.Config(prefix)
	cfg.Deletable = deletable
	cfg.DeleteAnyKey = deleteAnyKey
	return store.New(cfg)
}

// Backend is CH_DEPLOY_TEST_BACKEND: "miniflare" under test/miniflare/run.sh, empty for real R2.
func Backend() string { return os.Getenv("CH_DEPLOY_TEST_BACKEND") }

// SkipOnMiniflare skips a test of R2 behaviour that Miniflare does not reproduce.
func SkipOnMiniflare(t testing.TB, reason string) {
	t.Helper()
	if Backend() == "miniflare" {
		t.Skip("Miniflare: " + reason)
	}
}

// NewRunPrefix returns a fresh run prefix, for example
// "ch-deploy-test/20260930T101500Z-1a2b3c4d5e6f".
func NewRunPrefix(now time.Time) string {
	return store.TestPrefix + now.UTC().Format(TimeFormat) + "-" + randomHex(6)
}

// CheckRunPrefix accepts only prefixes strictly below store.TestPrefix.
func CheckRunPrefix(p string) error {
	p = strings.Trim(p, "/")
	if !strings.HasPrefix(p+"/", store.TestPrefix) || p+"/" == store.TestPrefix || strings.Contains(p, "..") {
		return fmt.Errorf("test prefix %q must be below %s", p, store.TestPrefix)
	}
	return nil
}

var (
	runPrefixOnce sync.Once
	runPrefix     string
	runPrefixErr  error
)

// RunPrefix is CH_DEPLOY_TEST_PREFIX, or a prefix generated once per process.
func RunPrefix() (string, error) {
	runPrefixOnce.Do(func() {
		runPrefix = strings.Trim(os.Getenv("CH_DEPLOY_TEST_PREFIX"), "/")
		if runPrefix == "" {
			runPrefix = strings.TrimSuffix(NewRunPrefix(time.Now()), "/")
		}
		runPrefixErr = CheckRunPrefix(runPrefix)
	})
	return runPrefix, runPrefixErr
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// New returns a store under a prefix of its own for this test, and removes
// everything there when the test ends. It skips the test without a bucket.
// Delete works only under deletable, as in production.
func New(t testing.TB, deletable ...string) *store.Store {
	t.Helper()
	return NewConfig(t, func(c *store.Config) { c.Deletable = deletable })
}

// NewConfig is New with the store configuration adjusted by edit.
// Prefix and credentials are set before edit runs and must stay.
func NewConfig(t testing.TB, edit func(*store.Config)) *store.Store {
	t.Helper()
	env, ok := FromEnv()
	if !ok {
		t.Skip("CH_DEPLOY_TEST_ENDPOINT and CH_DEPLOY_TEST_BUCKET are not set")
	}
	run, err := RunPrefix()
	if err != nil {
		t.Fatal(err)
	}
	prefix := run + "/" + unsafeName.ReplaceAllString(t.Name(), "_") + "-" + randomHex(4)
	cfg := env.Config(prefix)
	edit(&cfg)
	if cfg.Prefix != prefix || cfg.DeleteAnyKey {
		t.Fatal("NewConfig: prefix and DeleteAnyKey are fixed")
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := Purge(context.Background(), env, prefix); err != nil {
			t.Errorf("cleanup %s: %v", prefix, err)
		}
	})
	return s
}

// Purge deletes every object under prefix, which must be below store.TestPrefix.
func Purge(ctx context.Context, env Env, prefix string) (int, error) {
	if err := CheckRunPrefix(prefix); err != nil {
		return 0, err
	}
	s, err := env.Store(prefix, true)
	if err != nil {
		return 0, err
	}
	var keys []string
	if err := s.List(ctx, "", func(o store.Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, k := range keys {
		if err := s.Delete(ctx, k); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// PurgeStale deletes run prefixes whose timestamp is older than maxAge.
// Names that do not start with a timestamp are left alone.
func PurgeStale(ctx context.Context, env Env, maxAge time.Duration, now time.Time) ([]string, error) {
	root, err := env.Store(store.TestPrefix, false)
	if err != nil {
		return nil, err
	}
	runs := map[string]bool{}
	if err := root.List(ctx, "", func(o store.Object) error {
		name, _, _ := strings.Cut(o.Key, "/")
		runs[name] = true
		return nil
	}); err != nil {
		return nil, err
	}
	var purged []string
	var errs []error
	for name := range runs {
		if !Stale(name, maxAge, now) {
			continue
		}
		if _, err := Purge(ctx, env, store.TestPrefix+name); err != nil {
			errs = append(errs, err)
			continue
		}
		purged = append(purged, store.TestPrefix+name)
	}
	return purged, errors.Join(errs...)
}

// Stale reports whether a run prefix name carries a timestamp older than maxAge.
func Stale(name string, maxAge time.Duration, now time.Time) bool {
	if len(name) < len(TimeFormat) {
		return false
	}
	ts, err := time.Parse(TimeFormat, name[:len(TimeFormat)])
	return err == nil && now.Sub(ts) > maxAge
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
