// Package config holds the fixed repository structure and the launch parameters.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/sign"
	"github.com/ClickHouse/clickhouse-repos-manager/internal/store"
)

// The repository structure is the same as reprepro's distributions for packages.clickhouse.com.
const (
	Origin    = "ClickHouse"
	Label     = "ClickHouse"
	Component = "main"

	// StatePrefix holds the lock and the snapshots; configs/ is not served publicly.
	StatePrefix = "configs/ch-deploy/"
	// SnapshotsPrefix is the only place where the tool deletes, besides pruned by-hash indexes.
	SnapshotsPrefix = StatePrefix + "runs/"
	// SnapshotRetention is the number of release snapshots, and of by-hash generations, kept.
	SnapshotRetention = 30
)

var (
	Codenames     = []string{"lts", "stable"}
	Architectures = []string{"amd64", "arm64"}
)

// Environment variables. Secrets are read only from the environment or a file, never from a flag value.
const (
	EnvEndpoint       = "CH_DEPLOY_ENDPOINT"
	EnvBucket         = "CH_DEPLOY_BUCKET"
	EnvPrefix         = "CH_DEPLOY_PREFIX"
	EnvPublicURL      = "CH_DEPLOY_PUBLIC_URL"
	EnvSigningKey     = "CH_DEPLOY_SIGNING_KEY"
	EnvSigningKeyFile = "CH_DEPLOY_SIGNING_KEY_FILE"
	EnvPassphrase     = "CH_DEPLOY_SIGNING_PASSPHRASE"
	EnvSignWith       = "CH_DEPLOY_SIGN_WITH"
	EnvPartSize       = "CH_DEPLOY_PART_SIZE"
	EnvConcurrency    = "CH_DEPLOY_UPLOAD_CONCURRENCY"
	EnvAccessKeyID    = "AWS_ACCESS_KEY_ID"
	EnvSecretKey      = "AWS_SECRET_ACCESS_KEY"
)

// Launch holds the parameters that depend on the environment.
type Launch struct {
	Endpoint       string
	Bucket         string
	Prefix         string
	PublicURL      string
	SigningKeyFile string
	SignWith       string
	// PartSize and UploadConcurrency are numbers; empty means the default.
	PartSize          string
	UploadConcurrency string

	getenv func(string) string
}

// Register adds the launch flags to fs. Every flag falls back to its environment variable.
func (l *Launch) Register(fs *flag.FlagSet) {
	fs.StringVar(&l.Endpoint, "endpoint", "", "S3 endpoint URL (env "+EnvEndpoint+")")
	fs.StringVar(&l.Bucket, "bucket", "", "bucket name (env "+EnvBucket+")")
	fs.StringVar(&l.Prefix, "prefix", "", "key prefix inside the bucket (env "+EnvPrefix+")")
	fs.StringVar(&l.PublicURL, "public-url", "", "public base URL of the bucket (env "+EnvPublicURL+")")
	fs.StringVar(&l.SigningKeyFile, "signing-key-file", "", "file with the secret key export (env "+EnvSigningKeyFile+", or the key itself in "+EnvSigningKey+")")
	fs.StringVar(&l.SignWith, "sign-with", "", "fingerprint of the signing key (env "+EnvSignWith+")")
	fs.StringVar(&l.PartSize, "part-size", "", fmt.Sprintf("multipart part size in bytes, at least %d (env %s, default %d)", store.MinPartSize, EnvPartSize, store.DefaultPartSize))
	fs.StringVar(&l.UploadConcurrency, "upload-concurrency", "", fmt.Sprintf("requests that send data at once (env %s, default %d)", EnvConcurrency, store.DefaultUploadConcurrency))
}

// Resolve fills unset flags from the environment. Call it after parsing.
func (l *Launch) Resolve() {
	getenv := l.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, f := range []struct {
		v   *string
		env string
	}{
		{&l.Endpoint, EnvEndpoint},
		{&l.Bucket, EnvBucket},
		{&l.Prefix, EnvPrefix},
		{&l.PublicURL, EnvPublicURL},
		{&l.SigningKeyFile, EnvSigningKeyFile},
		{&l.SignWith, EnvSignWith},
		{&l.PartSize, EnvPartSize},
		{&l.UploadConcurrency, EnvConcurrency},
	} {
		if *f.v == "" {
			*f.v = getenv(f.env)
		}
	}
	l.PublicURL = strings.TrimSuffix(l.PublicURL, "/")
}

// HasStore reports whether bucket access is configured.
func (l *Launch) HasStore() bool {
	return l.Endpoint != "" || l.Bucket != ""
}

// HasSigner reports whether a signing key is configured.
func (l *Launch) HasSigner() bool {
	return l.SigningKeyFile != "" || l.env(EnvSigningKey) != "" || l.SignWith != ""
}

// Store connects to the bucket. Deletes are allowed only under the given prefixes.
func (l *Launch) Store(deletable ...string) (*store.Store, error) {
	cfg, err := l.StoreConfig(deletable...)
	if err != nil {
		return nil, err
	}
	return store.New(cfg)
}

// StoreConfig is the configuration Store connects with.
func (l *Launch) StoreConfig(deletable ...string) (store.Config, error) {
	cfg := store.Config{
		Endpoint:        l.Endpoint,
		Bucket:          l.Bucket,
		Prefix:          l.Prefix,
		AccessKeyID:     l.env(EnvAccessKeyID),
		SecretAccessKey: l.env(EnvSecretKey),
		Deletable:       deletable,
	}
	var err error
	if cfg.PartSize, err = number("part size", l.PartSize); err != nil {
		return store.Config{}, err
	}
	if cfg.UploadConcurrency, err = l.Concurrency(); err != nil {
		return store.Config{}, err
	}
	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, "--endpoint or "+EnvEndpoint)
	}
	if cfg.Bucket == "" {
		missing = append(missing, "--bucket or "+EnvBucket)
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		missing = append(missing, EnvAccessKeyID+" and "+EnvSecretKey)
	}
	if len(missing) > 0 {
		return store.Config{}, fmt.Errorf("bucket access needs %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// Concurrency is UploadConcurrency as a number, 0 for the default.
func (l *Launch) Concurrency() (int, error) {
	n, err := number("upload concurrency", l.UploadConcurrency)
	return int(n), err
}

// PublicBase is the public URL of the keys under Prefix, empty without PublicURL.
func (l *Launch) PublicBase() string {
	if l.PublicURL == "" {
		return ""
	}
	if p := strings.Trim(l.Prefix, "/"); p != "" {
		return l.PublicURL + "/" + p
	}
	return l.PublicURL
}

// number parses a positive decimal; empty is 0, the default. store.New checks the limits.
func number(what, s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s %q: want a positive number", what, s)
	}
	return n, nil
}

// Signer loads the signing key.
func (l *Launch) Signer() (*sign.Signer, error) {
	if l.SignWith == "" {
		return nil, errors.New("signing needs --sign-with or " + EnvSignWith)
	}
	var data []byte
	switch {
	case l.SigningKeyFile != "":
		var err error
		if data, err = os.ReadFile(l.SigningKeyFile); err != nil {
			return nil, fmt.Errorf("signing key: %w", err)
		}
	case l.env(EnvSigningKey) != "":
		data = []byte(l.env(EnvSigningKey))
	default:
		return nil, errors.New("signing needs --signing-key-file, " + EnvSigningKeyFile + " or " + EnvSigningKey)
	}
	return sign.Load(data, l.SignWith, []byte(l.env(EnvPassphrase)))
}

func (l *Launch) env(name string) string {
	if l.getenv != nil {
		return l.getenv(name)
	}
	return os.Getenv(name)
}
