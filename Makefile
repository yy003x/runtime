SHELL := /bin/bash

APP_NAME ?= sn-server
SERVER_ADDR ?= :8080
SN_CLI_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
SN_CLI_TAG ?= $(shell git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null)
SN_CLI_DIRTY ?= $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo true || echo false)
SN_CLI_VERSION ?= $(if $(strip $(SN_CLI_TAG)),$(SN_CLI_TAG),v0.0.0-dev+$(SN_CLI_COMMIT))
SN_CLI_BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
SN_CLI_LDFLAGS := -X github.com/yy003x/runtime/internal/cli/version.Version=$(SN_CLI_VERSION) -X github.com/yy003x/runtime/internal/cli/version.Commit=$(SN_CLI_COMMIT) -X github.com/yy003x/runtime/internal/cli/version.BuildDate=$(SN_CLI_BUILDDATE) -X github.com/yy003x/runtime/internal/cli/version.Dirty=$(SN_CLI_DIRTY)

GO ?= go
GOCACHE ?= $(shell $(GO) env GOCACHE)
GOMODCACHE ?= $(shell $(GO) env GOMODCACHE)
GO_ENV = env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE)

.PHONY: help tidy fmt fmt-check test test-serial test-race coverage build run dev check clean install publish publish-test release release-assets release-check provider-smoke sn-cli-build sn-cli-install sn-cli-test sn-cli-doctor

COVERAGE_PROFILE ?= /tmp/sn-runtime-coverage.out
COVERAGE_MIN ?= 65.0
SN_CLI_OVERWRITE_CONFIGS ?= 1

help:
	@echo "Release workflow:"
	@echo "  make install              - build and install the current checkout"
	@echo "  make release [TAG=vX.Y.Z] - validate, build assets and create a local annotated tag"
	@echo "  make publish [TAG=vX.Y.Z] - atomically push main and the current release tag"

tidy:
	$(GO_ENV) $(GO) mod tidy

fmt:
	$(GO_ENV) $(GO) fmt ./...

fmt-check:
	@files="$$(gofmt -l $$(find agent cmd command contract internal model profile provider run runtimetest session store transport -name '*.go' -type f))"; if [ -n "$$files" ]; then echo "Go files require formatting:"; echo "$$files"; exit 1; fi

test:
	$(GO_ENV) $(GO) test ./...

test-serial:
	$(GO_ENV) $(GO) test -p 1 ./... -count=1

test-race:
	$(GO_ENV) $(GO) test -race ./agent ./command ./model ./session ./run ./store/sqlite ./transport/http -count=1

coverage:
	$(GO_ENV) $(GO) test ./... -covermode=atomic -coverprofile="$(COVERAGE_PROFILE)" -count=1
	@total="$$($(GO_ENV) $(GO) tool cover -func="$(COVERAGE_PROFILE)" | awk '/^total:/ {gsub(/%/, "", $$3); print $$3}')"; \
	awk -v total="$$total" -v minimum="$(COVERAGE_MIN)" 'BEGIN { printf "total coverage: %.1f%% (minimum %.1f%%)\n", total, minimum; if (total + 0 < minimum + 0) exit 1 }'

build:
	mkdir -p bin
	$(GO_ENV) $(GO) build -o bin/$(APP_NAME) ./cmd/sn-server

sn-cli-build:
	mkdir -p bin
	$(GO_ENV) $(GO) build -ldflags "$(SN_CLI_LDFLAGS)" -o bin/sn-cli ./cmd/sn-cli

install: build sn-cli-build
	@overwrite_configs="$(SN_CLI_OVERWRITE_CONFIGS)"; \
	case "$$overwrite_configs" in \
	  1) overwrite_flag="--overwrite-configs" ;; \
	  0) overwrite_flag="" ;; \
	  *) echo "SN_CLI_OVERWRITE_CONFIGS must be 0 or 1" >&2; exit 1 ;; \
	esac; \
	bash install.sh --binary "$(CURDIR)/bin/sn-cli" --server "$(CURDIR)/bin/sn-server" --configs "$(CURDIR)/configs" --commands "$(CURDIR)/configs/commands" --runtime-config "$(CURDIR)/configs/runtime/runtime.json" --resources "$(CURDIR)/resources" $$overwrite_flag

sn-cli-install: install

release:
	@TAG="$(TAG)" bash scripts/release.sh

publish:
	@TAG="$(TAG)" bash scripts/publish.sh

publish-test:
	bash scripts/publish-test.sh

sn-cli-test:
	$(GO_ENV) $(GO) test ./agent ./command ./contract ./model ./profile ./provider/... ./session ./run ./store/sqlite ./transport/... ./internal/... ./runtimetest/...

sn-cli-doctor: sn-cli-build
	@home="$$(mktemp -d)"; trap 'rm -rf "$$home"' EXIT; mkdir -p "$$home/configs" "$$home/commands" "$$home/resources"; cp configs/*.json "$$home/configs/"; cp -R configs/commands/. "$$home/commands/"; cp configs/runtime/runtime.json "$$home/runtime.json"; cp -R resources/. "$$home/resources/"; SN_CLI_HOME="$$home" ./bin/sn-cli profile check >/dev/null; SN_CLI_HOME="$$home" ./bin/sn-cli system info

run:
	HTTP_ADDR=$(SERVER_ADDR) $(GO_ENV) $(GO) run ./cmd/sn-server

dev:
	@echo "starting dev loop on $(SERVER_ADDR)"
	@last_sig=""; \
	pid=""; \
	trap 'if [[ -n "$$pid" ]]; then kill "$$pid" 2>/dev/null || true; wait "$$pid" 2>/dev/null || true; fi; exit 0' INT TERM EXIT; \
	while true; do \
		sig="$$(find agent cmd command contract internal model profile provider run runtimetest session store transport configs resources -type f \( -name '*.go' -o -name '*.json' \) -print0 | xargs -0 stat -f '%m %N' | sort | shasum | awk '{print $$1}')"; \
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

release-assets:
	rm -rf dist
	mkdir -p dist
	@set -euo pipefail; \
	for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
		os="$${platform%/*}"; arch="$${platform#*/}"; stage="dist/.stage-$$os-$$arch"; \
			mkdir -p "$$stage/configs" "$$stage/commands" "$$stage/resources"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO_ENV) $(GO) build -ldflags "$(SN_CLI_LDFLAGS)" -o "$$stage/sn-cli" ./cmd/sn-cli; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO_ENV) $(GO) build -o "$$stage/sn-server" ./cmd/sn-server; \
			cp configs/*.json "$$stage/configs/"; \
			cp -R configs/commands/. "$$stage/commands/"; \
			cp configs/runtime/runtime.json "$$stage/runtime.json"; \
			cp -R resources/. "$$stage/resources/"; \
			COPYFILE_DISABLE=1 tar -czf "dist/sn-cli-$$os-$$arch.tar.gz" -C "$$stage" sn-cli sn-server configs commands runtime.json resources; \
		rm -rf "$$stage"; \
	done; \
	cd dist; \
	if command -v sha256sum >/dev/null 2>&1; then sha256sum sn-cli-*.tar.gz > checksums.txt; else shasum -a 256 sn-cli-*.tar.gz > checksums.txt; fi

release-check:
	SN_CLI_VERSION="$(SN_CLI_VERSION)" bash scripts/release-check.sh

provider-smoke: sn-cli-build
	bash scripts/provider-smoke.sh

clean:
	rm -rf bin dist
