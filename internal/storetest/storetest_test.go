package storetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

func TestCheckRunPrefix(t *testing.T) {
	for p, ok := range map[string]bool{
		"":                          false,
		"ch-deploy-test":            false,
		"ch-deploy-test/":           false,
		"deb/pool":                  false,
		"ch-deploy-testX/a":         false,
		"ch-deploy-test/../deb":     false,
		"ch-deploy-test/20260930T1": true,
		"/ch-deploy-test/a/b/":      true,
	} {
		if err := CheckRunPrefix(p); (err == nil) != ok {
			t.Errorf("CheckRunPrefix(%q) = %v, want ok=%v", p, err, ok)
		}
	}
}

func TestNewRunPrefix(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 15, 0, 0, time.UTC)
	p := NewRunPrefix(now)
	if !strings.HasPrefix(p, store.TestPrefix+"20260930T101500Z-") || CheckRunPrefix(p) != nil {
		t.Fatalf("NewRunPrefix = %q", p)
	}
	if !Stale(strings.TrimPrefix(p, store.TestPrefix), time.Hour, now.Add(2*time.Hour)) {
		t.Fatalf("%q is not stale two hours later", p)
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for name, want := range map[string]bool{
		"20260929T110000Z-abc": true,
		"20260929T130000Z-abc": false,
		"20260930T115900Z":     false,
		"manual":               false,
		"2026-09-29":           false,
	} {
		if got := Stale(name, 24*time.Hour, now); got != want {
			t.Errorf("Stale(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestNewIsolatedAndPurged also checks that the per test prefix is empty after cleanup.
func TestNewIsolatedAndPurged(t *testing.T) {
	if _, ok := FromEnv(); !ok {
		t.Skip("CH_DEPLOY_TEST_ENDPOINT and CH_DEPLOY_TEST_BUCKET are not set")
	}
	ctx := context.Background()
	var inner *store.Store
	t.Run("inner", func(t *testing.T) {
		inner = New(t)
		if _, err := inner.PutIfAbsent(ctx, "a/b", []byte("x"), store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := inner.Delete(ctx, "a/b"); err == nil {
			t.Fatal("the test store deletes outside its deletable prefixes")
		}
	})
	n := 0
	if err := inner.List(ctx, "", func(store.Object) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d objects left after cleanup", n)
	}
}
