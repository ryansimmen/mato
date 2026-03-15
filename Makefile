.DEFAULT_GOAL := help

-include .env
export

.PHONY: all build clean fmt help install run test

BIN_DIR := bin
REPO ?= $(error REPO is not set. Create a .env file with REPO=<path> or pass it on the command line)

all: fmt build test ## Run formatting, build, and tests

build: ## Build mato binary
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/mato ./cmd/mato

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

fmt: ## Format Go source files
	go fmt ./...

install: ## Install mato binary to GOBIN
	go install ./cmd/mato

run: ## Run agent in Docker (use COPILOT_ARGS to pass args to copilot, e.g. COPILOT_ARGS="--model gpt-5.3-codex")
	@if [ -z "$(REPO)" ]; then echo "REPO is required"; exit 1; fi
	go run ./cmd/mato --repo "$(REPO)" $(COPILOT_ARGS)

test: ## Run tests
	go test ./...

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
