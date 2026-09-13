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

# Cross-compiled binaries. FORCE hands the up-to-date check to the Go
# build cache instead of make.
.PHONY: FORCE
FORCE:

dist/mellomting-linux-%: FORCE
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$* $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/mellomting

dist/mellomting-darwin-arm64: FORCE
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/mellomting

.PHONY: release
release: dist/mellomting-linux-amd64 dist/mellomting-linux-arm64 dist/mellomting-darwin-arm64 ## cross-compile release artifacts into dist/ (linux/amd64, linux/arm64, darwin/arm64)
	@if command -v sha256sum > /dev/null; then \
		(cd dist && sha256sum mellomting-linux-amd64 mellomting-linux-arm64 mellomting-darwin-arm64 > SHA256SUMS); \
	else \
		(cd dist && shasum -a 256 mellomting-linux-amd64 mellomting-linux-arm64 mellomting-darwin-arm64 > SHA256SUMS); \
	fi

# Debian packages (deploy/nfpm.yaml). nfpm runs through `go run`, so Go
# is the only build dependency and the packages build on macOS too.
#
# DEB_VERSION maps `git describe` output onto dpkg ordering: a leading v
# is dropped, a post-tag suffix -3-g8bb19d1 becomes +3.g8bb19d1 (after
# the tag), and any other hyphenated suffix such as -rc1 becomes ~rc1
# (before the tag).
NFPM_VERSION ?= v2.47.0
NFPM := $(GO) run github.com/goreleaser/nfpm/v2/cmd/nfpm@$(NFPM_VERSION)
DEB_VERSION := $(shell echo '$(VERSION)' | sed -E -e 's/^v//' -e 's/-dirty$$/.dirty/' -e 's/-([0-9]+)-g([0-9a-f]+)/+\1.g\2/' -e 's/-/~/g')
DEB_ARCHES := amd64 arm64

# The package installs the binary in /usr/bin (Debian policy forbids
# /usr/local), so the unit's ExecStart is rewritten; the grep fails the
# build if the line in deploy/ changed shape and the rewrite missed.
dist/deb/mellomting.service: deploy/mellomting.service
	mkdir -p dist/deb
	sed 's#^ExecStart=/usr/local/bin/mellomting #ExecStart=/usr/bin/mellomting #' $< > $@
	@grep -q '^ExecStart=/usr/bin/mellomting ' $@ || { echo "ExecStart rewrite failed in $@"; exit 1; }

.PHONY: deb
deb: $(DEB_ARCHES:%=dist/mellomting-linux-%) dist/deb/mellomting.service ## build dist/mellomting_<version>_<arch>.deb for linux/amd64 and linux/arm64
	@for arch in $(DEB_ARCHES); do \
		cp dist/mellomting-linux-$$arch dist/deb/mellomting && \
		ARCH=$$arch DEB_VERSION=$(DEB_VERSION) $(NFPM) package --config deploy/nfpm.yaml --packager deb --target dist/ || exit 1; \
	done

.PHONY: test
test:
	$(GO) test ./...
	$(GO) test -race ./...

.PHONY: check
check: ## full quality gate (AGENTS.md)
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }
	$(GO) build ./...
	$(GO) vet ./...
	# The journeys sit behind //go:build integration, so `go vet ./...`
	# never compiles them and a stale CLI invocation in one is invisible
	# until the CI integration job runs it on Linux.
	$(GO) vet -tags integration ./integration/
	$(GO) test ./...
	$(GO) test -race ./...
	# staticcheck/govulncheck are run on the primary linux build when
	# installed (AGENTS.md); CI installs both. SA4023 under GOOS=darwin
	# is a known non-issue (landlock.Apply must fail closed off-Linux,
	# see AGENTS.md); scoping to GOOS=linux keeps the gate free of it.
	@if command -v staticcheck >/dev/null 2>&1; then GOOS=linux staticcheck ./...; else echo "staticcheck not installed; skipping (AGENTS.md)"; fi
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck ./...; else echo "govulncheck not installed; skipping (AGENTS.md)"; fi

.PHONY: clean
clean:
	rm -rf bin dist
