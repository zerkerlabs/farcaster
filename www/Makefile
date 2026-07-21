# Farcaster website (www/) — build & quality harness.
#
# `make check` is THE contract for this module: lint (astro check) + build +
# broken-internal-link check. Mirrors the per-module pattern in
# gateway/Makefile and facilitator/Makefile; CI runs this exact target as the
# check-www context — the per-module pattern, extended to the repo's only Node
# module.

NPM ?= npm

.DEFAULT_GOAL := check

.PHONY: check
check: install lint build check-links ## Run the full quality gate (lint + build + link-check)

.PHONY: install
install: ## Install dependencies (from package-lock.json, exactly)
	$(NPM) ci

.PHONY: lint
lint: ## Type/content-check the site with astro check
	$(NPM) run lint

.PHONY: build
build: ## Build the static site into ./dist
	$(NPM) run build

.PHONY: check-links
check-links: ## Fail on broken internal links in the built output
	$(NPM) run check-links

.PHONY: audit
audit: ## Fail on high-severity dependency advisories (PR checklist; not in `check` — it hits the network)
	$(NPM) audit --audit-level=high

.PHONY: dev
dev: ## Run the site locally
	$(NPM) run dev

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
