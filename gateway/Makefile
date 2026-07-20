# Farcaster gateway — build & quality harness.
#
# `make check` is THE contract for this module. It is what every agent runs
# before opening a PR and what CI runs as the merge gate (context check-gateway).
# Keep this target and the CI workflow in lockstep.

GO         ?= go
BINARY     ?= farcaster
CMD        ?= ./cmd/farcaster
VERSION    ?= dev
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS    := -X github.com/zerkerlabs/farcaster/gateway/internal/version.Version=$(VERSION) \
              -X github.com/zerkerlabs/farcaster/gateway/internal/version.Commit=$(COMMIT)

.DEFAULT_GOAL := check

.PHONY: check
check: tidy-check fmt-check vet lint test ## Run the full quality gate (build is implied by vet/test)

.PHONY: build
build: ## Build the gateway binary into ./bin
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

.PHONY: run
run: ## Run the gateway locally
	$(GO) run -ldflags "$(LDFLAGS)" $(CMD)

.PHONY: dev-auth
dev-auth: ## Run the gateway locally with auth against a dev mock OIDC issuer
	./scripts/dev-auth.sh

.PHONY: integration-test
integration-test: ## Run integration tests (needs TEST_DATABASE_URL + a Postgres)
	# -p 1: the integration suites share one DB and TRUNCATE shared tables per
	# test, so packages must run serially (see ci.yml for the full rationale).
	$(GO) test -tags=integration -race -count=1 -p 1 ./...

.PHONY: test
test: ## Run tests with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format the codebase with gofumpt
	gofumpt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofumpt-formatted
	@out="$$(gofumpt -l .)"; \
	if [ -n "$$out" ]; then echo "not gofumpt-formatted:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## Sync go.mod / go.sum
	$(GO) mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod / go.sum are not tidy
	$(GO) mod tidy
	@git diff --exit-code -- go.mod go.sum || \
		{ echo "go.mod/go.sum not tidy; run 'make tidy' and commit"; exit 1; }

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
