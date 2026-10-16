package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, env map[string]string, args ...string) *Launch {
	t.Helper()
	l := &Launch{getenv: func(k string) string { return env[k] }}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	l.Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	l.Resolve()
	return l
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	env := map[string]string{EnvEndpoint: "https://env", EnvBucket: "env-bucket", EnvPublicURL: "https://pub/"}
	l := parse(t, env, "--bucket", "flag-bucket")
	if l.Endpoint != "https://env" || l.Bucket != "flag-bucket" {
		t.Fatalf("got endpoint %q bucket %q", l.Endpoint, l.Bucket)
	}
	if l.PublicURL != "https://pub" {
		t.Fatalf("public URL %q keeps the trailing slash", l.PublicURL)
	}
}

func TestStoreNeedsEverything(t *testing.T) {
	l := parse(t, map[string]string{EnvBucket: "b"})
	_, err := l.Store()
	if err == nil {
		t.Fatal("Store without endpoint and credentials succeeded")
	}
	for _, want := range []string{EnvEndpoint, EnvAccessKeyID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	l = parse(t, map[string]string{EnvEndpoint: "https://e", EnvBucket: "b", EnvAccessKeyID: "a", EnvSecretKey: "s"})
	if _, err := l.Store(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadParameters(t *testing.T) {
	base := map[string]string{EnvEndpoint: "https://e", EnvBucket: "b", EnvAccessKeyID: "a", EnvSecretKey: "s", EnvPartSize: "6000000", EnvConcurrency: "3"}
	cfg, err := parse(t, base, "--upload-concurrency", "8").StoreConfig()
	if err != nil || cfg.PartSize != 6000000 || cfg.UploadConcurrency != 8 {
		t.Fatalf("config %+v, %v", cfg, err)
	}
	for _, bad := range [][]string{{"--part-size", "64MiB"}, {"--part-size", "0"}, {"--upload-concurrency", "-1"}} {
		if _, err := parse(t, base, bad...).StoreConfig(); err == nil || !strings.Contains(err.Error(), "want a positive number") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := parse(t, base, "--part-size", "1000").Store(); err == nil {
		t.Error("a part size below the minimum was accepted")
	}
}

func TestPublicBase(t *testing.T) {
	for _, tc := range []struct{ url, prefix, want string }{
		{"", "p", ""},
		{"https://pub/", "", "https://pub"},
		{"https://pub", "/ch-deploy-test/run/", "https://pub/ch-deploy-test/run"},
	} {
		if got := parse(t, map[string]string{EnvPublicURL: tc.url, EnvPrefix: tc.prefix}).PublicBase(); got != tc.want {
			t.Errorf("PublicBase(%q, %q) = %q, want %q", tc.url, tc.prefix, got, tc.want)
		}
	}
}

func TestSignerSources(t *testing.T) {
	file, fpr := os.Getenv("CH_DEPLOY_TEST_SIGNING_KEY_FILE"), os.Getenv("CH_DEPLOY_TEST_SIGN_WITH")
	if file == "" || fpr == "" {
		t.Skip("CH_DEPLOY_TEST_SIGNING_KEY_FILE and CH_DEPLOY_TEST_SIGN_WITH are not set")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"file flag":   {EnvSignWith: fpr},
		"file env":    {EnvSignWith: fpr, EnvSigningKeyFile: file},
		"content env": {EnvSignWith: fpr, EnvSigningKey: string(data)},
	} {
		t.Run(name, func(t *testing.T) {
			var args []string
			if name == "file flag" {
				args = []string{"--signing-key-file", file}
			}
			s, err := parse(t, env, args...).Signer()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(s.Fingerprint(), strings.ReplaceAll(fpr, " ", "")) {
				t.Fatalf("signs with %s, want %s", s.Fingerprint(), fpr)
			}
		})
	}
}

func TestSignerErrors(t *testing.T) {
	if _, err := parse(t, nil).Signer(); err == nil || !strings.Contains(err.Error(), EnvSignWith) {
		t.Fatalf("missing fingerprint: %v", err)
	}
	if _, err := parse(t, map[string]string{EnvSignWith: strings.Repeat("A", 40)}).Signer(); err == nil {
		t.Fatal("missing key succeeded")
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := parse(t, map[string]string{EnvSignWith: strings.Repeat("A", 40)}, "--signing-key-file", missing).Signer(); err == nil {
		t.Fatal("missing key file succeeded")
	}
}
