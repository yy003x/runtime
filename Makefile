SHELL := /bin/bash

APP_NAME ?= sn-server
SERVER_ADDR ?= :8080
SN_CLI_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
SN_CLI_TAG ?= $(shell git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null)
SN_CLI_DIRTY ?= $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo true || echo false)
SN_CLI_VERSION ?= $(if $(strip $(SN_CLI_TAG)),$(SN_CLI_TAG),v0.0.0-dev+$(SN_CLI_COMMIT))
SN_CLI_BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
RUNTIME_LDFLAGS := -X agent-runtime/internal/agentrun.Version=$(SN_CLI_VERSION)
SN_CLI_LDFLAGS := $(RUNTIME_LDFLAGS) -X agent-runtime/internal/cli/version.Version=$(SN_CLI_VERSION) -X agent-runtime/internal/cli/version.Commit=$(SN_CLI_COMMIT) -X agent-runtime/internal/cli/version.BuildDate=$(SN_CLI_BUILDDATE) -X agent-runtime/internal/cli/version.Dirty=$(SN_CLI_DIRTY)

GO ?= go
GOCACHE ?= /tmp/go-build
GOMODCACHE ?= /tmp/go-mod
GO_ENV = env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE)

.PHONY: help tidy fmt fmt-check test test-serial test-race coverage build run dev check clean install release release-check provider-smoke sn-cli-build sn-cli-install sn-cli-test sn-cli-doctor

COVERAGE_PROFILE ?= /tmp/sn-runtime-coverage.out
COVERAGE_MIN ?= 65.0
SN_CLI_OVERWRITE_CONFIGS ?= 1

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
	@echo "  make install           - build/install sn-cli; overwrite same-name configs by default"
	@echo "                           use SN_CLI_OVERWRITE_CONFIGS=0 to keep existing configs"
	@echo "  make release           - build release archives for supported platforms"
	@echo "  make release-check     - validate, build and smoke-test release assets"
	@echo "  make provider-smoke    - opt-in real provider smoke (requires SN_REAL_PROVIDER_SMOKE=1)"
	@echo "  make sn-cli-test       - run sn-cli Go tests"
	@echo "  make sn-cli-doctor     - run sn-cli system doctor"
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
	$(GO_ENV) $(GO) test -race ./internal/agentrun ./internal/capability ./internal/provider ./internal/provider/native ./internal/mcp ./internal/transport -run 'Native|APIRuntime|MCP|Loop|Memory|Concurrent|Idempotent|HTTP|Queue|Dispatch|Submit|Reconcile' -count=1

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
	@overwrite_configs="$(SN_CLI_OVERWRITE_CONFIGS)"; \
	case "$$overwrite_configs" in \
	  1) overwrite_flag="--overwrite-configs" ;; \
	  0) overwrite_flag="" ;; \
	  *) echo "SN_CLI_OVERWRITE_CONFIGS must be 0 or 1" >&2; exit 1 ;; \
	esac; \
	bash install.sh --binary "$(CURDIR)/bin/sn-cli" --configs "$(CURDIR)/configs" --resources "$(CURDIR)/resources" $$overwrite_flag

sn-cli-install: install

sn-cli-test:
	$(GO_ENV) $(GO) test ./internal/agentrun ./internal/provider/... ./internal/executor ./internal/daemon ./internal/capability ./internal/transport ./internal/cli/...

sn-cli-doctor: sn-cli-build
	@home="$$(mktemp -d)"; trap 'rm -rf "$$home"' EXIT; mkdir -p "$$home/configs" "$$home/resources"; cp -R configs/. "$$home/configs/"; cp -R resources/. "$$home/resources/"; SN_CLI_HOME="$$home" ./cmd/sn-cli-wrapper system doctor --json

run:
	HTTP_ADDR=$(SERVER_ADDR) $(GO_ENV) $(GO) run ./cmd/sn-server

dev:
	@echo "starting dev loop on $(SERVER_ADDR)"
	@last_sig=""; \
	pid=""; \
	trap 'if [[ -n "$$pid" ]]; then kill "$$pid" 2>/dev/null || true; wait "$$pid" 2>/dev/null || true; fi; exit 0' INT TERM EXIT; \
	while true; do \
		sig="$$(find cmd internal configs resources -type f \( -name '*.go' -o -name '*.json' -o -name '*.yaml' -o -name '*.yml' \) -print0 | xargs -0 stat -f '%m %N' | sort | shasum | awk '{print $$1}')"; \
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
		mkdir -p "$$stage/configs" "$$stage/resources"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO_ENV) $(GO) build -ldflags "$(SN_CLI_LDFLAGS)" -o "$$stage/sn-cli" ./cmd/sn-cli; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO_ENV) $(GO) build -ldflags "$(RUNTIME_LDFLAGS)" -o "dist/sn-server-$$os-$$arch" ./cmd/sn-server; \
		cp -R configs/. "$$stage/configs/"; \
		cp -R resources/. "$$stage/resources/"; \
		COPYFILE_DISABLE=1 tar -czf "dist/sn-cli-$$os-$$arch.tar.gz" -C "$$stage" sn-cli configs resources; \
		rm -rf "$$stage"; \
	done; \
	cd dist; \
	if command -v sha256sum >/dev/null 2>&1; then sha256sum sn-cli-*.tar.gz sn-server-* > checksums.txt; else shasum -a 256 sn-cli-*.tar.gz sn-server-* > checksums.txt; fi

release-check:
	SN_CLI_VERSION="$(SN_CLI_VERSION)" bash scripts/release-check.sh

provider-smoke: sn-cli-build
	bash scripts/provider-smoke.sh

clean:
	rm -rf bin dist
