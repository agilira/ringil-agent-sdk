# Go Makefile - AGILira Standard
# Usage: make help

.PHONY: help build test race coverage fmt vet staticcheck errcheck gosec govulcheck gocyclo lint security check check-race fuzz chat tools deps clean
.DEFAULT_GOAL := help

TOOLS_DIR := $(HOME)/go/bin
# The live keys stay in the SDK's .env until they get one of their own here.
ENV_FILE ?= ../ringil-agent-sdk/.env

RED := \033[0;31m
GREEN := \033[0;32m
YELLOW := \033[1;33m
BLUE := \033[0;34m
NC := \033[0m

help: ## Show this help message
	@echo "$(BLUE)Available targets:$(NC)"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  $(GREEN)%-15s$(NC) %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the ringil binary
	go build -o ringil .

test: ## Run tests
	@echo "$(YELLOW)Running tests...$(NC)"
	go test ./...

race: ## Run tests with race detector
	go test -race ./...

coverage: ## Run tests with coverage
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

fmt: ## Format Go code
	@echo "$(YELLOW)Formatting Go code...$(NC)"
	go fmt ./...

vet: ## Run go vet
	@echo "$(YELLOW)Running go vet...$(NC)"
	go vet ./...

staticcheck: ## Run staticcheck
	@echo "$(YELLOW)Running staticcheck...$(NC)"
	$(TOOLS_DIR)/staticcheck ./...

errcheck: ## Run errcheck
	@echo "$(YELLOW)Running errcheck...$(NC)"
	$(TOOLS_DIR)/errcheck ./...

gosec: ## Run gosec security scanner
	@echo "$(YELLOW)Running gosec security scanner...$(NC)"
	$(TOOLS_DIR)/gosec -quiet ./...

govulcheck: ## Run govulncheck
	@echo "$(YELLOW)Running govulncheck...$(NC)"
	govulncheck ./...

gocyclo: ## Fail on any function with cyclomatic complexity of 10 or more
	@echo "$(YELLOW)Running gocyclo...$(NC)"
	@test -z "$$($(TOOLS_DIR)/gocyclo -over 9 .)" || ($(TOOLS_DIR)/gocyclo -over 9 . && exit 1)

lint: staticcheck errcheck gocyclo ## Run all linters
	@echo "$(GREEN)All linters completed.$(NC)"

security: gosec govulcheck ## Run security checks
	@echo "$(GREEN)Security checks completed.$(NC)"

FUZZTIME ?= 30s
fuzz: ## Run every fuzz target (FUZZTIME, default 30s each)
	@for f in $$(grep -ho '^func Fuzz[A-Za-z0-9]*' *_test.go | cut -d' ' -f2); do \
		go test -run='^$$' -fuzz="^$$f"'$$' -fuzztime=$(FUZZTIME) . || exit 1; done

chat: build ## Chat with the agent using the keys in ENV_FILE
	@test -f $(ENV_FILE) || (echo "$(RED)$(ENV_FILE) missing$(NC)" && exit 1)
	./ringil --env-file $(ENV_FILE) chat

check: fmt vet lint security test ## Run all checks (format, vet, lint, security, test)
	@echo "$(GREEN)All checks passed!$(NC)"

check-race: fmt vet lint security race ## Run all checks including race detector
	@echo "$(GREEN)All checks with race detection passed!$(NC)"

tools: ## Install development tools
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install github.com/kisielk/errcheck@latest
	go install github.com/securego/gosec/v2/cmd/gosec@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/fzipp/gocyclo/cmd/gocyclo@latest

deps: ## Download, verify and tidy dependencies
	go mod download && go mod verify && go mod tidy

clean: ## Clean test artifacts and the binary
	go clean -testcache
	rm -f coverage.out coverage.html ringil
