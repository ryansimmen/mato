.DEFAULT_GOAL := help

.PHONY: all build clean fmt help run test

BIN_DIR := bin
REPO ?= /home/ryansimmen/staging-labs

all: fmt build test ## Run formatting, build, and tests

build: ## Build mato binary
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/mato .

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

fmt: ## Format Go source files
	go fmt ./...

run: ## Run agent in Docker (use -- to pass args to copilot, e.g. -- --model claude-opus-4.6)
	@if [ -z "$(REPO)" ]; then echo "REPO is required"; exit 1; fi
	go run . --repo "$(REPO)"

test: ## Run tests
	go test ./...

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
