# Tool versions are pinned so local runs match CI.
GOLANGCI_LINT_VERSION ?= v2.15.0

# Override with a locally installed binary, e.g. `make lint GOLANGCI_LINT=golangci-lint`.
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOVULNCHECK   ?= go run golang.org/x/vuln/cmd/govulncheck@latest

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := check

.PHONY: check
check: vet test lint ## Run vet, tests, and lint (what CI runs, minus govulncheck)

.PHONY: build
build: ## Build the durable-mcp binary into ./bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/durable-mcp ./cmd/durable-mcp

.PHONY: test
test: ## Run all tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and write an HTML coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: vuln
vuln: ## Scan dependencies and the standard library for known vulnerabilities
	$(GOVULNCHECK) ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf bin coverage.out coverage.html

.PHONY: help
help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-8s %s\n", $$1, $$2}'
