# Runtime vNext 契约

本文是 Runtime 的当前总契约。代码、严格 loader、SQLite schema 和测试与本文冲突
时，必须在同一次变更中消除差异。

## 1. 边界与 Owner

| 领域 | Owner | 负责 | 不负责 |
| --- | --- | --- | --- |
| Profile facade | `profile/` | 配置加载、类型分流、shortcut 映射 | 执行、历史 |
| Command adapter | `command/` | CLI grammar、effective argv/env/cwd | Session/Tmux 状态 |
| Model Core | `model/`、`contract/` | 单次 canonical model call | tool loop、存储 |
| API Driver | `provider/*` | HTTP/SSE codec、Provider error | retry、tool、Session |
| Session Service | `session/` | Session/Turn/history/execution | 自动执行 canonical tool |
| Tmux Service | `tmux/` | 专用 tmux server/window lifecycle | Session/history |
| Agent Kernel | `agent/` | 唯一 model/tool loop、预算、暂停恢复 | Profile、SQLite |
| Run Harness | `run/` | durable identity、queue、journal、checkpoint | Session 策略 |
| Store | `store/sqlite/` | SQLite WAL 与 terminal barrier | 业务 workflow |
| Transport | `internal/cli`、`transport/http` | decode/call/encode | 第二套状态机 |

依赖方向为 adapter → application/domain → contract。`internal/runtimebootstrap/` 是
唯一 composition root。`agent/` 不读 Profile 或数据库；Provider driver 每次只做
一次 HTTP attempt；CLI/HTTP 不拼装独立历史。

## 2. Profile、shortcut 与 command adapter

Profile 位于 `configs/<id>.json`，必须以 `type=cli|api` 分流。CLI Profile 字段：

```text
command args env model effort prompt exec cwd
```

不接受 `binary`、`transport`、`prompt_delivery` 或 `effort_adapter`。API Profile
保持自己的 Provider schema。`commands/<id>.json` 只引用现有 CLI Profile。

执行矩阵：

| 入口 | effective mode | 执行 owner | 记录 |
| --- | --- | --- | --- |
| `sn-cli <command-id>` | Profile `exec` | process replacement | 无 |
| `sn-cli profile <id>` CLI | Profile/`--exec` | process replacement | 无 |
| `sn-cli profile <id>` API | API | Model Core | 无 |
| `sn-cli session run|submit` CLI | 固定 exec | Session child | Turn/Execution |
| `sn-cli session run|submit` API | API | Session executor | Turn/Execution |
| `sn-cli tmux start` | 固定 interactive | Tmux window | 无 Session |

`launch` 不属于公开 Profile。Session 和 Tmux 不读取 Profile `exec`。

Command adapter 按 `filepath.Base(command)` 选择，首期支持 Codex 与 Claude。adapter
用显式 option grammar：

- 区分 command/common、exec-only 和 mode selector；
- 识别并替换 model、effort、exec 和 canonical-output selector；
- 对重复、stateful、改变 final shape 或无法安全归类的配置 fail closed；
- Profile/Session/Tmux 输入用 `--` 结束 options，并保证 prompt 为最终 argv token；
- shortcut 的 native args 不解析，按原顺序追加；
- spawn 前校验 env expansion、cwd、PATH、单 token 与总 argv/env budget。

Profile `prompt`、typed `--prompt`、piped stdin、位置 input 按顺序合并。CLI Profile
exec prompt 必须非空；interactive 可为空。两种 mode 都 process replacement，
leading global `--json` 不包装其原生输出。

`profile check` 是纯静态校验，不解析真实 env/PATH/cwd，不读取 prompt file。

## 3. Canonical Model Contract

`contract.GenerateRequest` 由 `model_profile` 和 Provider-neutral `ModelRequest`
组成。Driver 负责：

- canonical request 到 Provider payload；
- JSON/SSE 解析和增量 event；
- finish reason、usage、request ID；
- auth、HTTP 和协议错误归一化。

Driver 不读取 Session、skill 或 memory，不执行工具，不写 Store。secret 只从
`auth.from_env` 解析，不进入 Profile output、event 或数据库。

OpenAI-compatible Chat Completions 使用 `max_completion_tokens`；
Anthropic-compatible Messages 使用 `max_tokens`。Canonical
`max_output_tokens` 由 Driver 映射到 wire 字段。

## 4. Session

Session 拥有：

```text
Session
  └─ Turn
       ├─ canonical user/assistant/tool messages
       ├─ ProfileRef 与 request/config digest
       ├─ Execution attempt
       └─ ContextManifest 与 lifecycle event
```

每个 Turn 可独立选择 Profile，provider 只能在 Turn 边界切换。API tool call 进入
`requires_action`，Session 不执行 tool。CLI 子进程内部自行执行的工具是 opaque
provider 行为，不投影成 Runtime tool lifecycle。

CLI executor 固定 command adapter exec/canonical：

- Codex：`exec --ephemeral --json`，只有唯一 `turn.completed` 前最后一个 completed
  `agent_message` 可成为 assistant；
- Claude：`-p --output-format json`，只接受唯一、成功、`is_error=false` 的
  `type=result` document；
- OS exit=0 与 protocol success terminal 都必须满足；
- stdout 只承载机器协议，stderr 只承载诊断；partial output 不伪造成 assistant。

managed process 使用独立 process group，stdout/stderr 并发读取并受硬上限保护。
Execution 只持久化 observed byte count、prefix digest、truncated/limit facts 和
Runtime typed summary；不落原始 stderr、resolved secret、完整 argv/env。

spawn 通过 private helper 的 marker-before-exec handshake：`spawn_intent` 和
PID/PGID/start-token 必须先持久化，收到 go 后 helper 才 unlink manifest 并
`exec` Provider。任何可能已运行而 terminal 未提交的 CLI attempt 都视为未知，
不自动重放。

公开恢复流程：

```text
session reconcile --session-id <id>
session reconcile --session-id <id> --terminate
session reconcile --session-id <id> --acknowledge-unknown
run reconcile --run-id <id>
```

只有 identity 完全匹配时才发送 TERM→bounded wait→KILL。歧义保持 blocked。
durable Session Run 在启动恢复时从 running 转为 `needs_reconciliation`，不回队列。

Session request 的公开 DTO 只保存 digest/ref。冻结的 CLI snapshot 与 base prompt
存入 SQLite `private_request_json`，Go 字段为 `json:"-"`，不得出现在
get/list/result/events/export/log/error。同一 Session 同时只允许一个非 terminal
durable Session Run。

Session fact `schema_version=2`。旧 schema 不读、不迁移。

## 5. Tmux

Tmux 是独立 interactive process manager：

- 固定短 `-S` socket、session `sn-session` 和 source-controlled
  `resources/tmux.conf`；
- 每次 `start` 建一个 window，Profile 固定 interactive mode；
- initial prompt 是最终 argv token，后续 `send` 才使用 paste buffer；
- server marker 绑定 full canonical-home digest、uid、config digest 和
  server incarnation；
- `tmux_id`、window/pane identity、owner 和 incarnation 每次 mutation 都复核；
- start 使用 ready/go gate 和 mode 0600、消费即 unlink 的 launch manifest；
- tmux server 只继承 sanitized env，不缓存 Profile secret；
- `stop` 只 kill 精确 managed window；最后一个 window 退出后关闭 sentinel/server；
- registry 只表示当前 live/dead window，不建立 durable transcript/history。

详细协议见 [Tmux 管理契约](tmux-contract.md)。

## 6. Agent Kernel

Agent 只接受 API Profile。Kernel 使用注入的 `model.Generator`、`ToolExecutor` 和
`EffectRecorder`，按 model → tool validation → tool execution → message append
循环。工具执行前保存 prepared checkpoint；`tool.started` 后结果未知时 Run 进入
`needs_reconciliation`。

默认 tool 只读。`write_file`、`exec_command` 必须在 `runtime.json` 显式启用，
且受 workspace roots、symlink 和大小门禁。

## 7. Durable Run

```text
queued → running ─┬→ paused → queued
                  ├→ needs_reconciliation
                  ├→ completed
                  ├→ failed
                  └→ cancelled
```

`retry` 创建新 Run 并以 `retry_of` 关联。terminal publish barrier 必须在一个
SQLite transaction 内提交 result/error、terminal event/state、`run.settled`、
settled sequence 和 queue removal。settled 后禁止追加 event。

SQLite `PRAGMA user_version=2`。unknown、更高、旧或混合 schema fail closed。

## 8. CLI 与 HTTP

固定 CLI namespace：

```text
profile session tmux agent run server help version
```

Runtime machine contract 为 `schema_version=1`、`contract_version=3`。
CLI Profile/shortcut 和 `tmux attach` 不属于 machine wrapper。

HTTP 使用同一 application service，严格拒绝未知字段并限制 request size。Session
新增 execution query 与 reconcile route；HTTP 不提供 Tmux 控制，也不能上传
command、env、Provider payload 或 tool handler。

## 9. 激活与非兼容声明

contract-v3 archive 带 activation epoch。legacy v0.1.1 updater 的 staged
`profile list` 必须在 release payload、binary、配置或受管 resource file mutation
前失败；v0.1.1 自身不可反向修复的 layout bootstrap 只允许创建其固定的空 legacy
directory。当前 installer/updater 只能由 staged candidate 在
maintenance/lifecycle lock、quiescence 和 schema preflight 全部通过后激活。

staged gate 读取 candidate payload 自身的 `resources/release.json`，不信任
active/merged home 的同名文件，也没有环境 token bypass。激活事务先持久化 journal
与 state guard，再用 no-replace regular file 暂时占用 active `bin/`、`configs/`；
二次进程扫描按 inode 和 PID/start-token 判定。任何无法证明全旧或全新的恢复状态都
保留 guard/barrier，禁止自动放行。journal 在 `committed|rolled_back` terminal
phase 仍阻断所有入口，直到 stage、rename、guard/journal 的 durable cleanup 完成。
installer 只允许 home 外部的 install-dir；激活后使用稳定 directory FD 和
no-clobber `symlinkat` 创建 command link，不覆盖任何已有 entry。

运行中的 server、managed Tmux window、active/unknown Session execution、
queued/running/paused/needs-reconciliation Run、目标 home binary process 或
schema 1 状态都阻止激活。`--overwrite-configs` 不绕过运行态门禁。

唯一例外是仓库根目录的 local-source `make install`：它不是 release/update
语义，固定覆盖 source configs，并由 staged candidate 在 lifecycle lock 内安全
停止 server。该模式仍要求 Tmux 和目标 binary process quiescent，但允许不解析旧
Session/Run schema；只有发布 artifact 全部提交并验证后，才在 guard 下幂等删除
Session、Session private state 和 `runtime.db*`，然后解除 journal。安装终态固定
为 server stopped，且这项 reset 授权不能由 archive installer 或
`server update` 获得。

vNext 不读取旧 Profile 字段、旧 Session carrier、旧 Session/SQLite schema、旧
Run artifact、旧 SDK contract 或旧 namespace shim。唯一硬兼容面是有效 CLI
Profile 对应的 `sn-cli cx|cc|cx-*` 原生命令执行。
