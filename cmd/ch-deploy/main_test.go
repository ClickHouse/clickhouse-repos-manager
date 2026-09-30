package main

import (
	"bytes"
	"strings"
	"testing"
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

func TestCheckNeedsSomething(t *testing.T) {
	for _, env := range []string{"CH_DEPLOY_ENDPOINT", "CH_DEPLOY_BUCKET", "CH_DEPLOY_SIGNING_KEY", "CH_DEPLOY_SIGNING_KEY_FILE", "CH_DEPLOY_SIGN_WITH"} {
		t.Setenv(env, "")
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "nothing to check") {
		t.Fatalf("exit %d, stderr %q", code, &stderr)
	}
}
