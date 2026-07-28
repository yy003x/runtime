# Runtime vNext 契约

本文是当前 Runtime 的总契约。代码、严格 loader、SQLite schema 和测试与本文冲突
时，必须在同一次变更中消除差异。

## 1. 边界与 Owner

| 层 | Owner | 负责 | 不负责 |
| --- | --- | --- | --- |
| Profile facade | `profile/` | 统一配置、类型分流、subcommand 映射 | 执行、历史、tool loop |
| Command Bridge | `command/` | argv/env、TTY/tmux/terminal、进程语义 | 模型和记录 |
| Model Core | `model/`、`contract/` | 单次 model call、canonical contract | tool loop、fallback、存储 |
| API Driver | `provider/*` | HTTP/SSE codec、Provider error | retry、tool、Session |
| Session Service | `session/` | Session/Turn/history/context projection | 自动执行 tool、业务 workflow |
| Agent Kernel | `agent/` | 唯一 model/tool loop、预算、暂停和恢复 | profile、SQLite、业务编排 |
| Run Harness | `run/` | durable identity、queue、journal、checkpoint | 业务 Session 和长期 memory |
| Store | `store/sqlite/` | SQLite WAL 与 terminal barrier | 业务策略 |
| Transport | `internal/cli`、`transport/http` | decode/call/encode | 第二套状态机 |

依赖方向为 adapter → application/domain → contract。`agent/` 不读取 profile/config，
不打开数据库；`provider/*` 不执行工具；HTTP/CLI 不拼装独立历史。

## 2. Profile 与直接执行

所有 Profile 位于 `configs/<id>.json`，使用 `type=cli|api` 的 discriminated
schema。`commands/<id>.json` 只登记顶层 subcommand 到 Profile 的映射。保留 ID、
未知字段、无效映射、symlink 或非 JSON entry 都在 loader 阶段失败；类型分流后
CLI 与 API 仍进入独立领域，不共享执行器。

`profile <id>` 只执行一次：

- command：按配置的 transport 和 prompt delivery 启动一次目标进程；
- model：执行一次 `Generate`，tool call 以 `requires_action` 返回；
- 不创建 Session、Turn 或 durable Run；
- 不自动 retry、fallback 或 tool loop。

source 提供 `commit` CLI Profile 和同名 subcommand 映射。`sn-cli commit` 与
`sn-cli profile commit` 复用同一配置，均只执行一次 CLI；提交规划使用只读
sandbox，真正的 Git 写入仍由调用方控制。

顶层动态 command 只接受 `transport=tty`，并用 process replacement 保留目标
CLI 原生行为。tmux/terminal profile 必须走 `profile <id>`，其返回值只承诺 launch
handle；`manual` 不自动注入 prompt。

## 3. Canonical Model Contract

`contract.GenerateRequest` 由 `model_profile` 和 Provider-neutral
`ModelRequest` 组成。Message 首期只支持 text、tool call、tool result。
Driver 每次只允许一个 HTTP attempt，负责：

- canonical request 到 Provider payload；
- JSON/SSE 解析和增量事件；
- finish reason、usage、request ID；
- 认证、HTTP 和协议错误归一化。

Driver 不读取 Session、asset、skill 或 memory，不执行工具，不写 Store。
OpenAI-compatible 和 Anthropic-compatible profile 必须提供完整 HTTPS endpoint；
Runtime 不猜测 `/v1`、endpoint path 或 auth scheme。secret 只从
`auth.from_env` 解析，不进入 profile output、event 或数据库。

Provider Profile 的默认 token limit 使用实际协议字段：OpenAI-compatible
Chat Completions 使用 `max_completion_tokens`，Anthropic-compatible Messages
使用 `max_tokens`。Canonical `ModelRequest.options.max_output_tokens` 仍表示
Provider-neutral 输出预算，由 Driver 映射到 wire payload。

## 4. Session

`session_id`、`turn_id`、`run_id`、`execution_id` 是不同 identity；`task_id`
只是 caller correlation。

- model profile：历史由 Runtime 读取并投影为结构化 `messages[]`；
- command profile：历史投影为转义、定界、最大 256 KiB 的文本；
- tool call：Session 进入 `requires_action`，外部必须提交原始
  `tool_call_id`、content 和 idempotency key；
- tmux/terminal：只记录 `transcript_only`，不伪造结构化 final；
- Session 原始事实不可由 checkpoint 或 projection 覆盖。

Session 文件与 SQLite Run 是两个事实域，不承诺跨存储原子事务。所有 Agent 到
Session 的投影携带稳定 correlation，重复投影必须幂等。

## 5. Agent Kernel

Agent 只接受 model profile。Kernel 使用注入的 `model.Generator`、
`ToolExecutor` 和 `EffectRecorder`：

1. 调用一次 model；
2. 校验 tool name、call ID 和 arguments；
3. 按顺序执行 tool；
4. 追加 assistant/tool message；
5. 在预算内继续下一 round。

工具执行前必须保存 prepared checkpoint，再记录 `tool.started`。如果进程在
`tool.started` 后、result 提交前退出，Run 进入 `needs_reconciliation`，不得
自动重放可能有副作用的工具。pause/resume 使用带 `pause_id` 的 typed payload。

默认只启用 `read_file`、`list_directory`。`write_file`、`exec_command`
必须在 `runtime.json` 显式启用；所有路径受 workspace roots、symlink 和大小门禁，
command 使用 argv 直启，不经过 shell。

## 6. Durable Run

状态：

```text
queued → running ─┬→ paused → queued
                  ├→ needs_reconciliation
                  ├→ completed
                  ├→ failed
                  └→ cancelled
```

`retry` 创建新 Run，并以 `retry_of` 关联旧 Run。SQLite WAL 表至少包含：

```text
runs
queue
events
checkpoints
model_calls
tool_effects
```

terminal publish barrier 必须在一个 transaction 中完成：

```text
result/error
terminal event
terminal state
run.settled
settled_sequence
queue removal
```

`run.settled` 后禁止追加事件。watcher 只读 committed event；SSE 连接断开不取消
durable Run。worker 遇到单个任务失败继续处理队列；只有 Store/claim 级错误才退出。

## 7. Transport

CLI namespace 固定为：

```text
profile
session
agent
run
system
help
version
```

HTTP：

```text
POST /v1/model/generate

GET|POST /v1/sessions
POST     /v1/sessions/gc
GET      /v1/sessions/{id}
GET      /v1/sessions/{id}/messages
GET      /v1/sessions/{id}/events
GET      /v1/sessions/{id}/watch
POST     /v1/sessions/{id}/turns
POST     /v1/sessions/{id}/turns/{turn_id}/tool-results

POST /v1/agent/run

GET|POST /v1/runs
POST     /v1/runs/gc
GET      /v1/runs/{id}
GET      /v1/runs/{id}/events
POST     /v1/runs/{id}:cancel
POST     /v1/runs/{id}:resume
```

JSON request 严格拒绝未知字段并限制大小。streaming 使用 SSE，event `sequence`
作为 `id`，支持 `after_seq` 或 `Last-Event-ID` 续读。HTTP 只能选择已加载 profile
和已注册 tool，不能上传 command、env 或 handler。

## 8. 非兼容声明

vNext 不读取无 `type` 的旧 Profile、旧 `runtime.yaml`、旧 Run artifact 或旧 SDK
contract；不提供旧 namespace shim。唯一硬兼容面是有效 CLI Profile 对应的
`sn-cli cx|cc|cx-*` 原生命令执行。
