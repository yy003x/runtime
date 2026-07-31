# Runtime vNext 配置

## Source 与 active home

```text
source configs/*.json                  → ${SN_CLI_HOME}/configs/*.json
source configs/runtime/runtime.json    → ${SN_CLI_HOME}/runtime.json
source resources/                      → ${SN_CLI_HOME}/resources/
```

正式 release 的 Profile 文件集合由 `scripts/release-profile-files.sh` 显式列出；
release asset 不按 glob 收集未登记的本地 Profile。根目录 `make install`、doctor
和 provider smoke 仍按 `configs/*.json` 读取全部 source Profile，便于本地调试。

Profile loader 只读 active `configs/*.json`。每份文件必须用 `type=cli|api`
显式选择执行域；不存在 command ID 或第二层映射，也不 fallback 到无 `type` 的
旧 Profile、`binary/transport/prompt_delivery/effort_adapter` 或 `runtime.yaml`。

`resources/schema/profile.schema.json` 和 `runtime.schema.json` 是公开的 JSON
文档结构契约；Go loader 是涉及 adapter grammar、环境引用、跨字段关系、duration
上下限和文件系统事实的语义契约。两层都属于 normative validation，不能把 Schema
当作仅供编辑器提示的近似描述。仓库使用同一组 valid/invalid fixture 同时验证
Schema 与 loader；无法只由 JSON Schema 表达的上下文规则另有 loader 边界测试。
这类规则在 Schema 中使用 `x-sn-*` annotation 标出；标准 Draft 2020-12
validator 可以把 `format` 和自定义 annotation 当提示，但不能替代 Runtime
loader。测试保持单向不变量：任何 loader-valid 配置必须 schema-valid；只有写明
`semanticRule` 的 fixture 才允许“Schema 接受、loader 拒绝”。

## CLI Profile

```json
{
  "type": "cli",
  "command": "codex",
  "args": ["--skip-git-repo-check"],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip",
    "REMOVE_ME": null
  },
  "model": "gpt-5.6-sol",
  "effort": "high",
  "prompt": "",
  "exec": false,
  "cwd": ""
}
```

- `command` 是单个可执行文件名或路径，不经过 shell。首期仅登记
  `codex`、`claude` adapter，按 `filepath.Base(command)` 选择；
- `args` 一字符串一 argv token。Profile 中的 option 必须在对应 adapter 的显式
  grammar 内，不能包含额外 positional prompt；
- `env` 继承启动环境后覆盖；`null` 删除变量；
- `${NAME}` 是唯一插值语法。`args/env/cwd` 从进程启动时的 inherited
  environment snapshot 展开一次，Profile `env` 条目不能彼此引用；
- `model`、`effort`、`prompt`、`cwd` 可省略，`exec` 默认 `false`；
- `cwd` 在实际调用时必须解析成可进入目录。CLI ingress 可按 caller cwd 解析相对
  路径；HTTP CLI executor 只能使用 absolute override 或 Profile absolute `cwd`；
- `prompt` 使用 file-or-text 规则：若输入指向现有 regular file，则安全读取文件；
  不存在时按普通字符串；symlink、非 regular file、无效 UTF-8、NUL 或超限失败。

`model`、`effort` 和 `exec` 不直接拼接到配置尾部。adapter 会识别并替换
`args` 中同类 selector，重建 command/global options、mode selector、mode-only
options、`--` 和最终 prompt 的正确顺序；重复或无法安全归类的配置 fail closed。

### Profile typed 参数

```text
sn-cli <id> \
  [--model <model>] \
  [--effort <low|medium|high|xhigh|max>] \
  [--prompt <file-or-text>] \
  [--exec|--exec=true|--exec=false] \
  [--cwd <dir>] \
  [input]

sn-cli profile <id> \
  [--model <model>] \
  [--effort <low|medium|high|xhigh|max>] \
  [--prompt <file-or-text>] \
  [--exec|--exec=true|--exec=false] \
  [--cwd <dir>] \
  [input]
```

两种写法加载同一 Profile，进入同一 typed parser，并产生相同的
stdout/stderr/exit；顶层写法不是 raw argv passthrough。
每个 option 最多一次。`model/effort/exec/cwd` 覆盖 Profile 默认值；
`--prompt` 是追加输入，不覆盖 Profile prompt。最终 prompt 按以下顺序用换行连接
非空片段：

```text
Profile prompt → --prompt → piped stdin → positional input
```

`--exec` 等价于 `--exec=true`，不接受 `--exec true` 这种会消费位置输入的形式。
`--` 后最多一个 input。每个输入片段和最终 prompt 上限为 96 KiB；Runtime 还会在
spawn 前校验单 token 与总 argv/env/指针预算。

CLI Profile 的两种 mode 都在校验后 process replacement：

- effective `exec=true`：prompt 必须非空；stdin 固定 `/dev/null`；
- effective `exec=false`：prompt 可为空；非空 prompt 仍是最终 argv token；
  stdin 不是 TTY 时重新绑定 `/dev/tty`，没有 controlling TTY 则失败。

leading global `--json` 不包装 CLI Profile 的 stdout/stderr/exit code。`exec`
只选择目标命令的 interactive 或 non-interactive mode，不表示后台运行，也不表示
在哪个终端承载。

随 release 提供的 `cx-remote` 是受管远程任务 Profile：固定
`gpt-5.6-sol/xhigh`、`workspace-write`、`approval=never`，工作目录之外只增加
`${HOME}/mycode`，并使用隔离的 `CODEX_HOME=${HOME}/.codex-ait`。调用方应通过
`session run|submit` 使用它，不再动态拼沙箱、`--add-dir`、model 或 effort。

### 隐式 Profile 与保留 ID

`sn-cli <profile-id>` 直接解析 `configs/<profile-id>.json`，完全等价于
`sn-cli profile <profile-id>`。`type=cli|api` 决定使用 command adapter 还是 model
adapter；不会根据 ID 前缀猜测类型。两种入口都使用 Runtime typed option。例如：

```text
sn-cli cx --model one-off-model "回复 OK"
sn-cli api-cc "回复 OK"
```

固定根 namespace `profile|session|tmux|agent|run|server|help|version`、Profile
管理 action `list|show|check`，以及已退役 action 名 `exec|open` 是保留 Profile
ID。`configs/` 出现同名文件时 loader fail closed；`exec|open` 只保留名字，不
恢复旧 action，也不按路由优先级静默遮蔽。

`profile list|show|check` 是管理 action。`show` 不展开 secret；`check` 只做静态、
符号化验证，不解析真实 env/PATH/cwd，不读取 prompt file，也不调用 Provider。
这些 Profile 管理动作和直接 Profile 调用不加载 `runtime.json`；无关的 Agent、
scheduler 或 Run 配置错误不会阻止一次 Profile 查询或调用。

## API Profile

API Profile 保持独立 schema，不增加 CLI-only 字段。OpenAI-compatible 示例：

```json
{
  "type": "api",
  "driver": "openai-compatible",
  "endpoint": "https://example.invalid/v1/chat/completions",
  "model": "example",
  "auth": {
    "header": "Authorization",
    "scheme": "Bearer",
    "from_env": "MODEL_API_KEY"
  },
  "defaults": {
    "max_completion_tokens": 16384,
    "temperature": 0.2
  },
  "timeout": "5m",
  "context": {
    "window_tokens": 32768,
    "reserved_output_tokens": 8192,
    "keep_recent_turns": 8,
    "summary_enabled": false
  }
}
```

Anthropic-compatible 使用 Messages API 原生字段：

```json
{
  "type": "api",
  "driver": "anthropic-compatible",
  "endpoint": "https://example.invalid/v1/messages",
  "model": "example",
  "auth": {
    "header": "x-api-key",
    "from_env": "MODEL_API_KEY"
  },
  "defaults": {
    "max_tokens": 16384,
    "temperature": 0.2
  },
  "timeout": "5m"
}
```

`endpoint` 必须是完整 HTTPS endpoint。`headers` 只允许非 secret literal；
认证值只从 `auth.from_env` 获取。直接 API Profile 与 API Session Turn 共同支持：

```text
OpenAI-compatible: --max-completion-tokens <n>
Anthropic-compatible: --max-tokens <n>
共有: --temperature <0..2>
Profile direct only: --system <text> --stream --request-file <path|->
```

`--request-file` 读取 Runtime canonical `ModelRequest`，不是原始 Provider
payload。Driver 内部使用 Provider streaming 构建 canonical event，`--stream`
只控制 CLI 输出。

Durable Agent 创建 Run 时会把完整 API Profile、Profile digest 和 concrete
Provider driver semantic identity 冻结进 Store-private execution snapshot。
`auth.from_env` 只保存变量名、header 和 scheme，resolved secret value 不持久化也
不参与 digest；相同变量名下轮换 secret 不构成 drift。endpoint、model、literal
headers、defaults、context、timeout、driver 或 `from_env` 名称变化都会改变
snapshot。这里的 current config 指当前执行进程已经加载的 Profile，不表示每轮从
磁盘重新读取文件。

## Session 与 Tmux

Session 与 Tmux 不读取 Profile `exec`：

- `sn-cli session run|submit ... <cli-profile> <input>` 固定使用 adapter
  `exec=true`，由 Session managed subprocess 捕获 canonical
  stdout/stderr/exit；CLI Turn override 只支持 `--model`、`--effort`、`--cwd`；
- `sn-cli tmux start ... <cli-profile> [input]` 固定使用 adapter
  `exec=false`，在专用 tmux server 的 `sn-session` 中创建一个 window；
- 隐式和显式 Profile direct 才使用 Profile `exec` 默认值。

Session 不再提供 `--prompt-file`、`--session-file`、`--terminal-driver`、
`--command-arg`、`--launch` 或 `attach/send/interrupt/stop`。输入只来自 piped
stdin 和最后一个位置参数。
Tmux 管理详见 [Tmux 管理契约](tmux-contract.md)。

`session list|show|messages|events|logs|executions|execution|reconcile`
与 `session configure|export|delete|gc` 只加载 Session maintenance service，
不依赖 Profile 目录、Provider 或 `runtime.json`。只有 `session run|submit`
加载执行所需依赖。

Run 同样按 action 最小加载：`get|list|result|events|watch` 只需要 SQLite Run
Store；`cancel|reconcile` 只需要 Run Store 与 Session maintenance service，不依赖
current Profile、Provider、tool 或 `runtime.json`；`gc` 只在省略
`--older-than` 时额外读取 retention 配置。`submit|resume|retry` 和 worker
execution 才加载完整执行依赖。Agent Retry 使用原 private snapshot 与 current
loaded snapshot 比较，不按当前配置生成一份新 snapshot。

## runtime.json

```json
{
  "agent": {
    "tools": ["read_file", "list_directory"],
    "workspace_roots": ["/absolute/project"],
    "max_rounds": 16,
    "max_tool_calls": 64,
    "max_total_tokens": 0,
    "max_wall_time": "15m"
  },
  "scheduler": {
    "workers": 1,
    "poll_interval": "250ms"
  },
  "run": {
    "settled_retention": "168h"
  }
}
```

文件缺失时使用代码默认值。存在时必须是 regular file、非 symlink、严格 JSON，
未知字段失败。`workspace_roots` 未配置时以当前启动 cwd 为唯一 root；默认 tool
只读，`write_file` 必须显式启用；`exec_command` 已移除，配置该名称会失败。
`tools` 和
`workspace_roots` 显式配置时必须是 array，不能写 `null`。duration 使用 Go
duration 语法，并额外满足：

- `agent.max_wall_time`: `1s..24h`；
- `scheduler.poll_interval`: `10ms..1m`；
- `run.settled_retention`: `1h..8760h`。

“文件缺失时使用默认值”只适用于普通 Runtime bootstrap。activation payload、
需要保留的 active 配置和 staged home 都使用 required loader：`runtime.json`
缺失、symlink 或在检查/open 间被替换时直接拒绝，不能回退为默认值。

Agent tool execution snapshot 冻结 enabled definitions、tool
implementation/semantic version，以及 canonical `workspace_roots`、cwd 和 root
device/inode identity。它与 model/Provider snapshot、Agent execution contract
version 共同形成 Run 的 combined `config_digest`；绑定 Session 时还单独保存
Session 自己的 profile-only request/config digest。新 model/tool side effect 前
current snapshot 必须完整相等，但恢复 durable terminal/effect、取消和 reconcile
不依赖 current tool configuration。implementation version 是人工执行契约；改变
未被 snapshot 其它字段表达的行为时必须同步 bump，不能用 build 或 release version
代替。

## 升级 schema

contract v3 使用 Session fact `schema_version=2` 和 SQLite
`PRAGMA user_version=4`，不读取旧 schema，也不自动 migration。普通
network/archive 安装需要在升级前：

1. 用旧 binary 导出需要保留的 Session；
2. 停止 `sn-server` 和所有 `sn-cli tmux` managed window；
3. 把 `sessions/` 与 `state/runtime.db*` 移到可恢复备份；
4. 旧字段 Profile 需要显式备份后使用 `install.sh --overwrite-configs`，或手工改成
   新 schema；
5. 再执行当前 release 的 `install.sh`。

安装和 self-update 必须在替换 binary/resources 前完成 active-home preflight。
`--overwrite-configs` 只授权替换配置，不绕过 live server、Tmux、Session 或 Run
门禁。v0.1.1 legacy updater 不允许直接激活 contract-v3 release。

根目录 `make install` 是独立的本地源码调试策略，不使用上述数据保留流程，也不
需要额外 Make 变量。它固定校验并安装完整 source bundle，自动安全停止受管
`sn-server`，覆盖 source configs，并在 activation journal/guard 仍生效时删除
`sessions/`、`state/session-locks/`、`state/session-invocations/`、
`state/session-mutations/`、`state/session-trash-moves/` 和
`state/runtime.db{,-wal,-shm,-journal}`。Runtime artifact 完整提交并校验前不删除
这些状态；成功后不重启 server。该授权不传递给 archive installer 或
`server update`。
installer 会在任何目录创建前解析 canonical home/install-dir，并拒绝 install-dir
位于 Runtime home 内。尚不存在的路径组件只接受 printable ASCII，避免
case-insensitive filesystem 上无法在无写 dry-run 中证明安全的 Unicode alias；
已存在的 Unicode ancestor 不受影响。激活后的 command symlink 通过逐路径组件
no-follow 的 directory descriptor 管理，并再次强制位于 home 外。installer 在
activation mutation 前先持久化 `.sn-cli.<link-name>.owner.json`、持有其 `flock`
并以 no-clobber `symlinkat` 发布 exact command link；已有 regular file、目录或
非目标 symlink 均不替换。activation 失败或 reservation release 不删除 owner/link，
后续 retry 只能在 owner 内容、parent/link/owner inode 与 exact target 全部保持一致
时复用。

激活期间 `${SN_CLI_HOME}/state/activation.guard.json` 与 durable journal 保护 v3
入口，active `bin/`、`configs/` 还会短暂替换为 regular-file barrier 以阻断
v0.1.1 的 layout 初始化。恢复只接受 journal 中 exact artifact set、owner
PID/start-token 与 old/new/guard digest 一致的状态。journal 在
`committed|rolled_back` terminal phase 仍是入口 barrier，直到目录树、rename、
guard/journal 删除都按顺序 fsync 完成。

## Server

- `HTTP_ADDR`：监听地址，默认 `127.0.0.1:8080`；
- `SN_SERVER_TOKEN`：Bearer token；
- 非 loopback 地址必须设置 token；
- `SN_CLI_HOME`：active Runtime home。

`server start` 只启动 `${SN_CLI_HOME}/bin/sn-server`。PID identity、process lease、
lifecycle/maintenance lock、日志和升级 guard 位于 `${SN_CLI_HOME}/state/`。
首次启动和 server 已运行时，human 输出都包含 `pid=<正整数>`；leading-global
JSON 调用 `sn-cli --json server start` 固定返回 `running=true` 和同一个正整数
`pid`，供第三方托管。
