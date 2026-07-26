# Session、History 与 Context Runtime 契约

> 状态：现行规范。实现、CLI、HTTP、Workbench BFF 和测试必须与本文一致。

## 1. Owner 与目标

Go Runtime 是会话输入、输出、运行关系和上下文清单的唯一 owner：

- `Session` 表示跨 Provider、跨进程、跨多轮的逻辑会话。
- loop 显式关联既有 Session 后，planner 与 `run_agent` Turn 复用相同 Session；Runtime 在启动阶段校验 Session 存在且 project 一致。
- `Turn` 表示一次用户输入及其响应生命周期。
- `RunAttempt` 表示某个 Turn 的一次 Provider 尝试。
- `Execution` 表示 API、managed CLI、tmux 或 terminal 执行容器；交互 carrier Execution 可以跨 Turn。
- `result.json` 仍是单次 run 的不可变结果；Session 通过 `result_ref` 引用，不复制结果事实。
- `History` 是从 Session 构建的可重建查询视图，不是第二套事实源。
- Workbench UI/BFF 通过 Runtime CLI/HTTP 访问会话数据。

关系如下：

```text
Session
  └── Turn
        └── RunAttempt ──result_ref──> runs/.../result.json

Execution(API/cli_managed/tmux/terminal) 与 Turn 正交关联
```

## 2. 目录与事实源

```text
~/.sn/
├── sessions/<date>/<session_id>/
│   ├── session.json
│   ├── messages.jsonl
│   ├── events.jsonl
│   ├── context/
│   │   ├── current.json
│   │   └── checkpoints/<turn_id>.json
│   ├── memory/{working.json,candidates.json}
│   ├── turns/<turn_id>/
│   │   ├── turn.json
│   │   ├── context-manifest.json
│   │   └── attempts/<run_id>.json
│   └── executions/<execution_id>.json
├── runs/<run_type>/<date>/<run_id>/
│   ├── request.json
│   ├── status.json
│   ├── events.jsonl
│   ├── output.log
│   └── result.json
├── history/
│   ├── index.json
│   └── trash/<timestamp>/<session_id>/
├── state/
│   ├── runs/
│   └── sessions/locks/
└── memory/                         # 显式管理的全局 capability
    ├── durable.json
    └── candidates.json
```

边界：

- `sessions/` 是持久化会话事实源；删除采用移动到 `history/trash`，可恢复。
- `runs/` 是一次执行的 artifact，不保存长期会话正文。
- `history/index.json` 仅用于 list/filter；损坏或丢失可由 Store 从 Session 事实重建，不暴露额外 CLI 层级。
- `state/` 只保存活动锁、registry、lease 等运行态。
- Session 正文只保存在 `sessions/`，UI 直接读取规范化会话数据。

## 3. 标识与状态

`session_id`、`turn_id`、`run_id`、`execution_id` 是独立标识。`session open` 创建 carrier 时不得复用 Session ID、Run ID 与 Execution ID；关系只能通过持久记录显式关联。

Run 状态继续使用：

```text
pending | running | result_pending | done | failed | blocked | cancelled
```

tmux 无法可靠判断模型完成时，Turn 记录为 `submitted`，并标记 `capture_quality=transcript_only`，不得伪装为结构化完成。

Session 独立使用 `idle | active | blocked | archived`，不得把最后一个 Run 的 `done/failed/cancelled` 直接写成 Session 状态；并发 Turn 中任一活动即为 `active`。

## 4. 记录与保留策略

`SessionPolicy` 在 Runtime service 层统一决策，CLI parser、HTTP handler、Provider 不得各自发明默认值。

记录模式：

- `full`：记录规范化消息、关系和事件。
- `metadata`：只记录生命周期、Provider、时间、状态，不保存 prompt/output。
- `off`：不创建逻辑 Session。

保留策略：

- `ephemeral`：一次性任务；只允许通过显式 GC 回收，不在启动路径静默删除。
- `standard`：普通多轮会话。
- `pinned`：明确长期保留。

`session gc` 默认 dry-run，只选择早于 cutoff 且状态为 `idle|archived` 的
ephemeral Session。`--apply` 在 Session 文件锁内再次核对 retention、更新时间与
状态，然后把完整目录移动到 `history/trash`；不会直接不可恢复删除，也不会回收
active/blocked、standard 或 pinned Session。

默认值：

| 入口 | record_mode | retention | capture_quality |
| --- | --- | --- | --- |
| 普通 HTTP/API Run | off | - | structured |
| 顶层 CLI profile native direct | off | - | - |
| 顶层 API profile direct request | off | - | - |
| `profile exec` | off | - | - |
| `skill run` | off | - | - |
| `session run|submit` | full | standard | structured/parsed |
| `session open --carrier tmux` | full | standard | transcript_only |
| `session open --carrier terminal` | full | standard | transcript_only |
| help/version/profile validate | off | - | - |

顶层 native direct、direct API request、`profile exec` 和 `skill run` 都不进入 Session；需要记录必须显式使用 `session run|submit|open`。

## 5. result 复用

Session 不复制 `result.json`：

```json
{
  "run_id": "turn-...",
  "run_type": "turn",
  "result_file": "~/.sn/runs/turn/.../result.json",
  "result_digest": "sha256:..."
}
```

UI 消息正文已经规范化保存在 `sessions/.../messages.jsonl`，因此 run artifact 清理不会让会话列表失去基本展示能力；大对象、日志和业务输出只保存引用。

tmux 退出时也写公共 `result.json`，但固定标记 `result_kind=execution_summary`、`capture_quality=transcript_only`；该结果只描述执行容器，不冒充模型 final answer。

managed Run 的 `result.json` 用 optional `assistant_message` 承载完整用户可见答复；`contract_version=1` 下 `summary` 保留旧消费者可读取完整内容的兼容语义。Session assistant message 统一读取 `assistant_message || summary`，再按 `TrimSpace` 后最多 512 rune、截断追加单个 `…` 的规则派生 metadata 与列表摘要。`SessionRecord.summary_source` 标记 `from_session_message`；只有旧 import 没有可用 assistant content 时才回退为 `from_imported_summary`。

## 6. ContextCompiler

每个 managed Turn 写入 `context-manifest.json`，记录：

- 被纳入上下文的 message sequence range 与 digest。
- profile config digest。
- `allowed_actions` / `forbidden_actions` policy digest。
- Skill ID、源路径与内容 digest。
- Runtime 静态可确定且实际授权的 local Tool schema digest；动态 MCP tools 由 profile config digest 与 Provider lifecycle events 补足。
- Memory read ID、类型、来源与内容 digest。
- profile 声明或保守默认的总容量、effective 输出预留、输入预算、压缩阈值、估算输入 token、`capacity_source` 与 `pressure_state`。
- counted/unknown 静态上下文组件、估算完整性和 estimator source。
- 触发压缩时的 Session 相对 checkpoint ref、stable JSON digest、覆盖 message sequence range 与异常清洗原因。

manifest 不写 token、secret、cookie、Authorization、完整环境变量或 tool 参数原文。Provider 事件进入 Session history 前再次做敏感 key 脱敏。

Runtime 从完整的规范化 `user/assistant` Session 记录投影本轮 Provider 输入：API 使用结构化 messages，managed CLI 使用带边界标记的 history block。切换 Provider 时使用本轮 profile 重新计算容量和投影，不复用 Provider 私有 snapshot。

输入估算包含投影后的历史、当前 prompt、只读 injected memory、managed result contract、可观测 system/skill/runtime memory/local tool schema 和固定 framing；外部 CLI context、动态 MCP schema 与模型 tokenizer 特殊成本作为 unknown component 记录，不以 `0` 冒充已计量。counted 静态组件同时形成只读 snapshot；各 Provider adapter 在 `Prepare` 前复算总 digest，发生漂移时以 `context_inputs_changed` 失败，不沿用旧 projection。Runtime 先检查 hard budget，再检查 70% proactive threshold。阈值以上使用已完成 Turn 的 Session 短摘要生成 `<session_checkpoint>`，优先保留 `keep_recent_turns` 个最近 Turn 原文；主动压缩失败但 raw 仍在预算内时回退 raw。只有 raw 超过预算且 checkpoint 仍无法装入，或此时 `summary_enabled=false`，才返回 `context_overflow`。

checkpoint 写入 `context/checkpoints/<turn_id>.json`，manifest/current 只保存相对 Session root 的 ref；digest 使用 `encoding/json.Marshal(ContextCheckpoint)` compact bytes 的 SHA-256。Session export 携带各 Turn 实际引用的 checkpoint，import 复核 identity/digest 后重写 ref，并只从最新有效 checkpoint 重建 `context/current.json`。旧绝对 ref 仅在仍安全落于同一 Session canonical 路径时可由 export 读取；悬空、越界或不匹配 ref 在导出副本/导入 manifest 中清除并记录 `checkpoint_error`。原始 Session messages 始终保持不变；被 overflow 拒绝的 user input、Turn、Attempt、Execution 和带 `projection_error` 的 manifest 仍完整记录。

managed Provider 通过 `AGENTRUN_SESSION_ID`、`AGENTRUN_TURN_ID` 关联会话，并可读取 `SN_RUNTIME_CONTEXT_MANIFEST`、`SN_RUNTIME_SKILLS_DIR`、`SN_RUNTIME_TOOLS_DIR`、`SN_RUNTIME_MEMORY_FILE`、`SN_RUNTIME_MEMORY_CANDIDATES_FILE`、`SN_RUNTIME_MEMORY_INPUT_FILE`。这些环境变量是复用入口，不赋予绕过 capability permission 的权限。

## 7. Skill、Tool 与 Memory

- API runtime 的 tool lifecycle 通过 Provider `Sink.Event` 同步到 Session events。
- managed CLI/tmux 只能记录 Runtime 已装配的 context manifest 和可观察 transcript；没有 Provider 结构化事件时不推断 tool call。
- Runtime working memory 只来自当前 Session 的 `memory/working.json`；无显式 Session 的 Run 不自动加载或固化长期 memory。
- API Agent 的 `memory_write` 只写当前 Session 的 `memory/candidates.json`，ID 由 Runtime 生成，并携带 Session/Turn/Run provenance，返回 `promoted=false`。
- working 写入必须显式执行 `sn-cli memory promote <candidate...> --session-id <id>`。
- memory 条目保留 `source/confidence/expires_at` 等 provenance；已过期条目不会参与 recall。
- Workbench project/global memory 仍由 Workbench owner 管理，通过 HTTP `memory[]` 只读注入；Runtime 记录 digest 和本次输入快照，不扫描或写回 `ai-workbench/memory`。
- CLI/BFF 使用 `session run|submit --memory-file <path>` 注入同一类只读 memory。文件必须是最大 1 MiB 的 regular JSON file，且只包含一个 `{id,type?,content,source?}[]`；其内容不得拼入规范化 user message。
- 用户显式执行 `memory add <id> <content> --session-id <id>` 视为人工确认，可直接写 Session working memory。
- `memory.delete` 仍是独立权限。

这避免模型在一次执行中把未经确认的内容直接固化为长期事实。

## 8. CLI

```bash
sn-cli session run cx "创建结构化会话"
sn-cli session submit --session-id <id> cc "切换 Provider 后台继续"
sn-cli session submit --session-id <id> --memory-file route-memory.json cx
sn-cli loop run --session-id <id> --input "协同执行" --planner-config api-cx --capability agent.run

sn-cli session open --carrier tmux --session-id <id> cx --no-alt-screen
sn-cli session open --carrier terminal --session-id <id> cc
sn-cli session list [--project <project>] [--state <state>] [--tag <tag>]
sn-cli session show --session-id <id>
sn-cli session messages --session-id <id> [--after-seq N]
sn-cli session events --session-id <id> [--after-seq N]
sn-cli session logs --session-id <id> [--tail N]
sn-cli session send --session-id <id> "继续"
sn-cli session interrupt --session-id <id>
sn-cli session stop --session-id <id>
sn-cli session attach --session-id <id>
sn-cli session configure --session-id <id> --runtime cli --profile cx --record-mode full --retention pinned
sn-cli session export --session-id <id> --output session.json
sn-cli session delete --session-id <id>
sn-cli session gc [--older-than-hours 24] [--limit 100] [--apply]

sn-cli memory list --session-id <id> --state candidate
sn-cli memory promote candidate-1 --session-id <id>
```

所有 Provider 的逻辑会话和 carrier lifecycle 都收敛到 `session` namespace。`session run|submit` 形成结构化 Turn；`session open` 只形成 transcript-only Execution。tmux 支持 `send|attach`，terminal 不支持时明确报错。

`retention` 是 Session 级策略，既有 Session 的 Turn 只能继承，不能在单个 Turn 上覆盖；需要变更时使用 `session configure`。`record_mode` 可以按 Turn 向更严格方向收窄；Session policy 从 metadata 改为 full 只影响后续 Turn，不会补录既有 transcript。

## 9. HTTP

```text
GET  /v1/sessions
POST /v1/sessions
GET  /v1/sessions/{session_id}
GET  /v1/sessions/{session_id}/messages?after_seq=N
GET  /v1/sessions/{session_id}/events?after_seq=N
GET  /v1/sessions/{session_id}/watch?after_seq=N
POST /v1/sessions/{session_id}/turns
POST /v1/sessions/gc
```

`watch` 默认返回一次增量事件快照；请求携带 `Accept: text/event-stream` 时进入
SSE watch。SSE 使用 Session event `sequence` 作为 `id`，接受 `after_seq` 或
`Last-Event-ID` 续读，发送 `session.event`、keep-alive comment 和最终
`session.end`。连接默认 30 秒，调用方应携带最后 sequence 重连；当前不提供
WebSocket。

`POST /v1/sessions/gc` 请求体：

```json
{
  "older_than_seconds": 86400,
  "limit": 100,
  "apply": false
}
```

默认保留期 24 小时、limit 100、`apply=false`，语义与 CLI 相同。

## 10. Workbench

- `wb.task.sn_cli.SnCLIClient` 是 BFF adapter，只调用 `sn-cli`。
- UI 会话 list/show/message/stop/configure 不直接读取 `~/.sn`。
- UI 的 profile list/validate/command preview 来自 `sn-cli profile list|validate|command`，避免 Workbench 配置与实际执行 profile 漂移。
- UI 创建会话时必须通过 `session submit` 原子提交首个 Turn，不先制造空 Session；后续每个 Turn 独立提交 `runtime/profile`，切换 profile 不修改或丢弃已有上下文。
- Workbench 路由、project 与 skill 提示通过 `--memory-file` 只读注入，原始用户输入必须原样成为 Session user message。
- UI 从 Session message metadata 展示该 Turn 实际 `provider/model/profile` 与 artifacts，从 `latest_context` 展示容量、估算用量和 checkpoint 状态。
- UI 不需要展示或续轮的 batch Agent 任务通过 `sn-cli profile exec <profile> <prompt>` 执行，不产生本地记录；CLI 的 `sn-cli <profile> <args...>` 保留给原生 TTY。需要本地会话展示的任务使用 `sn-cli session run`，形成 standard Session。
- UI 的 `/api/runtime/runs` 长任务面板通过逻辑 Session 与 `sn-cli session/run` namespace 启动、查询、续轮、日志和停止；Session ID、Run ID 和 Execution ID 分别暴露。
- binary、args 与 env 由独立的 `sn-cli` profile 声明并校验，BFF 只传 Runtime 允许的结构化参数。
- 会话数据由 Go Runtime 单点写入。
- Workbench 的业务 knowledge、outputs、project config 继续由 Workbench owner 管理。

因此不维护两份通用 Runtime：Go 负责 Provider、execution、run、session、history、context 和 Session memory；Python 只负责 HTTP BFF、UI view model、Workbench project/global memory 与业务编排。

## 11. 已知边界

- 顶层 native direct、direct API request、`profile exec` 和 `skill run` 不进入 Session，因此不记录 metadata、transcript 或 Run artifact。
- tmux/terminal transcript 不是结构化 assistant final text。
- `history/index.json` 当前是本地可重建 JSON 索引，不承诺 O(log n) 或全文搜索。
- Session Store、文件锁、queue 和 watch 都以单机 Runtime home 为边界，不承诺
  多副本写入、共享文件系统锁或分布式 lease。

未实现能力统一记录在 [PENDING.md](PENDING.md)。
