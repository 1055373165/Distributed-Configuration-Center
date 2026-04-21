# PaladinCore — developer Makefile.
# See docs/production-refactoring.md §2.3.H for the full toolchain rationale.

GO         ?= go
GOBIN      ?= $(shell $(GO) env GOPATH)/bin
PKGS       ?= ./...
LDFLAGS    ?= -s -w
BUILD_DIR  ?= bin

# Tool versions — bump together, pinned to reproducible state.
GOFUMPT_VER         := v0.7.0
STATICCHECK_VER     := 2024.1.1
GOLANGCI_LINT_VER   := v1.62.2
GOVULNCHECK_VER     := latest

.PHONY: help
help: ## Show available targets
	@awk 'BEGIN{FS=":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\n"} /^[a-zA-Z_-]+:.*?##/ {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ──────────────────────────────────────────────────────────────────────
# Build
# ──────────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build paladin-core and paladin-bench binaries
	@mkdir -p $(BUILD_DIR)
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/paladin-core ./cmd/paladin-core
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/paladin-bench ./cmd/paladin-bench

.PHONY: clean
clean: ## Remove build artifacts and data dirs
	rm -rf $(BUILD_DIR) data-node*/ .cluster-logs/ .cluster-pids

# ──────────────────────────────────────────────────────────────────────
# Test
# ──────────────────────────────────────────────────────────────────────

.PHONY: test
test: ## Run unit tests (no race)
	$(GO) test -count=1 $(PKGS)

.PHONY: test-race
test-race: ## Run unit tests with race detector — required green for merge
	$(GO) test -count=1 -race $(PKGS)

.PHONY: test-verbose
test-verbose: ## Run tests with -v
	$(GO) test -count=1 -v $(PKGS)

.PHONY: cover
cover: ## Produce coverage report at coverage.out / coverage.html
	$(GO) test -count=1 -race -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "→ open coverage.html"

# ──────────────────────────────────────────────────────────────────────
# Lint / quality gate (§2.3.H)
# ──────────────────────────────────────────────────────────────────────

.PHONY: lint
lint: lint-fmt lint-vet lint-staticcheck lint-golangci ## Run full lint suite

.PHONY: lint-fmt
lint-fmt: ## gofumpt check (fails if reformatting needed)
	@unformatted=$$($(GOBIN)/gofumpt -l -d .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofumpt diffs:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: lint-fmt-fix
lint-fmt-fix: ## gofumpt -w (rewrites files in-place)
	$(GOBIN)/gofumpt -w .

.PHONY: lint-vet
lint-vet: ## go vet
	$(GO) vet $(PKGS)

.PHONY: lint-staticcheck
lint-staticcheck: ## staticcheck
	$(GOBIN)/staticcheck $(PKGS)

.PHONY: lint-golangci
lint-golangci: ## golangci-lint aggregate run
	$(GOBIN)/golangci-lint run $(PKGS)

.PHONY: vuln
vuln: ## govulncheck — CVE scan against stdlib + deps
	$(GOBIN)/govulncheck $(PKGS)

.PHONY: lint-install
lint-install: ## Install pinned linter binaries into $GOBIN
	$(GO) install mvdan.cc/gofumpt@$(GOFUMPT_VER)
	$(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VER)
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VER)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER)
	@echo "→ linters installed to $(GOBIN)"

# ──────────────────────────────────────────────────────────────────────
# Bench
# ──────────────────────────────────────────────────────────────────────

.PHONY: bench-suite
bench-suite: ## Run the canonical benchmark sweep (see scripts/bench-suite.sh)
	./scripts/bench-suite.sh

.PHONY: bench-smoke
bench-smoke: build ## Quick 10s write_only smoke bench against a local cluster
	@echo "→ start local cluster (via scripts/cluster-local.sh) before running this"
	$(BUILD_DIR)/paladin-bench run --scenario write_only --duration 10s --concurrency 4

# ──────────────────────────────────────────────────────────────────────
# Cluster
# ──────────────────────────────────────────────────────────────────────

.PHONY: cluster-up
cluster-up: build ## Start 3-node local cluster
	./scripts/cluster-local.sh

.PHONY: cluster-down
cluster-down: ## Stop local cluster
	./scripts/cluster-stop.sh

# ──────────────────────────────────────────────────────────────────────
# Composite / CI
# ──────────────────────────────────────────────────────────────────────

.PHONY: ci
ci: lint test-race vuln ## What CI runs — must be green before merge

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy
