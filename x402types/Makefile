# Module quality gate (ADR-0010). `check` is the contract; CI runs it per module.
GO ?= go
.DEFAULT_GOAL := check

.PHONY: check tidy-check fmt-check vet lint test build fmt tidy
check: tidy-check fmt-check vet lint test build ## Full per-module gate

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
