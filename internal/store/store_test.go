package store

import "testing"

func TestCanDelete(t *testing.T) {
	s := &Store{deletable: []string{"configs/ch-deploy/runs/"}}
	for key, want := range map[string]bool{
		"configs/ch-deploy/runs/20260929T122902Z-v26.3.36.6-lts/manifest.json": true,
		"configs/ch-deploy/runs/":          false,
		"configs/ch-deploy/lock":           false,
		"deb/pool/main/c/clickhouse/x.deb": false,
		"configs/ch-deploy/runs/../lock":   false,
		"configs/ch-deploy/runsX/a":        false,
	} {
		if got := s.canDelete(key); got != want {
			t.Errorf("canDelete(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestNewRejectsEmptyDeletable(t *testing.T) {
	_, err := New(Config{Endpoint: "https://example.com", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", Deletable: []string{"/"}})
	if err == nil {
		t.Fatal("an empty deletable prefix was accepted")
	}
}

func TestDeleteAnyKeyOnlyBelowTestPrefix(t *testing.T) {
	cfg := Config{Endpoint: "https://example.com", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", DeleteAnyKey: true}
	for prefix, ok := range map[string]bool{
		"":                       false,
		"deb":                    false,
		"ch-deploy-test":         false,
		"ch-deploy-test/":        false,
		"ch-deploy-test/../deb":  false,
		"ch-deploy-testX/run":    false,
		"ch-deploy-test/run":     true,
		"/ch-deploy-test/run/x/": true,
	} {
		cfg.Prefix = prefix
		s, err := New(cfg)
		if (err == nil) != ok {
			t.Errorf("prefix %q: err %v, want ok=%v", prefix, err, ok)
		}
		if err == nil && !s.canDelete("deb/pool/x.deb") {
			t.Errorf("prefix %q: delete any key does not allow every key", prefix)
		}
	}
}

func TestCopySource(t *testing.T) {
	for key, want := range map[string]string{
		"deb/pool/main/c/clickhouse/clickhouse-client_26.3.36.6_amd64.deb": "b/deb/pool/main/c/clickhouse/clickhouse-client_26.3.36.6_amd64.deb",
		"deb/pool/main/f/foo/foo_1.0+dfsg~1_amd64.deb":                     "b/deb/pool/main/f/foo/foo_1.0+dfsg~1_amd64.deb",
		"a b/c%d?e#f": "b/a%20b/c%25d%3Fe%23f",
	} {
		if got := copySource("b", key); got != want {
			t.Errorf("copySource(%q) = %q, want %q", key, got, want)
		}
	}
}
