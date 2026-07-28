# Runtime vNext 配置

## Source 与 active home

```text
source configs/*.json                  → ${SN_CLI_HOME}/configs/*.json
source configs/commands/*.json         → ${SN_CLI_HOME}/commands/*.json
source configs/runtime/runtime.json    → ${SN_CLI_HOME}/runtime.json
source resources/                      → ${SN_CLI_HOME}/resources/
```

Profile loader 只读 active `configs/*.json`，每份文件必须以 `type=cli|api` 明确
选择执行域；不读取 `commands/` 作为 Profile，不 fallback 到无 `type` 的旧格式或
`runtime.yaml`。

## CLI Profile

```json
{
  "type": "cli",
  "binary": "codex",
  "effort_adapter": "codex-config",
  "args": ["exec", "--skip-git-repo-check", "--model", "example"],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip",
    "REMOVE_ME": null
  },
  "transport": "tty",
  "prompt_delivery": "argv"
}
```

- `binary`、每个 `args` item 都是独立 argv token，不经过 shell；
- `env` 继承父进程后覆盖；`null` 删除变量；
- `${VAR}` 是唯一插值语法，缺失变量在启动前失败；
- CLI Profile 没有顶层 `model` 字段；目标 CLI 的模型通过 `args` 中的
  `--model <id>` 等原生参数配置；
- `effort_adapter` 可选，只允许 `codex-config|claude-flag`。它显式声明
  `sn-cli profile <id> --effort low|medium|high|xhigh|max` 如何映射到目标 CLI；
  Runtime 不根据 `binary` 名称推断；
- 合法组合由 loader 与 `profile.schema.json` 同时约束。

`codex-config` 把 override 追加为两个 argv token：
`-c model_reasoning_effort=<level>`；`claude-flag` 追加为
`--effort <level>`。追加位置位于 Profile 固定 `args` 之后、调用方 input 之前，
因此任务级 typed override 可以覆盖 Profile 默认值。未声明 adapter 的 CLI
Profile、以及当前 API Profile，使用 `--effort` 时会在启动 Provider 前失败。
顶层 shortcut（例如 `sn-cli cx ...`）仍透明透传 native argv，不解释 Runtime
typed override。

`transport` 决定进程在哪里运行：

| 值 | 行为 | 结果捕获 |
| --- | --- | --- |
| `tty` | 当前进程或当前 stdio 中执行 | 自动命令可解析 stdout |
| `tmux` | 创建 detached tmux session | 只有 launch handle，`transcript_only` |
| `terminal` | macOS 新建 Ghostty/iTerm2 窗口 | 只有 launch handle，`transcript_only` |

`prompt_delivery` 决定输入如何交给目标 CLI：

| 值 | 行为 | 合法 transport |
| --- | --- | --- |
| `argv` | 把 prompt 追加成最后一个 argv token | 全部 |
| `stdin` | 把 prompt 写入 stdin | `tty` |
| `paste` | 启动后粘贴 prompt 并回车 | `tmux|terminal` |
| `manual` | Runtime 不注入 prompt，用户在交互界面输入 | 全部 |

顶层 shortcut 只接受 `type=cli` 且 `transport=tty` 的 Profile；
`tmux|terminal` 和 API Profile 通过 `profile <id>` 使用。shortcut 不执行
`prompt_delivery` 的单 prompt 解析，而是把调用方参数作为 native argv 原样传给
目标 CLI；`profile <id>` 才按表中的 delivery 规则处理 input。macOS terminal
driver 支持 `ghostty|iterm2`；launch 成功只代表已创建窗口/会话。

`commit.json` 是普通 CLI Profile：Codex 的 `exec`、model、reasoning 和
sandbox 都必须写成独立 argv token。`sn-cli commit` 与
`sn-cli profile commit` 复用这份配置；默认使用 `read-only` sandbox，因为提交
规划不拥有文件或 Git 写权限。

## API Profile

API Profile 的 `model` 字段没有删除，它是 HTTP 请求必需的模型 ID。CLI Profile
由目标命令自己的 argv 选择模型，API Profile 则由 Runtime Driver 写入请求体。

OpenAI-compatible Profile 当前对应 Chat Completions：

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

Anthropic-compatible Profile 使用 Messages API 的原生字段：

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

Anthropic `x-api-key` 可省略 `scheme`。`endpoint` 必须是完整 HTTPS endpoint。
`headers` 只允许非 secret literal；认证值只从 `auth.from_env` 获取。

直接调用中的 Provider request option 按 Profile Driver 区分：

```text
OpenAI-compatible:
  --max-completion-tokens <n>  → max_completion_tokens

Anthropic-compatible:
  --max-tokens <n>             → max_tokens

两者共有的 request option:
  --temperature <0..2>
  --system <text>

CLI transport option:
  --stream
  --request-file <path|->
```

`--system` 是 Runtime 的 Provider-neutral 便利参数：OpenAI Driver 将它编码成
`messages` 中的 `system` message，Anthropic Driver 将它编码成顶层 `system`。
`--request-file` 读取 Runtime canonical `ModelRequest`，不是原始 OpenAI/Anthropic
请求体。`--stream` 控制 Runtime 是否把规范化 event 输出为 NDJSON；Driver
内部始终用 Provider streaming 构建统一 event model，因此它不是原始请求体的
透传开关。token limit 和 temperature 才由 Driver 写成对应 Provider 字段。

如果配置 `context.window_tokens`，输入预算为：

```text
window_tokens - max(
  reserved_output_tokens or 8192,
  driver-specific default token limit
)
```

必须至少留下 2 input tokens。当前实现对超预算输入 fail closed，不静默截断原始
Session message。

## tmux Profile 与 Session

需要 Runtime 启动并控制 tmux 时，单独配置一个 CLI Profile：

```json
{
  "type": "cli",
  "binary": "codex",
  "args": [
    "--model",
    "gpt-5.6-sol"
  ],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip"
  },
  "transport": "tmux",
  "prompt_delivery": "paste"
}
```

假设保存为 `configs/cx-tmux.json`，一次不记录的启动使用：

```bash
sn-cli profile cx-tmux "分析当前仓库"
```

需要 Session 记录和 carrier 控制时：

```bash
sn-cli session run cx-tmux "分析当前仓库"
# 从 JSON 结果取得 session_id
sn-cli session attach --session-id <session_id>
sn-cli session send --session-id <session_id> "继续"
sn-cli session interrupt --session-id <session_id>
sn-cli session stop --session-id <session_id>
```

`session run --session-id <id> cx-tmux "..."` 会启动一个新的 tmux execution；
要继续当前 tmux，使用 `session send`，不要再执行一个 Turn。tmux 只保存
`transcript_only` 和 launch handle，Runtime 不把 pane transcript 伪装成结构化
assistant message，因此它适合启动、关联和控制交互 CLI，不适合作为高质量模型
会话历史来源。

## 顶层子命令

`commands/<id>.json` 只做显式映射，不复制执行配置：

```json
{
  "profile": "commit"
}
```

shortcut 目标必须是 `type=cli` 且 `transport=tty` 的 Profile。未登记在
`commands/` 中的 Profile 只能通过 `sn-cli profile <id>` 使用。固定 namespace
不能被映射覆盖；映射到不存在、API、tmux 或 terminal Profile 时 loader 失败。

## runtime.json

```json
{
  "terminal": {"driver": "ghostty"},
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

文件缺失时使用代码默认值。文件存在时必须是 regular file、非 symlink、严格 JSON，
未知字段失败。

`workspace_roots` 未配置时，以当前启动 cwd 为唯一 root。默认 tool 只读；
`write_file` 和 `exec_command` 需要明确加入 `agent.tools`。

## Server

- `HTTP_ADDR`：监听地址，默认 `127.0.0.1:8080`；
- `SN_SERVER_TOKEN`：Bearer token；
- 非 loopback 地址必须设置 token；
- `SN_CLI_HOME`：active Runtime home。

`sn-cli server start` 只启动 `${SN_CLI_HOME}/bin/sn-server`。严格 PID 身份、
进程 lease、生命周期互斥锁和日志均位于 `${SN_CLI_HOME}/state/`；`stop` 只有在
PID、进程启动标识和 lease 全部匹配时才会发送信号。旧版或损坏的 PID 文件不会被
兼容读取，也不会被静默忽略；应先确认旧进程已经停止，再按错误提示移除 stale 文件。
