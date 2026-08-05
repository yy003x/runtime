---
design_id: profile-session-tmux-protocol
design_type: architecture
scope: repo
project_id:
data_status: checked
design_role: contract
implementation_status: implemented
phase_mode: single
owner: wb-design
manager: wb-design
last_updated: 2026-07-31
---

# CLI Profile、Session Executor 与 Tmux 管理协议

## 结论

CLI Profile 只配置 command、typed selector、args、env、prompt 和 cwd，execution
mode 由公开入口固定。bare CLI Profile 是 interactive direct，`sn-cli exec` 是
non-interactive CLI 调用，`sn-cli req` 是一次 API request；`sn-cli profile` 只管理
配置。`sn-cli session` 是 Provider-neutral 的 canonical Session 服务，
`sn-cli tmux` 是长期交互进程管理器；三者使用独立解析和状态机，只共享纯 command
adapter。

## 目标与非目标

### 目标

- 把 CLI Profile 收敛为 typed 配置和 override 协议，由 adapter 生成顺序确定的
  argv、env 和 cwd。
- 支持 Codex、Claude 的 `model`、`effort`、mode 和 prompt 映射，解决 Profile
  固定参数与动态 override 重复、顺序错误的问题。
- 保留 `sn-cli session` 的 Session、Turn、Message、Execution、history 和
  context owner 身份，并允许每个 Turn 选择 API 或 CLI executor。
- 新增独立的 `sn-cli tmux`，在固定 tmux session `sn-session` 中管理多个
  interactive window。
- 让 namespace 固定执行语义：bare Profile/`exec` 只接受 CLI Profile，`req` 只接受
  API Profile；Profile `type` 做严格配对校验。

### 非目标

- 不增加字段 alias、自动 migration、第二套 artifact reader 或兼容 shim。
- 不让 Tmux transcript、pane 内容或 paste 输入自动进入 Session history。
- 不让 `sn-cli tmux` 进入 HTTP API、durable Run 或 Agent Kernel。
- 不修改 active `${SN_CLI_HOME:-~/.sn}`；实现与验证只修改 source，并使用临时
  `SN_CLI_HOME`。

## 当前实现事实

- CLI Profile 使用 `type=cli`、`command` 和 typed 字段；loader 与 schema 对未知
  字段 fail closed。
- bare CLI direct 与 `sn-cli exec` 使用独立 typed parser；`model`、`effort`、`cwd`
  按请求优先，prompt 按 Profile、`--prompt`、stdin、位置参数的顺序合并。
- `command/` 已实现 Codex、Claude adapter registry，负责 selector 去重、mode
  转换、canonical output、argv 顺序、PATH/cwd/env 解析和 invocation budget。
- Session 已使用 schema=2 的 Provider-neutral Session/Turn/Execution；`session
  exec` 由自有 managed subprocess 固定以 non-interactive mode 执行，`session req`
  执行一次 API request。invocation 由 Session 自行构建，
  不复用 Tmux 状态。
- `tmux/` 与 `sn-cli tmux` 已作为独立领域实现，固定使用专用 socket 和
  `sn-session`，以 live window registry 管理 interactive command。
- machine envelope 使用 `schema_version=1`、`contract_version=4`；Session fact
  与 Agent LoopState 使用 schema=2，SQLite 使用 schema=4，所有版本都必须显式匹配。
- `configs/*.json` 是唯一 Profile 配置层；各执行 namespace 使用同一个 loader 和
  typed adapter，Profile ID 直接对应同名配置文件。

## 设计

### 1. 公开入口与固定语义

| 入口 | Profile 类型 | effective mode | 承载与记录 |
| --- | --- | --- | --- |
| `sn-cli <cli-id> ...` | `cli` | interactive direct | typed 参数校验后 process replacement；本地 CLI 日志；无 Session/Run |
| `sn-cli exec <cli-id> ...` | `cli` | non-interactive exec | typed 参数校验后 process replacement；本地 CLI 日志；无 Session/Run |
| `sn-cli req <api-id> ...` | `api` | one request | 一次 HTTP model call；本地 API 日志；无 Session/Run |
| `sn-cli session exec <cli-id> ... [--queue]` | `cli` | managed non-interactive | Session managed subprocess；canonical Turn + CLI 日志；可选 durable queue |
| `sn-cli session req <api-id> ... [--queue]` | `api` | one request | Session API executor；canonical Turn + API 日志；可选 durable queue |
| `sn-cli tmux start <cli-id> ...` | `cli` | interactive | 固定 `sn-session` 新 window；本地 CLI 日志；无 Runtime Session |
| `sn-cli agent <api-id> ... [--queue]` | `api` | model/tool loop | Agent Kernel；durable Run；每轮本地 API 日志 |

执行入口固定 effective mode，Profile 不保存 mode 字段。这样同一 CLI Profile
可以分别用于一次 TTY 调用、一次 non-interactive 调用、canonical Session Turn 和
长期 Tmux TUI。namespace 决定执行领域，`type=cli|api` 做严格配对校验。
固定根 namespace `exec|req|profile|session|tmux|agent|run|server|help|version` 和 Profile
管理 action `list|show|check` 都是保留 Profile ID，loader 遇到同名配置即失败。
`profile` 不再执行 Profile；`run` 不接受 fresh submission，只查询或控制已有
Durable Run，`retry` 仍是基于已有终态 Run 的控制动作。
所有拥有 Profile 的入口都要求 Profile ID 紧跟 namespace/action，option 位于其后，
input 必须最后。

这里的本地日志是 `${SN_CLI_HOME}/logs/YYMMDD/{cli,api}.jsonl` 下的 best-effort
execution diagnostics，不是新增的 Session/Run 状态层。查询、校验和 queue submit
不写；worker 真正执行才写。API 日志保存 protocol-encoded HTTP
`request/response/error`，secret 只保留 `${VAR}` 引用或 `[REDACTED]`；任何日志
失败都不得改变执行结果，当前也没有自动 GC 或旧日志迁移。

Runtime machine mode 继续只认 argv 第一项的 leading global `--json`，即
`sn-cli --json session list`；namespace 后的同名参数绝不被 root 截获。管理命令的
machine success 与 error 都携带 `schema_version=1`、
`contract_version=4`。bare CLI direct 与 `exec` 即使带 leading global `--json` 也
仍 process replacement，不把目标 CLI 输出伪装成 Runtime JSON；Profile ID 后的
`--json` 由 Profile typed parser 拒绝，不透传给目标命令。

### 2. CLI Profile 协议

CLI Profile 使用以下字段：

```json
{
  "type": "cli",
  "command": "codex",
  "args": [],
  "env": {},
  "model": "gpt-5.6-sol",
  "effort": "high",
  "prompt": "",
  "cwd": ""
}
```

- `command` 替代 `binary`，是单个可执行文件名或路径，不经过 shell。
- `args` 一字符串一 argv token；不得包含 prompt。adapter 负责识别 command
  options、mode selector 和 subcommand options，并重建正确顺序。
- `model`、`effort`、`prompt`、`cwd` 均可省略；execution mode 由入口固定。
- Profile `prompt` 与 typed `--prompt` 使用同一 file-or-text 规则：相对路径基于
  入口的 invocation base；CLI 入口的 base 是调用方 cwd，HTTP 的 CLI executor
  必须从请求得到 absolute `cwd` 或使用 Profile 中的 absolute `cwd`。现有 regular
  file 读取内容，不存在则按普通字符串处理，存在但不是 regular file 时 fail
  closed；HTTP 不得用 server 启动 cwd 猜测相对文件。
- `cwd` 展开环境引用后必须是可进入的目录；只有 CLI ingress 接受相对路径并基于
  当次捕获的 caller cwd 解析。
- `env` 继续支持 `${NAME}` 展开和 `null` 删除，secret 只从环境变量获取。
- CLI ingress 才允许把相对 `cwd` 基于当次捕获的 caller cwd 解析；HTTP 的 cwd
  override 必须为 absolute，若只配置了相对 Profile `cwd` 也拒绝。任何 HTTP
  执行都不得使用 server 启动 cwd。
- `args/env/cwd` 中的 `${NAME}` 只从进程启动时不可变的 inherited-environment
  snapshot 展开一次；Profile `env` 条目彼此不能引用，不递归展开，JSON map 顺序
  不影响结果。缺失引用只在真实 invocation 构建时失败。
- CLI Profile 只接受本节定义的字段；严格 loader 不接受未知字段。
- API Profile schema 和 Provider driver 保持独立，不增加 CLI-only 字段。

bare CLI direct 与 `exec` 的 typed 参数为：

```text
--model <model>
--effort <low|medium|high|xhigh|max>
--prompt <file-or-text>
--cwd <dir>
[input]
```

`model`、`effort`、`cwd` 是 scalar override，typed 参数优先于 Profile 配置。

`prompt` 不是 scalar override，而是追加输入源。Profile prompt、`--prompt`、piped
stdin 和最后一个位置参数按此顺序合并，非空片段用换行连接，最终形成一个 prompt。
不提供 raw argv passthrough。

- 每个 typed option 最多出现一次；未知 option、超过一个 positional、positional
  后继续出现 option 都 fail closed。`--` 后只能有零或一个 input。
- file、stdin 和 string fragment 都必须是 UTF-8、无 NUL；读取上限与最终 prompt
  token 上限均为 128,000 bytes。该值低于 Linux 常见的 128 KiB 单 argv string
  边界（含结尾 NUL）。file 使用 no-follow open 后 `fstat`，拒绝 symlink、
  非 regular file 和 stat/open race。
- adapter 在 prompt 前加入 `--` option terminator；prompt 仍是最后一个 argv
  token，支持以 `-` 开头的内容，也不会被 variadic option 消费。
- Invocation 构建时预检每个展开后的 argv/env token 不超过 128,000 bytes，总
  `argv+env` 不超过 `min(512 KiB, ARG_MAX-32 KiB)`；无法探测 `ARG_MAX` 时使用
  128 KiB 保守总预算。总量计算包含每个 token 的 NUL、argv/env pointer table 和
  executable path；预算为负或不足时直接失败。超限必须在 spawn 前返回 typed
  error。
- bare Profile interactive 只要继承的 stdin 不是真 TTY，就必须把 `/dev/tty`
  重新绑定为 stdin；没有 controlling TTY 时 fail closed。`exec` 与 Session
  canonical child 固定从 `/dev/null` 读取 stdin，防止目标 CLI 再次消费或拼接
  调用方 stdin。

- `exec` 的 prompt 必须非空；Session 输入也始终非空。
- bare interactive direct 的 prompt 可以为空；非空 prompt 仍作为最终 argv token
  交给当前 TTY 中的 TUI。
- bare direct 与 `exec` 都不提供 Runtime `--json` 包装；两种 mode 均在校验后
  process replacement，继承目标进程的 stdout、stderr、signal 和 exit code。
- 两条 CLI 入口都读取 Profile `prompt`，并按相同优先级应用 typed 参数。

保留 `profile list|show|check`。`show` 只展示未展开配置，禁止显示 resolved env；
`check` 是纯静态、符号化校验：只验证 schema、引用语法、adapter、option grammar
和 typed 字段，并用 placeholder 构建 interactive/native、exec/native、
exec/canonical 三种 plan；不得解析真实 env、`PATH`/command、cwd 存在性或读取
prompt file，也不调用 Provider。真实 env/filesystem/argv budget 只在 invocation
时验证，因此 source 中未注入的 secret/runtime 引用不会阻断 install/update/release。

### 3. Command adapter

`command/` 保存纯 adapter registry，不拥有 Profile、Session 或 Tmux 状态机：

```text
Resolve(command basename) -> Adapter
Adapter.Build(BuildRequest{
  mode, outputProtocol, effectiveConfig, argvPrompt
}) -> Invocation
Adapter.Decode(result) -> canonical assistant output
```

`mode` 取 `interactive|exec`；`outputProtocol` 取 `native|canonical`。
interactive 只允许 `native`。bare direct 与 `exec` 使用 `native`，Session managed
exec 固定使用 `canonical`，只有 canonical invocation 的结果才可交给
`Adapter.Decode`。Profile、Session 和带初始输入的 Tmux start 都只提供可空的
`argvPrompt`。

首期 adapter：

- `codex`
  - interactive：`codex [global options] [prompt]`
  - exec：`codex [global options] exec [exec options] [prompt]`
  - effort：`-c model_reasoning_effort=<value>`
- `claude`
  - interactive：`claude [options] [prompt]`
  - exec：`claude [options] -p [prompt]`
  - effort：`--effort <value>`

adapter 以 `filepath.Base(command)` 选择；未登记 command 在执行前返回 typed
Profile error。adapter 必须：

- 用显式 option table 登记 name、alias、arity、scope、variadic 和
  `--name=value` 形式，把 `args` 分为 command options 和 exec-only options；
- 识别配置中已有的 model、effort 和 exec selector；
- 用 typed effective value 替换已识别值，禁止重复输出；
- 在 mode 切换时加入或移除 selector 及不合法的 mode-only options；
- `outputProtocol=native` 不注入机器输出 selector，保留合法 native output
  options；`outputProtocol=canonical` 必须识别、替换并去重机器输出 selector：
  Codex 恰好一个 `--json`，Claude 恰好一个 `--output-format json`；
- 与 canonical protocol 不兼容的伴随参数（例如只适用于 Claude
  `stream-json` 的参数，或把单个 JSON result 改为逐轮数组的 `--verbose`）返回
  typed argument-conflict error，不静默保留或猜测；
- 当入口提供 `argvPrompt` 时，保证它是最终一个 argv token；未提供时不得自行加入
  Profile prompt；
- 对无法安全归类的冲突参数 fail closed，不经 shell 猜测。

首期 option registry 至少明确登记并测试以下 scope；表外 option 出现在 Profile
`args` 时 fail closed：

| command | command/common scope | exec-only / mode selector | canonical 禁止 |
| --- | --- | --- | --- |
| Codex | `-c|--config`、`--enable`、`--disable`、`--strict-config`、`-i|--image`、`-m|--model`、`--oss`、`--local-provider`、`-p|--profile`、`-s|--sandbox`、`-C|--cd`、`--add-dir`、`-a|--ask-for-approval`、`--search` 与 dangerous flags | `exec|e`、`--skip-git-repo-check`、`--ephemeral`、`--ignore-user-config`、`--ignore-rules`、`--color`、`--json`、`-o|--output-last-message`、`--output-schema` | `resume`、`fork`、`--output-schema`、`--output-last-message` 和 stdin/input 替代 |
| Claude | permission/tool/system/model/effort/MCP/settings/plugin 等已登记 option；所有 arity 与 variadic alias 明确 | `-p|--print`、`--output-format`、`--input-format`、`--no-session-persistence` | `--verbose`、`-c|--continue`、`-r|--resume`、`--session-id`、`--fork-session`、`--bg|--background`、`--worktree`、`--tmux`、stream/json-schema/debug-file/replay/result-shape 改写 |

Codex `-c|--config` 只把 key 恰为 `model` 或 `model_reasoning_effort` 的值视为
typed selector，其余 config token 保序保留；同 key 重复按 scalar conflict 处理。
variadic option 到下一个已登记 option、mode selector 或 `--` 为止，零值时失败。
不得把未知的 bare token 猜成 option value；除 adapter 自己管理的 mode selector 和
最终 prompt 外，Profile `args` 不允许其它 positional/subcommand。

selector 优先级固定为：

```text
请求 typed override
> Profile typed 字段
> Profile args 中唯一、可识别的 selector
> Provider 默认值
```

无更高优先级时，重复 scalar selector 直接失败。Profile `args` 中的 Codex
`-C|--cd` 和等价 Claude cwd selector 一律拒绝，工作目录只由 typed `cwd` 和
OS process cwd 决定。可执行文件查找使用 effective `PATH` 与 effective cwd，
不能在 env/cwd 生效前使用调用进程的 `exec.LookPath`。

canonical mode 额外固定为 stateless：

- Codex 注入并去重 `--ephemeral --json`；
- Claude 注入并去重 `--no-session-persistence --output-format json`；
- 拒绝 resume、continue、session-id、fork、background、alternate input、
  stream output、output-schema/result-file 等会引入隐藏上下文或改变 final shape
  的参数。

Profile、Session、Tmux 各自解析请求；它们只调用 adapter 的纯构建/解码接口，
不相互调用 CLI parser 或生命周期服务。Profile 与 Session 向 adapter 提供
`argvPrompt`；Tmux start 也把可选初始输入作为 `argvPrompt`，只有后续
`tmux send` 使用 paste。调用方式由入口固定；Profile `args` 中的 native output
flags 不能改变 Session 的 canonical output protocol。

### 4. Session

Session 继续拥有：

```text
Session
  └─ Turn
       ├─ canonical user/assistant/tool messages
       ├─ ProfileRef 与 effective config digest
       ├─ Execution attempt
       └─ ContextManifest 与 lifecycle events
```

- 每个 Turn 独立选择 Profile；provider 只能在 Turn 边界切换。
- active Turn 或 `requires_action` 未闭合时不得切换 executor。
- API executor 继续投影 canonical `messages[]`，tool call 继续进入
  `requires_action`，Session 不执行 tool。
- CLI executor 完全不消费承载配置，固定调用 command adapter 的 exec mode，
  在 Session 自有 managed subprocess 中捕获 stdout、stderr、exit code。
- “Session 不执行 tool”只表示 Runtime 不调度 canonical tool call。Codex/Claude
  子进程内部自行执行的 tool 与副作用属于 opaque CLI executor 行为，不投影成
  Runtime `requires_action`，也不形成第二套 Runtime tool lifecycle。
- CLI executor 强制使用 command 的稳定机器输出：
  - Codex 使用 `exec --ephemeral --json`，decoder 逐行验证 JSONL；顶层
    `error|turn.failed` 失败，`item.completed` 的 error 只作为诊断，未知 additive
    event 可忽略；只有唯一 `turn.completed` 前最后一个 completed
    `agent_message` 可成为 canonical assistant message；
  - Claude 使用 `-p --output-format json`，decoder 只接受成功且 `is_error=false`
    的唯一 JSON document、EOF、`type=result`、`subtype=success` 和 string
    `result`；
  - stdout 只承载机器协议，stderr 只承载诊断；进度、tool event 和诊断都不得写成
    assistant message。
- OS exit=0 与协议 success terminal 是双门禁。child 自发非零 exit/signal、缺少或
  重复 terminal、terminal 前无 assistant result 或 decode 失败都使 Turn
  `failed`；由父进程主动终止时按下文记录的触发原因映射，不能仅看最终 OS signal。
  即使 stdout 中存在 partial assistant text 也不得追加 assistant message。
- managed runner 并发读取两个 pipe，stdout 机器协议上限为 16 MiB、stderr 上限为
  256 KiB、单个 JSONL record 与最终 canonical assistant text 上限均为 1 MiB；
  任一超限终止进程并返回
  typed output-limit error。Session 不完整保留原始 provider stdout/stderr，只在
  Execution 保存 observed byte count、observed-prefix digest、typed summary，
  以及分别的 `truncated` 和 `limit_exceeded` 事实；不持久化 stderr excerpt。
  未超限时 observed
  digest 才表示完整 stream；超限时必须明确只表示终止读取前已观察的前缀。
  typed summary 只能取 Runtime 自有的有限 code/state/count，不保存 Provider 原始
  文本、argv、resolved env 或可能含 secret 的 path/value。
  canonical 输入输出分别由 user/assistant Message 保存。
- CLI history 继续生成有界、转义后的 history/current-input prompt，并在前面合并
  Profile base prompt；base prompt 是 invocation 前缀，不另存为本轮 user
  Message。最终合并值同时受 128,000-byte argv-token 门禁。
- `session exec|req <profile>` 默认等待 executor terminal result；Profile ID 后的
  `--queue` 由 durable Run worker 执行同一 Session service，不能在仅启动进程后把
  Turn 标为 settled。

managed subprocess 使用独立 process group。direct CLI 用 signal-aware context；
SIGINT、SIGTERM、Run cancel、timeout 或 output-limit 都按
TERM→有界等待→KILL 终止整个 group，并完整 wait/reap。Execution 区分
spawn_intent、running、settled，保存 owner/child process identity、可空
`exit_code`、signal 和 typed outcome；未启动与 exit=0 不得混淆。

为消除 spawn→marker 的重放窗口，Session 不直接 spawn Provider binary，而是
spawn 当前 `sn-cli` 的私有 exec helper。父进程先持久化 `spawn_intent`；helper
创建独立 process group 后阻塞在 inherited handshake pipe。父进程取得并持久化
helper PID/PGID/process-start-token 和 `running` marker 后才发送单字节 go；
pipe 在 go 前 EOF 时 helper 不执行 Provider 并退出。go 后 helper 打开
mode=0600/no-follow/size-bound 的 invocation manifest、立即 unlink，再 `exec`
Provider；exec 保持同一 PID/start token。这样任何可能已执行 Provider 的 attempt
都有 marker-before-exec 的可验证 identity，manifest 不进入 export 且由 bounded
cleanup 清理残留。

CLI process 已 spawn 后若 Runtime 崩溃，outcome 与副作用一律视为未知，不自动重放：

- SQLite startup reconcile 把所有 `state=running AND kind=session` 的 Run
  无条件改为 `needs_reconciliation`，绝不重新入队；Agent Run 继续使用自身
  tool-effect 规则。父进程若已拿到 Session terminal、但尚未提交 Run terminal，
  后续 `run reconcile` 可按相关 digest 安全补齐同一结果；
- direct 或 durable Session 的 stale active Turn 保持 Session `blocked` 和
  unknown-outcome，不直接转 idle。若原 owner 仍存活则只返回 active conflict；
  任何后续执行都不能复用该 Turn；
- 实现不得根据 partial stdout 猜测成功，也不得把同一 `run_id` 再次交给 CLI。

公开恢复出口为
`session reconcile --session-id <id> [--terminate] [--acknowledge-unknown]`
及等价 HTTP POST。默认只探测 owner/helper identity：已能证明 helper 不存在时把
Turn 标为 failed/unknown-outcome；`--terminate` 仅在 PID、PGID、start token 和
group leader 全部匹配时执行 TERM→wait→KILL，确认 group 消失后再 settle。identity
歧义、leader 已消失但 group 仍可能存活、permission error 或 PID reuse 都保持
blocked，不发送信号。操作者在外部确认无存活副作用进程后，可显式使用
`--acknowledge-unknown` 解除歧义并以 failed 结案；该 flag 不能与 `--terminate`
同时使用。durable Turn 结案后再由现有 `run reconcile` 读取 Session terminal 并把
对应 `needs_reconciliation` Run 结为 failed；两步均幂等、digest 不匹配则
conflict。HTTP 同时提供对应 Run reconcile action。

每个 Turn 持久化完整 request digest、未展开且不含 resolved secret 的 Profile
config digest、已解析 base-prompt digest 和 absolute cwd。带 `--queue` 的
`session exec|req <profile>` 在
ingress 解析并冻结 caller-relative prompt file 内容、typed model/effort/cwd 和
non-secret Profile snapshot/config digest；snapshot 保存 `command/args/env` 引用和
typed 字段，不保存 resolved env secret。durable Run 的公开 `Request` 只携带
digest/ref；`CLIExecutionSnapshot` 与已解析 base-prompt 内容进入 SQLite schema=2
独立的 store-only `private_request_json`，对应 Go 字段强制 `json:"-"`。内容受
128,000-byte 限制，DB/file mode=0600；CLI/HTTP 的 Run get/list/result/events、Session
export、human render、日志和 error 都不得序列化它。worker、idempotency 比较和
retry/clone 通过 Store 内部接口读取 private payload，公开 DTO 永远 redacted。
worker 不用自己的 cwd 重新解释，也不重新读取 prompt file；执行前
若当前 Profile ID 已删除或其 non-secret digest 漂移则在 spawn 前 conflict，绝不
换用新配置。env 引用在 worker 执行时从当时环境解析，这是不持久化 secret 的显式
取舍，并在 Execution 中只记录引用名和 snapshot digest。相同 `run_id` 只有全部
digest 一致才幂等返回；任一漂移都 conflict 或进入 reconciliation。

同一 `session_id` 同时只允许一个非 terminal durable Session Run；SQLite 使用
原子约束拒绝第二个 queued/running/paused/needs_reconciliation Run，不依赖多 worker
竞争后的偶然失败。Session 返回 `requires_action` 时对应 durable Run 正常
`completed` 并写入该 Session result；后续 Turn 仍由 Session blocked 门禁控制。

Session 使用独立 parser。CLI executor 的 Turn override 只支持 `--model`、
`--effort` 和 `--cwd`；不暴露 `--prompt` 或其它 CLI Profile 参数。
API executor 继续使用 Provider-neutral model request options。两种 executor 的
本轮用户输入都只来自位置参数或 piped stdin。

`model/effort/cwd` 对 API Profile 明确拒绝；API model options 对 CLI Profile
明确拒绝。相同 DTO 贯通 CLI、HTTP Session turn、HTTP durable Run、SQLite
`request_json` 和 Session service。

CLI 发起的同步/queued Turn 把相对 `cwd` 在 ingress 解析为 absolute path。HTTP
Session turn/durable Run 使用 CLI Profile 时，`cwd` 必须已经是 absolute，或该
Profile 自身提供 absolute `cwd`；否则返回 `invalid_request`。HTTP 永不把 server
进程 cwd 当成调用方 cwd。

Turn 当前输入按 piped stdin、最后一个位置参数的顺序合并，非空片段用换行连接，
合并结果必须非空。
`tool-result --content-file` 是独立的 tool result 输入，继续保留。

Session `Execution` 不再保存 `Transport`、`LaunchHandle` 或 detached
`TurnSubmitted` 语义，改为保存 executor kind、effective request/config digest、
exit/result/error、stdout/stderr observed byte count 与 observed-prefix digest、
各 stream 的 `truncated`、`limit_exceeded`、typed summary 和 process lifecycle。
新增 `session executions|execution` 以及对应只读 HTTP route；`session export`
包含安全 Execution fields。Session Store、retention、export、delete 和 GC 保持由
`session/` 独占。

Session fact 必须显式使用 `schema_version=2`，SQLite 必须显式使用
`PRAGMA user_version=4`。缺失、不相等或混合 schema 都 fail closed，不实现字段补齐、
版本推断或自动 migration。unsupported state 只能在停服后整体移到可恢复备份，
再初始化当前 schema；空目录可以直接初始化。安装/update preflight 必须在替换
binary 前同时验证 configs、每个 Session fact 和 SQLite schema，不能形成
“binary 已换但状态不可读”的半激活。

仓库根目录 `make install` 是本地源码调试的显式 destructive 例外：它固定覆盖
source `configs/`、`resources/tools/` 与 `release/runtime.json` 到对应 active
配置，校验 candidate 后自动停止受管 server，并显式授权不解析现有
Session/Run state。运行态只在 staged artifact 全部提交并验证、activation guard
仍生效时删除；成功后不重启。普通 archive/network install 与 `server update`
继续遵守 exact-schema preflight。

typed error 复用 canonical contract：option/selector conflict 使用
`invalid_request/profile`，argv/history/output 上限使用
`context_overflow/request|transport`，decoder 失败使用
`invalid_provider_response/transport`。terminal state 按触发原因固定：用户
SIGINT/SIGTERM、parent context cancel 或 Run cancel → `cancelled` +
`cancelled/run|transport`；deadline → `failed` + `timeout/transport`；
output-limit → `failed` + `context_overflow/transport`；child 自发 signal/nonzero
exit → `failed` + transport/provider typed error。父进程随后发送的 TERM/KILL 不得
覆盖原始触发原因。未知 crash outcome 使用 `conflict/run`。不另建第二套 Session
error enum。

### 5. Tmux

新增 `tmux/` 领域与 `sn-cli tmux` namespace：

```text
sn-cli tmux start <profile-id> [typed-options] [input]
sn-cli tmux list
sn-cli tmux show --tmux-id <id>
sn-cli tmux send --tmux-id <id> <input>
sn-cli tmux attach --tmux-id <id>
sn-cli tmux interrupt --tmux-id <id>
sn-cli tmux stop --tmux-id <id>
```

- 创建窗口的公开 action 固定为 `start`。
- `start` 只接受 CLI Profile，固定使用 adapter interactive mode。
- `start` typed 参数为 `--model`、`--effort`、`--prompt`、`--cwd`；
  `model`、`effort`、`cwd` 是 scalar override，`--prompt` 是追加输入源
  而不是 override。
- `start` 可无输入；有输入时按 Profile prompt、typed `--prompt`、piped stdin、
  位置参数的顺序合并，并作为 adapter interactive invocation 的最终 argv token。
  首次输入不使用 paste，不依赖不可证明的 TUI readiness。
- `send` 按 piped stdin、位置参数的顺序合并非空片段，合并结果必须非空；使用
  `load-buffer -b <unique> -` 从 stdin 写入、`paste-buffer -dpr` 和单独 Enter，
  不经过 shell。输入必须 UTF-8、无 NUL且不超过 1 MiB；所有失败路径删除临时
  buffer。成功只表示 tmux accepted，不表示 TUI 已消费。
- 除 `attach` 外都支持全局前置 `--json`；`attach` 是 human-only。
- `interrupt` 只向目标 pane 发送 `C-c`。
- `stop` 只 kill 经校验的目标 window，绝不影响其它 managed window；优雅中断由
  `interrupt` 单独承担。删除最后一个 managed window 后，允许在同一 lock 内再删除
  仅供 bootstrap 的 sentinel 并关闭空专用 server。

Tmux 显式使用 `-S` socket，socket 内 session 名固定为 `sn-session`。为避免 Darwin
`sockaddr_un.sun_path=104` 等平台路径上限，socket 固定为
`/tmp/sn-cli-tmux-<uid>/<home-digest-prefix16>.sock`：父目录 mode=0700 且由当前
uid 拥有，启动前计算完整 canonical-home digest，并用 server marker 校验完整值。
prefix 碰撞、目录/socket owner/mode 异常、symlink 或 marker 不匹配都 fail closed，
不能复用或覆盖。active home 只保存 lock/manifest，不保存 socket。
所有 tmux client 显式使用 active `${SN_CLI_HOME}/resources/tmux.conf`；该文件只由
source/payload `release/tmux.conf` 经 activation 映射。client 禁用用户
`~/.tmux.conf`，清空 `update-environment`，固定 `automatic-rename off`、
`allow-rename off`，并保证目标 command 启动前 `remain-on-exit on` 已生效。首次启动
不用会立即退出的 `start-server`：先生成 `server_incarnation`，用 fixed config
创建 remain-on-exit 的 inert sentinel window；sentinel name 原子编码完整
canonical-home digest 和 incarnation
`__sn_sentinel_<full-home-digest>_<incarnation>`。同一 initial tmux command queue
再写入一个 canonical、
base64url 的完整 server marker
`{full_home_digest,schema,owner_uid,sentinel_id,server_incarnation,tmux_conf_digest}`，
再创建目标 window。sentinel 不是 Tmux registry record，不分配 `tmux_id`，
list/show 不暴露；它在 server 生命周期内保留，最后一个 managed window stop 时在
同一 lock 内 kill 整个只剩 sentinel 的专用 session/server。server 已存在时必须先
验证 marker 与唯一 sentinel。若首次 client/server 在 marker 前崩溃，只允许在
socket owner 正确、恰好一个 name 中完整 home digest 与当前 home 相等的 inert
sentinel、无其它 window/pane/user record 时执行 bounded `kill-server` 并重建；
prefix16 碰撞但 sentinel full digest 不同或任一条件不满足时绝不清理，仍 fail
closed。已有
完整 marker 但 bootstrap digest/版本不支持或 home 不匹配时不自动清理。

所有 Tmux action 使用 active-home scoped `flock`：`list/show` 持 shared lock，
`start/send/interrupt/stop` 和 registry mutation 持 exclusive lock；lock 覆盖一次
完整 tmux client command queue，不能在 validate 与 action 之间释放。send 的
load-buffer、identity conditional、paste、Enter 和 cleanup 必须在同一个 tmux
client 连接/command queue 中完成；interrupt/stop 同理。每个 record 都绑定
`server_incarnation`；server 重启即使复用 `@0/%0` 也不能通过 conditional。
`attach` 在单个 tmux client command queue 中先校验完整 incarnation/record 再
attach/switch，连接建立后属于该 server，不跨重建复用目标。lock 只是临时协调原语，
不是 Tmux Store。`tmux` 是可选外部依赖；缺失或版本/能力不足返回 typed capability
error。除 `start` 外的命令只加载 layout、tmux capability 与 live registry，即使
Profile 或 `runtime.json` 已损坏也能管理既有 window。

专用 tmux server 只继承最小 sanitized env，不缓存 Profile secret。每次 start 把
resolved invocation 写入 mode=0600、no-follow、大小受限的 launch manifest；pane
只启动当前 `sn-cli` 的内部 helper。由于已存在的 tmux server 不能继承调用方 pipe
FD，helper 使用 active-home 中 mode=0600 的 ready/go gate 握手：先校验
active-home/owner/schema/manifest，写入含自身 PID/start token 的 ready fact，然后在
有界时间内等待 go；marker commit 前未收到 go 就退出，绝不执行 Provider。start
校验 pane PID/helper identity、resolved executable regular/executable identity、
manifest digest 和 ready fact，提交 registered marker 后才原子写 go。helper 收到
go 后打开并立即 unlink manifest/gate，再设置 exact env/cwd 并 `exec` target
command，同时保留 tmux 注入的 `TERM`、`TMUX`、`TMUX_PANE`；exec 保持 helper
PID/start token。manifest/gate 路径和 tmux user options 不包含 secret；start 失败
与后续 bounded orphan cleanup 都清理未消费文件。

每次 `start` 先生成带 UTC millisecond 时间的 UUIDv7-style opaque `tmux_id`
（时间段 + CSPRNG，Tmux 领域自行校验），`new-window -n` 的固定 window name 保存
完整可恢复的 provisional ID，且 bootstrap 已禁用 automatic/allow rename；
权威记录是一个 canonical JSON→base64url 编码的 window user option，包含
`tmux_id`、ProfileRef、cwd、command/config digest、创建时间、window/pane ID、
helper pane PID/PGID、process start token、resolved executable identity、
server incarnation 和 registry version。动态值不直接依赖分隔符或“不含换行”
假设。

`start` 的唯一 commit point 是 registered marker，严格顺序为：写完全部非 marker
options，验证 blocked helper/manifest/executable、单 pane、单 session link和独立
前台 process group，最后写 marker，再发 go 并等待 gate consumed 或 pane terminal
后返回。此前任一步失败都用精确 window ID 删除目标并清理 manifest/gate/buffer。
marker 后不再返回会诱发调用方重试的普通 create error：go 写入成功但 ack 未知时
返回 machine success、`launch_accepted=true,state=starting`；go 明确未写入或 helper
在执行前失败时返回 machine success、
`launch_accepted=false,state=exited,launch_error=<typed safe error>`，保留 window
供 show/stop。marker 后崩溃仍留下可见的 `starting|exited` registered record，不会
重放。缺 marker
的 crash orphan 通过固定 provisional name/option 在 `list` 中显示为 `orphaned`，
可用同一 `tmux_id` 执行 `stop`；orphan stop 只要求专用 server owner、唯一匹配的
window ID/name、单 pane/link，随后使用精确 `kill-window`，不要求并不存在的完整
target identity。marker 已存在但 record 缺字段、版本错误或 ID 重复时返回 typed
corruption，不静默忽略。

`send`、`interrupt`、`stop` 每次都复核 server owner、record、window/pane ID、
单 pane/link 和 live identity。`stop` 校验后使用 tmux server 内生命周期唯一的
window ID 执行 `kill-window`，不使用存在 check→signal TOCTOU 的 raw
`kill(-PGID)`；dead window 同样通过 `kill-window` 清除。主动脱离 PTY/session 的
daemon 后代不属于 Tmux 管理契约。

Tmux registry 只管理当前专用 tmux server 中的 live/dead window：

- `remain-on-exit` 保留自然退出 window 及 exit status，直到显式 `stop`；
- `list/show` 直接查询并校验完整 tmux user options，不建立第二份文件事实源；
- `list` 在 server 不存在时成功返回空数组，并按创建时间、`tmux_id` 稳定排序；
  `show/send/interrupt/stop` 的 not-found 与重复 stop 返回 typed machine error；
  `send/interrupt` 对 exited window 返回 conflict，`stop` 明确接受
  `running|starting|exited|orphaned` 并删除 window；
- 最后一个 window 被删除后，`sn-session` 可以自然消失，下次 `start` 重建；
- 不持久化明文 paste、完整 transcript 或 canonical message；
- 不创建 `session_id`、`turn_id`、durable Run、HTTP route、delete 或 GC。

Tmux machine success envelope 固定如下；字段只做 additive 扩展，不复用 Session
结构：

```text
start/show:
  {schema_version, contract_version, tmux_window, launch_accepted?}
list:
  {schema_version, contract_version, tmux_windows: []}
send/interrupt/stop:
  {schema_version, contract_version, tmux_id, action, accepted}

tmux_window:
  {schema_version, tmux_id, state, created_at, window_id, pane_id,
   profile_id?, cwd?, config_digest?, exit_code?, signal?, launch_error?}
```

`tmux_window.schema_version=1`；`state` 只取
`starting|running|exited|orphaned`，`exit_code` 用 pointer/nullable 表达，不能把
“未退出”与 exit=0 混淆。`send|interrupt` 的 `accepted=true` 只表示 tmux 接受；
`stop` 第一次成功后记录已不存在，因此重复 stop 返回 typed not-found，而不是伪造
幂等成功。orphan 的 `created_at` 从 provisional `tmux_id` 的 UUIDv7 时间段恢复
（tmux 3.6b 不提供可靠的 `#{window_created}`），其
`profile_id/cwd/config_digest` 不可证明时必须省略；排序仍按
`created_at,tmux_id`。human list/show 使用同一事实生成，不另行探测猜测。

Tmux error 只复用 canonical code/phase：tmux binary 缺失为
`provider_unavailable/transport`，能力或 bootstrap/registry schema 不支持为
`protocol_error/transport`，owner/incarnation/identity mismatch 为
`conflict/transport`，marker-present corruption 为 `protocol_error/transport`，
无效/不存在的 `tmux_id` 为 `invalid_request/request`，对非 running window
`send|interrupt` 为 `conflict/transport`。不新增 Tmux 私有 error envelope。

`attach` 要求 stdin/stdout 都是真 TTY。已经位于同一专用 tmux server 时使用
`switch-client`；位于其它 tmux server 时拒绝 nested attach；外部 attach 到目标
window 会改变固定 `sn-session` 的 current window，这是明确接受的共享 tmux 语义。

因此 Session 与 Tmux 不共享领域记录。若实现中确实出现重复的 regular-file、
atomic write 或 lock 需求，只能提取窄的 `internal` I/O 原语；不得让 Tmux 调用
`session.Store`，也不预先新增通用 Record 状态机。

### 6. Composition、配置与运行态

- `internal/cli` 只负责各 namespace 的 decode/call/encode。
- `internal/runtimebootstrap` 分别组装 Profile、Session 和 Tmux service。
- `fixedNamespaces` 包含 `exec|req|profile|session|tmux|agent|run|server|help|version`；
  与 `list|show|check` 一起成为保留 Profile ID，不提供 alias 或 shim。
- `runtime.json` 只保存 Agent、scheduler 和 Run 的当前配置。
- source/payload `release/tmux.conf` 与 `release/release.json` 分别保存无 secret
  的固定 bootstrap config 和当前 activation/contract/schema identity，并映射到
  active `resources/{tmux.conf,release.json}`；active home 只新增
  `state/tmux.lock`、短生命周期的 0700/0600 manifest/gate 和升级 journal/guard，
  socket 位于受控 `/tmp` 根；不新增 Tmux history。
- source/payload 配置布局固定为 `configs/*.json`、`resources/schema/*.json`、
  `resources/tools/*.json` 和 `release/{runtime.json,tmux.conf,release.json}`；
  activation 分别映射到 active `configs/`、`resources/schema/`、`tools/`、根
  `runtime.json` 与 `resources/{tmux.conf,release.json}`，不读取旧 payload shape。
- source `configs/*.json` 只使用 `command` 和 typed 字段，不保存 execution mode；
  任意合法 CLI Profile 都可由 bare direct 或 `tmux start` 以 interactive mode
  运行，也可由 `exec` 或 `session exec` 以 non-interactive mode 运行。各 Profile
  的 model/effort selector 使用同名 typed 字段，args 保持 one-token-per-argv。
- 普通 archive/network installer 默认保护符合当前 schema 的 active configs；
  `--overwrite-configs` 显式授权全量替换。根目录 `make install` 固定使用完整 source
  bundle 覆盖配置并清理 Session/Run 状态，不暴露额外 Make 参数。
- install/self-update 在任何 binary/resource 替换前，用 staged candidate 的
  layout-only upgrade preflight 检查目标 home；只要专用 server/socket 仍存在、
  marker 无法验证或有 managed window 就 fail-before-mutation，并提示先执行
  `tmux stop`。`--overwrite-configs`
  不绕过 live Tmux 门禁。这样更新后的 active
  `${SN_CLI_HOME}/resources/tmux.conf` digest 不会与仍在运行
  的 server 混用；release smoke 覆盖 live server 阻断和 stop 后成功激活。
- CLI 当前 `contract_version=4`；error envelope、`server info/doctor` capabilities、
  release assertions 和 HTTP/CLI 文档必须同步。

#### 安装激活门禁

- payload `release/release.json` 必须完整匹配 activation epoch 4、CLI contract、
  Session schema 和 Run schema；未知、缺失或不相等的字段直接拒绝。
- `install.sh` 与 updater 不分段替换 active artifact，而是把已校验 payload 交给
  staged candidate 的 `server upgrade-activate`。该 action 从 preflight 到最后
  rename 全程持有 active-home maintenance lock 和 server lifecycle/Tmux lock。
  事务先 durable 写入包含 nonce、owner PID/start-token、original/staged/guard
  digest 和 exact current artifact set 的 journal，再写 state guard；随后依次把
  active `bin/`、`configs/`、`tools/` 原子移到 transaction backup，并以 no-replace
  regular-file barrier 占位，阻断并发入口和路径重建，再二次确认 quiescence。
  其他 artifact 提交后，再按固定顺序切换 `bin/`、`configs/`、`tools/`；原
  `bin/` 中非 Runtime regular files 保留。
- quiescence 同时要求：`sn-server` 未运行；专用 Tmux server 不存在；无
  active/unknown Session execution；无 queued/running/paused/
  `needs_reconciliation` Run；系统进程表中不存在除 activation helper 外、executable
  inode identity 等于切换前目标 home `sn-cli|sn-server` 的进程。coordinator 排除
  同时绑定 PID/start-token；未知 Run/Session state、SQLite quick-check、sidecar、
  process identity、guard 或 lock 任一歧义都 fail closed。rollback 先恢复
  tools/configs/其他 artifact、最后恢复并校验 bin；只有能证明 all-original 或
  all-staged 时才删除 guard 和 journal。journal 在 `committed|rolled_back`
  terminal phase 仍是入口 barrier；
  stage tree、跨目录 rename、guard/journal 更新与删除均按依赖顺序 fsync，恢复
  不完整时保留 barrier。activation crash 由当前 installer 根据 journal/guard
  identity 恢复。
- installer 在任何目录创建前 canonicalize Runtime home 与 install-dir，拒绝
  install-dir 位于 Runtime home 内；尚不存在的路径组件只接受 printable ASCII，
  排除 case-insensitive filesystem 无法在无写 dry-run 中证明的 Unicode alias。
  激活成功后的 command link 通过逐组件 no-follow directory descriptor 与
  no-clobber `symlinkat` 创建并再次强制位于 home 外；只接受已经精确指向当前 home
  binary 的 symlink，不覆盖其它 filesystem entry。
- 普通 install/update activation 不迁移、不删除 unsupported state。仅 local-source
  `make install` 在 staged artifact 完整提交后按 journal 记录删除 `sessions/`、
  Session private state 与 `runtime.db*`，且不把该授权传给 archive 或 updater。

### 7. 关键取舍

- 由入口固定 execution owner 与 target mode：bare direct/`exec` process
  replacement，Session managed child，Tmux managed window；Profile 不保存
  execution-mode 字段。
- Tmux 以 tmux server/window options 作为 live source of truth，排除独立 durable
  Tmux history；这避免双写、GC、隐私和 transcript 边界。
- Session 只接受有 terminal result 的 executor，排除 `transcript_only` 伪完成。
- command adapter 可以共享，Profile/Session/Tmux parser 和状态机不能共享。
- 配置、Session fact、Agent LoopState、SQLite 和 activation journal 都要求完整匹配
  当前 schema，不提供 alias、补齐或第二套 reader。
- unsupported state 采用 preflight fail-closed + recoverable backup/reset，排除隐式
  migration、自动重放和半激活。

## 实现核验与维护门禁

- `command/`、`profile/`、`session/`、`tmux/`、`run/`、`store/sqlite/` 与
  `transport/http/` 保持独立 owner；共享只限纯 adapter 和窄 internal 原语。
- Adapter、Profile、Session、Tmux、schema、activation、installer、HTTP/CLI
  contract 均有对应测试或 release smoke。
- 架构、契约、安装或发布变更必须继续通过：

  ```bash
  make fmt-check
  make test-serial
  make test-race
  go vet ./...
  make release-check SN_CLI_VERSION=<valid-semver>
  git diff --check
  ```

- 所有测试和 release check 只使用临时 `SN_CLI_HOME`，不得修改 active `~/.sn`。
- 若后续改动无法证明 adapter argv 分类、Session terminal result、Tmux target
  identity 或 activation rollback，应 fail closed 并先修订本 contract，不增加
  alias、隐式 migration 或兼容 shim。
