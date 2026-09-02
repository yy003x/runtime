# SN Runtime 契约

本文是 Runtime 的当前总契约。代码、严格 loader、SQLite schema 和测试与本文冲突
时，必须在同一次变更中消除差异。

## 1. 架构、边界与 Owner

| 领域 | Owner | 负责 | 不负责 |
| --- | --- | --- | --- |
| Profile facade | `pkg/profile/` | 唯一配置目录加载、类型分流、ID 解析 | 执行、历史 |
| Command adapter | `pkg/command/` | CLI grammar、effective argv/env/cwd | Session/Tmux 状态 |
| Model Core | `pkg/model/`、`pkg/contract/` | 单次 canonical model call | tool loop、存储 |
| API Driver | `pkg/provider/*` | HTTP/SSE codec、Provider error | retry、tool、Session |
| Provider HTTP helper | `internal/infrastructure/providerhttp/` | Driver 共用的 response limit、network/status error mapping | 公开 API、Provider codec |
| Tool Config/MCP adapter | `internal/infrastructure/toolconfig/`、`internal/infrastructure/toolmcp/` | 严格 manifest、单次 MCP call | model loop、retry、持久化 |
| Session Service | `pkg/session/` | Session/Turn/history/execution | 自动执行 canonical tool |
| Native console carrier | `internal/infrastructure/tmux/` | private default/dedicated tmux session/window lifecycle、opaque binding | 公开 Go API、Session/history、Turn/Run 创建 |
| Agent Kernel | `pkg/agent/` | 唯一 model/tool loop、预算、暂停恢复 | Profile、SQLite |
| Run Harness | `pkg/run/` | durable identity、queue、journal、checkpoint | Session 策略 |
| Store | `pkg/store/sqlite/` | SQLite WAL 与 terminal barrier | 业务 workflow |
| Inbound adapter | `internal/interfaces/cli/`、`pkg/transport/http/` | decode/call/encode | 第二套状态机 |

源码只使用三个 Go 代码根：

```text
cmd/                         可执行程序入口
pkg/                         对外可 import 的 Go API 与 adapter
internal/
  domain/                    Runtime 私有值对象与不变量
  application/               激活、启动、工具装配与 use-case 编排
  infrastructure/            配置、文件、日志、协议和进程 adapter
  interfaces/                入站 CLI adapter
  testkit/                   仅测试可复用资产
```

这是面向 CLI Runtime + Agent 的 DDD 适配，不套用电商 HTTP 服务模板：公开 Runtime
领域能力保留在 `pkg/`，HTTP handler 因可嵌入而保留在 `pkg/transport/http/`；私有
四层用于隔离值对象、工作流、出站 adapter 与入站 CLI。依赖守卫固定为：

- `internal/domain` 不依赖 application、infrastructure、interfaces 或 `pkg`；
- `internal/infrastructure` 不反向依赖 application/interfaces；
- `internal/application` 不依赖 interfaces；
- `pkg` 不依赖 internal application/interfaces；
- `pkg/` 下每个 package 都是已登记的公开 Go API/adapter，不嵌套私有 `internal`
  package；
- `internal/application/runtimebootstrap/` 是唯一 composition root；
- `pkg/agent/` 不读 Profile 或数据库；Provider driver 每次只做一次 HTTP attempt；
  CLI/HTTP 不拼装独立历史。

调用方只能通过 CLI、HTTP 或 `github.com/yy003x/runtime/pkg/...` 集成，不直接读写
`${SN_CLI_HOME:-~/.sn}`。本次包迁移不保留旧的
`github.com/yy003x/runtime/{agent,session,...}` import shim。

用户语境中的 SN root（`SN_ROOT` 概念）等同 Runtime Home；唯一公开配置环境变量是
`SN_CLI_HOME`，缺省为 `~/.sn`，不提供第二个 home alias。

source、release payload 与 active home 的配置映射固定为：

```text
source/payload configs/*.json          → active configs/*.json
source/payload resources/tools/*.json  → active tools/*.json
source/payload release/runtime.json    → active runtime.json
source/payload resources/schema/*.json → active resources/schema/*.json
source/payload release/tmux.conf       → active resources/tmux.conf
source/payload release/release.json    → active resources/release.json
```

archive binary 仍位于 payload 根级 `sn-cli|sn-server`。active home 不是合法的
source/payload reader，也不得反向生成 source。未来可交付的 `skills/`、`mcp/`
资产只能扩展在 source/payload `resources/` 下。

## 2. 配置、Profile 与 command adapter

Profile 位于 `configs/<id>.json`，必须以 `type=cli|api` 分流。CLI Profile 字段：

```text
command args env model effort prompt cwd
```

loader 只接受上述字段。不存在 command ID、第二层映射或 raw/native argv
passthrough。`resources/schema/{profile,runtime,tool}.schema.json` 与严格 Go loader
共同构成规范：Schema 负责文档 shape，loader 负责 adapter grammar、环境引用、
duration、跨字段约束与文件系统事实；loader-valid 必须 schema-valid。

CLI Profile 的 `command` 不经过 shell，只按 basename 选择已登记的 Codex/Claude/Grok
adapter；`args` 一字符串一 argv token；`env` 在 inherited environment 上覆盖，
`null` 删除变量；`${NAME}` 是唯一插值语法。`model`、`effort`、`prompt`、`cwd`
可省略，执行 mode 只由 CLI namespace 决定。旧 `exec` 字段和所有未知字段均拒绝。

API Profile 只接受以下字段：

| 字段 | 契约 |
| --- | --- |
| `type` | 必须为 `api` |
| `driver` | `openai|anthropic` |
| `endpoint` / `base_url` | 必须且只能配置一个；HTTPS；`base_url` 不接受 query/fragment |
| `model` | 必填 Provider 模型名 |
| `headers` | 可选；secret 使用 `${VAR}`；resolved value 不持久化 |
| `parameters.max_tokens` | canonical 默认输出上限；Anthropic 必填 |
| `parameters.temperature` | 可选，`0..2` |
| `parameters.top_p` | 可选，`0..1` |
| `parameters.stop_sequences` | 可选，最多四个非空字符串 |
| `timeout` | 必填 Go duration，范围 `(0,24h]` |
| `context.window_tokens` | Session 本地上下文窗口；`0`/省略为 `32768` |
| `context.reserved_output_tokens` | 本地输出预留；`0`/省略基础值为 `8192` |
| `context.keep_recent_turns` | `summary_enabled` 压缩时保留的最近 turn 下限；`0`/省略为 `1`；冻结进 snapshot |
| `context.summary_enabled` | `true` 时投影对溢出历史做压缩（见 §4），否则溢出 fail-closed；冻结进 snapshot |

`runtime.json` 缺失时普通 bootstrap 使用默认值；存在时必须是 no-follow regular
file、严格 JSON 且无未知字段。当前 shape 与默认语义为：

```json
{
  "agent": {
    "tools": ["read_file", "list_directory", "web_search", "web_fetch"],
    "workspace_roots": [],
    "max_rounds": 16,
    "max_tool_calls": 64,
    "max_total_tokens": 0,
    "max_wall_time": "15m"
  },
  "scheduler": {"workers": 1, "poll_interval": "250ms"},
  "run": {
    "settled_retention": "168h",
    "reaper": {
      "interval": "5m",
      "paused_ttl": "30m",
      "needs_reconciliation_ttl": "24h"
    }
  },
  "tmux": {"server_mode": "default"},
  "mcp": {
    "requested_protocol_version": "2025-06-18",
    "allowed_protocol_versions": ["2025-06-18", "2024-11-05"]
  }
}
```

实际文件可以省略使用默认值的字段；`workspace_roots` 省略时取启动 cwd，显式配置
必须是非 `null` array。duration 额外限制：`agent.max_wall_time=1s..24h`、
`scheduler.poll_interval=10ms..1m`、`run.settled_retention=1h..8760h`；reaper TTL
写 `0` 表示禁用对应回收，`run.reaper.interval=0..1h`，两个 TTL 均为
`0..720h`。activation/staged-home 使用 required loader，缺失配置不能回退到
默认值。

执行矩阵：

| 入口 | effective mode | 执行 owner | 记录 |
| --- | --- | --- | --- |
| `sn-cli <cli-id>` | interactive direct | process replacement | `cli.jsonl`；无 Session/Run |
| `sn-cli exec <cli-id>` | non-interactive exec | process replacement | `cli.jsonl`；无 Session/Run |
| `sn-cli call <api-id>` | API request | Model Core | `api.jsonl`；无 Session/Run |
| `sn-cli session exec <cli-id> [--queue]` | non-interactive managed exec | Session child | `cli.jsonl` + Turn/Execution；queue 时另有 Run |
| `sn-cli session call <api-id> [--queue]` | API request | Session executor | `api.jsonl` + Turn/Execution；queue 时另有 Run |
| `sn-cli session open <cli-id>` | Provider-native interactive TUI | Tmux PTY + native_tui Session identity | opaque lifecycle Run/Execution；无 canonical transcript/Turn |
| `sn-cli agent <api-id> [--queue]` | API model/tool loop | Agent Kernel | 每轮 `api.jsonl` + Durable Run |

namespace 先固定 execution mode，Profile `type` 再做严格配对校验。Profile 不保存
execution mode；Session 与 private tmux carrier schema 不因 CLI ingress 改名而变化。

`session open|send|attach|interrupt|close|close-all` 是显式 composition：`open` 使用
interactive adapter，在私有 tmux PTY 中直接启动 Provider 原生 TUI，
并发布 `interface=native_tui` Session fact。tmux window 只保存
`binding={kind:"session",id:<session_id>}`；初始 input 是 Provider interactive argv 的
最终 prompt token，后续 `send` 使用 tmux paste buffer。`open` 发布一个 running
的 `kind=native_tui` Durable Run，并生成唯一 `execution_id`；该 Execution 只记录
Provider process/window 的 opaque lifecycle，不保存输入输出，也不声称交互任务完成。
`accepted=true` 只表示 tmux 接受该 transport mutation。pane 文本、终端回显与输入均
不进入 canonical history，也不创建 Turn/Message/Event 或 transcript。Session file
Store 与 SQLite Run Store 不共享 transaction；因此不再复制一套 Session Event 或
Execution fact，而由 `session show` 只读投影唯一 lifecycle Run/Execution，避免两个
canonical owner 产生 partial consistency。
terminal lifecycle event 仍由 Run Store 在 settle transaction 中写入，并通过
`run events --run-id <run_id>` 查询；它不是 Session Event。

`send` 是终端输入，受 PTY line discipline 与 Provider line editor 语义约束，不承诺
大块结构化 payload 的完整消费；这类 machine task 应使用 `session exec`。

`session open` 只接受 CLI Profile，默认 detached，`--attach` 仅用于 human TTY，
`--attach` 与 `--detach` 互斥。`--prompt` 接受 text 或相对 invocation cwd 的 regular
file，并按 Profile base prompt、typed prompt、stdin、位置 input 的顺序合并。未给
`--session-id` 时生成新 ID；显式 ID 也必须尚不存在。
当前不从 Runtime Session ID 推断 Codex/Claude/Grok native resume identity，因此已存在或已
关闭的 `native_tui` Session 不能用 `session open` 重开。`send` 只对 running binding
注入 raw input，`interrupt` 发送 `C-c`。Provider process 自然退出时，supervisor 先在
同一 SQLite transaction 中提交 terminal Run、result/error、terminal event 与
`run.settled`，再精确关闭绑定 window；exit code 0 对应 `completed`，其它 exit/signal
对应 `failed`。`close` 先把仍 open 的 lifecycle Run settle 为 `cancelled`，再停止绑定
window；carrier stop 会向 supervisor 发送 `SIGTERM`，supervisor 显式向 Provider 转发
`SIGHUP|SIGTERM`，最多等待 2 秒后发送 `SIGKILL`，并在确认 supervisor identity 已消失后
才返回成功。`C-c`/`SIGINT` 只中断 Provider 当前交互，不启动强制退出计时。关闭保留
Session fact。`run cancel|retry|resume` 不接管 `native_tui` lifecycle；
关闭必须使用 `session close|close-all`。上述 Session ID 不能传给 `session exec|call`。

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

bare Profile 只接受 CLI Profile；`exec` 只接受 CLI Profile；`call` 只接受 API
Profile。Profile ID 必须紧跟拥有它的 namespace/action，option 位于其后，input
必须最后。固定根 namespace
`doctor|exec|call|profile|session|agent|job|server|help|version|update`，以及 Profile 管理
action `list|show|check` 都是保留 Profile ID；loader 遇到冲突即失败。

`profile` 只提供 `list|show|check` 管理动作，不执行 Profile。`profile check` 是纯
静态校验，不解析真实 env/PATH/cwd，不读取 prompt file。

## 3. 本地执行、审计与进程日志

`${SN_CLI_HOME}/logs` 是 best-effort 本地诊断面，不是 canonical Session/Run Store，
不参与 replay、幂等、恢复、terminal barrier 或 API contract：

```text
logs/
  YYMMDD/
    cli.jsonl
    api.jsonl
    audit.jsonl
  sn-server.log
```

每天、每种 Profile 类型只使用一个 append-only JSONL 文件。`time` 使用本地时间的
`YYYY-MM-DD HH:mm:ss`。当前不做 retention、总量限制或自动 GC；旧 flat log 保持
原样，不迁移，也没有兼容 reader。

记录门禁固定为：必须有 Profile ID，并且进入真实执行边界。CLI 在最终 invocation
已构建并准备 launch 时写一条；API 只有在 driver 调用 `http.Client.Do` 时写一条。
Profile 查询/校验、Session/Run/Tmux 查询与控制、无网络的前置校验失败、queue submit
都不写 `cli.jsonl`/`api.jsonl`；queued Session/Agent 由 worker 真正执行时写，Agent
每个 Provider round 各写一条。HTTP `POST /v1/model/generate` 同样进入统一 API 日志。MCP HTTP 不属于
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

`audit.jsonl` 是脱敏控制面审计，`schema_version=1`，时间为 UTC RFC3339Nano。CLI
记录 `doctor`，Session 的执行提交/terminal/lifecycle mutation，Tmux mutation，Agent
提交，Run mutation 与 server lifecycle/update；只读 list/show/query 不记录。HTTP
记录每个请求的 method、规范化 route、HTTP status 和通过严格校验的 Session/Run ID。
字段只允许 `source,namespace,action,outcome,targets,error_code,error_phase,http_status`，
不得保存任意 argv、query、request/response body、prompt/send 内容、error message、
header、cookie 或 resolved secret。审计写入是 best-effort；只有 `doctor` 会显式探测
并报告 audit sink 不可写。

`sn-server.log` 仅接收后台 server process 的 stdout/stderr。它与每日 JSONL 同属
Runtime log root，不得重新写入 `state/`。

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

当估算输入超过 input budget 时，默认 fail-closed（`context_overflow`）。Profile
`context.summary_enabled=true` 时，Session 投影改为**确定性压缩**：丢弃整数个最旧
turn，保留 `context.keep_recent_turns`（下限 1）个最近 turn 的逐字内容，使投影落入
预算；即便压到下限仍超限则仍 fail-closed。当前增量是纯截断（不插入摘要消息、不保留
被丢弃内容的语义信息；模型生成摘要是后续增量）。压缩事实记录在 append-only
`sessions/<id>/summaries.jsonl`（每行 `SummaryRecord`，自带 `summary_version=1`）：
`range_start_seq/range_end_seq` 标记被丢弃的 canonical 区间（`range_end_seq` 为首个
保留消息序号），`compacted_range_digest` 是被丢弃消息 canonical JSON 的 sha256。该
`summary_id` 写入当 turn `ContextManifest.CheckpointRef`，`CheckpointDigest` 写入
summary 整体 digest。`summary_enabled` 与 `keep_recent_turns` 均进 `config_digest`，
Run 中途翻转触发 drift fail-closed。

压缩把"发给模型的消息 = canonical 历史的逐字前缀"推广为 **grounded 投影** =
`canonical[range_end_seq:]`。Agent 的 `BaseMessageCount` 前缀校验据此偏移：`SettleAgent`
与 `findAgentTurn` 读取 manifest 的 `CheckpointRef`，重算被丢弃前缀的 digest 与
`compacted_range_digest` 比对，匹配则按偏移比较尾部而非逐字前缀；篡改 `messages.jsonl`
被丢弃前缀使 digest 不匹配即 fail-closed。`messages.jsonl` 仍严格 append-only，`summaries.jsonl`
复用 append-kind mutation journal 的 PrefixDigest 回滚。Session `schema_version` 保持 3
（复用已预留的 `CheckpointRef/CheckpointDigest`，未改 shape）。

Provider transient error（`RuntimeError.retryable=true`，含 `rate_limited`、
`provider_unavailable`、`timeout`）由 `model.Service` 外层的 `ResilientModel`
按 `retry_after_ms` 与指数退避做有界重试（默认最多 3 次）。Driver 仍只执行
单次 HTTP attempt，重试不改变 execution snapshot、不跨越 before-effect gate；
non-retryable error 与 reconcile 路径抑制的 error 不重试。每次重试的流式
event 在内部捕获，仅成功 attempt 的事件重放到 sink，失败 attempt 不泄漏部分
事件。

重试判定与边界固定为：

| 来源 | Retryable | 说明 |
| --- | --- | --- |
| HTTP 401 / 403 | 否 | `authentication_failed` / `permission_denied` |
| HTTP 429 | 是 | `rate_limited`，携带 `Retry-After` 换算的 `retry_after_ms` |
| HTTP 5xx | 是 | `provider_unavailable` |
| 其它 HTTP `>=300` | 否 | `protocol_error` |
| `context.DeadlineExceeded` / 传输超时 | 是 | `timeout` |
| `context.Canceled` | 否 | `cancelled`，不重试 |
| 其它传输错误 | 是 | `provider_unavailable` |

`RetryPolicy` 默认：`MaxAttempts=3`（含首次）、`BaseDelay=200ms`、按
`BaseDelay * 2^attempt` 指数退避、`MaxDelay=5s`、`Jitter=0.2`。Provider 返回的
`Retry-After`（换算为 `retry_after_ms`）优先于指数退避，但被 `MaxDelay` 封顶，
单次模型调用的退避不会超过 `MaxDelay`，避免单点 Provider 卡死整个 Run。每次重试
仍是 Driver 的一次全新 HTTP attempt，`SingleAttemptClient` 禁止自动 redirect，
单次 attempt 不产生多个 HTTP 请求。

## 5. Session

canonical 文件布局固定为：

```text
sessions/<session_id>/
  session.json
  messages.jsonl
  events.jsonl
  turns/<turn_id>/turn.json
  turns/<turn_id>/context-manifest.json
  executions/<execution_id>.json
  context/current.json
sessions/_system/index.json
sessions/_system/trash/<timestamp>/<session_id>/
state/session-locks/
state/session-invocations/
state/session-mutations/
state/session-trash-moves/
state/runtime.db
```

`sessions/_system/index.json` 是 Session list 的 derived read model。正常启动只严格
读取和校验该 index，不再扫描全部 Session/Turn/Execution/JSONL；index 缺失时从
canonical `session.json` 重建，index 损坏、schema 不匹配或事实冲突时 fail closed。
每次 Session mutation 在同一 Session 锁和 recovery contract 内提交 canonical facts，
随后在全局 index `flock` 下做单 Session upsert/remove；committed mutation 或 trash
move crash recovery 必须先幂等修复 index，再清理 journal。`doctor` 与 activation
preflight 显式执行全量 canonical fact 校验，并核对 index 与 `session.json` 完全一致；
该 O(N) 校验不属于普通 Runtime bootstrap。

`session_id` 标识 Runtime conversation，`turn_id` 标识一次用户输入及其 terminal
result，`execution_id` 标识一次具体 API attempt 或 managed process，`run_id` 标识
一次 durable execution，`task_id` 是 caller-owned correlation label。所有 ID 均不
复用。

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
- Grok：`--output-format json --single=<prompt>`，prompt 必须用 `=` 附着（`--` 会被
  当成 prompt 本身），只接受唯一成功 JSON object 的 `text`；`type=error` 拒绝；
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

Session fact 必须显式声明 `schema_version=3` 和
`interface=managed|native_tui`；缺失或不相等都拒绝，不推断、不补齐。
`session exec|call` 只创建或复用 `managed` Session；`session open` 只创建
`native_tui` Session。两种 interface 不能共用 Session ID，schema v2 不自动迁移。

API Session 返回 canonical tool call 后，Turn 进入 `requires_action`、Session 进入
blocked；内部领域协议以原始 `tool_call_id`、content、`is_error` 和 idempotency key
闭合 pending tool。相同 key+payload 重放返回相同 receipt，key 或 call ID 冲突
fail closed。stock CLI/HTTP 当前不发布 tool-result 写入口，自动 tool loop 只属于
Agent；CLI Provider 子进程内部的 tool 行为保持 opaque。

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
SQLite transaction。canonical Session `schema_version` 保持 3。

delete/GC rename 使用 `state/session-trash-moves/<session_id>.json` 的 private
`version=1` journal，绑定 source root identity，target 只允许
`_system/trash/<timestamp>/<session_id>`。恢复只接受“匹配 source、target
缺失”或“source 缺失、匹配 target”两种状态；前者执行 no-replace rename 并同步
source/target 目录；两种状态确认后都从 index 移除该 Session，再清理 journal。两端同时
存在、同时缺失或 identity 漂移均保留 journal 并 fail closed。local-source
activation reset 同步删除
`sessions/`、`state/session-locks/`、`state/session-invocations/`、
`state/session-mutations/`、`state/session-trash-moves/` 和
`state/native-tui-invocations/`、`state/runtime.db*`。

Session filesystem 的操作基于 pinned directory FD、逐组件 `O_NOFOLLOW`、
single-link regular-file 和 device/inode 复核。删除前先 no-replace 移到随机 private
quarantine，复核 inode；不匹配则尝试恢复原名并失败收口。这一模型覆盖
symlink/hardlink、路径替换、确定性并发 swap 和 crash，不宣称抵抗已获得同 UID
任意代码执行、可持续枚举 quarantine 名称或使用 ptrace/kill 的攻击者；POSIX
不存在 compare-by-inode unlink。

Retention 只接受 `ephemeral|standard|pinned`。`session gc` 默认 dry-run，只选择超过
cutoff 且当前仍为 `retention=ephemeral,state=idle` 的 Session；`--apply` 在 Session
锁内复核后通过 trash-move journal 移入 `_system/trash`。结果区分锁外扫描的
`candidates`、实际移动的 `moved` 与并发变化后安全跳过的 `skipped`；单个候选失效
不使整批失败。`session delete` 同样是可恢复移动，不物理擦除 trash。

`session close-all` 对调用开始时的 Session-bound window 做快照，按每个 opaque
binding 的 `tmux_id` 精确停止 window。它只处理 Session binding，也不删除
`native_tui` Session fact；每个仍 open 的 lifecycle Run 都先 settle 为 `cancelled`，
再停止对应 window，并等待相应 supervisor identity 消失。空快照成功返回
`closed_count=0`。中途失败返回错误并保留已完成的
close，剩余项可安全重跑。

## 6. Private Tmux carrier

Tmux 是 `native_tui` Session 的私有 interactive process carrier，不是公开 CLI
namespace：

- `runtime.json` 的 `tmux.server_mode=default|dedicated` 选择普通 `-L default`
  server 或按 Runtime home 派生的短 `-S` socket；缺省为 `default`；
- session 固定为 `sn-session`；default 使用用户正常 tmux 配置且允许其他 session
  共存，dedicated 只读取 active `${SN_CLI_HOME}/resources/tmux.conf`；该文件由
  source/payload `release/tmux.conf` 经 activation 映射；
- 每次 `session open` 建一个 Session-bound window，Profile 固定 interactive mode；
- initial prompt 是最终 argv token，后续 `send` 才使用 paste buffer；
- default session marker 或 dedicated server marker 绑定 full canonical-home
  digest、uid、config digest 和 incarnation；
- `tmux_id`、window/pane identity、owner 和 incarnation 每次 mutation 都复核；
- open 使用 ready/go gate 和 mode 0600、消费即 unlink 的 launch manifest；
- tmux server 只继承 sanitized env，不缓存 Profile secret；
- private stop 只 kill 精确 managed window；最后一个 window 退出后，default 只关闭
  `sn-session`，dedicated 关闭 sentinel/server；
- registry 只表示当前 live/dead window，不建立 durable transcript/history。

`default` 模式遵循当前 `TMUX_TMPDIR` 与用户 `~/.tmux.conf`。Runtime 只拥有带合法
session marker 的 `sn-session`；同名 foreign session 一律拒绝接管，名称相似的其它
session 不受影响。`dedicated` 模式的 sentinel 只负责维持隔离 server，不是用户
window。修改 `server_mode` 不迁移 live window；同一 Runtime home 在另一模式仍有
managed window 时返回 conflict。

private start 先生成 `tmux_id` 并创建 blocked bootstrap helper。helper 写 ready fact后等待 go；
Runtime 校验 pane、process、executable 与 manifest，再提交 registered marker 并释放
target。marker 前失败删除 window，marker 后失败保留可由 Session owner 收口的
`starting|exited` record。`session open` 的 target 是 Profile 解析后的 Provider CLI；
输入只经过 Provider argv 或 tmux paste buffer，不建立第二条传输通道。

window state 只接受 `starting|running|exited|orphaned`。Session-bound native TUI
在 lifecycle Run 成功收口后自动 stop。private send/interrupt 只接受 running，
private stop 接受所有 state。
`send` 合并 stdin/位置 input，要求非空 UTF-8、无 NUL、最大 1 MiB，并通过唯一
paste buffer 发送；success 只表示 tmux accepted。`interrupt` 发送 `C-c`，`stop`
使用 tmux window identity 精确 `kill-window`，不按 PID/PGID 猜测。`attach` 只支持
human TTY；同一 managed server 内允许 switch，从其它 tmux server nested attach
则拒绝。当前模式下 `sn-session` 不存在时，`list` 成功返回空集合。

window 可保存唯一 opaque `binding={kind,id}`。Tmux 只校验形态和 registry 唯一性，
不读取绑定目标 Store；`session open` 使用该绑定关联 `native_tui` Session。
`accepted=true` 只确认 tmux transport mutation，不证明 Provider 已消费输入、产生输出
或完成任务；native TUI 的 terminal Run 仅证明 opaque Provider process lifecycle 已
收口，不是 canonical Turn completion。

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
source/payload 是 `resources/tools/<name>.json`。loader 接受 `schema_version=1`、
`executor.type=mcp`，`effect` 为三档之一（`read_only`/`write_local`/`write_external`），
文件 basename 必须等于 local tool name，并拒绝与 builtin 同名。`risk` 为
`low`/`high` 两档：写副作用（`write_local`/`write_external`）必须显式声明 risk，
`read_only` 缺省视为 `low`。`runtime.json` 只保存 enabled name；bootstrap 将
builtin 与选中的 manifest 组合为一个 Registry，任何 enabled name 无 owner 都
fail closed。默认启用 `web_search` 和 `web_fetch`，分别绑定 BigModel MCP 的
`web_search_prime` 与 `webReader`，认证引用均为
`Bearer ${Z_AI_API_KEY}`。

每个 MCP handler execution 建立一个有界 Streamable HTTP session，执行
`initialize`、`notifications/initialized` 和一次 `tools/call`；不 retry、不跟随
redirect。客户端在 `initialize` 中声明 `requested_protocol_version`，服务端在
`allowed_protocol_versions` 集合内协商回任一版本即接受，否则按不支持收口；两者
均可经 `runtime.json` 的 `mcp` 段配置，缺省为 requested `2025-06-18`、allowed
`{2025-06-18, 2024-11-05}`，对齐 2026-07-28 Streamable HTTP spec 的可配置协商。
manifest 的 canonical definition、endpoint、remote tool、header 环境
引用、timeout 和 response limit 冻结进 child tool snapshot，resolved secret 不
进入 snapshot、digest、event 或错误。网络、HTTP、JSON-RPC、协议与远端 tool
错误用有界、脱敏的 `ToolResult{IsError:true}` 闭合只读 effect，使模型可在下一轮
解释失败；Session 和 `call` 不执行该 handler。

builtin 工具新增 `shell`（`effect=write_external`、`risk=high`），以 argv 形式
（不经 shell 插值）在独立进程组内执行子进程，有界捕获 stdout/stderr，并在 ctx
deadline 或输出超限时向整个进程组 `SIGTERM`→`SIGKILL`。它是进程级、无容器隔离的
工具，靠风险分级与人工确认兜底，默认不在 `runtime.json` 的 `agent.tools` 中启用。
high-risk 写副作用在真正执行前必须经过 UserConfirmation：handler 首次返回
`Pause{Kind:"user_confirmation"}`，Kernel 据此**不闭合** durable effect（保持
`started`），Run 进入 `paused`；`job continue` 携带 `{approved:bool}` 后 Kernel 把
approval 附进 `ToolRequest` **重跑** handler 才触发真正副作用，再用真实结果
`Completed`。期间崩溃则 effect 仍是 `started`，按既有不变量进入
`needs_reconciliation`（副作用未发生，fail-closed）。这是 stock builtin 工具首次
产生 Pause：`shell` 默认不在 `agent.tools` 中启用，需显式配置后才会出现在
tool loop 中。`server info` 的 capability map 是静态声明，仍不列举 `resume`；
pause/resume 仍是 Kernel-level extension，由 Agent Run 的 paused 事实驱动。

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
`job reconcile` 仍按精确 `run_id` 收口。`paused` 不走 reconciliation，只能通过
`job continue` 恢复。pause/resume 是 Kernel extension：底层 CLI/API/Store 和
validator 保留，`shell` 等 high-risk 写工具经 `user_confirmation` pause 产生
暂停态。

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

`job reconcile` 本身是操作者对 unknown outcome 的显式确认。Session Run 从已经
人工 reconciliation 的 Session evidence 收口；Agent Run 由 Agent executor
收口。两种 kind 均不得 replay 原执行。已经 reconciliation 的 Agent terminal
result 带显式 acknowledgement marker，重复调用幂等返回该 record；普通 terminal
Agent Run 不会被误报为已 reconciliation。

实现 `CompletionValidator` 的 executor 在 Run 返回 `completed` 后、terminal
publish barrier 之前执行 completion validation gate：`completion_criteria`
声明 `command` 类型检查，以 Run CWD 为工作目录执行，非零退出视为未达标。
验证未通过（`Passed=false`）以 canonical `validation_failed`/`phase=run` 错误
settle 为 `failed` 并保留产物；验证器自身报错（无法判定）则进入
`needs_reconciliation`。未声明 criteria 或 executor 不实现该接口时，沿用
既有「信任 executor 终态」语义。`completion_criteria` 属公开 Run request 字段。

`run trace` 聚合单个 Run 的 record 与 event/model_call/tool_effect journal 为
只读 Trace，复用四张表已有的 `run_id` 关联，不引入独立 `trace_id` 列或并行 trace
存储；`model_calls.provider_request_id` 作为单次模型调用的 span 标识。Trace 属
query 路径，只加载 Run Store。

sn-server 后台 reaper 周期扫描 `paused` 与 `needs_reconciliation` Run，对
`updated_at` 早于 `run.reaper.paused_ttl` / `run.reaper.needs_reconciliation_ttl`
（默认 30m / 24h，0 禁用）的 Run 以 `timeout`/`phase=run` 错误 settle 为 `failed`，
避免单点卡死永久占用队列或 `runs_one_open_session` 唯一槽位。reaper 使用
`(state,cancel_requested,updated_at,run_id)` 索引和按 `updated_at,run_id` 升序的稳定
keyset cursor 分页，因此超过 1000 条时也不会被普通 list limit 或新记录饿死。每个
候选 settlement 在同一 SQLite transaction 内以原 `state+updated_at` 和
`cancel_requested=0` 为原子前置条件；并发 resume/state transition 返回 conflict，
并发 cancellation 返回 cancellation reservation，只有这两类 ownership race 被跳过，
其它 query/settlement 错误向上返回并终止 reaper。sn-cli 单命令模式不启动 reaper。

SQLite `PRAGMA user_version=6`。Store `Open` 与 activation preflight 复用同一份精确
schema manifest，校验 7 张表、4 个显式 index，并额外核对 reaper index 的
unique/partial 属性和列序；对象缺失、定义变化、未知对象、版本不相等或混合 schema
均 fail closed。

## 9. CLI 与 HTTP

固定 CLI namespace：

```text
doctor exec call profile session agent job server help version update
```

Runtime machine contract 为 `schema_version=1`、`contract_version=8`。
bare CLI direct、`exec` 和 `session attach` 不属于 machine wrapper。

根路由优先级为：只允许位于 argv 第一项的 global `--json`；help/version；固定
namespace；active CLI Profile；否则失败。namespace/Profile 后出现的 `--json` 是
该 action 的普通参数并按未知 option 拒绝。固定 namespace 与 Profile 管理 action
`list|show|check` 都是保留 Profile ID。

help 使用单一 topic grammar：

```text
sn-cli help [tui|exec|call|doctor|profile|session|agent|job|server|update]
sn-cli --json help [topic]
```

不为各 namespace 增加第二套 `<namespace> --help` alias。根 human help 必须列出公开
入口、`open` / `close-all` 生命周期边界与 active log 布局；topic human
help 和 machine help 由同一 `name/summary/usage/notes` 定义生成。help/version 不加载
Runtime Home，也不写 execution/audit log。

当前公开 CLI 契约为：

```text
sn-cli <cli-profile-id> [options] [input]
sn-cli <cli-profile-id> resume [native-session-id] [--model M] [--effort E]
sn-cli <cli-profile-id> --resume [native-session-id] [--model M] [--effort E]
sn-cli exec <cli-profile-id> [options] [input]
sn-cli call <api-profile-id> [options] [input]
sn-cli doctor
sn-cli help [topic]
sn-cli profile list|show|check

sn-cli session exec <cli-profile-id> [options] [input]
sn-cli session call <api-profile-id> [options] [input]
sn-cli session open <cli-profile-id> [--attach|--detach] [options] [input]
sn-cli session send|attach|interrupt|close --session-id <id>
sn-cli session close-all
sn-cli session list [--state <state>] [--interface managed|native_tui]
sn-cli session show|messages|events|logs|executions|execution
sn-cli session reconcile|configure|export|delete|gc

sn-cli agent <api-profile-id> [options] [input]
sn-cli job get|list|result|trace|events|watch|cancel|continue|retry|reconcile|gc
sn-cli server info|start|status|stop
sn-cli update [options]
sn-cli update upgrade-check [options]
```

`session open` 是唯一 public native TUI creation action；不支持省略 action，因为
`sn-cli <cli-profile-id>` 已是 direct Profile 入口。`sn-cli tmux` 不提供 alias 或
compatibility shim；`tmux` 不再是保留 namespace，可作为普通 Profile ID。

顶层 `sn-cli doctor` 加载当前 Runtime contract，静态校验所有 Profile，并按 CLI
Profile 的 `command/cwd/env/PATH` 解析 executable；`args` 中的环境引用属于调用时输入，
只校验引用语法，不要求 doctor 进程提供。doctor 还检查 API/Tool manifest 认证环境引用、
SQLite Run Store、audit log 写入，以及当前模式的 tmux binary/version/`sn-session`
identity；不调用 Provider/MCP 远端。machine result
成功时报告各缺失项、`tmux_window_count`、`tmux_error`、`audit_log_error` 和 log
root；失败时返回单一 canonical error envelope，并在 message 中列出失败项。

bare Profile/`exec` 只接受 CLI Profile，`call`/`agent` 只接受 API Profile；Profile ID
必须紧跟拥有它的 namespace/action，option 位于其后，input 至多一个并且最后。
`<cli-profile-id> resume` 与 `--resume` 等价，只续接 Codex/Claude/Grok 自己的 native
session，不创建 Runtime Session/Run；adapter 分别映射为 Codex `resume`、Claude `--resume`
与 Grok `--resume`。`profile check`
只做符号化静态校验，不解析真实 env/PATH/cwd、不读 prompt file、不调用 Provider。

执行入口与事实边界：

| 入口 | 执行语义 | canonical state |
| --- | --- | --- |
| bare CLI Profile | interactive process replacement | 无 Session/Run |
| `exec` | non-interactive process replacement | 无 Session/Run |
| `call` | 单次 API request | 无 Session/Run |
| `session exec|call` | 一个 recorded Turn；`--queue` 时入队 | Session；可选 Run |
| `session open` | tmux PTY 中的 Provider 原生 TUI | `native_tui` Session + opaque lifecycle Run/Execution；无 Turn/transcript |
| `agent` | durable model/tool loop；`--queue` 只入队 | Run；可选 Session |
| `job` | 查询/控制已有 Run | 不提交 fresh work |

管理 action 默认输出 compact human text。leading global `--json` 选择
`schema_version=1,contract_version=8` machine envelope；stream/watch 使用 NDJSON 或
SSE，并以 event sequence 续读。非流失败 stdout 为空，stderr 只有一个 compact
error document。bare direct/`exec` 继承目标进程 stdout/stderr/exit，`session attach`
只支持 human TTY。

Run Record 可以公开 Runtime-owned `request_digest/config_digest`，但
`private_request_json`、Agent execution snapshot 和最新 Resume input 均不属于
public machine contract，不得经 CLI/HTTP query、result、event、watch、log 或 error
输出。

Run application composition 按 action 分层：query/watch/trace 只加载 Run Store；
cancel/reconcile 只加载 Run Store 和 Session maintenance service；GC 仅在未显式
提供 cutoff 时读取 retention 配置。上述 maintenance 路径必须能只用 private
snapshot 和 durable evidence 工作，不加载 current Profile、Provider 或 tool。
带 `--queue` 的 `session exec|call`、`agent [--queue]`、resume/retry 和 worker execution 才
加载完整执行依赖。`job` namespace 不接受 fresh submission，只查询或控制已有
Durable Run；`retry` 仍是基于已有终态 Run 的控制动作。

HTTP route 固定为：

| Method/path | 语义 |
| --- | --- |
| `GET /healthz` | 进程存活探针；不读取 canonical Store |
| `GET /readyz` | worker/reaper/HTTP execution plane readiness |
| `POST /v1/model/generate` | 单次 canonical model request，可流式 |
| `GET|POST /v1/sessions` | list/create Session |
| `POST /v1/sessions/gc` | Session GC，默认 dry-run |
| `GET /v1/sessions/{id}` | Session record |
| `POST /v1/sessions/{id}:reconcile` | Session reconciliation |
| `GET /v1/sessions/{id}/messages|events|executions` | Session facts |
| `GET /v1/sessions/{id}/executions/{execution_id}` | execution fact |
| `GET /v1/sessions/{id}/watch` | Session SSE |
| `POST /v1/sessions/{id}/turns` | 同步执行一个 Session Turn |
| `POST /v1/agent/run` | 同步 durable Agent，可 SSE |
| `GET|POST /v1/runs` | list / queued submission |
| `POST /v1/runs/gc` | settled Run GC，默认 dry-run |
| `GET /v1/runs/{id}` | Run record |
| `POST /v1/runs/{id}:cancel|resume|reconcile` | Run control |
| `GET /v1/runs/{id}/events` | event list 或 SSE |

HTTP 不提供 Tmux、native CLI direct、Session export/delete/configure 或 Run retry
route，也不能上传 command、env、raw Provider payload 或 tool handler。
`POST /v1/runs` fresh submission 只接受 `kind=agent|session`；`GET /v1/runs` 的查询
filter 另接受 `kind=native_tui`。native TUI lifecycle Run 的 HTTP/CLI cancel、resume、
retry 均拒绝，关闭由 `session close|close-all` owner 执行。

HTTP 使用同一 application service。所有 JSON body 共同限制 request size、合法
UTF-8、单一完整 JSON value，并拒绝重复 key 和 trailing data；固定 Runtime DTO
要求 non-null object、拒绝未知字段和任意显式 `null`，无业务字段的 POST control
action 还必须精确为 `{}`。`/v1/model/generate` 使用 strict object 后执行 canonical
`GenerateRequest.Validate`；`/v1/runs/{run_id}:resume` 使用上述 strict resume
envelope，只有 envelope 的 `input` 字段 shape/null 由 pause schema 决定。NUL
只由 canonical text field validator 拒绝，不是 decoder 对所有 JSON string 的
全局禁令。Session/Run collection query 使用 allowlist、单值和显式空值校验；
malformed、未知、重复或显式空参数均拒绝，只有省略才使用领域默认值。Session 新增
execution query 与 reconcile route。

HTTP、CLI machine error 与 server Bearer 认证使用 canonical `RuntimeError`。
malformed resource ID 是 `invalid_request/400`；合法 ID 指向不存在的资源是
`not_found/404`；Store 故障是 `internal/500`。不得用空 list/event 假装目标资源
存在，也不得把 Store 故障降级成 not-found。

`sn-server` 读取 `HTTP_ADDR`（默认 `127.0.0.1:8080`）、`SN_SERVER_TOKEN` 与
`SN_CLI_HOME`。监听非 loopback 地址必须配置 Bearer token。`server start` 只启动
active `${SN_CLI_HOME}/bin/sn-server`；PID identity、process lease、日志与生命周期
由 Runtime 管理：PID/lease/lifecycle lock 位于 active `state/`，process log 位于
active `logs/sn-server.log`。启动前同样必须通过 activation gate；server 才启动
scheduler 与 Run reaper，单次 `sn-cli` 命令不启动后台 worker。listener 已绑定且
全部后台 goroutine 已建立后 `/readyz` 才返回 `200`；任一 worker、reaper 或 HTTP
serve 意外退出会先撤销 readiness，再取消同组任务、graceful shutdown 并令进程非零
退出。unready 期间仅新的 durable submission `POST /v1/runs` 返回可重试 `503`，
已有 Run 的 query/cancel/resume/reconcile 仍可用于诊断与收口；`/healthz` 只表示进程
仍存活。配置 `SN_SERVER_TOKEN` 时 probe 与所有 Runtime route 一样先做 Bearer 认证，
未认证请求不能观察 readiness；loopback 且未配置 token 时 probe 可直接访问。

## 10. 激活协议

release archive 带 activation epoch、当前 contract、Tool manifest、Session 和 Run
schema 版本；精确值由 `release/release.json` 与编译期 canonical constants 一致性门禁
共同约束。installer/updater 只能由 staged candidate 在 maintenance/lifecycle lock、
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
其它 managed resource 按 source→active 映射覆盖 active home。candidate 完整验证后，
该模式先按 `sn-cli session close-all` 语义关闭全部 Session-bound native TUI，再在
lifecycle lock 内安全停止 server。任何剩余 Tmux carrier、目标 binary process、identity
漂移或其它运行态 blocker 都继续 fail closed，不再为旧版 supervisor 提供安装期进程组
清理例外。该模式显式授权在不
解析现有 Session/Run state 的情况下重置本地运行态；只有 staged artifact 全部提交
并验证后，才在 guard 下幂等删除
`sessions/`、`state/session-locks/`、`state/session-invocations/`、
`state/session-mutations/`、`state/session-trash-moves/` 和
`state/runtime.db*`，然后解除 journal。安装终态固定为 server stopped，且这项
reset 授权不能由 archive installer 或 `sn-cli update` 获得。

所有公开配置、Session/Run fact、SDK request 和 machine output 都必须完整符合当前
schema。CLI Profile 只进入 bare direct、`exec`、`session exec` 或 `session open`；
API Profile 只进入 `call`、`session call` 或 `agent`。类型不匹配 fail closed，不提供
alias、自动迁移或 legacy ingress。
