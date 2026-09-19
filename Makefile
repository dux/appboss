GO ?= go
WATCH ?= watchexec
BIN_DIR := ./bin
BINARY := $(BIN_DIR)/dboss
CMD := ./cmd/dboss
SSHKEY := $(BIN_DIR)/sshkey
SSHKEY_CMD := ./cmd/sshkey

.DEFAULT_GOAL := help

.PHONY: help build sshkey test fmt vet lint check demo demo-watch kill clean

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build dboss and sshkey into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(BINARY) $(CMD)
	$(GO) build -o $(SSHKEY) $(SSHKEY_CMD)

sshkey: ## Build sshkey into ./bin/sshkey
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(SSHKEY) $(SSHKEY_CMD)

test: ## Run the test suite
	$(GO) test ./...

fmt: ## Format Go source files
	$(GO) fmt ./...

vet: ## Run go vet
	$(GO) vet ./...

lint: vet ## Run go vet and staticcheck
	$(GO) tool staticcheck ./...

check: lint test ## Run static checks and tests

demo: build ## Run the local demo daemon
	@printf '%s\n' \
		'Management console: http://dboss.lvh.me (also :3100)' \
		'Apps:' \
		'  http://sinatra.lvh.me' \
		'  http://bun.lvh.me'
	sudo $(BINARY) start -c ./demo/dboss.yaml

demo-watch: build ## Rebuild and restart the demo on changes
	$(WATCH) --restart --clear \
		--watch ./cmd \
		--watch ./internal \
		--watch ./demo \
		--watch ./web \
		--watch ./go.mod \
		--watch ./go.sum \
		--ignore './demo/.dboss/**' \
		-- $(MAKE) demo

kill: build ## Kill all demo apps and listeners in the app port range
	$(BINARY) kill -c ./demo/dboss.yaml

clean: ## Remove generated binaries
	rm -rf $(BIN_DIR)
