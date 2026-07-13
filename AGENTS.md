# AGENTS.md

## Project Goal
Build a Go-based single-agent runtime.

Core requirements:
- Create agent instances from persona profiles.
- Support OpenAI and Anthropic through a unified LLM adapter.
- Implement context memory management up to 128K tokens.
- Keep the architecture extensible for future tools / MCP / workflow support.
- Current phase is MVP only. Do not implement multi-agent orchestration.
- Include `sn-cli` as a local terminal entrypoint for Codex/Claude/session workflows.
- `sn-cli` is in D1-A mode: it is distributed in this repository but still delegates native runtime actions to a checked-out `sinan` runtime via `SINAN_ROOT`.

## Architecture Principles
1. Persona is not just a prompt string.
   Persona = Prompt + Policy.
2. Business code must not directly depend on provider-specific request formats.
3. Memory assembly must be explicit, testable, and bounded by token budget.
4. Final model context is dynamically assembled from:
   - persona/system instruction
   - recent turns
   - rolling summary
   - retrieved long-term memory
5. Keep the assembled input under 131072 tokens.
6. Always reserve output token budget and safety buffer.

## Scope for MVP
Implement:
- persona YAML loading
- persona rendering
- unified llm.Client interface
- OpenAI adapter
- Anthropic adapter
- short-term memory store
- rolling summary compaction
- context assembly with token budget trimming
- minimal HTTP API:
  - POST /v1/agents
  - POST /v1/chat
  - GET /v1/sessions/{session_id}/memory

Do not implement yet:
- MCP
- tool calling
- multi-agent workflows
- vector database
- auth/rbac
- frontend
- deployment manifests
- asynchronous job system

## Code Style
- Always pass context.Context
- No global mutable state
- Return wrapped errors with context
- Keep package boundaries clean
- Prefer small interfaces
- Avoid unnecessary abstraction
- Keep provider-specific mapping isolated in provider adapters
- Write unit tests for core logic
- No real remote API calls in unit tests

## Memory Policy
- Max context budget: 131072 tokens
- Reserve response tokens
- Reserve safety buffer
- Keep recent turns in raw form
- Compress older turns into rolling summary blocks
- Retrieval memory can be a stub in MVP
- Summary blocks should be structured, not only free text

## Testing Requirements
Add tests for:
- persona loader
- provider selection from config/persona
- memory compaction threshold behavior
- context trimming under token budget
- runtime with mocked llm client

## Implementation Order
1. config
2. persona
3. llm abstraction
4. provider adapters
5. memory manager
6. agent runtime
7. HTTP handlers
8. tests
9. README

## Deliverables
- Clean project structure
- Compilable Go code
- Minimal working HTTP service
- Config-driven provider switching
- Persona-driven agent creation
- Memory manager with 128K token policy
- README with run instructions
