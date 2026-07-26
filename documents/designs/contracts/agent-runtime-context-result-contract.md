---
design_id: agent-runtime-context-result-contract
design_type: architecture
scope: repo
project_id:
data_status: checked
design_role: contract
implementation_status: implemented
phase_mode: single
owner: wb-design
manager: wb-design
last_updated: 2026-07-26
---

# Agent Runtime 上下文与结果契约

## 定位与 Owner

本仓库是通用 Go Agent Runtime 及其公开 CLI、HTTP、SDK 和持久化契约的权威实现。

- `internal/agentrun`（包括 Service）负责 Session 原始事实、history projection、capacity 决策、context manifest、checkpoint 和 result 到 Session message 的投影。
- `internal/provider` 负责 profile capacity 输入、静态上下文解析、Provider request 装配、snapshot 一致性验证和协议适配。
- `internal/cli`、`internal/transport` 只透传结构化输入，不另行实现 context 策略。
- 调用方拥有业务路由和 caller-owned memory；Runtime 只消费公开输入，不识别业务类型、不写回调用方数据。

本契约不改变顶层 CLI、profile 分流、`--` 语义、direct/managed 边界或 `contract_version=1`。

## Capacity

### 容量来源

`effective_window` 使用 profile 的 `context_window_tokens`；缺失时为保守默认 `32768`，并记录 `capacity_source=conservative_default`。

`profile_effective_reserved` 取以下已存在值的最大值：

- profile `reserved_output_tokens`；
- API profile `max_tokens`；
- 两者都缺失时使用默认 `8192`。

API 请求级 `max_tokens` 必须是正整数。运行期：

```text
effective_reserved_output_tokens =
  max(profile_effective_reserved, request_max_tokens_override)

input_budget_tokens =
  effective_window - effective_reserved_output_tokens
```

较低的请求 override 不扩大 profile 输入预算；较高的 override 收紧预算。`input_budget_tokens` 必须至少为 `2`，否则 profile loader 或 request preflight 返回 `invalid_context_capacity`。非法 request `max_tokens` 返回 `invalid_provider_override`。Runtime 不做 `window/4` 或其它静默修正。

`compaction_at_tokens = max(1, floor(input_budget_tokens * 70 / 100))`。

### Projection 决策

Runtime 固定先判断 hard budget，再判断 proactive threshold：

- `raw > input_budget_tokens`：必须压缩；无法压入预算时返回 `context_overflow`。
- `raw == input_budget_tokens`：输入合法，可主动压缩；失败时回退 raw。
- `compaction_at_tokens < raw < input_budget_tokens`：可主动压缩；失败时回退 raw。
- `raw <= compaction_at_tokens`：直接使用 raw。

`summary_enabled=false` 只禁止 checkpoint；阈值本身不导致失败。只有 hard overflow 且不能压缩时失败。

manifest 的 `pressure_state` 为：

- `below_threshold`
- `threshold_compaction`
- `budget_compaction`
- `raw_fallback`
- `overflow_summary_disabled`
- `overflow_compaction_failed`

失败 projection 仍随 Turn、Attempt 和 Execution 持久化，并写入 `projection_error`。

## 初始输入估算与单次 Projection

Service 在 `BeginRun` 前调用 `provider.EstimateStaticContext`。provider 返回：

- 有序 `counted_components`；
- 有序 `unknown_components`；
- counted 静态内容的只读 snapshot 与总 digest。

counted components 包括：

- history/checkpoint messages；
- current prompt、managed result contract 与 injected memory；
- API Runtime system prompt、已渲染 local skills、Runtime memory；
- Native system prompt/persona；
- 已授权 local tool schema 与内置 memory tool schema；
- Runtime 固定 framing。

unknown components 包括：

- 外部 CLI Provider 自有 system/tool context；
- 动态 MCP `ListTools` 后才可见的 schema；
- 模型 tokenizer special tokens 与其它不可观测 Provider framing。

unknown component 不以 `0` 冒充已计量。`estimation_complete` 是 optional boolean：缺失表示旧 manifest，`false` 表示存在 unknown component，`true` 表示本轮可观测组件已完整计量。估算器为 `utf8_heuristic_v1`，不是 Provider 实际 token usage。

同一 Turn 只生成一个 `contextProjection`；它同时写 manifest 并交付正式 Provider request，不再二次投影。各 CLI、API、Native、tmux adapter 在 `Prepare` 前调用 `ValidateStaticContextSnapshot` 复算静态 digest；发生漂移时返回 `context_inputs_changed`。API/Native 实际装配使用 snapshot 的 system/skill/memory/local tool schema。

后续 agent round 的 assistant/tool result 不属于初始 manifest estimate，由 API/Native engine 的运行期 token budget 管理。

## Result 与 Session Summary

v1 builtin result 保持：

- `summary` 必填且保留旧消费者可读取完整内容的兼容语义；
- `assistant_message` 是 optional additive field；
- `Result.SessionMessage()` 优先读取 `assistant_message`，缺失时回退 `summary`；
- Runtime 自产结果同时把完整内容写入 `assistant_message` 与 `summary`。

Runtime 内部需要完整内容的消费者统一调用 `SessionMessage()`。Loop planner action 解析失败时返回 `invalid_loop_action`，不拼接、不猜测、不自动重试。

Session 短摘要不改变 result.json：

1. 对 `SessionMessage()` 执行 `TrimSpace`；
2. 按 Unicode code point（Go rune）保留前 512 个；
3. 截断时追加单个 `…`；
4. 空内容保持空，不调用模型、不做语义改写。

`SessionRecord.summary_source` 为：

- `from_session_message`：存在可用 assistant content；
- `from_imported_summary`：旧 import 没有 assistant content，只能回退已有 summary。

v1 artifacts 校验：

- `artifacts` 必须是 object array；
- `type` 可缺失，存在时必须是非空 string；
- `path`、`uri` 存在时必须是 string；
- Runtime 自产 artifact 始终带非空 `type`；
- custom result schema 由调用方 schema 决定。

## Checkpoint 与 Session Roundtrip

新 checkpoint ref 固定为相对 Session root 的：

```text
context/checkpoints/<turn_id>.json
```

`turn_id` 复用 `validateRunID` 字符集和长度约束。ref 必须是 canonical relative path，禁止 `.`、`..`、slash/backslash 逃逸和 symlink 逃逸。

新 digest 规则：

```text
summary_digest_kind = stable_json_sha256
summary_digest = sha256(json.Marshal(ContextCheckpoint))
```

`ContextCheckpoint` 是不含 map 的强类型 struct；digest 使用 Go `encoding/json.Marshal` compact bytes，不使用带缩进的文件 bytes。

Session export：

- 遍历全部 Turn manifest；
- 校验 ref、Session/Turn identity 和 digest；
- 导出按 Turn ID 索引的 `context_checkpoints`；
- 兼容读取仍位于同一 Session canonical path 的旧绝对 ref 与 file-byte digest；
- 导出副本单向归一化为 stable digest，并用 `legacy_summary_digest` 保留不同的旧值；
- 无效 checkpoint 清空 ref/digest/range、设置 `compacted=false` 并记录 `checkpoint_error`。

Session import：

- 只创建不存在的 Session，不合并非空 Session；
- 在写入前拒绝非法/重复 Turn、Attempt、Execution 和 orphan checkpoint 引用；
- 对 checkpoint identity/digest 不一致只清洗对应 manifest，其它 Turn 可继续导入；
- 写入 canonical relative ref；
- 只从 sequence 最新的有效 checkpoint 重建 `context/current.json`；
- 没有有效 checkpoint 时不创建 current。

稳定 `checkpoint_error` 分类：

- `checkpoint_missing`
- `checkpoint_digest_mismatch`
- `checkpoint_identity_mismatch`
- `checkpoint_ref_invalid`
- `checkpoint_ref_outside_session`

Projection 始终从 Session 原始 messages 重建，不解引用历史 manifest checkpoint；checkpoint 缺失只影响 audit/export 完整性。

## Metadata 安全

result.json 与显式 result CLI/HTTP 读取是原始 Run 管理面，不静默改写 producer 事实。

所有进入 Session history 的 metadata 必须经过 `redactMetadata`：

- `CompleteRun` 写 assistant message 前；
- `AppendMessage`；
- `Import` 的 message metadata 和 event data。

递归脱敏覆盖 `map[string]any`、`map[string]string`、`[]any` 和 `[]map[string]any`。包含 `token`、`secret`、`password`、`authorization` 或 `cookie` 的 key 写为 `[REDACTED]`。因此原始 artifacts 不会未经脱敏传播到 Session export 或普通 Session/UI projection。

## 配置边界

当前发行 profile 未声明经权威来源确认的真实模型 context window，因此继续使用 `conservative_default=32768`；不得根据模型印象写入更大容量。

JSON Schema 表达字段类型和正整数约束；`reserved/max_tokens/window` 的跨字段关系由 profile loader 与 request preflight 校验。

## 验证门禁

相关改动至少通过：

- `make fmt-check`
- `make sn-cli-test`
- `go vet ./...`
- `make test-serial`
- `make release-check SN_CLI_VERSION=<valid-semver>`
- `git diff --check`
