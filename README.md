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
- 🤖 **Autonomous agent loops** — model + configured builtin tools (`read_file`,
  `list_directory`, opt-in `write_file`) with budget limits and streaming events.
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
# One model API call (needs the profile's auth env var set)
sn-cli api-cx "Reply OK"

# Open the Codex/Claude interactive TUI
sn-cli cx

# Run a CLI one-shot and wait for it to exit
sn-cli cx --exec "Summarize this repo"
```

### A recorded session

```bash
# Run one recorded turn, then reuse the same session across turns/providers
sn-cli --json session run api-cx "First turn"     # grab session_id from JSON
sn-cli session run --session-id <session_id> api-cc "Second turn"
sn-cli session messages --session-id <session_id> # read the history
```

### A durable background run

`session submit` / `run submit` only enqueue — a worker must be running to
dequeue. Start the server first:

```bash
sn-cli --json server start
sn-cli --json session submit --task-id analysis --cwd "$PWD" cx-deep "Run in background"
sn-cli run watch --run-id <run_id>     # stream events until it settles
```

### An autonomous agent loop

```bash
sn-cli agent run --profile api-cx --max-wall-time 20m "Review this repo and report"
```

The full set of commands, arguments, and end-to-end workflows is in the
[sn-cli reference](SN-CLI-USAGE.md).

## Concepts

One CLI, several execution boundaries. Each entry has a clearly defined scope
and persistence target:

```text
sn-cli <id> ──────────┐
sn-cli profile <id> ──┴─┬─ type=cli ─> Command Bridge ─> CLI process
                        └─ type=api ─> Model Core ─────> HTTP/SSE

sn-cli session ... ───> Session Service ──> command or model
sn-cli tmux ... ───────> Tmux Service ─────> interactive command window
sn-cli agent run ─────> Agent Kernel ─────> model + configured tools
sn-cli run ... ───────> Run Harness ──────> SQLite WAL
```

| Entry | Purpose | Persisted |
|---|---|---|
| `sn-cli <profile-id>` | one CLI/API profile call (no record) | — |
| `sn-cli profile <profile-id>` | identical to the implicit form | — |
| `sn-cli session run\|submit` | Session / Turn / Message / Event / Execution | file-based session |
| `sn-cli tmux ...` | dedicated tmux interactive window | tmux registry (no transcript) |
| `sn-cli agent run` | API-only model/tool loop | durable run (session optional) |
| `sn-cli run ...` | durable run queue & control plane | SQLite WAL |

Key boundaries worth remembering up front:

- A **profile** is `cli` (wraps a CLI) or `api` (calls a model). `type` decides the adapter.
- **Sessions never auto-execute tool calls** — a tool call from the model pauses
  the turn at `requires_action`. Autonomous tool loops belong to `agent run`.
- **Tmux never creates a session** — it only manages an interactive window.
- **Submitting a run doesn't start the server** — enqueue and worker are decoupled.

The precise contracts (state machines, crash recovery, digest/drift gating,
filesystem safety model) live in the [contract docs](#documentation); this README
intentionally stays at the usage level.

## Configuration

Profiles are a single config layer — one JSON file per profile:

```text
<runtime-home>/configs/<profile-id>.json   # source: configs/*.json
<runtime-home>/runtime.json                # source: configs/runtime/runtime.json
<runtime-home>/resources/                  # JSON schemas, tmux.conf, release.json
```

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
  "driver": "openai-compatible",
  "base_url": "https://your-provider/compatible-mode",
  "model": "your-model",
  "auth": { "header": "Authorization", "scheme": "Bearer", "from_env": "YOUR_API_KEY" },
  "defaults": { "max_tokens": 16384 },
  "timeout": "5m"
}
```

Secrets are read only from environment variables (`auth.from_env`) — never
written into profile files. `runtime.json` configures the agent's builtin tools,
budgets, scheduler, and run retention. Full field reference, override order, and
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
internal/    cli adapter, runtime bootstrap, config loader, builtin tools
cmd/         sn-cli, sn-server entry points
configs/     source CLI/API profiles + runtime template
resources/   strict JSON schemas, tmux.conf, release.json
```

Domain packages don't read CLI args, don't open the config dir, and don't depend
on HTTP. `internal/runtimebootstrap` is the composition root; providers, SQLite,
CLI, HTTP, and builtin tools are all adapters.

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
