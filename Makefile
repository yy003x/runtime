SHELL := /bin/bash

APP_NAME ?= agent-arch
SERVER_ADDR ?= :8080
BASE_URL ?= http://localhost:8080
PERSONA_ID ?= default
MODEL ?= MiniMax-M2.7
SN_CLI_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
SN_CLI_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
SN_CLI_BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
SN_CLI_LDFLAGS := -X agent-arch/sncli/internal/version.Version=$(SN_CLI_VERSION) -X agent-arch/sncli/internal/version.Commit=$(SN_CLI_COMMIT) -X agent-arch/sncli/internal/version.BuildDate=$(SN_CLI_BUILDDATE)

GO ?= go
GOCACHE ?= /tmp/go-build
GOMODCACHE ?= /tmp/go-mod
GO_ENV = env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE)

.PHONY: help tidy fmt test build run dev check clean sn-cli-build sn-cli-install sn-cli-test sn-cli-doctor

help:
	@echo "Available targets:"
	@echo "  make tidy              - sync go modules"
	@echo "  make fmt               - format go files"
	@echo "  make test              - run unit tests"
	@echo "  make build             - build server binary"
	@echo "  make sn-cli-build      - build sn-cli binary"
	@echo "  make sn-cli-install    - install sn-cli launcher into ~/.local/bin"
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

test:
	$(GO_ENV) $(GO) test ./...

build:
	mkdir -p bin
	$(GO_ENV) $(GO) build -o bin/$(APP_NAME) ./cmd/server

sn-cli-build:
	mkdir -p runs/global/sn-cli/storage/current/bin
	$(GO_ENV) $(GO) build -ldflags "$(SN_CLI_LDFLAGS)" -o runs/global/sn-cli/storage/current/bin/sn-cli ./sncli/cmd/sn-cli

sn-cli-install:
	bash scripts/install-sn-cli.sh

sn-cli-test:
	$(GO_ENV) $(GO) test ./sncli/...

sn-cli-doctor: sn-cli-build
	./cmd/sn-cli-wrapper doctor --json

run:
	HTTP_ADDR=$(SERVER_ADDR) $(GO_ENV) $(GO) run ./cmd/server

dev:
	@echo "starting dev loop on $(SERVER_ADDR)"
	@last_sig=""; \
	pid=""; \
	trap 'if [[ -n "$$pid" ]]; then kill "$$pid" 2>/dev/null || true; wait "$$pid" 2>/dev/null || true; fi; exit 0' INT TERM EXIT; \
	while true; do \
		sig="$$(find cmd internal configs -type f \( -name '*.go' -o -name '*.yaml' -o -name '*.yml' \) -print0 | xargs -0 stat -f '%m %N' | sort | shasum | awk '{print $$1}')"; \
		if [[ "$$sig" != "$$last_sig" ]]; then \
			if [[ -n "$$pid" ]]; then \
				echo "change detected, restarting"; \
				kill "$$pid" 2>/dev/null || true; \
				wait "$$pid" 2>/dev/null || true; \
			fi; \
			last_sig="$$sig"; \
			(HTTP_ADDR=$(SERVER_ADDR) $(GO_ENV) $(GO) run ./cmd/server) & \
			pid="$$!"; \
		fi; \
		sleep 1; \
	done

check: fmt test

clean:
	rm -rf bin
