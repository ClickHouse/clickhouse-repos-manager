# ch-deploy

Publishes ClickHouse release packages (DEB, RPM, TGZ) to
[packages.clickhouse.com](https://packages.clickhouse.com), which is served
from a Cloudflare R2 bucket.

It is a single static binary. It talks to R2 through the S3 API and signs
repository metadata in process, so the release runner needs no FUSE mount,
`reprepro`, `createrepo_c` or `gpg`.

## Status

Work in progress. Available commands:

* `ch-deploy version`
* `ch-deploy check`: checks bucket access and the signing key.

## Launch parameters

Every flag falls back to an environment variable. Secrets are read only from
the environment or from a file, never from a flag value.

| Flag | Environment | Meaning |
|---|---|---|
| `--endpoint` | `CH_DEPLOY_ENDPOINT` | S3 endpoint URL |
| `--bucket` | `CH_DEPLOY_BUCKET` | bucket name |
| `--prefix` | `CH_DEPLOY_PREFIX` | key prefix inside the bucket |
| `--public-url` | `CH_DEPLOY_PUBLIC_URL` | public base URL of the bucket |
| | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | bucket credentials |
| `--signing-key-file` | `CH_DEPLOY_SIGNING_KEY_FILE` | secret key export: armored, binary or base64 |
| | `CH_DEPLOY_SIGNING_KEY` | the secret key export itself |
| | `CH_DEPLOY_SIGNING_PASSPHRASE` | passphrase, if the key has one |
| `--sign-with` | `CH_DEPLOY_SIGN_WITH` | full fingerprint of the signing (sub)key |

The key named by `--sign-with` signs exactly; there is no fallback to another
subkey or to the primary key.

## Development

```sh
make test        # unit tests; bucket and mock key tests skip without their variables
make test-local  # all tests, bucket tests against a fresh Miniflare container
make test-remote # all tests, bucket tests against a real bucket
make dist        # static binaries for linux/amd64, linux/arm64, darwin/arm64 and SHA256SUMS
```

`make test-local` needs Docker. It builds `test/miniflare/` (a local R2
simulator with the S3 API), starts it on a random local port with an empty
in-memory bucket, runs the tests and removes the container. Pass `go test`
arguments with `ARGS`, for example
`make test-local ARGS='-run TestPutIfMatch ./internal/store'`.

`make test-remote` runs the same tests against a real bucket, `packages-dev`
unless `CH_DEPLOY_TEST_BUCKET` says otherwise:

```sh
CH_DEPLOY_TEST_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
make test-remote
```

All test data lives below `ch-deploy-test/`. A run uses the prefix
`ch-deploy-test/<UTC time>-<random>/`, and every test a prefix of its own
inside it, deleted when the test ends. After `go test` exits, even on a
panic or Ctrl+C, the script deletes the run prefix and any run prefix older
than 24 hours. The same cleanup works by hand:
`go run ./test/cleanup [-stale 24h] [prefix...]`. It refuses prefixes outside
`ch-deploy-test/`.

Tests use `internal/storetest`: `storetest.New(t)` returns an isolated store
and skips the test when `CH_DEPLOY_TEST_ENDPOINT` or `CH_DEPLOY_TEST_BUCKET`
is unset. `test/miniflare/run.sh` sets `CH_DEPLOY_TEST_BACKEND=miniflare`, and
`storetest.SkipOnMiniflare` skips tests of R2 behaviour that Miniflare does
not reproduce. `storetest.Faults` wraps a store and fails one mutating call:
before it is sent, after it was applied (the response is lost), or by sending
it twice as an SDK retry does; `storetest.ForEachFault` runs a scenario once
per call and mode.

A key exported by gpg can be checked with
`CH_DEPLOY_TEST_SIGNING_KEY_FILE` (base64 secret export) and
`CH_DEPLOY_TEST_SIGN_WITH`. Signatures are also verified with `gpgv` when it
is installed.
