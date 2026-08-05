override SHELL := /bin/bash
MAKEFLAGS += --no-print-directory

APP_NAME ?= sn-server
override APP_NAME := $(value APP_NAME)
SERVER_ADDR ?= 127.0.0.1:8080
override SERVER_ADDR := $(value SERVER_ADDR)

ifeq ($(origin SN_CLI_COMMIT), undefined)
SN_CLI_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
else
override SN_CLI_COMMIT := $(value SN_CLI_COMMIT)
endif
ifeq ($(origin SN_CLI_TAG), undefined)
SN_CLI_TAG := $(shell git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null)
else
override SN_CLI_TAG := $(value SN_CLI_TAG)
endif
ifeq ($(origin SN_CLI_DIRTY), undefined)
SN_CLI_DIRTY := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo true || echo false)
else
override SN_CLI_DIRTY := $(value SN_CLI_DIRTY)
endif
ifeq ($(origin SN_CLI_BUILDDATE), undefined)
SN_CLI_BUILDDATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
else
override SN_CLI_BUILDDATE := $(value SN_CLI_BUILDDATE)
endif
ifeq ($(origin SN_CLI_VERSION), undefined)
SN_CLI_VERSION := $(if $(strip $(SN_CLI_TAG)),$(SN_CLI_TAG),v0.0.0-dev+$(SN_CLI_COMMIT))
else
override SN_CLI_VERSION := $(value SN_CLI_VERSION)
endif

SN_CLI_DEFAULT_LDFLAGS := -X github.com/yy003x/runtime/internal/cli/version.Version=$(SN_CLI_VERSION) -X github.com/yy003x/runtime/internal/cli/version.Commit=$(SN_CLI_COMMIT) -X github.com/yy003x/runtime/internal/cli/version.BuildDate=$(SN_CLI_BUILDDATE) -X github.com/yy003x/runtime/internal/cli/version.Dirty=$(SN_CLI_DIRTY)
ifeq ($(origin SN_CLI_LDFLAGS), command line)
override SN_CLI_LDFLAGS := $(value SN_CLI_LDFLAGS)
else
SN_CLI_LDFLAGS := $(SN_CLI_DEFAULT_LDFLAGS)
endif

GO ?= go
override GO := $(value GO)
GOCACHE ?=
override GOCACHE := $(value GOCACHE)
GOMODCACHE ?=
override GOMODCACHE := $(value GOMODCACHE)
override MAKE_STEP := bash scripts/make-step.sh
V ?= 0
override V := $(value V)

COVERAGE_PROFILE ?= /tmp/sn-runtime-coverage.out
override COVERAGE_PROFILE := $(value COVERAGE_PROFILE)
COVERAGE_MIN ?= 65.0
override COVERAGE_MIN := $(value COVERAGE_MIN)
TAG ?=
override TAG := $(value TAG)
override RUNTIME_ROOT := $(shell pwd -P)

export APP_NAME SERVER_ADDR
export SN_CLI_COMMIT SN_CLI_TAG SN_CLI_DIRTY SN_CLI_BUILDDATE SN_CLI_VERSION SN_CLI_LDFLAGS
export GO GOCACHE GOMODCACHE V
export COVERAGE_PROFILE COVERAGE_MIN TAG RUNTIME_ROOT

.PHONY: help tidy fmt fmt-check test test-serial test-race coverage build run dev check clean install publish publish-test release release-assets release-check provider-smoke sn-cli-build sn-cli-install sn-cli-test sn-cli-doctor make-step-contract-test _make-variable-probe

help:
	@printf '%s\n' \
		"Develop:" \
		"  make build                    build bin/sn-server" \
		"  make sn-cli-build             build bin/sn-cli" \
		"  make run                      run sn-server" \
		"  make dev                      watch sources and restart sn-server" \
		"  make tidy | fmt | clean       maintain local sources and artifacts" \
		"" \
		"Validate:" \
		"  make check                    run fmt-check and tests" \
		"  make test-serial | test-race  run deterministic or race tests" \
		"  make coverage                 enforce the configured minimum coverage" \
		"  make sn-cli-test              test sn-cli domains and adapters" \
		"  make sn-cli-doctor            build and inspect an isolated sn-cli home" \
		"  make make-step-contract-test  validate Make output and argv safety" \
		"  make release-check            run the complete release gate" \
		"" \
		"Install:" \
		"  make install                  replace local binaries/configs and Runtime state" \
		"                                stop sn-server; do not restart it" \
		"" \
		"Release:" \
		"  make release-assets           build cross-platform archives and checksums" \
		"  make release [TAG=vX.Y.Z]     validate, build assets and create a local annotated tag" \
		"  make publish [TAG=vX.Y.Z]     atomically push main and the current release tag" \
		"  make publish-test             validate the publish workflow" \
		"" \
		"Output:" \
		"  finite tasks show stage/result/elapsed; successful child output stays quiet" \
		"  make V=1 <target>              show safely quoted argv and stream child output"
	@printf '  server address: %s\n' "$${SERVER_ADDR}"
	@printf '  coverage minimum: %s%%\n' "$${COVERAGE_MIN}"

tidy:
	@$(MAKE_STEP) --stage tidy --meta scope=module -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" mod tidy

fmt:
	@$(MAKE_STEP) --stage fmt --meta scope=./... -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" fmt ./...

fmt-check:
	@$(MAKE_STEP) --stage fmt-check --meta scope=go -- \
		bash -c 'cd "$${RUNTIME_ROOT}"; files="$$(gofmt -l $$(find agent cmd command contract internal model profile provider run runtimetest session store tmux transport -name "*.go" -type f))"; if [[ -n "$$files" ]]; then printf "Go files require formatting:\n%s\n" "$$files"; exit 1; fi'

test:
	@$(MAKE_STEP) --stage test --meta scope=./... -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" test ./...

test-serial:
	@$(MAKE_STEP) --stage test-serial --meta scope=./... -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" test -p 1 ./... -count=1

test-race:
	@$(MAKE_STEP) --stage test-race --meta scope=runtime-domains -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" test -race ./agent ./command ./model ./session ./run ./store/sqlite ./tmux ./transport/http -count=1

coverage:
	@$(MAKE_STEP) --stage coverage --meta "minimum=$${COVERAGE_MIN}%" --meta "profile=$${COVERAGE_PROFILE}" -- \
		bash -c '"$${GO}" -C "$${RUNTIME_ROOT}" test $$( "$${GO}" -C "$${RUNTIME_ROOT}" list ./... | grep -v /cmd/ ) -covermode=atomic -coverprofile="$${COVERAGE_PROFILE}" -count=1 && total="$$("$${GO}" -C "$${RUNTIME_ROOT}" tool cover -func="$${COVERAGE_PROFILE}" | awk "$$1")" && awk -v total="$$total" -v minimum="$${COVERAGE_MIN}" "BEGIN { printf \"total coverage: %.1f%% (minimum %.1f%%)\\n\", total, minimum; if (total + 0 < minimum + 0) exit 1 }"' _ '/^total:/ {gsub(/%/, "", $$3); print $$3}'

bin:
	@$(MAKE_STEP) --stage prepare-bin --meta path=bin -- \
		mkdir -p "$${RUNTIME_ROOT}/bin"

build: | bin
	@$(MAKE_STEP) --stage build --meta "binary=bin/$${APP_NAME}" -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" build -o "$${RUNTIME_ROOT}/bin/$${APP_NAME}" ./cmd/sn-server

sn-cli-build: | bin
	@$(MAKE_STEP) --stage sn-cli-build --meta binary=bin/sn-cli --meta "version=$${SN_CLI_VERSION}" -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" build -ldflags "$${SN_CLI_LDFLAGS}" -o "$${RUNTIME_ROOT}/bin/sn-cli" ./cmd/sn-cli

install: build sn-cli-build
	@$(MAKE_STEP) --live --stage install --meta local_source_install=1 -- \
		bash "$${RUNTIME_ROOT}/install.sh" \
			--binary "$${RUNTIME_ROOT}/bin/sn-cli" \
			--server "$${RUNTIME_ROOT}/bin/sn-server" \
			--configs "$${RUNTIME_ROOT}/configs" \
			--resources "$${RUNTIME_ROOT}/resources" \
			--release "$${RUNTIME_ROOT}/release" \
			--local-source-install

sn-cli-install: install

release:
	@$(MAKE_STEP) --live --stage release --meta "tag=$${TAG:-auto}" -- \
		bash "$${RUNTIME_ROOT}/scripts/release.sh"

publish:
	@$(MAKE_STEP) --live --stage publish --meta "tag=$${TAG:-current}" -- \
		bash "$${RUNTIME_ROOT}/scripts/publish.sh"

publish-test:
	@$(MAKE_STEP) --stage publish-test --meta scope=workflow -- \
		bash "$${RUNTIME_ROOT}/scripts/publish-test.sh"

sn-cli-test:
	@$(MAKE_STEP) --stage sn-cli-test --meta scope=cli -- \
		"$${GO}" -C "$${RUNTIME_ROOT}" test ./agent ./command ./contract ./model ./profile ./provider/... ./session ./run ./store/sqlite ./tmux ./transport/... ./internal/... ./runtimetest/...

sn-cli-doctor: sn-cli-build
	@$(MAKE_STEP) --live --stage sn-cli-doctor --meta home=temporary -- \
		bash -c 'set -euo pipefail; home="$$(mktemp -d)"; cleanup() { rm -rf -- "$$home"; }; trap cleanup EXIT; trap "exit 129" HUP; trap "exit 130" INT; trap "exit 143" TERM; mkdir -p "$$home/configs" "$$home/tools" "$$home/resources/schema"; cp "$${RUNTIME_ROOT}"/configs/*.json "$$home/configs/"; cp "$${RUNTIME_ROOT}"/resources/tools/*.json "$$home/tools/"; cp "$${RUNTIME_ROOT}/release/runtime.json" "$$home/runtime.json"; cp -R "$${RUNTIME_ROOT}/resources/schema/." "$$home/resources/schema/"; cp "$${RUNTIME_ROOT}/release/release.json" "$${RUNTIME_ROOT}/release/tmux.conf" "$$home/resources/"; SN_CLI_HOME="$$home" "$${RUNTIME_ROOT}/bin/sn-cli" profile check >/dev/null; SN_CLI_HOME="$$home" "$${RUNTIME_ROOT}/bin/sn-cli" server info'

run:
	@$(MAKE_STEP) --live --stage run --meta "address=$${SERVER_ADDR}" -- \
		env HTTP_ADDR="$${SERVER_ADDR}" "$${GO}" -C "$${RUNTIME_ROOT}" run ./cmd/sn-server

dev:
	@$(MAKE_STEP) --live --stage dev --meta "address=$${SERVER_ADDR}" -- \
		bash "$${RUNTIME_ROOT}/scripts/dev.sh"

check: fmt-check test

release-assets:
	@$(MAKE_STEP) --stage release-assets --meta output=dist -- \
		bash "$${RUNTIME_ROOT}/scripts/release-assets.sh"

release-check:
	@$(MAKE_STEP) --live --stage release-check --meta "version=$${SN_CLI_VERSION}" -- \
		bash "$${RUNTIME_ROOT}/scripts/release-check.sh"

provider-smoke: sn-cli-build
	@$(MAKE_STEP) --live --stage provider-smoke --meta scope=configured-providers -- \
		bash "$${RUNTIME_ROOT}/scripts/provider-smoke.sh"

clean:
	@$(MAKE_STEP) --stage clean --meta paths=bin,dist -- \
		rm -rf "$${RUNTIME_ROOT}/bin" "$${RUNTIME_ROOT}/dist"

make-step-contract-test:
	@$(MAKE_STEP) --live --stage make-step-contract-test --meta scope=make-and-dev -- \
		bash "$${RUNTIME_ROOT}/scripts/make-step-test.sh"

_make-variable-probe:
	@$(MAKE_STEP) --live --stage make-variable-probe \
		--meta "app=$${APP_NAME}" \
		--meta "address=$${SERVER_ADDR}" \
		--meta "version=$${SN_CLI_VERSION}" \
		-- \
		/usr/bin/printf '%s\n' \
		"APP_NAME=$${APP_NAME}" \
		"SERVER_ADDR=$${SERVER_ADDR}" \
		"TAG=$${TAG}" \
		"GO=$${GO}" \
		"GOCACHE=$${GOCACHE}" \
		"GOMODCACHE=$${GOMODCACHE}" \
		"SN_CLI_COMMIT=$${SN_CLI_COMMIT}" \
		"SN_CLI_TAG=$${SN_CLI_TAG}" \
		"SN_CLI_DIRTY=$${SN_CLI_DIRTY}" \
		"SN_CLI_BUILDDATE=$${SN_CLI_BUILDDATE}" \
		"SN_CLI_VERSION=$${SN_CLI_VERSION}" \
		"SN_CLI_LDFLAGS=$${SN_CLI_LDFLAGS}" \
		"COVERAGE_PROFILE=$${COVERAGE_PROFILE}" \
		"COVERAGE_MIN=$${COVERAGE_MIN}" \
		"RUNTIME_ROOT=$${RUNTIME_ROOT}"
