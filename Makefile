VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

.PHONY: build dist test test-local test-remote test-reprepro vet fmt check-fmt clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ch-deploy ./cmd/ch-deploy

# Static binaries plus SHA256SUMS, as published in releases.
dist:
	rm -rf dist && mkdir -p dist
	for p in $(PLATFORMS); do \
		CGO_ENABLED=0 GOOS=$${p%/*} GOARCH=$${p#*/} go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/ch-deploy-$${p%/*}-$${p#*/} ./cmd/ch-deploy || exit 1; \
	done
	cd dist && shasum -a 256 ch-deploy-* > SHA256SUMS

test:
	go test ./...

# Bucket tests against a fresh Miniflare container (needs Docker).
test-local:
	test/miniflare/run.sh $(ARGS)

# Bucket tests against CH_DEPLOY_TEST_ENDPOINT (bucket packages-dev by default)
# under a random prefix, deleted afterwards.
test-remote:
	test/remote.sh $(ARGS)

# The DEB repository against reprepro 5.5.1, read by apt on Ubuntu 14.04 to
# 26.04 (needs Docker).
test-reprepro:
	test/reprepro/run.sh $(ARGS)

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal test

check-fmt:
	@test -z "$$(gofmt -l cmd internal test)" || { gofmt -l cmd internal test; exit 1; }

clean:
	rm -rf bin dist
