# Module quality gate (ADR-0010). `check` is the contract; CI runs it per module.
GO ?= go
.DEFAULT_GOAL := check

.PHONY: check tidy-check fmt-check vet lint test build fmt tidy integration-test e2e-test
check: tidy-check fmt-check vet lint test build ## Full per-module gate

integration-test: ## Run integration tests (needs TEST_DATABASE_URL + a Postgres — the facilitator's own DB, spec 0007 Decision 9)
	# -p 1: the integration suites share one DB and TRUNCATE shared tables per
	# test, so packages must run serially.
	$(GO) test -tags=integration -race -count=1 -p 1 ./...

e2e-test: ## Run the flag-gated Base Sepolia settle e2e (spec 0007 T8; needs the FACILITATOR_E2E_* env — a funded gas wallet + Sepolia RPC + a USDC-holding payer)
	$(GO) test -tags=e2e -count=1 -run TestSepolia ./internal/e2e/...

tidy-check:
	$(GO) mod tidy
	@git diff --exit-code -- go.mod go.sum || { echo "go.mod/go.sum not tidy; run 'make tidy'"; exit 1; }

fmt-check:
	@out="$$(gofumpt -l .)"; if [ -n "$$out" ]; then echo "not gofumpt-formatted:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

test:
	$(GO) test -race -count=1 ./...

build:
	$(GO) build ./...

fmt:
	gofumpt -w .

tidy:
	$(GO) mod tidy
