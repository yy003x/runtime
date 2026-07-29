# Runtime vNext 配置

## Source 与 active home

```text
source configs/*.json                  → ${SN_CLI_HOME}/configs/*.json
source configs/commands/*.json         → ${SN_CLI_HOME}/commands/*.json
source configs/runtime/runtime.json    → ${SN_CLI_HOME}/runtime.json
source resources/                      → ${SN_CLI_HOME}/resources/
```

Profile loader 只读 active `configs/*.json`。每份文件必须用 `type=cli|api`
显式选择执行域；不读取 `commands/` 作为 Profile，不 fallback 到无 `type` 的旧
Profile、`binary/transport/prompt_delivery/effort_adapter` 或 `runtime.yaml`。

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

### `sn-cli profile` typed 参数

```text
sn-cli profile <id> \
  [--model <model>] \
  [--effort <low|medium|high|xhigh|max>] \
  [--prompt <file-or-text>] \
  [--exec|--exec=true|--exec=false] \
  [--cwd <dir>] \
  [input]
```

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

### 顶层 command shortcut

`commands/<id>.json` 只做显式映射：

```json
{
  "profile": "cx"
}
```

shortcut 目标必须是 `type=cli`。它使用 Profile 的 `command,args,env,model,
effort,exec,cwd` 构建配置前缀，但忽略 Profile `prompt`，不解析 typed override，
并把调用方 native argv 和 stdin 原样交给目标命令：

```text
sn-cli cx --model one-off-model
```

这里的 `--model` 属于 Codex，不属于 Runtime。固定 namespace 不能被
`commands/` 覆盖。

`profile list|show|check` 是管理 action。`show` 不展开 secret；`check` 只做静态、
符号化验证，不解析真实 env/PATH/cwd，不读取 prompt file，也不调用 Provider。

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
认证值只从 `auth.from_env` 获取。直接 API Profile 与 API Session Turn 支持：

```text
OpenAI-compatible: --max-completion-tokens <n>
Anthropic-compatible: --max-tokens <n>
共有: --temperature <0..2> --system <text>
Profile direct only: --stream --request-file <path|->
```

`--request-file` 读取 Runtime canonical `ModelRequest`，不是原始 Provider
payload。Driver 内部使用 Provider streaming 构建 canonical event，`--stream`
只控制 CLI 输出。

## Session 与 Tmux

Session 与 Tmux 不读取 Profile `exec`：

- `sn-cli session run|submit ... <cli-profile> <input>` 固定使用 adapter
  `exec=true`，由 Session managed subprocess 捕获 canonical
  stdout/stderr/exit；CLI Turn override 只支持 `--model`、`--effort`、`--cwd`；
- `sn-cli tmux start ... <cli-profile> [input]` 固定使用 adapter
  `exec=false`，在专用 tmux server 的 `sn-session` 中创建一个 window；
- Profile direct 和顶层 shortcut 才使用 Profile `exec` 默认值。

Session 不再提供 `--prompt-file`、`--terminal-driver`、`--command-arg` 或
`attach/send/interrupt/stop`。输入只来自 piped stdin 和最后一个位置参数。
Tmux 管理详见 [Tmux 管理契约](tmux-contract.md)。

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
只读，`write_file` 和 `exec_command` 必须显式启用。

## 升级 schema

contract v3 同时把 Session fact 和 SQLite `PRAGMA user_version` 提升到 2，不读取
schema 1，也不自动 migration。普通 network/archive 安装需要在升级前：

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
`sessions/`、`state/session-locks/`、`state/session-invocations/` 和
`state/runtime.db{,-wal,-shm,-journal}`。Runtime artifact 完整提交并校验前不删除
这些状态；成功后不重启 server。该授权不传递给 archive installer 或
`server update`。
installer 会在任何目录创建前解析 canonical home/install-dir，并拒绝 install-dir
位于 Runtime home 内。尚不存在的路径组件只接受 printable ASCII，避免
case-insensitive filesystem 上无法在无写 dry-run 中证明安全的 Unicode alias；
已存在的 Unicode ancestor 不受影响。激活后的 command symlink 通过逐路径组件
no-follow 的 directory descriptor 和 no-clobber `symlinkat` 创建，并再次强制位于
home 外；已有 regular file、目录或非目标 symlink 均不替换。

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
