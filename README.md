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
* `ch-deploy lock --tag <tag> --date <tag date> [--run-id <id>] [--run-url <url>]`:
  takes the global release lock, or resumes the release that holds it. The
  first lock on a bucket adopts the live repository state.
* `ch-deploy publish [--only-tgz] [--only-rpm] [--only-deb] <files...>`:
  publishes the packages of the release that holds the lock, or continues an
  interrupted publish of it. The tag and the version come from the lock. Every
  file name must carry that version, and the package types must have the same
  packages and architectures; `--only-*` narrows that check to the named
  types. Flags come before the files. With `--public-url` it also checks the
  packages, the index metadata and the DEB `by-hash` files at the public URL.
  This build knows DEB and TGZ. Each package comes for `amd64` and `arm64`, or
  for `all`.
* `ch-deploy publish --dry-run --tag <tag> --out <dir> [--only-*] <files...>`:
  builds the release on top of the live repository without a lock and without
  any write to the bucket, so the read only token is enough. It writes the
  metadata into `<dir>` at the live keys and prints the added packages and a
  diff. Without a signing key the DEB `Release` is built unsigned.
* `ch-deploy verify deb [--channel <codename>]`: verifies the signed chain of
  every codename, the `by-hash` files, and every pool file by size and, for
  single part objects, by MD5; without `--channel` every `.deb` in the pool
  must be referenced. With `--public-url` it also checks the metadata there.
  Any problem gives exit code 1.
* `ch-deploy lock status`: prints the lock and its ETag, and the last lines of
  the assets list of the release that holds it.
* `ch-deploy unlock --tag <tag>`: after the client tests passed, prunes old
  snapshots and the DEB `by-hash` files only they named, freezes the tag and
  frees the lock.
* `ch-deploy rollback --tag <tag>`: undoes the release that holds the lock:
  the metadata goes back to the previous snapshot, and every object the
  release created is deleted.
* `ch-deploy lock break --tag <tag> [--force]`: frees the lock without
  touching any file. DANGEROUS, break glass only.

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
| `--public-key-file` | `CH_DEPLOY_PUBLIC_KEY_FILE` | public keys that verify a DEB codename without `Public.key` |
| `--part-size` | `CH_DEPLOY_PART_SIZE` | multipart part size in bytes, default 64 MiB, at least 5 MiB |
| `--upload-concurrency` | `CH_DEPLOY_UPLOAD_CONCURRENCY` | requests that send data at once, and parallel checks, default 4 |

The key named by `--sign-with` signs exactly; there is no fallback to another
subkey or to the primary key. `--sign-with` only selects the key that signs;
it plays no part in verification. DEB metadata is verified with the keys in
`dists/<codename>/Public.key`. Where a codename has no `Public.key` yet
(production before the first release with this tool), `--public-key-file`
verifies it, for example with `rpm/lts/repodata/repomd.xml.key`; otherwise
the public part of the signing key does. A `Public.key` in the bucket always
wins. `verify` prints the key that signed each codename; a signature that
does not verify is a problem, and the other checks go on. `lock` and
`publish` stop on it.

## Development

```sh
make test        # unit tests; bucket and mock key tests skip without their variables
make test-local  # all tests, bucket tests against a fresh Miniflare container
make test-remote # all tests, bucket tests against a real bucket
make test-reprepro # the DEB repository against reprepro 5.5.1 and apt on Ubuntu 14.04 to 26.04
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

For `packages-dev` the script sets `CH_DEPLOY_TEST_PUBLIC_URL` to the public
URL of the bucket, which the `publish` test checks; without it a local server
stands in. Miniflare cannot list multipart uploads, so there failed uploads
are aborted instead of resumed, and the resume tests skip.

All test data lives below `ch-deploy-test/`. A run uses the prefix
`ch-deploy-test/<UTC time>-<random>/`, and every test a prefix of its own
inside it, deleted with its open multipart uploads when the test ends. After `go test` exits, even on a
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
per call and mode. `storetest.NewMem()` is an in-memory bucket for fast
protocol tests and the full fault matrix.

`make test-reprepro` needs Docker. It builds `test/reprepro/`, reprepro
5.5.1 from source as the release runners have it, and runs `TestReprepro`
(build tag `reprepro`). The same lts and stable releases of test packages go
through `ch-deploy` and through reprepro, signed with the same test key. The
test compares the pool, the index entries as field sets and the `Release`
files. Then apt in the native `ubuntu:14.04` to `ubuntu:26.04` images updates,
shows and installs from both repositories, and the results must match. The
only expected differences are what `ch-deploy` adds: `Acquire-By-Hash` with
the `by-hash` files, `Packages.xz` and `Public.key`.

A key exported by gpg can be checked with
`CH_DEPLOY_TEST_SIGNING_KEY_FILE` (base64 secret export) and
`CH_DEPLOY_TEST_SIGN_WITH`. Signatures are also verified with `gpgv` when it
is installed.
