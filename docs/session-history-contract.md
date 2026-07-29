# Session 与 History 契约

## 文件布局与 schema

```text
sessions/
  <session_id>/
    session.json
    messages.jsonl
    events.jsonl
    turns/<turn_id>/turn.json
    turns/<turn_id>/context-manifest.json
    executions/<execution_id>.json
    context/current.json
  _system/
    index.json
    trash/<timestamp>/<session_id>/
state/
  session-locks/
  runtime.db
```

Session fact 固定 `schema_version=2`，SQLite 固定 `PRAGMA user_version=2`。schema 1、
unknown、更高或混合状态 fail closed，不提供 runtime reader 或自动 migration。

写入使用 Session-scoped `flock`、regular-file/symlink 门禁、大小限制和 atomic JSON
replace。JSONL sequence 在单 Session 内单调递增；`_system/index.json` 可重建，
不是原始事实。

## Identity

```text
session_id   Runtime conversation
turn_id      一次用户输入及其 terminal result
run_id       一次 durable execution
execution_id 一次具体 API attempt 或 managed process
task_id      caller-owned correlation label
```

ID 不复用。每个 Turn 可选择 API 或 CLI Profile；executor/provider 只能在 Turn
边界切换，active 或 `requires_action` 未闭合时禁止切换。

## Context projection

每个 Turn 先追加原始 user message，再从原始 messages 构造唯一 projection：

- API：保留结构化 role/content/tool_calls/tool_call_id；
- CLI：生成转义、定界、有界的 history/current-input prompt，再在最前面合并
  Profile base prompt；
- manifest 记录 profile、request/config/base-prompt digest、sequence range、
  token estimate、capacity、pressure 和 correlation；
- overflow Turn 仍记录 user input、failed 状态和 typed error；
- projection 不改写或删除原始 messages。

Profile base prompt 是 invocation 前缀，不另存为本轮 user Message。最终 CLI
prompt 仍受 96 KiB argv-token 门禁。

## CLI Execution

Session 对 CLI Profile 固定 `exec=true`，不读取 Profile `exec`，也不使用 Tmux。
command adapter 构建 canonical invocation，managed helper 在 marker-before-exec
handshake 后启动 Provider。

Execution 保存：

- executor kind、request/config digest 和 absolute cwd；
- `spawn_intent|running|settled` lifecycle；
- owner/helper PID、PGID 和 process-start identity；
- exit code/signal、typed result/error；
- stdout/stderr observed bytes、prefix digest、truncated、limit-exceeded 和有限
  Runtime summary。

不保存原始 provider stderr、resolved env secret、完整 argv/env 或 partial
assistant。OS exit=0 和 canonical protocol terminal 双门禁通过后，才追加唯一
assistant Message。

Runtime 进程崩溃后，已经可能运行的 child 不自动重放。stale Turn 保持 blocked；
durable Session Run 进入 `needs_reconciliation`。

## Tool result

API Session 返回 canonical tool call 后：

1. Turn 为 `requires_action`，Session 为 blocked；
2. 外部提交原始 `tool_call_id`、content、is_error、idempotency key；
3. 同 key 同 payload 重放返回相同 receipt；
4. key 或 call ID 冲突 fail closed；
5. pending tool 全满足后 Session 回到 idle，下一 Turn 才能继续。

Session 不执行 canonical tool。CLI Provider 进程内部的工具行为是 opaque executor
行为，不进入上述协议。

## Reconciliation

```text
session reconcile --session-id <id>
session reconcile --session-id <id> --terminate
session reconcile --session-id <id> --acknowledge-unknown
```

默认只探测 process identity；`--terminate` 只有 PID、PGID、start token 和 group
leader 全匹配时才发送信号；`--acknowledge-unknown` 用于操作者已经在外部确认没有
存活副作用进程的情况，两 flag 互斥。结案后，durable Run 再通过 `run reconcile`
读取 Session terminal result。所有动作幂等，digest 不匹配为 conflict。

## Private durable payload

`session submit` 在 ingress 冻结 caller-relative prompt file、typed override、
non-secret Profile snapshot 和 digest。公开 Run request 只保留 digest/ref；
snapshot 与解析后的 base prompt 存入 SQLite `private_request_json`，对应 Go 字段
强制 `json:"-"`。

private payload 不得出现在 Run query/result/event、Session export、human output、
日志或 error。worker 不重新读取 prompt file，也不用自己的 cwd 解释输入；Profile
删除或 non-secret config 漂移在 spawn 前 conflict。env secret 只在 worker 执行时
从当前环境解析。

同一 `session_id` 同时只允许一个 queued/running/paused/needs-reconciliation
durable Session Run，由 SQLite 原子约束保证。

## Agent projection 与 retention

Agent 默认不创建 Session。显式 `--session-id` 时，Agent 读取结构化历史并以稳定
correlation 幂等投影新增 assistant/tool messages。SQLite Run 和文件 Session 不建
伪跨 Store transaction。

Retention：

```text
ephemeral | standard | pinned
```

`session gc` 默认 dry-run，只选择超过 cutoff、非 active/blocked 的 ephemeral
Session；`--apply` 在锁内复核后移动到 `_system/trash`。`session delete` 同样是
可恢复移动。
