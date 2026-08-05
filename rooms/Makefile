# Module quality gate. `check` is the contract; CI runs it per module.
GO ?= go
BINARY ?= rooms
CMD ?= ./cmd/rooms
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/zerkerlabs/gateway/rooms/internal/version.Version=$(VERSION) \
           -X github.com/zerkerlabs/gateway/rooms/internal/version.Commit=$(COMMIT)

.DEFAULT_GOAL := check

.PHONY: check tidy-check fmt-check vet lint test build run fmt tidy
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
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

run: ## Run the rooms service locally
	$(GO) run -ldflags "$(LDFLAGS)" $(CMD)

fmt:
	gofumpt -w .

tidy:
	$(GO) mod tidy
