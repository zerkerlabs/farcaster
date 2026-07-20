# Farcaster monorepo — top-level build harness (ADR-0010).
#
# The repo is a Go workspace (go.work) of independent modules. `make check` fans
# the per-module gate out over every module; CI runs each module's `check` as its
# own required status context (check-gateway, check-facilitator, ...). The dev-only
# mock-oidc module is intentionally excluded from the gate.
#
# www/ (the Astro/Starlight site) is intentionally excluded too: it isn't a Go
# module, so it can't fan out via `make -C $m check` like the rest of
# GO_MODULES. Its own gate lives at www/Makefile and runs as the separate
# check-www CI context — `make check` at the repo root does not cover it.

GO_MODULES := gateway x402types facilitator sdk/go

.DEFAULT_GOAL := check

.PHONY: check
check: ## Run every module's quality gate
	@for m in $(GO_MODULES); do echo "== check $$m =="; $(MAKE) -C $$m check || exit 1; done

.PHONY: fmt
fmt: ## gofumpt every module
	@for m in $(GO_MODULES); do $(MAKE) -C $$m fmt; done

.PHONY: tidy
tidy: ## go mod tidy every module
	@for m in $(GO_MODULES); do $(MAKE) -C $$m tidy; done

.PHONY: test
test: ## Test every module
	@for m in $(GO_MODULES); do $(MAKE) -C $$m test; done

# Gateway passthroughs (the runnable service).
.PHONY: run dev-auth integration-test
run: ## Run the gateway locally
	$(MAKE) -C gateway run
dev-auth: ## Run the gateway locally against a dev mock OIDC issuer
	$(MAKE) -C gateway dev-auth
integration-test: ## Run the gateway integration tests
	$(MAKE) -C gateway integration-test

.PHONY: tools
tools: ## Install pinned dev tools shared across modules
	go install mvdan.cc/gofumpt@v0.10.0
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
