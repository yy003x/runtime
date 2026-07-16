SHELL := /bin/bash

APP_NAME ?= sn-server
SERVER_ADDR ?= :8080
SN_CLI_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
SN_CLI_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
SN_CLI_BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
RUNTIME_LDFLAGS := -X agent-runtime/internal/agentrun.Version=$(SN_CLI_VERSION)
SN_CLI_LDFLAGS := $(RUNTIME_LDFLAGS) -X agent-runtime/internal/cli/version.Version=$(SN_CLI_VERSION) -X agent-runtime/internal/cli/version.Commit=$(SN_CLI_COMMIT) -X agent-runtime/internal/cli/version.BuildDate=$(SN_CLI_BUILDDATE)

GO ?= go
GOCACHE ?= /tmp/go-build
GOMODCACHE ?= /tmp/go-mod
GO_ENV = env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE)

.PHONY: help tidy fmt fmt-check test test-serial test-race coverage build run dev check clean install release sn-cli-build sn-cli-install sn-cli-test sn-cli-doctor

COVERAGE_PROFILE ?= /tmp/sn-runtime-coverage.out
COVERAGE_MIN ?= 65.0

help:
	@echo "Available targets:"
	@echo "  make tidy              - sync go modules"
	@echo "  make fmt               - format go files"
	@echo "  make fmt-check         - verify Go formatting without modifying files"
	@echo "  make test              - run unit tests"
	@echo "  make test-serial       - run unit tests serially"
	@echo "  make test-race         - run key race tests"
	@echo "  make coverage          - enforce repository coverage threshold"
	@echo "  make build             - build server binary"
	@echo "  make sn-cli-build      - build sn-cli binary"
	@echo "  make install           - build and install sn-cli into ~/.sn"
	@echo "  make release           - build release archives for supported platforms"
	@echo "  make sn-cli-test       - run sn-cli Go tests"
	@echo "  make sn-cli-doctor     - run sn-cli doctor"
	@echo "  make run               - run HTTP server"
	@echo "  make dev               - run server with simple auto-restart on file changes"
	@echo "  make check             - run fmt + test"
	@echo "  make clean             - remove build output"

tidy:
	$(GO_ENV) $(GO) mod tidy

fmt:
	$(GO_ENV) $(GO) fmt ./...

fmt-check:
	@files="$$(gofmt -l $$(find cmd internal test -name '*.go' -type f))"; if [ -n "$$files" ]; then echo "Go files require formatting:"; echo "$$files"; exit 1; fi

test:
	$(GO_ENV) $(GO) test ./...

test-serial:
	$(GO_ENV) $(GO) test -p 1 ./... -count=1

test-race:
	$(GO_ENV) $(GO) test -race ./internal/agentrun ./internal/capability ./internal/provider ./internal/provider/native ./internal/mcp ./internal/transport -run 'Native|APIRuntime|MCP|Loop|Memory|Concurrent|Idempotent|HTTP' -count=1

coverage:
	$(GO_ENV) $(GO) test ./... -covermode=atomic -coverprofile="$(COVERAGE_PROFILE)" -count=1
	@total="$$($(GO_ENV) $(GO) tool cover -func="$(COVERAGE_PROFILE)" | awk '/^total:/ {gsub(/%/, "", $$3); print $$3}')"; \
	awk -v total="$$total" -v minimum="$(COVERAGE_MIN)" 'BEGIN { printf "total coverage: %.1f%% (minimum %.1f%%)\n", total, minimum; if (total + 0 < minimum + 0) exit 1 }'

build:
	mkdir -p bin
	$(GO_ENV) $(GO) build -ldflags "$(RUNTIME_LDFLAGS)" -o bin/$(APP_NAME) ./cmd/sn-server

sn-cli-build:
	mkdir -p bin
	$(GO_ENV) $(GO) build -ldflags "$(SN_CLI_LDFLAGS)" -o bin/sn-cli ./cmd/sn-cli

install: sn-cli-build
	bash install.sh --binary "$(CURDIR)/bin/sn-cli" --configs "$(CURDIR)/configs"

sn-cli-install: install

sn-cli-test:
	$(GO_ENV) $(GO) test ./internal/agentrun ./internal/provider/... ./internal/executor ./internal/daemon ./internal/capability ./internal/transport ./internal/cli/...

sn-cli-doctor: sn-cli-build
	@home="$$(mktemp -d)"; trap 'rm -rf "$$home"' EXIT; mkdir -p "$$home/configs"; cp -R configs/. "$$home/configs/"; SN_CLI_HOME="$$home" ./cmd/sn-cli-wrapper doctor --json

run:
	HTTP_ADDR=$(SERVER_ADDR) $(GO_ENV) $(GO) run ./cmd/sn-server

dev:
	@echo "starting dev loop on $(SERVER_ADDR)"
	@last_sig=""; \
	pid=""; \
	trap 'if [[ -n "$$pid" ]]; then kill "$$pid" 2>/dev/null || true; wait "$$pid" 2>/dev/null || true; fi; exit 0' INT TERM EXIT; \
	while true; do \
		sig="$$(find cmd internal configs -type f \( -name '*.go' -o -name '*.json' -o -name '*.yaml' -o -name '*.yml' \) -print0 | xargs -0 stat -f '%m %N' | sort | shasum | awk '{print $$1}')"; \
		if [[ "$$sig" != "$$last_sig" ]]; then \
			if [[ -n "$$pid" ]]; then \
				echo "change detected, restarting"; \
				kill "$$pid" 2>/dev/null || true; \
				wait "$$pid" 2>/dev/null || true; \
			fi; \
			last_sig="$$sig"; \
			(HTTP_ADDR=$(SERVER_ADDR) $(GO_ENV) $(GO) run ./cmd/sn-server) & \
			pid="$$!"; \
		fi; \
		sleep 1; \
	done

check: fmt-check test

release:
	rm -rf dist
	mkdir -p dist
	@set -euo pipefail; \
	for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
		os="$${platform%/*}"; arch="$${platform#*/}"; stage="dist/.stage-$$os-$$arch"; \
		mkdir -p "$$stage/configs"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO_ENV) $(GO) build -ldflags "$(SN_CLI_LDFLAGS)" -o "$$stage/sn-cli" ./cmd/sn-cli; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO_ENV) $(GO) build -ldflags "$(RUNTIME_LDFLAGS)" -o "dist/sn-server-$$os-$$arch" ./cmd/sn-server; \
		cp -R configs/. "$$stage/configs/"; \
		COPYFILE_DISABLE=1 tar -czf "dist/sn-cli-$$os-$$arch.tar.gz" -C "$$stage" sn-cli configs; \
		rm -rf "$$stage"; \
	done; \
	cd dist; \
	if command -v sha256sum >/dev/null 2>&1; then sha256sum sn-cli-*.tar.gz sn-server-* > checksums.txt; else shasum -a 256 sn-cli-*.tar.gz sn-server-* > checksums.txt; fi

clean:
	rm -rf bin dist
