#!/usr/bin/env bash
# Run go test against a real bucket under a fresh run prefix, then delete it
# and run prefixes older than 24 hours. Arguments go to go test.
set -euo pipefail

: "${CH_DEPLOY_TEST_ENDPOINT:?is not set}"
: "${AWS_ACCESS_KEY_ID:?is not set}"
: "${AWS_SECRET_ACCESS_KEY:?is not set}"
export CH_DEPLOY_TEST_ENDPOINT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
export CH_DEPLOY_TEST_BUCKET=${CH_DEPLOY_TEST_BUCKET:-packages-dev}
# The public URL check of publish reads this; other buckets get a local stand in.
if [ "$CH_DEPLOY_TEST_BUCKET" = packages-dev ]; then
	export CH_DEPLOY_TEST_PUBLIC_URL=${CH_DEPLOY_TEST_PUBLIC_URL:-https://pub-fbb7005823dc4b079b49d72501aa08db.r2.dev}
fi

cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
cleanup=$tmp/cleanup
go build -o "$cleanup" ./test/cleanup
CH_DEPLOY_TEST_PREFIX=$("$cleanup" -new-prefix)
export CH_DEPLOY_TEST_PREFIX

# A failed cleanup fails the run even when the tests passed.
finish() {
	local status=$?
	"$cleanup" -stale 24h "$CH_DEPLOY_TEST_PREFIX" || [ "$status" -ne 0 ] || status=1
	rm -rf "$tmp"
	exit "$status"
}
trap finish EXIT

echo "bucket $CH_DEPLOY_TEST_BUCKET, prefix $CH_DEPLOY_TEST_PREFIX"
# The release tests build their steady state with real releases, each test in
# a prefix of its own; 8 at once take about 3 minutes on R2 without hitting its
# request rate limit.
[ $# -gt 0 ] || set -- -race -timeout 30m -parallel 8 ./...
go test "$@"
