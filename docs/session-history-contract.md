# Session 与 History 契约

## 文件布局

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
```

写入使用 session-scoped `flock`、regular-file/symlink 门禁、大小限制和 atomic JSON
replace。`messages.jsonl`、`events.jsonl` 的 sequence 在单 Session 内单调递增。
`_system/index.json` 是可重建索引，不是原始事实。

## Identity

```text
session_id   Runtime conversation
turn_id      一次用户输入及其输出
run_id       一次可恢复执行
execution_id 一次具体进程或 HTTP attempt
task_id      caller-owned correlation label
```

ID 不复用。可选 caller ID 必须通过相同格式校验。

## Context Projection

每个 Turn 先追加原始 user message，再从当前 Session 原始 messages 构造唯一
projection 和 manifest。

- model：保留结构化 role/content/tool_calls/tool_call_id；
- command：生成 `<runtime_session_history>` 和 `<current_user_input>` 定界块，
  JSON 与 HTML 双重转义，最大 256 KiB；
- manifest 记录 profile/config/message/input digest、sequence range、估算 token、
  capacity、pressure 和 correlation；
- overflow Turn 仍记录 user input、失败状态和 typed error；
- projection 不改写或删除原始 messages。

## Tool result

model Session 返回 tool call 后：

1. Turn 状态为 `requires_action`；
2. Session 状态为 `blocked`；
3. 外部提交 `tool_call_id`、content、is_error、idempotency_key；
4. 同 key 同 payload 重放返回同一 receipt；
5. key 或 call ID 冲突 fail closed；
6. pending tool 全部满足后 Session 回到 idle，下一 Turn 才能继续。

Session 本身从不执行 tool。

## Agent projection

Agent 默认不创建 Session。显式 `--session-id` 时，Agent 读取结构化历史，并将新增
assistant/tool messages 以稳定 `run_id/turn_id/execution_id` 幂等投影回来。
SQLite Run 和文件型 Session 不跨 Store 建立伪事务；崩溃后通过 correlation 重放，
不确定时进入 `needs_reconciliation`。

## Retention

```text
ephemeral | standard | pinned
```

`session gc` 默认 dry-run，只选择超过 cutoff、非 active/blocked 的 ephemeral
Session；`--apply` 再次在锁内核对后移动到 `_system/trash`，不永久删除。
显式 `session delete` 同样是可恢复移动。
