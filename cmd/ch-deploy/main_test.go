package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: nil, code: 2, stderr: "Usage: ch-deploy"},
		{args: []string{"help"}, code: 0, stderr: "check"},
		{args: []string{"nope"}, code: 2, stderr: `unknown command "nope"`},
		{args: []string{"version"}, code: 0, stdout: "ch-deploy dev"},
		{args: []string{"version", "extra"}, code: 2, stdout: "unexpected arguments"},
		{args: []string{"check", "--bad-flag"}, code: 2, stdout: "flag provided but not defined"},
		{args: []string{"check", "-h"}, code: 0, stdout: "-sign-with"},
		{args: []string{"help"}, code: 0, stderr: "lock break"},
		{args: []string{"lock", "-h"}, code: 0, stdout: "-run-url"},
		{args: []string{"lock"}, code: 2, stdout: "--tag and --date are required"},
		{args: []string{"lock", "--tag", "v1.2.3.4-lts"}, code: 2, stdout: "--tag and --date are required"},
		{args: []string{"lock", "--tag", "v1.2.3.4-lts", "--date", "2026-09-29T12:29:02Z"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"lock", "extra"}, code: 2, stdout: "unexpected arguments"},
		{args: []string{"lock", "status", "-h"}, code: 0, stdout: "-endpoint"},
		{args: []string{"lock", "status"}, code: 1, stderr: "bucket access needs"},
		{args: []string{"lock", "break"}, code: 2, stdout: "--tag is required"},
		{args: []string{"lock", "break", "-h"}, code: 0, stdout: "-force"},
		{args: []string{"help"}, code: 0, stderr: "rollback"},
		{args: []string{"unlock"}, code: 2, stdout: "--tag is required"},
		{args: []string{"unlock", "-h"}, code: 0, stdout: "-tag"},
		{args: []string{"rollback", "--tag", "v1.2.3.4-lts"}, code: 1, stderr: "bucket access needs"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("CH_DEPLOY_ENDPOINT", "")
			t.Setenv("CH_DEPLOY_BUCKET", "")
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, &stdout, &stderr)
			}
			if !strings.Contains(stdout.String(), tc.stdout) || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("stdout %q (want %q), stderr %q (want %q)", &stdout, tc.stdout, &stderr, tc.stderr)
			}
		})
	}
}

func TestLockCommands(t *testing.T) {
	env, prefix := storetest.NewPrefix(t)
	t.Setenv("CH_DEPLOY_ENDPOINT", env.Endpoint)
	t.Setenv("CH_DEPLOY_BUCKET", env.Bucket)
	t.Setenv("CH_DEPLOY_PREFIX", prefix)
	sh := func(code int, want string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got != code || !strings.Contains(stdout.String()+stderr.String(), want) {
			t.Fatalf("%q: exit %d (want %d), want %q\nstdout: %s\nstderr: %s", args, got, code, want, &stdout, &stderr)
		}
	}
	sh(0, "no lock (not adopted)\n", "lock", "status")
	sh(1, "no lock (not adopted)", "lock", "break", "--tag", "v26.3.36.6-lts")
	sh(0, `"state": "locked"`, "lock", "--tag", "v26.3.36.6-lts", "--date", "2026-09-29T12:29:02Z", "--run-id", "42", "--run-url", "https://ci/42")
	sh(0, `"run_url": "https://ci/42"`, "lock", "status")
	sh(0, "\netag: ", "lock", "status")
	sh(1, "held by another release", "lock", "--tag", "v26.7.19.5-stable", "--date", "2026-10-01T08:00:00Z")
	sh(1, `tag "v1-lts"`, "lock", "--tag", "v1-lts", "--date", "2026-10-01T08:00:00Z")
	sh(0, `"state": "free"`, "lock", "break", "--tag", "v26.3.36.6-lts")
	sh(1, "the lock is free", "lock", "break", "--tag", "v26.3.36.6-lts")
	sh(0, `"previous": "00000000T000000Z-adopted"`, "lock", "--tag", "v26.7.19.5-stable", "--date", "2026-10-01T08:00:00Z")
	sh(1, "unlock needs state committed, the state is locked", "unlock", "--tag", "v26.7.19.5-stable")
	sh(1, "held by v26.7.19.5-stable, not v26.3.36.6-lts", "rollback", "--tag", "v26.3.36.6-lts")
	sh(0, `"state": "free"`, "rollback", "--tag", "v26.7.19.5-stable")
	sh(1, "the lock is free", "unlock", "--tag", "v26.7.19.5-stable")
}

func TestCheckNeedsSomething(t *testing.T) {
	for _, env := range []string{"CH_DEPLOY_ENDPOINT", "CH_DEPLOY_BUCKET", "CH_DEPLOY_SIGNING_KEY", "CH_DEPLOY_SIGNING_KEY_FILE", "CH_DEPLOY_SIGN_WITH"} {
		t.Setenv(env, "")
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "nothing to check") {
		t.Fatalf("exit %d, stderr %q", code, &stderr)
	}
}
