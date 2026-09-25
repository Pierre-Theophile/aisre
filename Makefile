# SPDX-License-Identifier: Apache-2.0
#
# Developer entrypoints for sre-agent. Everything CI runs is reachable from here.

BINARY      := aisre
BIN_DIR     := bin
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

# Constitution X: one static binary, no cgo, anywhere.
export CGO_ENABLED := 0

# Fixtures the `verify` target replays. Overridable: make verify FIXTURES=fixtures/baseline-*
FIXTURES    ?= fixtures/*/

.DEFAULT_GOAL := build
.PHONY: all build test lint gen verify vet fmt clean tools generate-check help

all: gen build test lint ## Generate, build, test and lint

build: ## Build the static binary into bin/aisre
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)

test: ## Run the Go test suite (needs PG_DSN for store-backed tests)
	go test -count=1 $(PKG)

.PHONY: test-race
test-race: ## Run tests with the race detector (needs cgo, so a C toolchain)
	CGO_ENABLED=1 go test -race -count=1 $(PKG)

vet: ## Run go vet
	go vet $(PKG)

fmt: ## Format Go sources
	gofmt -s -w $$(git ls-files '*.go' | grep -v '\.pb\.go$$' | grep -v '\.connect\.go$$')

lint: ## Run golangci-lint (install with `make tools`)
	golangci-lint run

gen: ## Regenerate protobuf/Connect code from api/ and re-apply SPDX headers
	buf lint
	buf generate
	./scripts/add-spdx-headers.sh

generate-check: gen ## Fail if generated code is out of date (CI)
	@git diff --exit-code -- api || \
		{ echo 'generated code is stale: run `make gen` and commit the result'; exit 1; }

# `fixture verify` gives every fixture, and every shuffle permutation, a database of its own, so
# the role in the DSN needs CREATEDB; the database the DSN names is only the maintenance
# connection. PG_DSN is passed explicitly rather than left to the environment so that `make
# verify` says what it needs when it is missing instead of failing inside the binary.
PG_DSN ?= postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable

verify: build ## Replay every fixture and diff against its goldens (needs PG_DSN, role with CREATEDB)
	$(BIN_DIR)/$(BINARY) fixture verify $(FIXTURES) --report --db "$(PG_DSN)"

clean: ## Remove build output
	rm -rf $(BIN_DIR)

tools: ## Install the developer toolchain into $(GOBIN)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/bufbuild/buf/cmd/buf@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'
