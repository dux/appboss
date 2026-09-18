GO ?= go
WATCH ?= watchexec
BIN_DIR := ./bin
BINARY := $(BIN_DIR)/dboss
CMD := ./cmd/dboss

.DEFAULT_GOAL := help

.PHONY: help build test fmt vet check demo demo-watch kill clean

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build dboss into ./bin/dboss
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(BINARY) $(CMD)

test: ## Run the test suite
	$(GO) test ./...

fmt: ## Format Go source files
	$(GO) fmt ./...

vet: ## Run go vet
	$(GO) vet ./...

check: vet test ## Run static checks and tests

demo: build ## Run the local demo daemon
	@printf '%s\n' \
		'Management console: http://boss.lvh.me:8080 (also :3100)' \
		'Apps:' \
		'  http://sinatra.lvh.me:8080' \
		'  http://bun.lvh.me:8080'
	$(BINARY) start -c ./demo/dboss.yaml

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
