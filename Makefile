# Makefile for hephaestus
#
# Tool versions here are the ones CI uses; TestMakefile_ToolVersionsMatchCI
# keeps them equal. Releases are published only by the release workflow on a
# tag push; `make publish vX.Y.Z` checks, creates and pushes that tag and
# follows the workflow (see docs/releasing.md).

BUILD_DIR = $(CURDIR)/build
BINARY_NAME = hephaestus
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS = -s -w -X "main.version=$(VERSION)"
GO := CGO_ENABLED=0 go

GOLANGCI_LINT_VERSION = v2.15.0
GOVULNCHECK_VERSION = v1.9.0
FUZZTIME = 10s

.PHONY: all
all: help

# Build the binary into ./build
.PHONY: build
build: clean
	@mkdir -p $(BUILD_DIR)
	@$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY_NAME) .

# Install with go install
.PHONY: install
install:
	@$(GO) install -trimpath -ldflags '$(LDFLAGS)' .

# Run unit and integration tests
.PHONY: test
test:
	@$(GO) test -count=1 ./...

# Run the tests with no network (Linux: network namespace, needs sudo)
.PHONY: test-offline
test-offline:
	@scripts/test-offline.sh

# Run tests verbosely with a clean test cache
.PHONY: test-verbose
test-verbose:
	@go clean -testcache
	@$(GO) test -count=1 -v ./...

# Run tests with the race detector (needs cgo)
.PHONY: race
race:
	@CGO_ENABLED=1 go test -race -count=1 ./...

# Run go vet for the host and for Windows
.PHONY: vet
vet:
	@$(GO) vet ./...
	@GOOS=windows $(GO) vet ./...

# Check formatting
.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "run gofmt -w"; exit 1)

# Run golangci-lint as CI does
.PHONY: golangci-lint
golangci-lint:
	@go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

# Scan dependencies and the standard library for known vulnerabilities
.PHONY: vuln
vuln:
	@go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Run all local quality gates
.PHONY: lint
lint: fmt-check vet golangci-lint test race vuln

# Run every fuzz target briefly (FUZZTIME=30s make fuzz for longer)
.PHONY: fuzz
fuzz:
	@scripts/fuzz.sh $(FUZZTIME)

# Real Homebrew end-to-end test (installs into the local Homebrew prefix,
# removes everything on exit; intended for disposable machines)
.PHONY: e2e-homebrew
e2e-homebrew:
	@scripts/homebrew-e2e.sh

# Build a local snapshot release of hephaestus itself with hephaestus
.PHONY: snapshot
snapshot: build
	@$(BUILD_DIR)/$(BINARY_NAME) run --snapshot --fresh --stages build,archive,checksum
	@scripts/verify-dist.sh dist

# Remove build artifacts
.PHONY: clean
clean:
	@rm -rf $(BUILD_DIR)

# Install development tools
.PHONY: dev-tools
dev-tools:
	go install "github.com/magicdrive/goreg@latest"

# Order imports (std, third party, github.com/magicdrive, local)
.PHONY: goreg
goreg:
	git ls-files '*.go' | grep -v '/testdata/' | xargs -I GOFILE goreg -w GOFILE

# Release Hephaestus (docs/releasing.md):
#   make publish v1.2.3          (or VERSION=v1.2.3, or tag=v1.2.3)
#   make publish-check v1.2.3    preflight only
#   make publish-status v1.2.3   what exists for a version
#   make publish-watch v1.2.3    follow an already pushed tag
# The version given as a goal is a no-op goal (.DEFAULT). It reaches the publish
# tool through the environment, never through a shell command line, and the
# tool validates it. YES=1 on the command line skips the prompt.
PUBLISH = go run ./internal/cmd/publish
PUBLISH_TARGETS = publish publish-check publish-status publish-watch publish-help

ifneq ($(filter $(PUBLISH_TARGETS),$(MAKECMDGOALS)),)
PUBLISH_GOAL := $(firstword $(MAKECMDGOALS))
PUBLISH_ARGS := $(wordlist 2,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS))
ifeq ($(filter $(PUBLISH_TARGETS),$(PUBLISH_GOAL)),)
$(error publish targets come first and alone: make publish v1.2.3)
endif
ifneq ($(filter $(PUBLISH_TARGETS),$(PUBLISH_ARGS)),)
$(error give one publish target at a time)
endif
ifneq ($(word 2,$(PUBLISH_ARGS)),)
$(error one version is expected, got: $(PUBLISH_ARGS))
endif
ifeq ($(PUBLISH_GOAL),publish-help)
ifneq ($(PUBLISH_ARGS),)
$(error publish-help takes no version)
endif
endif
export HEPHAESTUS_PUBLISH_ARGS := $(PUBLISH_ARGS)
export HEPHAESTUS_PUBLISH_VERSION := $(if $(filter command line,$(origin VERSION)),$(VERSION))
export HEPHAESTUS_PUBLISH_TAG := $(if $(filter command line,$(origin tag)),$(tag))
PUBLISH_YES := $(if $(and $(filter command line,$(origin YES)),$(filter 1,$(YES))),-yes)
ifneq ($(PUBLISH_ARGS),)
ifeq ($(filter v0% v1% v2% v3% v4% v5% v6% v7% v8% v9% 0% 1% 2% 3% 4% 5% 6% 7% 8% 9%,$(PUBLISH_ARGS)),)
$(error "$(PUBLISH_ARGS)" is not a version: make $(PUBLISH_GOAL) v1.2.3)
endif
# The version goal has no rule of its own: it is the publish target's
# argument. (No other target starts with v<digit>, so none is shadowed.)
.DEFAULT:
	@:
endif
endif

# Check, tag, push and follow a release
.PHONY: publish
publish:
	@$(PUBLISH) publish -from-make $(PUBLISH_YES)

# Release preflight only (no tag, no push, no GitHub writes)
.PHONY: publish-check
publish-check:
	@$(PUBLISH) check -from-make

# Show what exists for a version (read-only)
.PHONY: publish-status
publish-status:
	@$(PUBLISH) status -from-make

# Follow the release run of an already pushed tag and verify the release
.PHONY: publish-watch
publish-watch:
	@$(PUBLISH) watch -from-make

# Show how releasing works and its safety rules
.PHONY: publish-help
publish-help:
	@$(PUBLISH) help

.PHONY: help
help:
	@echo "Makefile commands:"
	@echo "  make build          - Build ./build/hephaestus"
	@echo "  make install        - go install with version information"
	@echo "  make test           - Run tests"
	@echo "  make test-verbose   - Run tests verbosely with a clean cache"
	@echo "  make race           - Run tests with the race detector"
	@echo "  make vet            - Run go vet (host and windows)"
	@echo "  make fmt-check      - Check gofmt"
	@echo "  make golangci-lint  - Run golangci-lint $(GOLANGCI_LINT_VERSION)"
	@echo "  make vuln           - Run govulncheck $(GOVULNCHECK_VERSION)"
	@echo "  make lint           - Run all local quality gates"
	@echo "  make fuzz           - Run every fuzz target (FUZZTIME=$(FUZZTIME))"
	@echo "  make e2e-homebrew   - Real Homebrew end-to-end test"
	@echo "  make snapshot       - Snapshot release of hephaestus with hephaestus"
	@echo "  make clean          - Remove build artifacts"
	@echo "  make dev-tools      - Install development tools"
	@echo "  make goreg          - Order imports with goreg"
	@echo "  make publish vX.Y.Z        - Release: preflight, confirm, tag, push, follow, verify"
	@echo "  make publish-check vX.Y.Z  - Release preflight only"
	@echo "  make publish-status vX.Y.Z - Show the release state of a version"
	@echo "  make publish-watch vX.Y.Z  - Follow an already pushed release tag"
	@echo "  make publish-help          - How releasing works"
	@echo "  make help           - Show this message"
