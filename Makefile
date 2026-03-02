.DEFAULT_GOAL := help

.PHONY: all build build-launcher clean clean-worktrees fmt help run run-launcher test

BIN_DIR := bin

all: fmt build test ## Run formatting, build, and tests

build: ## Build simenator binary
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/simenator ./cmd/simenator

build-launcher: ## Build simenator launcher binary
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/simenator-launcher ./cmd/simenator-launcher

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

clean-worktrees: ## Remove launcher agent worktrees and worktrees folder
	@set -eu; \
	repo_root=$$(git -C "$${SIMENATOR_WORKTREE_REPO:-/home/ryansimmen/staging-labs}" rev-parse --show-toplevel); \
	worktrees_root="$$repo_root.worktrees"; \
	echo "Cleaning worktrees under $$worktrees_root"; \
	git -C "$$repo_root" worktree list --porcelain | awk '/^worktree /{print substr($$0,10)}' | while IFS= read -r wt; do \
		case "$$wt" in "$$worktrees_root"/agent*) echo "Removing $$wt"; git -C "$$repo_root" worktree remove --force "$$wt" ;; esac; \
	done; \
	git -C "$$repo_root" worktree prune; \
	rm -rf "$$worktrees_root"

fmt: ## Format Go source files
	go fmt ./...

run: ## Run simenator chat app
	go run ./cmd/simenator

run-launcher: ## Run docker worktree launcher
	go run ./cmd/simenator-launcher

test: ## Run tests
	go test ./...

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
