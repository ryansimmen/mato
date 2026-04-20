.DEFAULT_GOAL := help

# The dev container can export a stale GOROOT that does not match the active
# Go toolchain binary. Let the Go tool infer its own root from the selected
# binary so build/test targets work consistently.
unexport GOROOT

.PHONY: all build clean deadcode fmt help install integration-test lint test test-race verify vet

BIN_DIR := bin
VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || printf dev)
GO_LDFLAGS := -X main.version=$(VERSION)

all: fmt vet build test ## Run formatting, vetting, build, and tests

build: ## Build mato binary
	mkdir -p $(BIN_DIR)
	go build -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/mato ./cmd/mato

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

fmt: ## Format Go source files
	go fmt ./...

install: ## Install mato binary to GOBIN
	go install -ldflags "$(GO_LDFLAGS)" ./cmd/mato

integration-test: ## Run integration tests with race detector
	go test -race -v ./internal/integration/...

test: ## Run tests with race detector
	go test -race ./...

test-race: ## Run tests with race detector and no cache
	go test -race -count=1 ./...

verify: ## Run full PR verification suite
	go build ./...
	go vet ./...
	go mod tidy -diff
	$(MAKE) lint
	$(MAKE) deadcode
	go test -count=1 ./...

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

deadcode: ## Run unused-code analyzers
	go tool staticcheck -checks U1000 ./...
	go tool deadcode -test ./...

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
