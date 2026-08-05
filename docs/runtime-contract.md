# SN Runtime 契约

本文是 Runtime 的当前总契约。代码、严格 loader、SQLite schema 和测试与本文冲突
时，必须在同一次变更中消除差异。

## 1. 边界与 Owner

| 领域 | Owner | 负责 | 不负责 |
| --- | --- | --- | --- |
| Profile facade | `profile/` | 唯一配置目录加载、类型分流、ID 解析 | 执行、历史 |
| Command adapter | `command/` | CLI grammar、effective argv/env/cwd | Session/Tmux 状态 |
| Model Core | `model/`、`contract/` | 单次 canonical model call | tool loop、存储 |
| API Driver | `provider/*` | HTTP/SSE codec、Provider error | retry、tool、Session |
| Tool Config/MCP adapter | `internal/toolconfig`、`internal/toolmcp` | 严格 manifest、单次只读 MCP call | model loop、retry、持久化 |
| Session Service | `session/` | Session/Turn/history/execution | 自动执行 canonical tool |
| Tmux Service | `tmux/` | 专用 tmux server/window lifecycle | Session/history |
| Agent Kernel | `agent/` | 唯一 model/tool loop、预算、暂停恢复 | Profile、SQLite |
| Run Harness | `run/` | durable identity、queue、journal、checkpoint | Session 策略 |
| Store | `store/sqlite/` | SQLite WAL 与 terminal barrier | 业务 workflow |
| Transport | `internal/cli`、`transport/http` | decode/call/encode | 第二套状态机 |

依赖方向为 adapter → application/domain → contract。`internal/runtimebootstrap/` 是
唯一 composition root。`agent/` 不读 Profile 或数据库；Provider driver 每次只做
一次 HTTP attempt；CLI/HTTP 不拼装独立历史。

## 2. Profile 与 command adapter

Profile 位于 `configs/<id>.json`，必须以 `type=cli|api` 分流。CLI Profile 字段：

```text
command args env model effort prompt cwd
```

loader 只接受上述字段。API Profile 保持自己的 Provider schema。不存在 command
ID、第二层映射或 raw/native argv passthrough。

执行矩阵：

| 入口 | effective mode | 执行 owner | 记录 |
| --- | --- | --- | --- |
| `sn-cli <cli-id>` | interactive direct | process replacement | `cli.jsonl`；无 Session/Run |
| `sn-cli exec <cli-id>` | non-interactive exec | process replacement | `cli.jsonl`；无 Session/Run |
| `sn-cli req <api-id>` | API request | Model Core | `api.jsonl`；无 Session/Run |
| `sn-cli session exec <cli-id> [--queue]` | non-interactive managed exec | Session child | `cli.jsonl` + Turn/Execution；queue 时另有 Run |
| `sn-cli session req <api-id> [--queue]` | API request | Session executor | `api.jsonl` + Turn/Execution；queue 时另有 Run |
| `sn-cli tmux start <cli-id>` | interactive | Tmux window | `cli.jsonl`；无 Session |
| `sn-cli agent <api-id> [--queue]` | API model/tool loop | Agent Kernel | 每轮 `api.jsonl` + Durable Run |

namespace 先固定 execution mode，Profile `type` 再做严格配对校验。Profile 不保存
execution mode；Session/Tmux schema 不因 CLI ingress 改名而变化。

Command adapter 按 `filepath.Base(command)` 选择，首期支持 Codex 与 Claude。adapter
用显式 option grammar：

- 区分 command/common、exec-only 和 mode selector；
- 识别并替换 model、effort、mode selector 和 canonical-output selector；
- 对重复、stateful、改变 final shape 或无法安全归类的配置 fail closed；Claude
  `--verbose` 会把 canonical JSON result 改成逐轮数组，必须在 Profile 检查阶段拒绝；
- Profile/Session/Tmux 输入用 `--` 结束 options，并保证 prompt 为最终 argv token；
- file、stdin、合并 prompt 与单个 argv/env token 上限为 128,000 bytes；
- spawn 前校验 env expansion、cwd、PATH、单 token 与总 argv/env budget。

Profile `prompt`、typed `--prompt`、piped stdin、位置 input 按顺序合并。`exec`
prompt 必须非空；bare interactive direct 可为空。两种 mode 都 process replacement，
leading global `--json` 不包装其原生输出。

bare Profile 只接受 CLI Profile；`exec` 只接受 CLI Profile；`req` 只接受 API
Profile。Profile ID 必须紧跟拥有它的 namespace/action，option 位于其后，input
必须最后。固定根 namespace
`exec|req|profile|session|tmux|agent|run|server|help|version`，以及 Profile 管理
action `list|show|check` 都是保留 Profile ID；loader 遇到冲突即失败。

`profile` 只提供 `list|show|check` 管理动作，不执行 Profile。`profile check` 是纯
静态校验，不解析真实 env/PATH/cwd，不读取 prompt file。

## 3. 本地 Profile 执行日志

`${SN_CLI_HOME}/logs` 是 best-effort 本地诊断面，不是 canonical Session/Run Store，
不参与 replay、幂等、恢复、terminal barrier 或 API contract：

```text
logs/
  YYMMDD/
    cli.jsonl
    api.jsonl
```

每天、每种 Profile 类型只使用一个 append-only JSONL 文件。`time` 使用本地时间的
`YYYY-MM-DD HH:mm:ss`。当前不做 retention、总量限制或自动 GC；旧 flat log 保持
原样，不迁移，也没有兼容 reader。

记录门禁固定为：必须有 Profile ID，并且进入真实执行边界。CLI 在最终 invocation
已构建并准备 launch 时写一条；API 只有在 driver 调用 `http.Client.Do` 时写一条。
Profile 查询/校验、Session/Run/Tmux 查询与控制、无网络的前置校验失败、queue submit
都不写；queued Session/Agent 由 worker 真正执行时写，Agent 每个 Provider round
各写一条。HTTP `POST /v1/model/generate` 同样进入统一 API 日志。MCP HTTP 不属于
Profile Provider attempt，不写 `api.jsonl`；其 call/result 由 Agent durable
effect/event 记录。

CLI 行固定字段为 `time,namespace,profile,source,command`。`command` 是 Runtime
交给 OS 的可读 invocation；Profile env 保留 `${VAR}` 引用，不写 resolved value。
API 行固定字段为
`time,namespace,profile,source,call_id,request,response,error`：

- `request={method,url,headers,body}` 是 driver protocol encoding 后交给
  `http.Client` 的 application-level request；不保存 curl 字符串；
- `response={status,headers,data}`；普通 JSON response 的 `data` 是单元素数组，
  SSE 按顺序收集所有有效 JSON `data:` frame，OpenAI `[DONE]` 不属于 JSON；
- network error 使用 `response:null`；`error` 是 Provider-neutral RuntimeError；
- `${VAR}` header 按 driver 的 auth shaping 保留引用，例如
  `Bearer ${MODEL_API_KEY}`；字面量敏感 header、cookie、URL query 和 response/error
  中回显的 secret 必须脱敏；
- `call_id` 每次 Provider call 唯一，不等同于 Provider request ID、Run ID 或 Tool
  call ID。

日志写入错误、锁冲突、非法路径或 observer panic 一律丢弃，不改变返回值、event、
exit code、retry 或 durable state，也不与请求/Session/Run 建事务。写入使用 private
directory/file mode、no-follow、single-link regular-file 校验和非阻塞文件锁，不做
`fsync`；因此日志允许缺失，不能用来证明 effect 是否发生。

## 4. Canonical Model Contract

`contract.GenerateRequest` 由 `model_profile` 和 Provider-neutral `ModelRequest`
组成。Driver 负责：

- canonical request 到 Provider payload；
- JSON/SSE 解析和增量 event；
- finish reason、usage、request ID；
- auth、HTTP 和协议错误归一化。

Driver 不读取 Session、skill 或 memory，不执行工具，不写 Store。secret 通过
headers 的 `${VAR}` 引用从执行环境展开，不进入 Profile output、event 或数据库。
Driver 禁止 `http.Client` 自动跟随 redirect；每次 `Stream` 只调用一次 request，3xx
作为该次 Provider response 进入 typed protocol error 和同一条 API 日志。

Profile 默认输出上限统一配置为 `parameters.max_tokens`，CLI override 统一为
`--max-tokens`。Canonical request 使用 `max_output_tokens`；openai driver
将它映射为 wire `max_completion_tokens`，anthropic driver 映射为
wire `max_tokens`。共有的可选 `temperature`、`top_p`、`stop_sequences` 同样先进入
canonical request，其中 OpenAI 将停止序列映射为 `stop`；未配置时 adapter 不发送，
目标模型不保证支持每个可选参数。Canonical tool Message 的
`is_error` 必须保存在 canonical tool Message；anthropic driver 将其映射到
`tool_result.is_error`，不能只保留在 lifecycle event 中。

API Profile 必须在完整 `endpoint` 与 `base_url` 中二选一。`endpoint` 原样请求；
`base_url` 保留已有路径前缀，openai driver 追加 `/v1/chat/completions`，
anthropic driver 追加 `/v1/messages`。`base_url` 不接受 query 或 fragment；
非默认路径使用显式 `endpoint`。

Profile `context.window_tokens` 只约束 Session 本地上下文投影，不作为 Provider
请求字段。未声明或为 `0` 时使用保守默认窗口 `32768`；输出预留使用显式
`context.reserved_output_tokens`、Profile 默认输出上限和请求级输出上限的最大值，
都未声明时默认 `8192`。输入预算是窗口减去有效输出预留，必须至少为 `2`；较低的
请求级输出上限不能扩大 Profile 输入预算，较高值必须收紧输入预算。

## 5. Session

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
- Claude：`-p --output-format json`，不允许 `--verbose` 改写 stdout shape，只接受
  唯一、成功、`is_error=false` 的 `type=result` document；
- OS exit=0 与 protocol success terminal 都必须满足；
- stdout 只承载机器协议，stderr 只承载诊断；partial output 不伪造成 assistant。

managed process 使用独立 process group，stdout/stderr 并发读取并受硬上限保护。
Execution 只持久化 observed byte count、prefix digest、truncated/limit facts 和
Runtime typed summary；不落原始 stderr、resolved secret、完整 argv/env。

spawn 通过 private helper 的 marker-before-exec handshake：`spawn_intent` 和
PID/PGID/start-token 必须先持久化。invocation manifest 位于
`state/session-invocations/`，是随机命名、mode `0600`、single-link 的私有文件；
helper 绑定 manifest directory/file device+inode，收到 go 后才 no-follow
读取、strict decode、按 identity unlink 并 `exec` Provider。服务初始化只清理
超过 24 小时且可证明为 Runtime-owned 的遗留 manifest。任何可能已运行而 terminal
未提交的 CLI attempt 都视为未知，不自动重放。

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

Agent 绑定 Session 时，Run 的 combined `request_digest/config_digest` 与
Session Turn/Execution 的 digest 是两个独立命名空间。Agent private execution
snapshot 单独保存 `session_request_digest/session_config_digest`，并用它们校验
Session projection；不得把包含 Agent、Provider 和 tool identity 的 combined
digest 写成 Session 自己的 profile-only digest。

Session fact 必须显式声明 `schema_version=2`；缺失或不相等都拒绝，不推断、不补齐。

文件型 Session Store 的一次 mutation 可能同时涉及 message/event JSONL、
Session/Turn/Execution 和 context manifest。它在 Session `flock` 内使用私有
`mutation_version=3` undo journal：replace 修改前持久化完整 preimage；JSONL
修改前持久化原始 `size + SHA-256 prefix_digest`。JSONL 追加通过 atomic
full-file rewrite 发布，不原地 append；prepared rollback 只有在当前 file identity
属于该 mutation、长度不少于原始 size 且前缀 digest 匹配时，才 atomic rewrite
回原前缀。新 Session root 还必须先用随机 nonce owner marker 与 journal 绑定，
再以 no-replace rename 发布；全部 fact 成功后再写 committed marker。commit 写入
报错以 strict/no-follow 重读到的磁盘状态为准，绝不把可证明 committed 的 mutation
回滚。启动和下一次同 Session 操作都先持锁恢复；prepared mutation 仅在
owner/scope/identity 完整匹配时回滚，committed mutation 保留并按
owner marker → journal 顺序清理。
recovery 只修复文件投影，不调用 Provider、不执行 command/tool，也不跨入
SQLite transaction。canonical Session `schema_version` 保持 2。

delete/GC rename 使用 `state/session-trash-moves/<session_id>.json` 的 private
`version=1` journal，绑定 source root identity，target 只允许
`_system/trash/<timestamp>/<session_id>`。恢复只接受“匹配 source、target
缺失”或“source 缺失、匹配 target”两种状态；前者执行 no-replace rename 并同步
source/target 目录；两种状态确认后都由上层重建 index，再清理 journal。两端同时
存在、同时缺失或 identity 漂移均保留 journal 并 fail closed。local-source
activation reset 同步删除
`sessions/`、`state/session-locks/`、`state/session-invocations/`、
`state/session-mutations/`、`state/session-trash-moves/` 和
`state/runtime.db*`。

Session filesystem 的操作基于 pinned directory FD、逐组件 `O_NOFOLLOW`、
single-link regular-file 和 device/inode 复核。删除前先 no-replace 移到随机 private
quarantine，复核 inode；不匹配则尝试恢复原名并失败收口。这一模型覆盖
symlink/hardlink、路径替换、确定性并发 swap 和 crash，不宣称抵抗已获得同 UID
任意代码执行、可持续枚举 quarantine 名称或使用 ptrace/kill 的攻击者；POSIX
不存在 compare-by-inode unlink。

## 6. Tmux

Tmux 是独立 interactive process manager：

- 固定短 `-S` socket、session `sn-session` 和 active
  `${SN_CLI_HOME}/resources/tmux.conf`；该文件由 source/payload
  `release/tmux.conf` 经 activation 映射；
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

## 7. Agent Kernel

Agent 只接受 API Profile。Kernel 使用注入的 `model.Generator`、`ToolExecutor` 和
`EffectRecorder`，按 model → tool validation → tool execution → message append
循环。工具执行前保存 prepared checkpoint；`tool.started` 后结果未知时 Run 进入
`needs_reconciliation`。Agent request 不接受 `cwd`；workspace roots/cwd 是
Runtime bootstrap 冻结到 tool execution snapshot 的配置，不是 per-Run override。

Run `AgentExecutor` 在创建 Run 前冻结完整、versioned、non-secret private
execution snapshot；Kernel 仍不读取 Profile、配置或数据库。snapshot 包含：

```text
execution_contract_version
model_execution_snapshot
tool_execution_snapshot
tool_execution_digest
session_request_digest       # 仅绑定 Session 时存在
session_config_digest        # 仅绑定 Session 时存在
config_digest
request_digest
```

`model_execution_snapshot` 保存完整 API Profile、Profile digest，以及实际选中
Provider driver 的 implementation 和 semantic version；`tool_execution_snapshot`
保存 tool implementation/version、canonical non-secret configuration 和完整
definitions。`config_digest` 绑定 Agent execution contract、model/Provider、tool
以及可选 Session config identity；`request_digest` 再绑定 immutable public Agent
request 和可选 Session request identity。Provider/tool implementation version 与
`execution_contract_version` 都是人工维护的执行语义版本，不是 build、release、
Git commit、CLI `contract_version` 或 LoopState schema version。

API Profile 只把 headers 中的 `${VAR}` 引用名（header 名 + 环境变量名）纳入
snapshot；resolved secret value 不进入 snapshot、digest、event 或 Store。每次新的
Provider call 仍从执行进程当前环境展开该引用，因此相同 `${VAR}` 引用名下的 secret
value 轮换不构成 execution drift；修改引用名或其它 Profile literal 则构成 drift。

fresh Run creation 和 Retry 在创建新 Run 前比较完整 current execution snapshot。执行时
还在首次 Session mutation、每个新 model call、fresh tool preparation 以及
fresh/recovered-prepared tool side effect 前复核 current model/Profile/driver、
tool identity/config/definitions、Agent version 和可选 Session digests。Resume
input 已 durable 接受但尚未推进 active pause 时也先复核；发生 drift 时保留原
pause，不执行新的 Provider/tool side effect。

恢复 durable `completed|failed|started` model/tool evidence、闭合已持久化 pause、
投影已知 terminal outcome，以及 cancel/reconcile 时，只使用 frozen snapshot 和
durable journal，不要求 current Profile、Provider 或 tool executor 仍存在或相同。
`started` tool effect 的结果仍是 unknown，必须进入 reconciliation，绝不因 current
环境可用而重放。frozen tool definitions 负责历史 request/schema 校验；只有实际
执行新的 handler 才使用 current tool executor。

Tool `input_schema` 在注册时编译，调用参数在任何 checkpoint、event 或 handler
副作用前按完整 JSON Schema 校验。prepared effect 的 request 和 LoopState 都持久
记录同一个原始 preparation `checkpoint_id`；恢复时同时核对 Run/Profile、
model request/result digest、round、Session canonical message prefix、seen tool
call、effect 和 event journal。任一证据缺失或冲突都 fail closed，不把 latest
checkpoint 冒充 preparation checkpoint。每个 completed `model_calls` 条目冻结完整
canonical `GenerateRequest`、request digest、完整 `ModelResult` 和 result digest；
恢复时按精确 assistant message boundary 重建 request/profile/trace，重新计算两个
digest，并从 durable result usage 汇总 `total_tokens`，验证 round/tool/token 计数
没有超过该 Run 的 effective budget。

内部 Agent LoopState checkpoint 使用 `schema_version=2`，并持久化
`base_message_count`。恢复时以该边界为锚，按每轮 durable model result、已闭合
tool effect 和经 pause schema 校验的 resume message 重建完整 message 序列，拒绝
额外、重复、乱序或无来源的 assistant/tool message。每个 historical effect 的
preparation checkpoint 都必须证明其 schema、Run/Profile、round、pending
call/cursor、seen 前缀和 committed event sequence；pause effect 在持久化前固定
`tool_call_id`，允许 journal 中保留多个已恢复的历史 `agent.paused`。

`model_calls` 以 `UNIQUE(run_id, sequence)` 约束 round evidence。Resume 使用不超过
1 MiB 的 strict envelope object：只接受 `pause_id` 和 `input`，拒绝未知/重复字段、
trailing data、`null` root 与 `null pause_id`；`input` 自身可为任意单一 JSON value
（包括 `null`），具体结构和 null 语义完全由 pause JSON Schema 决定。公开
Run Record 不序列化最新 resume input。

默认 tool 只读。`write_file` 必须在 `runtime.json` 显式启用，且受 workspace
roots、symlink 和大小门禁。builtin 集合不提供任意 subprocess 执行能力；
root/path 检查不是 OS sandbox，未知 tool 名称一律 fail closed。
`read_file`、`list_directory` 在参数 schema 验证通过后的无副作用文件系统拒绝，
以尺寸受限的稳定 JSON `ToolResult{IsError:true}` 闭合 effect 并允许下一轮模型继续；
内部结果编码失败或正常输出超限仍按未知 effect 安全收口。
三个文件工具在 Build 时固定 canonical workspace root 的 device/inode，执行时以
`O_NOFOLLOW + openat/fstatat` 逐组件绑定目录 fd；read 使用 nonblocking 单链接
regular fd 和 bounded read，list 绑定目录 fd，write 在固定 parent fd 内以
crypto-random `O_EXCL` 私有临时文件执行 file fsync、`renameat` 和 directory fsync。
该边界抵抗确定性的 root/parent/component/leaf path swap；遍历拒绝 symlink，
read 额外拒绝 FIFO、非 regular 和 `nlink>1` hardlink。不宣称抵抗已完全控制同
UID 进程、可 ptrace 或可直接操纵既有 fd 的攻击者。

外部 tool 的唯一运行配置面是 active `${SN_CLI_HOME}/tools/<name>.json`；
source/payload 是 `resources/tools/<name>.json`。当前 loader 只接受 `schema_version=1`、
`effect=read_only`、`executor.type=mcp`，文件 basename 必须等于 local tool
name，并拒绝与 builtin 同名。`runtime.json` 只保存 enabled name；bootstrap 将
builtin 与选中的 manifest 组合为一个 Registry，任何 enabled name 无 owner 都
fail closed。默认启用 `web_search` 和 `web_fetch`，分别绑定 BigModel MCP 的
`web_search_prime` 与 `webReader`，认证引用均为
`Bearer ${Z_AI_API_KEY}`。

每个 MCP handler execution 建立一个有界 Streamable HTTP session，执行
`initialize`、`notifications/initialized` 和一次 `tools/call`；不 retry、不跟随
redirect。manifest 的 canonical definition、endpoint、remote tool、header 环境
引用、timeout 和 response limit 冻结进 child tool snapshot，resolved secret 不
进入 snapshot、digest、event 或错误。网络、HTTP、JSON-RPC、协议与远端 tool
错误用有界、脱敏的 `ToolResult{IsError:true}` 闭合只读 effect，使模型可在下一轮
解释失败；Session 和 `req` 不执行该 handler。

private execution snapshot 是 Store-only 明文元数据，不是加密容器。它不会进入
公开 DTO、event、log 或 error，但会保存 endpoint、model、按契约视为 non-secret
的 literal header、tool schema、workspace root/cwd identity 等信息。digest 和
semantic identity 用于检测已表示的配置/实现漂移，不是 binary provenance
attestation、数字签名或 OS sandbox；未同步 bump semantic version 的实现变化，以及
已能以同 UID 篡改进程或 SQLite 的攻击者，均超出该门禁的保证范围。

`run reconcile --run-id <id>` 是 Agent unknown tool effect 的唯一显式收口入口：
它不重放 Agent/tool，而是保留 checkpoint、event 和 tool-effect evidence，并将
Run 结案为 failed。Agent 绑定 Session 时，Session 在 reconciliation 前保持
`blocked + active Turn(running) + Execution(settled/unknown)`；该命令先幂等收口
Session projection，再提交 Run terminal barrier。重复调用返回同一 terminal
record。Agent 在执行 tool 前在 Turn 上原子持久化 `agent_owned=true` owner
marker；若进程在 unknown projection 前退出，Execution 可以仍是 `running`，
`run reconcile` 仍按精确 `run_id` 收口。`paused` 不走 reconciliation，只能通过
`run resume` 恢复。pause/resume 是 Kernel extension：底层 CLI/API/Store 和
validator 保留，但 stock builtin/MCP tools 不产生 Pause，`server info` 因此不发布
`resume` capability。

## 8. Durable Run

```text
queued → running ─┬→ paused → queued
                  ├→ needs_reconciliation
                  ├→ completed
                  ├→ failed
                  └→ cancelled
```

`retry` 只接受 terminal Run，创建新 Run 并以 `retry_of` 关联。Agent Retry
byte-for-byte 保留原 `private_request_json`，不根据当前配置重新冻结；创建新 Run
前必须先把 current execution snapshot 与原 snapshot 完整比较，发生 drift 时拒绝
且不产生新 Run。相同 `${VAR}` 引用名下的 secret value 轮换不参与该比较。
terminal publish barrier 必须在一个 SQLite transaction 内提交 result/error、
terminal event/state、`run.settled`、settled sequence 和 queue removal。settled
后禁止追加 event。

Resume validator 先把 envelope 绑定到 immutable active pause bytes 和可选 expiry；
Store transaction 再核对 Run 仍为 `paused`、没有 cancellation reservation、pause
bytes 精确相等，并在事务内采样 `accepted_at`。等于 expiry 的 acceptance 有效，
晚于 expiry 才冲突。该事务同时写 canonical resume input/digest/`accepted_at`
journal、更新 latest `resume_accepted_at`、清理 pause/error/cancel flag，并通过
CAS 重新入队。非法 envelope、schema/expiry conflict、exact pause 漂移或 CAS
零行更新都整体回滚，不产生 journal、state 或 queue mutation；并发 resume 最多
一个成功。恢复时 latest acceptance 必须与 contiguous resume journal 的最后一条
精确一致。

`queued` 和 `paused` cancellation 先在 SQLite 持久化 reservation 并移除 queue，
再由 kind-specific finalizer 收口，最后通过 terminal publish barrier 转为
`cancelled`；启动 recovery 使用专用 keyset scan 排空遗留 reservation，不受普通
list limit 影响。`running` Run 的 owner worker 轮询同一 SQLite durable flag；
独立 CLI/HTTP 进程写入 reservation 后，worker 取消 execution context 并由
kind-specific finalizer 收口。普通 terminal/reconciliation publish 必须拒绝已有
reservation，只有 cancellation-owned publish 可以消费它。

`run reconcile` 本身是操作者对 unknown outcome 的显式确认。Session Run 从已经
人工 reconciliation 的 Session evidence 收口；Agent Run 由 Agent executor
收口。两种 kind 均不得 replay 原执行。已经 reconciliation 的 Agent terminal
result 带显式 acknowledgement marker，重复调用幂等返回该 record；普通 terminal
Agent Run 不会被误报为已 reconciliation。

SQLite `PRAGMA user_version=4`。缺失、不相等或混合 schema fail closed。

## 9. CLI 与 HTTP

固定 CLI namespace：

```text
exec req profile session tmux agent run server help version
```

Runtime machine contract 为 `schema_version=1`、`contract_version=4`。
bare CLI direct、`exec` 和 `tmux attach` 不属于 machine wrapper。

Run Record 可以公开 Runtime-owned `request_digest/config_digest`，但
`private_request_json`、Agent execution snapshot 和最新 Resume input 均不属于
public machine contract，不得经 CLI/HTTP query、result、event、watch、log 或 error
输出。

Run application composition 按 action 分层：query/watch 只加载 Run Store；
cancel/reconcile 只加载 Run Store 和 Session maintenance service；GC 仅在未显式
提供 cutoff 时读取 retention 配置。上述 maintenance 路径必须能只用 private
snapshot 和 durable evidence 工作，不加载 current Profile、Provider 或 tool。
带 `--queue` 的 `session exec|req`、`agent [--queue]`、resume/retry 和 worker execution 才
加载完整执行依赖。`run` namespace 不接受 fresh submission，只查询或控制已有
Durable Run；`retry` 仍是基于已有终态 Run 的控制动作。

本次 namespace 拆分只改变 CLI ingress。HTTP route/DTO、Session 文件 schema 与
SQLite Run schema 保持不变；`POST /v1/runs` 继续接受 HTTP queued submission。

HTTP 使用同一 application service。所有 JSON body 共同限制 request size、合法
UTF-8、单一完整 JSON value，并拒绝重复 key 和 trailing data；固定 Runtime DTO
要求 non-null object、拒绝未知字段和任意显式 `null`，无业务字段的 POST control
action 还必须精确为 `{}`。`/v1/model/generate` 使用 strict object 后执行 canonical
`GenerateRequest.Validate`；`/v1/runs/{run_id}:resume` 使用上述 strict resume
envelope，只有 envelope 的 `input` 字段 shape/null 由 pause schema 决定。NUL
只由 canonical text field validator 拒绝，不是 decoder 对所有 JSON string 的
全局禁令。Session/Run collection query 使用 allowlist、单值和显式空值校验；
malformed、未知、重复或显式空参数均拒绝，只有省略才使用领域默认值。Session 新增
execution query 与 reconcile route；HTTP 不提供 Tmux
控制，也不能上传 command、env、Provider payload 或 tool handler。

HTTP、CLI machine error 与 server Bearer 认证使用 canonical `RuntimeError`。
malformed resource ID 是 `invalid_request/400`；合法 ID 指向不存在的资源是
`not_found/404`；Store 故障是 `internal/500`。不得用空 list/event 假装目标资源
存在，也不得把 Store 故障降级成 not-found。

## 10. 激活协议

contract-v4 archive 带 activation epoch 4、当前 contract、Tool manifest、Session
和 Run schema
版本。installer/updater 只能由 staged candidate 在 maintenance/lifecycle lock、
quiescence 和 exact-schema preflight 全部通过后激活。

source 与 archive payload 使用同一配置布局：`configs/*.json`、
`resources/schema/*.json`、`resources/tools/*.json` 和
`release/{runtime.json,tmux.conf,release.json}`，binary 仍为 payload 根级
`sn-cli|sn-server`。activation 映射为 active `configs/`、`tools/`、根
`runtime.json`、`resources/schema/` 和
`resources/{tmux.conf,release.json}`；active home 自身不是合法 payload reader。
candidate 读取 payload 自身的 `release/release.json`。在创建 target 目录、lock、
stage 或停止 server 前，candidate 必须先对 payload 执行完整
`profile check`，并以 required/no-follow/inode-pinned 方式校验
`release/runtime.json`、`resources/tools/`、`release/tmux.conf` 和
`resources/schema/` 下三个具有固定 `$id`/root shape 且可编译的 JSON Schema；
构造 staged home 后再次验证同一组契约。激活事务先持久化 schema 3 journal 与
state guard，再用 no-replace regular file 暂时占用 active `bin/`、`configs/`、
`tools/`；
二次进程扫描按 inode 和 PID/start-token 判定。任何无法证明 all-original 或
all-staged 的恢复状态都保留 guard/barrier，禁止自动放行。journal 在
`committed|rolled_back` terminal
phase 仍阻断所有入口，直到 stage、rename、guard/journal 的 durable cleanup 完成。
installer 只允许 home 外部的 install-dir；activation mutation 前使用稳定
directory FD、durable owner sidecar 和 no-clobber `symlinkat` 预留 command link，
不覆盖任何已有 entry。失败或 release 不删除 owner/link；retry 必须重新验证
owner 内容、parent/link/owner inode 和 exact target 后才可复用。

运行中的 server、managed Tmux window、active/unknown Session execution、
queued/running/paused/needs-reconciliation Run、目标 home binary process 或
任意 unsupported schema 状态都阻止激活。`--overwrite-configs` 不绕过运行态门禁。

唯一例外是仓库根目录的 local-source `make install`：它不是 release/update
语义，固定把 source `configs/`、`resources/tools/`、`release/runtime.json` 与
其它 managed resource 按 source→active 映射覆盖 active home，并由 staged candidate
在 lifecycle lock 内安全
停止 server。该模式仍要求 Tmux 和目标 binary process quiescent，并显式授权在不
解析现有 Session/Run state 的情况下重置本地运行态；只有 staged artifact 全部提交
并验证后，才在 guard 下幂等删除
`sessions/`、`state/session-locks/`、`state/session-invocations/`、
`state/session-mutations/`、`state/session-trash-moves/` 和
`state/runtime.db*`，然后解除 journal。安装终态固定为 server stopped，且这项
reset 授权不能由 archive installer 或 `server update` 获得。

所有公开配置、Session/Run fact、SDK request 和 machine output 都必须完整符合当前
schema。CLI Profile 只进入 bare direct、`exec`、`session exec` 或 `tmux start`；
API Profile 只进入 `req`、`session req` 或 `agent`。类型不匹配 fail closed，不提供
alias、自动迁移或 legacy ingress。
