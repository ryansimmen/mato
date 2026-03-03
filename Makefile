.DEFAULT_GOAL := help

.PHONY: all build build-launcher clean fmt help run run-launcher run-task test

BIN_DIR := bin
REPO ?= /home/ryansimmen/staging-labs

all: fmt build test ## Run formatting, build, and tests

build: ## Build simenator binary
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/simenator ./cmd/simenator

build-launcher: ## Build simenator launcher binary
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/simenator-launcher ./cmd/simenator-launcher

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

fmt: ## Format Go source files
	go fmt ./...

run: ## Run simenator chat app
	go run ./cmd/simenator

run-task: ## Run simenator in autonomous task mode
	go run ./cmd/simenator -task

run-launcher: ## Run docker launcher
	@if [ -z "$(REPO)" ]; then echo "REPO is required"; exit 1; fi
	go run ./cmd/simenator-launcher --repo "$(REPO)"

test: ## Run tests
	go test ./...

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
