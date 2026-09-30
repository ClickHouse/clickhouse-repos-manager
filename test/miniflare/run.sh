#!/usr/bin/env bash
# Run go test against a fresh Miniflare container. Arguments go to go test.
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$dir/../.." && pwd)

access_key=$(printf 'A%.0s' {1..32})
secret_key=test-secret-key
bucket=packages-dev

# The layer cache makes this a no-op when nothing changed; the ID pins the image just built.
image=$(docker build -q -t ch-deploy-miniflare "$dir")

container=$(docker run -d --rm -p 127.0.0.1::8787 \
	-e MF_BUCKET="$bucket" -e MF_ACCESS_KEY_ID="$access_key" -e MF_SECRET_ACCESS_KEY="$secret_key" \
	"$image")
trap 'docker rm -f "$container" >/dev/null 2>&1 || true' EXIT

for _ in $(seq 60); do
	docker logs "$container" 2>&1 | grep -q '^ready ' && break
	if ! docker inspect "$container" >/dev/null 2>&1; then
		echo "miniflare container exited" >&2
		exit 1
	fi
	sleep 0.5
done
if ! docker logs "$container" 2>&1 | grep -q '^ready '; then
	docker logs "$container" >&2
	echo "miniflare did not become ready" >&2
	exit 1
fi

port=$(docker port "$container" 8787/tcp | head -n1 | sed 's/.*://')

cd "$root"
[ $# -gt 0 ] || set -- -race ./...
CH_DEPLOY_TEST_ENDPOINT="http://127.0.0.1:$port/cdn-cgi/local/r2/s3" \
	CH_DEPLOY_TEST_BUCKET="$bucket" \
	AWS_ACCESS_KEY_ID="$access_key" \
	AWS_SECRET_ACCESS_KEY="$secret_key" \
	go test "$@"
