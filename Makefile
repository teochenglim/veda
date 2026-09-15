.DEFAULT_GOAL := help
BINARY := veda

# Read the current version from the VERSION file (no external tooling required).
VERSION_CURRENT := $(shell cat VERSION 2>/dev/null || echo 0.0.0)

.PHONY: help
help: ## Show this menu
	@echo "Veda $(VERSION_CURRENT) - available targets:"
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Release cycle:"
	@echo "  make release VERSION=x.y.z   # bump VERSION, push HEAD, tag, push tag -> CI"

## --- develop -----------------------------------------------------------

.PHONY: build
build: ## Build the veda binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=v$(VERSION_CURRENT)" -o bin/$(BINARY) ./cmd/veda

.PHONY: run
run: ## Run the veda review UI locally (defaults to ~/.veda)
	go run ./cmd/veda ui

.PHONY: test
test: ## Run the full test suite with race detector and coverage
	go test ./... -race -cover

.PHONY: test-verbose
test-verbose: ## Run the full test suite with verbose per-test output
	go test ./... -race -cover -v

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source with gofmt
	gofmt -l -w .

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove local build artifacts
	rm -rf bin dist

.PHONY: smoke
smoke: build ## End-to-end smoke: init → MCP stdio round trip → export
	@VEDA_HOME=$$(mktemp -d)/.veda; \
	./bin/veda init --no-telemetry >/dev/null; \
	{ printf '%s\n%s\n%s\n' \
	  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
	  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
	  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember","arguments":{"content":"User prefers window seats on flights"}}}'; \
	  sleep 1; } | ./bin/veda serve --stdio >/dev/null 2>&1; \
	./bin/veda export | grep -q "window seats" && echo "smoke OK"

## --- supply-chain hardening ----------------------------------------------

.PHONY: github-action-bump
github-action-bump: ## Pin .github/workflows/*.yml actions to latest release, full commit SHA (uses pinact)
	@# Unauthenticated GitHub API calls are capped at 60/hour; export GITHUB_TOKEN to raise that limit.
	go run github.com/suzuki-shunsuke/pinact/cmd/pinact@latest run --update
	go run github.com/suzuki-shunsuke/pinact/cmd/pinact@latest run --verify
	@echo "Actions bumped and verified. Review the diff, then run 'make vet test' before committing."

## --- release --------------------------------------------------------------

.PHONY: version
version: ## Print the version currently in VERSION
	@echo $(VERSION_CURRENT)

.PHONY: bump
bump: ## Rewrite VERSION with the new version (VERSION=x.y.z required)
	@if [ -z "$(VERSION)" ]; then echo "Usage: make bump VERSION=x.y.z"; exit 1; fi
	@echo "$(VERSION)" > VERSION
	@echo "VERSION -> $(VERSION)"

.PHONY: release
release: ## Bump VERSION, push HEAD, tag, push the tag - triggers GitHub Actions (VERSION=x.y.z required)
	@if [ -z "$(VERSION)" ]; then echo "Usage: make release VERSION=x.y.z"; exit 1; fi
	$(MAKE) bump VERSION=$(VERSION)
	git add VERSION
	git commit --amend --no-edit
	git push origin HEAD
	git tag v$(VERSION)
	git push origin v$(VERSION)
	@echo "Released v$(VERSION) - GitHub Actions will build and publish."
