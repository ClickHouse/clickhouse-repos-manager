#!/usr/bin/env bash
# Build the reprepro 5.5.1 image and run TestReprepro: the same releases
# through ch-deploy and reprepro, then apt on every Ubuntu release against
# both. Arguments go to go test.
set -euo pipefail

cd "$(dirname "$0")/../.."
docker build -q -t ch-deploy-reprepro test/reprepro >/dev/null
export CH_DEPLOY_REPREPRO_IMAGE=ch-deploy-reprepro
go test -tags reprepro -count=1 -run TestReprepro "$@" ./internal/deb
