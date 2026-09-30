VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

.PHONY: build dist test vet fmt check-fmt clean

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

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

check-fmt:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }

clean:
	rm -rf bin dist
