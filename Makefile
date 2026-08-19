GO ?= go

# Build metadata (PLAN §89): version, git commit, build date, Go version.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

# The Go version is reported via runtime.Version() at runtime.
LDFLAGS := \
	-X 'mellomting/internal/version.Version=$(VERSION)' \
	-X 'mellomting/internal/version.Commit=$(COMMIT)' \
	-X 'mellomting/internal/version.Date=$(DATE)'

.PHONY: build
build: ## build bin/mellomting with build metadata embedded
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/mellomting ./cmd/mellomting

.PHONY: release
release: ## cross-compile the PLAN §89 linux release artifacts
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mellomting-linux-amd64 ./cmd/mellomting
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mellomting-linux-arm64 ./cmd/mellomting
	@if command -v sha256sum > /dev/null; then \
		(cd dist && sha256sum mellomting-linux-amd64 mellomting-linux-arm64 > SHA256SUMS); \
	else \
		(cd dist && shasum -a 256 mellomting-linux-amd64 mellomting-linux-arm64 > SHA256SUMS); \
	fi

.PHONY: test
test:
	$(GO) test ./...
	$(GO) test -race ./...

.PHONY: check
check: ## full quality gate (AGENTS.md)
	gofmt -l .
	$(GO) build ./...
	$(GO) vet ./...
	$(GO) test ./...
	$(GO) test -race ./...

.PHONY: clean
clean:
	rm -rf bin dist
