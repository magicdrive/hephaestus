# Makefile for hephaestus
#
# Tool versions here are the ones CI uses; TestMakefile_ToolVersionsMatchCI
# keeps them equal. Releases are cut by pushing a tag (see docs/releasing.md),
# never from this Makefile.

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
	@echo "  make help           - Show this message"
