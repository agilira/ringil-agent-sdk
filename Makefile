# Go Makefile - AGILira Standard
# Usage: make help

.PHONY: help test race coverage fmt vet staticcheck errcheck gosec govulcheck gocyclo lint security check check-race deps clean tools fuzz bench live live-local live-nim live-anthropic
.DEFAULT_GOAL := help

# Variables
TOOLS_DIR := $(HOME)/go/bin
# The library and its heavier parts are separate modules; every check runs on each.
MODULES := . ./mcp ./a2a ./memory/sqlite ./provider/anthropic ./secrets/keyring ./secrets/store
EACH = for m in $(MODULES); do (cd $$m && $(1)) || exit 1; done

# Colors for output
RED := \033[0;31m
GREEN := \033[0;32m
YELLOW := \033[1;33m
BLUE := \033[0;34m
NC := \033[0m # No Color

help: ## Show this help message
	@echo "$(BLUE)Available targets:$(NC)"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  $(GREEN)%-15s$(NC) %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

test: ## Run tests
	@echo "$(YELLOW)Running tests...$(NC)"
	@$(call EACH,go test ./...)

race: ## Run tests with race detector
	@echo "$(YELLOW)Running tests with race detector...$(NC)"
	@$(call EACH,go test -race ./...)

coverage: ## Run tests with coverage
	@echo "$(YELLOW)Running tests with coverage...$(NC)"
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	cd mcp && go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1
	cd a2a && go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html
	@echo "$(GREEN)Coverage report generated: coverage.html$(NC)"

fmt: ## Format Go code
	@echo "$(YELLOW)Formatting Go code...$(NC)"
	@$(call EACH,go fmt ./...)

vet: ## Run go vet
	@echo "$(YELLOW)Running go vet...$(NC)"
	@$(call EACH,go vet ./...)

staticcheck: ## Run staticcheck
	@echo "$(YELLOW)Running staticcheck...$(NC)"
	@if [ ! -f "$(TOOLS_DIR)/staticcheck" ]; then \
		echo "$(RED)staticcheck not found. Run 'make tools' to install.$(NC)"; \
		exit 1; \
	fi
	@$(call EACH,$(TOOLS_DIR)/staticcheck ./...)

errcheck: ## Run errcheck
	@echo "$(YELLOW)Running errcheck...$(NC)"
	@if [ ! -f "$(TOOLS_DIR)/errcheck" ]; then \
		echo "$(RED)errcheck not found. Run 'make tools' to install.$(NC)"; \
		exit 1; \
	fi
	@$(call EACH,$(TOOLS_DIR)/errcheck ./...)

gosec: ## Run gosec security scanner
	@echo "$(YELLOW)Running gosec security scanner...$(NC)"
	@if [ ! -f "$(TOOLS_DIR)/gosec" ]; then \
		echo "$(RED)gosec not found. Run 'make tools' to install.$(NC)"; \
		exit 1; \
	fi
	@$(call EACH,$(TOOLS_DIR)/gosec -quiet ./...)

govulcheck: ## Run govulncheck for vulnerability scanning
	@echo "$(YELLOW)Running govulncheck for vulnerability scanning...$(NC)"
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "$(RED)govulncheck not found. Run 'make tools' to install.$(NC)"; \
		exit 1; \
	fi
	@$(call EACH,govulncheck ./...)

gocyclo: ## Fail on any function with cyclomatic complexity of 10 or more
	@echo "$(YELLOW)Running gocyclo...$(NC)"
	@test -z "$$($(TOOLS_DIR)/gocyclo -over 9 .)" || ($(TOOLS_DIR)/gocyclo -over 9 . && exit 1)

lint: staticcheck errcheck gocyclo ## Run all linters
	@echo "$(GREEN)All linters completed.$(NC)"

security: gosec govulcheck ## Run security checks
	@echo "$(GREEN)Security checks completed.$(NC)"

FUZZTIME ?= 30s
fuzz: ## Run every fuzz target (FUZZTIME, default 30s each)
	@echo "$(YELLOW)Running fuzz tests...$(NC)"
	@for m in $(MODULES); do (cd $$m && for pkg in $$(go list ./...); do \
		dir=$$(go list -f '{{.Dir}}' $$pkg); \
		for f in $$(grep -ho '^func Fuzz[A-Za-z0-9]*' $$dir/*_test.go 2>/dev/null | cut -d' ' -f2); do \
			go test -run='^$$' -fuzz="^$$f"'$$' -fuzztime=$(FUZZTIME) $$pkg || exit 1; done; done) || exit 1; done
	@echo "$(GREEN)Fuzz tests completed.$(NC)"

live: ## Run live tests against the endpoint in .env (real calls, may cost)
	@test -f .env || (echo "$(RED).env missing$(NC)" && exit 1)
	@set -a && . ./.env && set +a && go test -tags live -run '^TestLive' -count=1 -v ./... && cd mcp && go test -tags live -run '^TestLive' -count=1 -v ./... && cd ../a2a && go test -tags live -run '^TestLive' -count=1 -v ./... && cd ../memory/sqlite && go test -tags live -run '^TestLive' -count=1 -v ./...

live-anthropic: ## Run the Claude provider's live tests (ANTHROPIC_API_KEY in .env; claude-haiku-5-5 unless RINGIL_ANTHROPIC_MODEL)
	@test -f .env || (echo "$(RED).env missing$(NC)" && exit 1)
	@set -a && . ./.env && set +a && cd provider/anthropic && go test -tags live -run '^TestLive' -count=1 -v ./...

NIM_MODEL ?= nvidia/nemotron-3.5-bolt-e4b
NIM_EMBED ?= nvidia/nemotron-3-embed-1b
live-nim: ## Run live tests on NVIDIA NIM's free models (NVIDIA_API_KEY in .env)
	@set -a && . ./.env && set +a && export RINGIL_BASE_URL=https://integrate.api.nvidia.com/v1 RINGIL_MODEL=$(NIM_MODEL) RINGIL_API_KEY="$$NVIDIA_API_KEY" RINGIL_EMBED_MODEL=$(NIM_EMBED) RINGIL_EMBED_INPUT_TYPE=1 && go test -tags live -run '^TestLive' -count=1 -v ./... && cd mcp && go test -tags live -run '^TestLive' -count=1 -v ./... && cd ../a2a && go test -tags live -run '^TestLive' -count=1 -v ./... && cd ../memory/sqlite && go test -tags live -run '^TestLive' -count=1 -v ./...

OLLAMA_URL ?= http://127.0.0.1:11434/v1
OLLAMA_MODEL ?= qwen2.5:3b
OLLAMA_EMBED ?= mxbai-embed-large
live-local: ## Run live tests on a local Ollama (no cost, no rate limits); keys from .env for Composio
	@set -a && . ./.env && set +a && export RINGIL_BASE_URL=$(OLLAMA_URL) RINGIL_MODEL=$(OLLAMA_MODEL) RINGIL_API_KEY= RINGIL_EMBED_MODEL=$(OLLAMA_EMBED) && go test -tags live -run '^TestLive' -count=1 -v -timeout 40m ./... && cd mcp && go test -tags live -run '^TestLive' -count=1 -v ./... && cd ../a2a && go test -tags live -run '^TestLive' -count=1 -v ./... && cd ../memory/sqlite && go test -tags live -run '^TestLive' -count=1 -v ./...

check: fmt vet lint security test ## Run all checks (format, vet, lint, security, test)
	@echo "$(GREEN)All checks passed!$(NC)"

check-race: fmt vet lint security race ## Run all checks including race detector
	@echo "$(GREEN)All checks with race detection passed!$(NC)"

tools: ## Install development tools
	@echo "$(YELLOW)Installing development tools...$(NC)"
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install github.com/kisielk/errcheck@latest
	go install github.com/securego/gosec/v2/cmd/gosec@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
	@echo "$(GREEN)Tools installed successfully!$(NC)"

deps: ## Download and verify dependencies
	@echo "$(YELLOW)Downloading dependencies...$(NC)"
	@$(call EACH,go mod download)
	@echo "$(YELLOW)Verifying dependencies...$(NC)"
	@$(call EACH,go mod verify)
	@echo "$(YELLOW)Tidying dependencies...$(NC)"
	@$(call EACH,go mod tidy)

clean: ## Clean test artifacts and cache
	@echo "$(YELLOW)Cleaning...$(NC)"
	go clean -testcache
	rm -f coverage.out coverage.html mcp/coverage.out a2a/coverage.out

bench: ## Run benchmarks
	@echo "$(YELLOW)Running benchmarks...$(NC)"
	go test -bench=. -benchmem ./...

ci: ## Run CI checks (used in GitHub Actions)
	@echo "$(BLUE)Running CI checks...$(NC)"
	@make fmt vet lint security test coverage
	@echo "$(GREEN)CI checks completed successfully!$(NC)"

dev: ## Quick development check (fast feedback loop)
	@echo "$(BLUE)Running development checks...$(NC)"
	@make fmt vet test
	@echo "$(GREEN)Development checks completed!$(NC)"

pre-commit: check ## Run pre-commit checks (alias for 'check')

all: clean tools deps check ## Run everything from scratch

# Show tool status
status: ## Show status of installed tools
	@echo "$(BLUE)Development tools status:$(NC)"
	@echo -n "staticcheck: "; [ -f "$(TOOLS_DIR)/staticcheck" ] && echo "$(GREEN)✓ installed$(NC)" || echo "$(RED)✗ missing$(NC)"
	@echo -n "errcheck:    "; [ -f "$(TOOLS_DIR)/errcheck" ] && echo "$(GREEN)✓ installed$(NC)" || echo "$(RED)✗ missing$(NC)"
	@echo -n "gosec:       "; [ -f "$(TOOLS_DIR)/gosec" ] && echo "$(GREEN)✓ installed$(NC)" || echo "$(RED)✗ missing$(NC)"
	@echo -n "govulncheck: "; command -v govulncheck >/dev/null 2>&1 && echo "$(GREEN)✓ installed$(NC)" || echo "$(RED)✗ missing$(NC)"