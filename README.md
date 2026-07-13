# agent-arch

A Go-based persona-driven single-agent runtime.

## Features
- Create agent sessions from persona YAML profiles
- Treat persona as prompt plus policy
- Use a unified `llm.Client` interface for OpenAI and Anthropic
- Map OpenAI requests to the Responses API shape
- Map Anthropic requests to the Messages API shape
- Assemble model context from persona instruction, rolling summary, retrieval stub, and recent raw turns
- Enforce a hard context budget using reserved response and safety buffers
- Expose a minimal in-memory HTTP API

## Project Layout
- `cmd/server`: HTTP server entrypoint
- `internal/config`: config loading
- `internal/persona`: persona loader and renderer
- `internal/llm`: unified client abstraction and provider adapters
- `internal/memory`: in-memory session memory, compaction, retrieval stub, and context assembly
- `internal/agent`: runtime orchestration
- `internal/transport`: HTTP handlers
- `test`: unit tests for the MVP requirements

## Configuration
The server reads:
- `configs/config.yaml`
- `configs/personas/*.yaml`

Provider credentials come from environment variables referenced by the config file:
```bash
export OPENAI_API_KEY=your-openai-key
export ANTHROPIC_AUTH_TOKEN=your-anthropic-token
```

Current Anthropic-compatible default config is pre-set for MiniMax:
```text
base_url = https://api.minimaxi.com/anthropic
model    = MiniMax-M2.7
```

## Run
```bash
make run
```

Development mode with auto-restart:
```bash
make dev
```

Default address:
```text
:8080
```

## Test
```bash
make test
```

Quick manual Anthropic run:
```bash
export ANTHROPIC_AUTH_TOKEN=your-anthropic-token
sh ./scripts/anthropic_5_turn.sh
```

## Make Targets
```bash
make help
make fmt
make test
make build
make sn-cli-build
make sn-cli-install
make sn-cli-test
make sn-cli-doctor
make run
make dev
make check
```

## sn-cli

`sn-cli` is built in this repository as the local terminal entrypoint for Codex / Claude workflows.

```bash
make sn-cli-build
make sn-cli-install
make sn-cli-test
./cmd/sn-cli-wrapper --help
./cmd/sn-cli-wrapper fake "hello"
./cmd/sn-cli-wrapper cx
./cmd/sn-cli-wrapper cc
./cmd/sn-cli-wrapper providers
./cmd/sn-cli-wrapper update --help
```

Runtime profile execution uses `configs/<profile>.yaml`:

```bash
sn-cli fake "hello"
sn-cli fake --session-id local-dev "hello again"
```

The MVP `fake` provider runs fully in-process and writes run artifacts under `runs/global/runtime/runs/<date>/<run_id>/`, including `request.json`, `resolved_config.json`, `events.jsonl`, `stdout.log`, `stderr.log`, `output.txt`, and `result.json`.

Current migration mode (D1-A): legacy native/shell runtime actions are still delegated to a `sinan` checkout via `SINAN_ROOT`.

Recommended local test flow:
```bash
export ANTHROPIC_AUTH_TOKEN=your-anthropic-token

make run
```

In another terminal:
```bash
export ANTHROPIC_AUTH_TOKEN=your-anthropic-token
sh ./scripts/anthropic_5_turn.sh
```

## Example API Flow
Create an agent:
```bash
curl -s http://localhost:8080/v1/agents \
  -H 'Content-Type: application/json' \
  -d '{"persona_id":"default"}'
```

Chat with the session returned above:
```bash
curl -s http://localhost:8080/v1/chat \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"sess_123","message":"Hello"}'
```

Five-turn Anthropic example:
```bash
curl -s http://localhost:8080/v1/agents \
  -H 'Content-Type: application/json' \
  -d '{"persona_id":"default","provider":"anthropic","model":"MiniMax-M2.7"}'

curl -s http://localhost:8080/v1/chat -H 'Content-Type: application/json' -d '{"session_id":"sess_123","message":"第1轮：请记住，我最喜欢的编程语言是 Go。"}'
curl -s http://localhost:8080/v1/chat -H 'Content-Type: application/json' -d '{"session_id":"sess_123","message":"第2轮：我们聊聊 HTTP API 设计。"}'
curl -s http://localhost:8080/v1/chat -H 'Content-Type: application/json' -d '{"session_id":"sess_123","message":"第3轮：再聊聊上下文裁剪策略。"}'
curl -s http://localhost:8080/v1/chat -H 'Content-Type: application/json' -d '{"session_id":"sess_123","message":"第4轮：总结一下前面的实现约束。"}'
curl -s http://localhost:8080/v1/chat -H 'Content-Type: application/json' -d '{"session_id":"sess_123","message":"第5轮：回答第1轮的问题，我最喜欢的编程语言是什么？"}'
```

Inspect session memory:
```bash
curl -s http://localhost:8080/v1/sessions/sess_123/memory
```

## Notes
- Memory is in-process only for the MVP.
- Retrieval is a stub and currently returns no long-term memory entries.
- Provider-specific request mapping is isolated in `internal/llm/openai` and `internal/llm/anthropic`.
