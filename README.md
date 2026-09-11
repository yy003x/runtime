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

- Wrap **Codex / Claude / Grok** CLIs behind a typed profile, or call a **model API**
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

- Developers who wrap Codex/Claude/Grok CLIs and want real session/run management
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
- 🪟 **Long-lived native TUI sessions** — provider TUI in a private default or
  isolated tmux PTY; open, send, attach, interrupt, and close by Runtime Session ID.
- 🧪 **Strict JSON Schema validation** — identical rules across CLI and HTTP;
  unknown fields and ambiguous states fail closed.
- 🌐 **HTTP / SSE control plane** — a loopback `sn-server` exposes the full
  Session / Run / Agent / Model API, with `/healthz` and execution-aware
  `/readyz` probes.

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

Source `make install` first closes every Session-bound native TUI, then stops
the managed `sn-server`. It does not restart the server. Any remaining tmux
carrier or live Runtime binary process blocks the install.

Verify the install and the active profiles:

```bash
sn-cli --version
sn-cli help session      # detailed help for one public command topic
sn-cli doctor            # check profiles, tools, run store, logs, and tmux
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
sn-cli call api-cx "Reply OK"

# Inspect the actual model request and response (two bare, redacted JSON values)
sn-cli call api-cx --raw "Reply OK"

# Open the Codex/Claude/Grok interactive TUI
sn-cli cx
sn-cli gk

# Run a CLI one-shot and wait for it to exit
sn-cli exec cx "Summarize this repo"
```

`call` defaults to assistant text. `--raw` prints two bare JSON values after
completion: the last HTTP attempt's Provider-encoded request body, then the original
response JSON (an array of original frames for SSE). No Runtime metadata, `attempts`
wrapper or extra `error` field is added. A Provider error JSON keeps its own fields;
missing or non-JSON responses are represented by `null`. Failures still exit non-zero
with stderr diagnostics, and preflight failures produce no raw values. `--raw` cannot
be combined with `--json` or `--stream`. All attempts, HTTP metadata and the final
Runtime error are kept in one best-effort `api.jsonl` record per logical call,
regardless of output mode. Credentials are redacted, but model content may remain
sensitive. Capture is not byte-exact HTTP and interrupted responses may be partial.

### A recorded session

```bash
# Run one recorded request, then reuse the same session across API profiles
sn-cli --json session call api-cx "First turn"     # grab session_id from JSON
sn-cli session call api-cc --session-id <session_id> "Second turn"
sn-cli session messages --session-id <session_id> # read the history
```

### A durable background run

`--queue` only enqueues — a worker must be running to dequeue. Start the server
first:

```bash
sn-cli --json server start
sn-cli --json session exec cx-deep --queue --task-id analysis --cwd "$PWD" "Run in background"
sn-cli job watch --run-id <run_id>     # stream events until it settles
```

### A tmux-backed native TUI Session

```bash
sn-cli --json session open cx --cwd "$PWD" "Inspect this repository"
sn-cli session send --session-id <session_id> "Continue with the next step"
sn-cli session attach --session-id <session_id>
sn-cli session close --session-id <session_id>
# Or close every bound native TUI window while retaining its Session fact:
sn-cli session close-all
```

`session open` launches the CLI Profile's provider-native interactive mode
directly inside a tmux PTY and publishes an `interface=native_tui` Session fact
with an opaque tmux binding. It opens detached by default; add `--attach` for an
immediate human attach. `session send` injects raw input into the TUI, so
`accepted=true` only confirms that tmux accepted the transport operation.
`session open` also creates one running `kind=native_tui` durable Run and one
opaque lifecycle Execution. Provider exit settles that Run and automatically
closes the tmux window; `session close` settles it as cancelled before stopping
the window, forwards termination to the Provider, forces exit after a bounded
grace period, and waits for the supervisor to exit. TUI input/output remains provider-owned and creates no canonical
Turn, Message, Event, or transcript.

Discover native TUI Sessions and inspect their lifecycle through the Session
owner:

```bash
sn-cli --json session list --interface native_tui
sn-cli --json session show --session-id <session_id>
```

The tmux server/window registry is a private PTY carrier. There is no
public `sn-cli tmux` namespace or compatibility alias; all native TUI
mutations use the owning Runtime Session ID.

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
sn-cli call <api-id> ────> Model Core ─────> one HTTP/SSE request

sn-cli session exec|call ─> Session Service ─> command or model
sn-cli session open ... ─> native_tui Session ─> provider TUI in tmux PTY
sn-cli agent <api-id> ─> Agent Kernel ─────> model + configured tools
sn-cli job ... ────────> Run Harness ──────> SQLite WAL control plane
```

| Entry | Purpose | Persisted |
|---|---|---|
| `sn-cli <cli-profile-id>` | interactive CLI direct call | local `cli.jsonl`; no Session/Run |
| `sn-cli exec <cli-profile-id>` | non-interactive CLI one-shot | local `cli.jsonl`; no Session/Run |
| `sn-cli call <api-profile-id>` | one API request | local `api.jsonl`; no Session/Run |
| `sn-cli session exec\|call <profile-id> [--queue]` | Session / Turn / Message / Event / Execution | file-based session; local execution log; optionally durable run |
| `sn-cli session open\|send\|attach\|interrupt\|close\|close-all` | provider-native TUI with Runtime identity | `interface=native_tui` Session + opaque lifecycle Run/Execution and tmux binding; no canonical transcript |
| `sn-cli agent <api-profile-id> [--queue]` | API-only model/tool loop | durable run; local API log per round (session optional) |
| `sn-cli job ...` | query and control existing durable runs | SQLite WAL |

Key boundaries worth remembering up front:

- A **profile** is `cli` (wraps a CLI) or `api` (calls a model). The namespace
  selects the execution contract and `type` validates that the profile belongs there.
- **Sessions never auto-execute tool calls** — a tool call from the model pauses
  the turn at `requires_action`. Autonomous tool loops belong to `agent`.
- The public owner of a provider-native TUI is always a **`native_tui`
  Session**. tmux remains the private PTY carrier and the provider still owns
  interaction semantics.
- `managed` Sessions from `session exec|call` and `native_tui` Sessions from
  `session open` cannot share a Session ID.
- **Submitting a run doesn't start the server** — enqueue and worker are decoupled.
- **Readiness follows the execution plane** — an unexpected worker or reaper
  exit makes the server unready and shuts it down; while unready, new durable
  submissions receive `503` without blocking existing-run control requests.

All diagnostics stay below `${SN_CLI_HOME:-~/.sn}/logs`: Profile execution
records use `YYMMDD/{cli,api}.jsonl`, redacted CLI/HTTP control-plane audit uses
`YYMMDD/audit.jsonl`, and the server process writes `sn-server.log`. None is
canonical Session/Run state or used for replay. Audit records contain only the
normalized action, stable target IDs, outcome, and error/status identity—never
prompt/send content or resolved secrets.

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

`runtime.json` sets `tmux.server_mode` to `default` (the default native tmux
server, visible to `tmux list-sessions`) or `dedicated` (the isolated Runtime
socket). Both modes use the fixed `sn-session` name and preserve the same
`tmux_id`-based management contract.

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
  "driver": "openai-chat-completions",
  "base_url": "https://your-provider/compatible-mode/v1",
  "model": "your-model",
  "headers": { "Authorization": "${YOUR_API_KEY}" },
  "parameters": { "max_tokens": 16384, "stream": true },
  "timeout": "5m"
}
```

For canonical generation, API `parameters` supports `max_tokens`, `temperature`, `top_p`, `stop_sequences`,
`stream`, and native option defaults under `extra_body`. `stream` is an optional
boolean (not `null`), defaulting to `true`:
`true` requests SSE, while `false` sends `stream:false` for a regular JSON response.
Both use the same HTTPS POST endpoint. `openai-chat-completions` omits
`stream_options` when false unless explicitly supplied through `extra_body`;
API-key adapters decode SSE or JSON according to the actual response Content-Type;
Codex OAuth always uses SSE for successful responses, as described below.
This Profile setting is independent of CLI `--stream`, `--json`, and `--raw`.
CLI `--stream` selects Runtime event output; the existing retry layer buffers
each attempt and publishes events only after success, not in real time.

API keys are referenced via `${VAR}` in `headers` and expanded from environment
variables at call time — the profile only stores the reference name, never the
value. The `openai-chat-completions` and `openai-responses` drivers auto-prepend
the `Bearer ` scheme to a bare `Authorization` value; the `anthropic-messages`
driver does not. `runtime.json` selects
the agent tools, budgets, scheduler, run retention, and tmux server mode; source
`resources/tools/` ships `web_search` and `web_fetch` MCP manifests using
`Z_AI_API_KEY`. Full field reference, override order, and
examples: [sn-cli reference](SN-CLI-USAGE.md) and
[SN Runtime contract](docs/runtime-contract.md).

## API protocols

`base_url` is the complete API prefix, including the provider's version path.
Runtime only appends `chat/completions` (`openai-chat-completions`), `responses`
(`openai-responses`), or `messages` (`anthropic-messages`). It never adds or
detects a version. For example,
`https://example.com/api/v3` becomes `https://example.com/api/v3/chat/completions`.
A complete `endpoint` is used unchanged. Existing unversioned base URLs must be
updated explicitly when their provider requires `/v1`; no automatic migration runs.

Use `driver: "openai-responses"` with `base_url: "https://api.openai.com/v1"`
for Responses. It supports single `call` requests (also stateless Go/HTTP model
generation), SSE/JSON, text, returned function calls, usage, and raw attempt logs.
Canonical requests default to `store:false` and map `max_tokens` to `max_output_tokens`.
SSE requires a terminal Response and projects its output once; intermediate frames
are preserved in raw/logs, not published as real-time canonical deltas.
The canonical Responses projection is intentionally limited to user text and
returned function calls; it is not a limitation of the native protocol.
Session/Agent support is unchanged. Returned function calls are never automatically executed.

`--raw` prints the actual outbound request body and captured upstream response body.
Responses uses `input` / `output`; Chat Completions uses `messages` / `choices`.
Fields such as `reasoning`, `usage`, `metadata`, and provider extensions are retained
from the upstream response, not added by Runtime or converted into Chat Completions.

Native generation options use `parameters.extra_body` in an API Profile for
defaults and canonical `options.extra_body` for per-request overrides. These keys
are merged into the actual HTTP body top level. `null`, `0`, and `false` are sent
unchanged; omitted options use the provider's defaults. Overrides replace the
whole value, not individual nested properties. For quick single-call experiments:

```bash
sn-cli call api-resp --raw --param 'reasoning={"effort":"low"}' --param 'metadata=null' "Hello"
sn-cli call api-cx --raw --param 'frequency_penalty=0' --param 'parallel_tool_calls=false' "Hello"
sn-cli call api-cc --raw --param 'thinking={"type":"disabled"}' --param 'top_k=0' "Hello"
sn-cli call api-cx --raw --image ./screenshot.png "What is in this image?"
```

`--param` is repeatable (last value per name wins) and cannot accompany
`--request-file`. `--image` is repeatable and encodes a local jpeg/png/gif/webp
file as canonical `content_parts` (`image_url` data URL). Chat Completions sends
OpenAI vision parts; Anthropic maps them to `type=image`. Responses/Codex reject
`content_parts`. Native values remain subject to upstream API/model support;
`null` and `0` are not universal no-ops. Canonical messages/tools/budgets and
native conversation/multimodal features are not replaced through this extension.
Anthropic thinking is currently single-call only because canonical history has no
native thinking/signature blocks. Full field examples and boundaries are in the
[request options contract](docs/runtime-contract.md#原生请求参数扩展).

For ChatGPT/Codex OAuth, use the included `api-codex` Profile:

Run `sn-cli auth` for the Auth TUI. Type `/` to discover `/login`, `/logout`,
`/status`, `/help`, and `/exit`; Tab completes without executing. `sn-cli auth login`
and `sn-cli auth logout` open the corresponding guide. Logout requires confirmation
(default: cancel); exiting the UI preserves the login. Reauthorization retains the
old credential until the new login is saved. Escape cancels; PgUp/PgDn scroll long
panels. The TUI requires interactive stdin/stdout without `--json` and never creates
a Session, Run, or execution log. Explicit-provider commands below always run directly;
use them for scripts. `sn-cli auth status` also works without a provider and is read-only.

```bash
sn-cli auth login openai-codex
sn-cli auth status openai-codex
sn-cli call api-codex "Hello"
sn-cli auth logout openai-codex
```

Browser login uses PKCE and a loopback callback; use `--device-code` for device
login, or `--no-browser` to open the printed URL manually. You complete the
authorization yourself. Credentials are private plaintext in
`${SN_CLI_HOME:-~/.sn}/state/auth.json` (0600, directory 0700), independently
managed from Codex CLI/Pi, and retained during Runtime updates. `status` reports
the local cache only; `logout` removes local credentials without remote revocation.

Calls automatically refresh access tokens when expired or within 5 minutes of
expiry, and save the rotated credentials for subsequent calls. No background
process is needed. While refresh authorization remains valid, token expiry does
not require another browser login. The TUI shows automatic-refresh status;
`/status` includes the access-token expiry for diagnostics, not a login deadline.

The `openai-codex-responses` driver uses only
`https://chatgpt.com/backend-api/codex/responses`, rejects custom headers and
Session/Agent/history continuation, and always requests SSE with `store:false`.
Successful HTTP responses are decoded and captured as SSE even if Content-Type is
missing or incorrect. `--raw` retains the frame array; logs retain the actual headers.
HTTP errors still preserve their JSON bodies and existing error classification.
If the Codex terminal Response has an empty output array, completed
`response.output_item.done` items supply its output in `output_index` order. A valid
terminal Response is still required; deltas are never duplicated or treated as completion.
`max_tokens`, `top_p`, and stop sequences are unsupported. Expiring credentials
refresh under a process lock; one conditional refresh/retry after HTTP 401 fits
within the existing total attempt limit. OAuth credentials never enter Profiles,
logs, or release archives. This is the Codex subscription backend, whose protocol
can change; see [authentication](https://learn.chatgpt.com/docs/auth) and the
[Runtime contract](docs/runtime-contract.md#codex-oauth-单次调用).

## Project layout

```text
cmd/             sn-cli and sn-server entry points
pkg/             public Go API: agent, command, contract, model, profile,
                 provider, run, session, store, and HTTP transport
internal/
  domain/        private Runtime values and invariants
  application/   activation, bootstrap, and tool/use-case orchestration
  infrastructure/ config, filesystem, log, process, private tmux, and MCP adapters
  interfaces/    inbound CLI adapter
  testkit/       reusable test-only assets
configs/         source CLI/API profiles
resources/       schemas and source tools
release/         runtime.json, tmux.conf, and release.json payload templates
docs/            canonical Runtime contract
scripts/         delivery and verification scripts
```

External Go consumers import `github.com/yy003x/runtime/pkg/...`; legacy root
package paths have no compatibility shim. `internal/application/runtimebootstrap`
is the composition root, and the four internal layers are adapted to a CLI
Runtime + Agent rather than an HTTP-service template. An architecture test
allowlists every direct module dependency of public `pkg/` packages so new
cross-domain or internal-adapter coupling requires an explicit review.

### Workbench integration boundary

`sn-runtime` owns execution, CLI/HTTP contracts, Sessions and durable Runs.
`sn-workbench` owns the application API, Web UI and business modules. Runtime
does not host a second product UI; callers use public CLI/HTTP entry points and
never read or write the active Runtime home directly.

Run lists accept exact `session_id`, `task_id`, and `profile_id` filters in
addition to state/kind, for example
`sn-cli --json job list --task-id <task-id> --limit 50` or
`GET /v1/runs?task_id=<task-id>&limit=50`. Pagination, SSE resume, and
heartbeats follow the [Runtime contract](docs/runtime-contract.md)
(`contract_version=17`).

## Documentation

| Document | Scope |
|---|---|
| [sn-cli reference](SN-CLI-USAGE.md) · 中文 | Full CLI command, argument, scenario, and example reference |
| [SN Runtime contract](docs/runtime-contract.md) | The sole architecture, configuration, CLI/HTTP, Session, Run, Agent, Tmux, and activation contract |

## Build & verify

```bash
make fmt-check
make test-serial
make test-race
make coverage
make coverage-critical
go vet ./...
make release-check SN_CLI_VERSION=v0.1.0
git diff --check
```

Tests and release checks use a temporary `SN_CLI_HOME` and never touch the
active `~/.sn`. See the [Makefile](Makefile) for all targets.

## Contributing

This repo follows Conventional Commits with Chinese subjects and strict
contract/code lock-step: changing a public command requires syncing its tests,
`sn-cli --help`, this README, `README.zh-CN.md`, `SN-CLI-USAGE.md`, and
`docs/runtime-contract.md`. The development workflow, architecture boundaries,
and verification gates are in [`AGENTS.md`](AGENTS.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

Copyright 2026 yangyang.
