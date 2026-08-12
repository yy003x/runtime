# SN Runtime

[![CI](https://github.com/yy003x/runtime/actions/workflows/ci.yml/badge.svg)](https://github.com/yy003x/runtime/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yy003x/runtime?include_prereleases&display_name=release)](https://github.com/yy003x/runtime/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/yy003x/runtime)](https://goreportcard.com/report/github.com/yy003x/runtime)
[![Go Version](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**A local-first Go runtime for running AI agents and model calls — unifying CLI wrappers, model APIs, durable sessions, long-lived tmux windows, autonomous agent loops, and a durable background queue behind one CLI and HTTP API.**

[English](README.md) · [简体中文](README.zh-CN.md)

---

## What it is

SN Runtime is a self-hosted execution layer for AI coding agents and model
calls. Instead of bolting session state, retry, cancellation, and tool loops
onto ad-hoc scripts, it gives you a small set of strict, composable entry
points:

- Wrap **Codex / Claude** CLIs behind a typed profile, or call a **model API**
  directly (OpenAI-compatible and Anthropic-compatible drivers).
- Record multi-turn **sessions** to a crash-consistent, file-based store.
- Keep a long-lived interactive **tmux** window you can attach to later.
- Run an autonomous **agent** loop (model → tool → tool-result → model).
- Submit **durable runs** to a SQLite queue and control them via CLI or HTTP.

Everything is **local-first**: your data lives under `${SN_CLI_HOME:-~/.sn}`
(SQLite WAL for runs, regular files for sessions). Contracts are **strict and
fail-closed** — unknown fields, schema drift, and ambiguous crash states are
rejected rather than silently papered over.

### Who it's for

- Developers who wrap Codex/Claude CLIs and want real session/run management
  instead of shell one-liners.
- Teams that want a scriptable, durable, self-hosted alternative to hosted
  agent platforms.
- Anyone building agent workflows who needs resumable runs, cancellation, and a
  clean HTTP control plane.

## Features

- 🖥️ **Local-first** — runs, sessions, and state stay on your machine under `~/.sn`.
- 🔌 **Provider-neutral** — OpenAI-compatible and Anthropic-compatible drivers
  behind one canonical model contract.
- 🧱 **Typed profiles** — `type=cli|api` routes to a Command Bridge or a Model
  Core; one config layer, no hidden mappings.
- 💾 **Durable & resumable runs** — SQLite WAL queue with cancel, retry, resume,
  and reconcile semantics that survive process exits.
- 🗂️ **Crash-consistent sessions** — atomic, journal-backed file store with
  identity-checked recovery (no heuristic repairs).
- 🤖 **Autonomous agent loops** — model + controlled builtin/MCP tools
  (including `web_search` and `web_fetch`) with budgets, durable effects, and
  streaming events.
- 🪟 **Long-lived tmux windows** — a dedicated tmux server you can start, send,
  attach, interrupt, and stop by stable ID.
- 🧪 **Strict JSON Schema validation** — identical rules across CLI and HTTP;
  unknown fields and ambiguous states fail closed.
- 🌐 **HTTP / SSE control plane** — a loopback `sn-server` exposes the full
  Session / Run / Agent / Model API.

## Quick start

### Install

One-line install from a GitHub Release (no source build needed):

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
```

Or build from source (requires Go 1.25):

```bash
git clone https://github.com/yy003x/runtime.git
cd runtime
make build sn-cli-build
make install
```

Verify the install and the active profiles:

```bash
sn-cli --version
sn-cli profile list      # list active profiles and their types
sn-cli profile check     # validate every profile's structure
```

> **Profiles are user-owned config files.** The bundled profiles under
> `~/.sn/configs/*.json` are **working examples** wired to specific providers
> and models (e.g. `api-cx` → Aliyun qwen, `api-cc` → GLM). To actually call a
> model, supply your own key via the referenced environment variable (e.g.
> `ALIYUN_API_KEY`) or edit the profile to point at your own endpoint and model.
> See [Configuration](#configuration) and the
> [sn-cli reference](SN-CLI-USAGE.md).

### Your first calls

```bash
# One model API call (needs the profile's referenced env var set)
sn-cli req api-cx "Reply OK"

# Open the Codex/Claude interactive TUI
sn-cli cx

# Run a CLI one-shot and wait for it to exit
sn-cli exec cx "Summarize this repo"
```

### A recorded session

```bash
# Run one recorded request, then reuse the same session across API profiles
sn-cli --json session req api-cx "First turn"     # grab session_id from JSON
sn-cli session req api-cc --session-id <session_id> "Second turn"
sn-cli session messages --session-id <session_id> # read the history
```

### A durable background run

`--queue` only enqueues — a worker must be running to dequeue. Start the server
first:

```bash
sn-cli --json server start
sn-cli --json session exec cx-deep --queue --task-id analysis --cwd "$PWD" "Run in background"
sn-cli run watch --run-id <run_id>     # stream events until it settles
```

### A tmux-backed durable session console

```bash
sn-cli --json session open cx --cwd "$PWD" "Inspect this repository"
sn-cli session send --session-id <session_id> "Continue with the next step"
sn-cli session attach --session-id <session_id>
sn-cli session close --session-id <session_id>
```

The window is only the terminal carrier. Every prompt consumed by the console
is executed as a durable Session Run and produces canonical Turn, Execution,
Message, and Event facts. `send` acceptance is not consumption or completion;
inspect `session events`,
`session messages`, or the resulting Run facts.

### An autonomous agent loop

```bash
sn-cli agent api-cc \
  "Find the latest Codex CLI release, read its official release page, and summarize the main changes."
```

The full set of commands, arguments, and end-to-end workflows is in the
[sn-cli reference](SN-CLI-USAGE.md).

## Concepts

One CLI, several execution boundaries. Each entry has a clearly defined scope
and persistence target:

```text
sn-cli <cli-id> ────────> Command Bridge ─> interactive CLI process
sn-cli exec <cli-id> ───> Command Bridge ─> one-shot CLI process
sn-cli req <api-id> ────> Model Core ─────> one HTTP/SSE request

sn-cli session exec|req ─> Session Service ─> command or model
sn-cli session open ... ─> Session/Run ─────> tmux-backed console
sn-cli tmux ... ───────> Tmux Service ─────> raw interactive command window
sn-cli agent <api-id> ─> Agent Kernel ─────> model + configured tools
sn-cli run ... ────────> Run Harness ──────> SQLite WAL control plane
```

| Entry | Purpose | Persisted |
|---|---|---|
| `sn-cli <cli-profile-id>` | interactive CLI direct call | local `cli.jsonl`; no Session/Run |
| `sn-cli exec <cli-profile-id>` | non-interactive CLI one-shot | local `cli.jsonl`; no Session/Run |
| `sn-cli req <api-profile-id>` | one API request | local `api.jsonl`; no Session/Run |
| `sn-cli session exec\|req <profile-id> [--queue]` | Session / Turn / Message / Event / Execution | file-based session; local execution log; optionally durable run |
| `sn-cli session open\|send\|attach\|interrupt\|close` | durable multi-turn Session console | Session facts + one durable Run per consumed prompt; opaque tmux binding |
| `sn-cli tmux ...` | dedicated tmux interactive window | tmux registry and local CLI log (no transcript) |
| `sn-cli agent <api-profile-id> [--queue]` | API-only model/tool loop | durable run; local API log per round (session optional) |
| `sn-cli run ...` | query and control existing durable runs | SQLite WAL |

Key boundaries worth remembering up front:

- A **profile** is `cli` (wraps a CLI) or `api` (calls a model). The namespace
  selects the execution contract and `type` validates that the profile belongs there.
- **Sessions never auto-execute tool calls** — a tool call from the model pauses
  the turn at `requires_action`. Autonomous tool loops belong to `agent`.
- Raw **`tmux` never creates a session**. `session open` is the explicit
  composition layer: tmux carries the console while Session/Run remain canonical.
- **Submitting a run doesn't start the server** — enqueue and worker are decoupled.

Best-effort execution diagnostics live under
`${SN_CLI_HOME}/logs/YYMMDD/{cli,api}.jsonl`. They are not canonical Session/Run
state and are never used for replay; logging failures never change execution
results. Queries and queue submission do not log—workers log actual execution.

The precise contracts (state machines, crash recovery, digest/drift gating,
filesystem safety model) live in the [contract docs](#documentation); this README
intentionally stays at the usage level.

## Configuration

Profiles are a single config layer — one JSON file per profile:

```text
source/payload configs/<profile-id>.json     → <runtime-home>/configs/<profile-id>.json
source/payload resources/tools/<tool>.json  → <runtime-home>/tools/<tool>.json
source/payload release/runtime.json         → <runtime-home>/runtime.json
source/payload resources/schema/*.json      → <runtime-home>/resources/schema/*.json
source/payload release/tmux.conf            → <runtime-home>/resources/tmux.conf
source/payload release/release.json         → <runtime-home>/resources/release.json
```

The repository source tree and every release archive use the same left-hand
layout. Activation is the only owner of the mapping into the active home; the
active home is not a source or archive template.

The profile ID is the filename without `.json`. A CLI profile wraps a command;
an API profile points at a provider:

```jsonc
// configs/cx.json — a CLI profile wrapping the Codex CLI
{
  "type": "cli",
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "xhigh",
  "env": { "CODEX_HOME": "${HOME}/.codex-aip" }
}

// configs/api-cx.json — an API profile calling a model endpoint
{
  "type": "api",
  "driver": "openai",
  "base_url": "https://your-provider/compatible-mode",
  "model": "your-model",
  "headers": { "Authorization": "${YOUR_API_KEY}" },
  "parameters": { "max_tokens": 16384 },
  "timeout": "5m"
}
```

Secrets are referenced via `${VAR}` in `headers` and expanded from environment
variables at call time — the profile only stores the reference name, never the
value. The openai driver auto-prepends the `Bearer ` scheme to a bare
`Authorization` value; the anthropic driver does not. `runtime.json` selects
the agent tools, budgets, scheduler, and run retention; source
`resources/tools/` ships `web_search` and `web_fetch` MCP manifests using
`Z_AI_API_KEY`. Full field reference, override order, and
examples: [sn-cli reference](SN-CLI-USAGE.md) and
[configuration contract](docs/configuration.md).

## Project layout

```text
agent/       autonomous model/tool loop (Agent Kernel)
command/     CLI Command Bridge domain
contract/    provider-neutral request / event / error contract
model/       single model call + API profile domain
profile/     command/model catalog facade
run/         durable run application domain (SQLite)
session/     local canonical session + context projection
tmux/        dedicated tmux server / window management
provider/    openai/ + anthropic/ drivers
store/sqlite/  run store adapter
transport/   http/ (HTTP/SSE) adapter
internal/    cli adapter, runtime bootstrap, config loader, builtin/MCP tools
cmd/         sn-cli, sn-server entry points
configs/     source CLI/API profiles
resources/   schema/ + source tools/ (future skills/ and mcp/ assets stay here)
release/     runtime.json, tmux.conf, and release.json payload templates
```

Domain packages don't read CLI args, don't open the config dir, and don't depend
on HTTP. `internal/runtimebootstrap` is the composition root; providers, SQLite,
CLI, HTTP, and tool executors are all adapters.

## Documentation

| Document | Scope |
|---|---|
| [sn-cli reference](SN-CLI-USAGE.md) · 中文 | Full CLI command, argument, scenario, and example reference |
| [SN Runtime contract](docs/runtime-contract.md) | Top-level contracts and architecture |
| [CLI routing contract](docs/cli-routing-contract.md) | Command routing and ID rules |
| [Configuration contract](docs/configuration.md) | Profile / runtime config schema |
| [Session & history contract](docs/session-history-contract.md) | Session state machine and crash recovery |
| [Tmux contract](docs/tmux-contract.md) | Tmux window management |
| [Integration architecture](docs/integration-arch.md) | How callers integrate over CLI / HTTP |

## Build & verify

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make release-check SN_CLI_VERSION=v0.1.0
git diff --check
```

Tests and release checks use a temporary `SN_CLI_HOME` and never touch the
active `~/.sn`. See the [Makefile](Makefile) for all targets.

## Contributing

This repo follows Conventional Commits with Chinese subjects and strict
contract/code lock-step: changing a public command requires syncing its tests,
`sn-cli --help`, this README, and the relevant contract docs. The development
workflow, architecture boundaries, and verification gates are in
[`AGENTS.md`](AGENTS.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

Copyright 2026 yangyang.
