# Session、History 与 Context Runtime 契约

> 状态：现行规范。实现、CLI、HTTP、Workbench BFF 和测试必须与本文一致。

## 1. Owner 与目标

Go Runtime 是会话输入、输出、运行关系和上下文清单的唯一 owner：

- `Session` 表示跨 Provider、跨进程、跨多轮的逻辑会话。
- loop 显式关联既有 Session 后，planner 与 `run_agent` Turn 复用相同 Session；Runtime 在启动阶段校验 Session 存在且 project 一致。
- `Turn` 表示一次用户输入及其响应生命周期。
- `RunAttempt` 表示某个 Turn 的一次 Provider 尝试。
- `Execution` 表示 API、managed CLI、direct TTY 或 tmux 执行容器，可以跨 Turn。
- `result.json` 仍是单次 run 的不可变结果；Session 通过 `result_ref` 引用，不复制结果事实。
- `History` 是从 Session 构建的可重建查询视图，不是第二套事实源。
- Workbench UI/BFF 只调用 Runtime CLI/HTTP，不再自行维护 `messages.jsonl`。

关系如下：

```text
Session
  └── Turn
        └── RunAttempt ──result_ref──> runs/.../result.json

Execution(API/cli_managed/cli_direct/tmux) 与 Turn 正交关联
```

## 2. 目录与事实源

```text
~/.sn/
├── sessions/<date>/<session_id>/
│   ├── session.json
│   ├── messages.jsonl
│   ├── events.jsonl
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
└── memory/                         # legacy/manual global capability，不由 Session Agent 自动读取
    ├── durable.json
    └── candidates.json
```

边界：

- `sessions/` 是持久化会话事实源；删除采用移动到 `history/trash`，可恢复。
- `runs/` 是一次执行的 artifact，不保存长期会话正文。
- `history/index.json` 仅用于 list/filter；损坏或丢失可执行 `history rebuild`。
- `state/` 只保存活动锁、registry、lease 等运行态。
- 不再复制 Session history 到 archive，也不从 run 目录反向拼装 UI 会话。

## 3. 标识与状态

`session_id`、`turn_id`、`run_id`、`execution_id` 是独立标识。为兼容既有 tmux CLI，`session start --run-id` 的 run ID 可以与逻辑 Session ID 同值，但两者仍写入不同模型。

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

- `ephemeral`：一次性任务，后续可由 GC 清理。
- `standard`：普通多轮会话。
- `pinned`：明确长期保留。

默认值：

| 入口 | record_mode | retention | capture_quality |
| --- | --- | --- | --- |
| 普通 HTTP/API Run | off | - | structured |
| 顶层 managed profile | off | - | structured/parsed |
| `task run` 且无显式 Session | off | - | structured/parsed |
| `session run` | full | ephemeral | structured/parsed |
| `turn run --session-id` | 继承 Session | 继承 Session | structured/parsed |
| 顶层 direct interactive TTY | off | - | - |
| `session exec` direct TTY | metadata | standard | metadata_only |
| tmux session | full | standard | transcript_only |
| help/version/config validate | off | - | - |

direct TTY 只有在引入 PTY 代理或 Provider 结构化事件 adapter 后才能升级 capture quality。

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

## 6. ContextCompiler

每个 managed Turn 写入 `context-manifest.json`，记录：

- 被纳入上下文的 message sequence range 与 digest。
- profile config digest。
- `allowed_actions` / `forbidden_actions` policy digest。
- Skill ID、源路径与内容 digest。
- Runtime 静态可确定且实际授权的 local Tool schema digest；动态 MCP tools 由 profile config digest 与 Provider lifecycle events 补足。
- Memory read ID、类型、来源与内容 digest。

manifest 不写 token、secret、cookie、Authorization、完整环境变量或 tool 参数原文。Provider 事件进入 Session history 前再次做敏感 key 脱敏。

Runtime 从最近 14 条规范化 `user/assistant` 消息编译实际 Provider 输入：API/native 使用结构化 messages，managed CLI 使用带边界标记的历史块。切换 Provider 时重新编译，不复用 Provider 私有 snapshot。

managed Provider 通过 `AGENTRUN_SESSION_ID`、`AGENTRUN_TURN_ID` 关联会话，并可读取 `SN_RUNTIME_CONTEXT_MANIFEST`、`SN_RUNTIME_SKILLS_DIR`、`SN_RUNTIME_TOOLS_DIR`、`SN_RUNTIME_MEMORY_FILE`、`SN_RUNTIME_MEMORY_CANDIDATES_FILE`、`SN_RUNTIME_MEMORY_INPUT_FILE`。这些环境变量是复用入口，不赋予绕过 capability permission 的权限。

## 7. Skill、Tool 与 Memory

- API/native runtime 的 tool lifecycle 通过 Provider `Sink.Event` 同步到 Session events。
- managed CLI/tmux 只能记录 Runtime 已装配的 context manifest 和可观察 transcript；没有 Provider 结构化事件时不推断 tool call。
- Runtime working memory 只来自当前 Session 的 `memory/working.json`；无显式 Session 的 Run 不自动加载或固化长期 memory。
- API Agent 的 `memory_write` 只写当前 Session 的 `memory/candidates.json`，ID 由 Runtime 生成，并携带 Session/Turn/Run provenance，返回 `promoted=false`。
- working 写入必须显式执行 `sn-cli capabilities memory promote --session-id <id> <candidate...>`。
- memory 条目保留 `source/confidence/expires_at` 等 provenance；已过期条目不会参与 recall。
- Workbench project/global memory 仍由 Workbench owner 管理，通过 HTTP `memory[]` 只读注入；Runtime 记录 digest 和本次输入快照，不扫描或写回 `ai-workbench/memory`。
- 用户显式执行 `capabilities memory write --session-id <id>` 视为人工确认，可直接写 Session working memory。
- `memory.delete` 仍是独立权限。

这避免模型在一次执行中把未经确认的内容直接固化为长期事实。

## 8. CLI

```bash
sn-cli history create --session-id <id> --project <project> --runtime api --profile ba [--tag <tag>]
sn-cli history list [--project <project>] [--state <state>] [--tag <tag>]
sn-cli history show --session-id <id>
sn-cli history messages --session-id <id> [--after-seq N]
sn-cli history events --session-id <id> [--after-seq N]
sn-cli history configure --session-id <id> --runtime cli --profile cx --record-mode full --retention pinned
sn-cli history export --session-id <id> --output session.json
sn-cli history import --input session-import.json
sn-cli history delete --session-id <id>
sn-cli history rebuild

sn-cli turn run -c cx --session-id <id> "继续"
sn-cli loop run --session-id <id> --input "协同执行" --planner-config ba --capability agent.run
sn-cli session run -c cx "一次性会话任务"
sn-cli session exec -c cx -- --help
sn-cli session history list

sn-cli capabilities memory candidates --session-id <id>
sn-cli capabilities memory promote candidate-1 --session-id <id>
```

`session list/status/send/...` 继续负责 tmux 活动执行；`history ...` 负责所有 Provider 的逻辑会话。
`retention` 是 Session 级策略，既有 Session 的 Turn 只能继承，不能在单个 Turn 上覆盖；需要变更时使用 `history configure`。`record_mode` 可以按 Turn 向更严格方向收窄；Session policy 从 metadata 改为 full 只影响后续 Turn，不会补录既有 transcript。

## 9. HTTP

```text
GET  /v1/sessions
POST /v1/sessions
GET  /v1/sessions/{session_id}
GET  /v1/sessions/{session_id}/messages?after_seq=N
GET  /v1/sessions/{session_id}/events?after_seq=N
GET  /v1/sessions/{session_id}/watch?after_seq=N
POST /v1/sessions/{session_id}/turns
```

`watch` 当前是增量事件读取，不承诺 WebSocket/SSE。UI 用 sequence cursor 轮询即可；后续可在不改变存储契约的情况下增加 SSE。

## 10. Workbench

- `wb.task.sn_cli.SnCLIClient` 是 BFF adapter，只调用 `sn-cli`。
- UI 会话 list/show/message/stop/configure 不直接读取 `~/.sn`。
- UI 的 profile choices/validate/command preview 同样来自 `sn-cli config choices|validate|command`，避免 Workbench 配置与实际执行 profile 漂移。
- UI 不需要会话的一次性 Agent 任务通过 `sn-cli task run/result` 执行，只产生 Run；需要本地会话展示的一次性任务改用 `sn-cli session run`，形成 `ephemeral` Session。
- UI 的 `/api/runtime/runs` 长任务面板通过带 `workbench-runtime` tag 的 Session 与 `sn-cli task/session` 启动、查询、续轮、日志和停止；对 UI 暴露的 `run_id` 是逻辑 Session ID，实际 attempt ID 作为 `provider_run_id` 返回。
- Workbench 不再接受 `command` / `provider_env` 直传执行，binary、args 与 env 必须由 `sn-cli` profile/preset 声明并校验，避免 BFF 绕过 Runtime 配置 owner。
- Workbench 不保留 Session/History 迁移器或双写路径；现行会话只写 Go Runtime。
- Workbench 的业务 knowledge、outputs、project config 继续由 Workbench owner 管理。

因此不维护两份通用 Runtime：Go 负责 Provider、execution、run、session、history、context 和 Session memory；Python 只负责 HTTP BFF、UI view model、Workbench project/global memory 与业务编排。

## 11. 已知边界

- direct TTY 当前只能可靠记录 metadata；不能保证 transcript。
- tmux transcript snapshot 不是结构化 assistant final text。
- `history/index.json` 当前是本地可重建 JSON 索引，不承诺 O(log n) 或全文搜索；达到查询压力后可在 Store 接口后替换为 SQLite/FTS。
- SSE/WebSocket、ephemeral 自动 GC、Provider 专属结构化 TTY adapter 属于后续增强，不得在文档中宣称已完成。
