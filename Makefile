.DEFAULT_GOAL := help

-include .env

.PHONY: all build clean fmt help install integration-test lint run test vet

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

install: ## Install mato binary to GOBIN and mato skill to ~/.copilot/skills/
	go install -ldflags "$(GO_LDFLAGS)" ./cmd/mato
	./scripts/install-skill.sh

integration-test: ## Run integration tests with race detector
	go test -race -v ./internal/integration/...

run: ## Run agent in Docker (use COPILOT_ARGS to pass args to copilot, e.g. COPILOT_ARGS="--model gpt-5.4")
	@if [ -z "$(REPO)" ]; then echo "REPO is required. Set REPO=<path> in .env or pass it on the command line."; exit 1; fi
	go run -ldflags "$(GO_LDFLAGS)" ./cmd/mato --repo "$(REPO)" $(COPILOT_ARGS)

test: ## Run tests with race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
