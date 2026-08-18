BINARY   := logstat
PKG      := ./cmd/logstat
DIST     := dist

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# nfpm wants a version without the leading "v".
PKGVER   := $(patsubst v%,%,$(VERSION))

LDFLAGS  := -s -w \
            -X main.version=$(VERSION) \
            -X main.commit=$(COMMIT) \
            -X main.date=$(DATE)

GO       ?= go
ARCHES   ?= amd64 arm64
GOBIN    := $(shell $(GO) env GOPATH)/bin

export CGO_ENABLED=0

.PHONY: all build build-all test test-short cover lint fmt vet package clean tools help

all: lint test build

## build: static binary for the host architecture
build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY) $(PKG)

## build-all: static linux binaries for every architecture in ARCHES
build-all:
	@for arch in $(ARCHES); do \
		echo "==> linux/$$arch"; \
		GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o $(DIST)/$(BINARY)_linux_$$arch/$(BINARY) $(PKG) || exit 1; \
	done

## test: full test suite with the race detector
test:
	$(GO) test -race -timeout 10m ./...

## test-short: skip the slow integration tests
test-short:
	$(GO) test -short -timeout 5m ./...

## cover: test suite with a coverage profile and a per-function summary
cover:
	$(GO) test -race -timeout 10m -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

## lint: gofmt, go vet and golangci-lint
lint: fmt vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m; \
	elif [ -x "$(GOBIN)/golangci-lint" ]; then \
		"$(GOBIN)/golangci-lint" run --timeout=5m; \
	else \
		echo "golangci-lint not installed, run 'make tools'"; exit 1; \
	fi

## fmt: fail if any file is not gofmt-clean
fmt:
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

## vet: go vet
vet:
	$(GO) vet ./...

## package: .deb and .rpm for every architecture in ARCHES (needs nfpm)
package: build-all
	@command -v nfpm >/dev/null 2>&1 || { echo "nfpm not installed, run 'make tools'"; exit 1; }
	@mkdir -p $(DIST)/pkgroot
	@for arch in $(ARCHES); do \
		cp $(DIST)/$(BINARY)_linux_$$arch/$(BINARY) $(DIST)/pkgroot/$(BINARY); \
		for format in deb rpm; do \
			echo "==> $$format linux/$$arch"; \
			VERSION=$(PKGVER) ARCH=$$arch nfpm package \
				--config nfpm.yaml --packager $$format --target $(DIST)/ || exit 1; \
		done; \
	done
	@rm -rf $(DIST)/pkgroot
	@ls -1 $(DIST)/*.deb $(DIST)/*.rpm

## tools: install the build-time helpers into GOPATH/bin
tools:
	$(GO) install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

## clean: remove build output
clean:
	rm -rf $(DIST) coverage.out

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
