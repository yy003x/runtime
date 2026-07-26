# sn-cli 配置契约

本地生效配置位于 `~/.sn/configs`。仓库 `configs/` 只包含 Provider profile 与 `runtime.yaml`；persona、skill、tool 和 schema 位于 `~/.sn/resources`，仓库模板位于 `resources/`。

每个 `configs/<profile_id>.json` 只定义一个 profile，ID 取文件名。JSON 采用严格校验：未知字段、CLI/API 字段混用、空必填值都会直接报错。机器可读约束见 [`provider-profile.schema.json`](../resources/schema/provider-profile.schema.json)，Runtime 设置见 [`runtime.schema.json`](../resources/schema/runtime.schema.json)。

## CLI profile

最小配置只有一个字段：

```json
{
  "command": "codex"
}
```

常用完整示例：

```json
{
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "xhigh",
  "args": [
    "--enable", "multi_agent",
    "-c", "approval_policy=never"
  ],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip",
    "OPENAI_API_KEY": null
  },
  "timeout_seconds": 900
}
```

| 字段 | 作用 | 必填 |
| --- | --- | --- |
| `command` | 实际执行的命令或可执行文件路径 | 是 |
| `model` | 目标 CLI 模型；省略时不生成 `--model` | 否 |
| `effort` | 推理等级；Codex 映射为 `model_reasoning_effort`，Claude 映射为 `--effort` | 否 |
| `args` | native direct 与 managed execution 共用的基础 argv；每个元素对应一个 argv token | 否 |
| `env` | 子进程环境；string 表示设置，`null` 表示删除 | 否 |
| `timeout_seconds` | managed execution 与 direct API request 的 deadline；native direct CLI 由目标程序管理生命周期 | 否 |

## Context 容量

CLI 与 API profile 共用以下可选字段：

| 字段 | 作用 |
| --- | --- |
| `context_window_tokens` | Provider/model 的总上下文容量，必须为正整数 |
| `reserved_output_tokens` | 为本轮输出预留的容量，必须为正整数 |
| `keep_recent_turns` | 生成 checkpoint 时优先保留原文的最近 Turn 数 |
| `summary_enabled` | 是否允许历史 checkpoint；设为 `false` 时仅在输入超过 hard budget 时失败 |

`profile_effective_reserved = max(reserved_output_tokens, API max_tokens)`；两者都缺失时为 `8192`。请求级 API `max_tokens` 只会通过 `max(profile_effective_reserved, request max_tokens)` 收紧本轮输入预算，不会因降低输出上限而扩大预算。`input_budget_tokens = context_window_tokens - effective_reserved_output_tokens`，且必须至少为 `2`；非法组合在 profile loader 或 request preflight 阶段以 `invalid_context_capacity` 拒绝，不做 `window/4` 等静默修正。

Runtime 先按 `input_budget_tokens` 判断 hard overflow，再判断 `floor(input_budget_tokens * 70%)` 主动压缩阈值。阈值以上可尝试压缩较早 Turn；压缩失败但原始输入仍不超过 hard budget 时回退原始历史。原始 Session 消息不会被覆盖或删除。未配置总容量时使用保守的 `32768` token，并在 context manifest 标记 `capacity_source=conservative_default`；发行 profile 的真实窗口没有权威声明时不得猜测写入。

顶层 `sn-cli <profile> [args...]` 使用 native direct 模式，不自动增加子命令，所有参数按原生 argv 传递。显式 `profile exec`、`session run|submit`、Loop 与 HTTP Run 才根据 `command` basename 选择 managed adapter：

- `codex`：managed execution 自动增加 `exec`，Provider 后参数作为 Codex 原生 argv。
- `claude`：managed execution 自动增加 `-p`，Provider 后参数作为 Claude 原生 argv。
- 其他命令：使用内部通用适配器，managed prompt 通过 stdin 发送，不增加厂商参数，也不接受 `effort`。

如果 `args` 已包含 Codex 的 `exec` 或 Claude 的 `-p` / `--print` 完整 token，Runtime 不会重复增加。CLI Provider 后的 token 不作为 Runtime option 解析，并按原顺序传给原生命令。`args` 不做 shell 拆词；例如 `"-c", "approval_policy=never"` 是两个 argv token，不能写成一个带空格的字符串。

```bash
sn-cli cx "hi"                     # native direct，等价于 codex "hi"
sn-cli profile exec cx --ephemeral "hi"  # 显式无记录 batch，使用 codex exec
sn-cli session run --session-id <id> cx --ephemeral "hi"
```

`session run|submit` 未使用 `--prompt-file` 或非空 stdin 时，CLI Provider 的最后一个 token 是 Session prompt；使用外部 prompt 时，Provider 后全部 token 都是原生 argv，调用方不要再追加 positional prompt。

### env 规则

子进程先继承当前进程环境，再应用 `env`：

- `"NAME": "value"`：设置固定值。
- `"NAME": "${SOURCE}"`：读取并展开当前进程的 `SOURCE`；未设置时立即报错。
- `"NAME": null`：从子进程环境删除 `NAME`。

只有 `${VAR}` 会展开；`$VAR` 与 `VAR` 都是普通字符串。Runtime 不加载 `.env` 或 direnv 文件。

## API profile

API profile 固定为四个必填字段，另有可选 headers、默认最大输出 token 数与超时：

```json
{
  "protocol": "openai",
  "base_url": "https://openrouter.ai/api/v1",
  "model": "z-ai/glm-5.1",
  "api_key": "${OPENROUTER_API_KEY}",
  "headers": {
    "HTTP-Referer": "https://client.example",
    "X-Client-ID": "${CLIENT_ID}"
  },
  "max_tokens": 16384,
  "timeout_seconds": 300
}
```

| 字段 | 作用 |
| --- | --- |
| `protocol` | `openai` 或 `anthropic`；必须显式提供 |
| `base_url` | compatible endpoint 的绝对 HTTP(S) 基础 URL |
| `model` | 请求模型 ID |
| `api_key` | 只能是完整 `${ENV_VAR}` 引用，禁止明文 |
| `headers` | 可选固定请求 headers；value 支持 `${VAR}` 展开 |
| `max_tokens` | 可选的默认最大输出 token 数，必须为正整数 |
| `timeout_seconds` | 可选的 profile deadline |

认证 header 由 Runtime 根据协议与 endpoint 生成。`headers` 不能覆盖 `Authorization`、`Proxy-Authorization` 或 `x-api-key`。`max_tokens` 沿用 OpenAI/Anthropic-compatible 的 provider 字段名；单次请求通过 `--max-tokens` 或 `runtimeapi.Request.max_tokens` 覆盖，不需要调用方重复传 profile 默认值。Provider 输出上限采用请求值；Session 输入预算则按上述保守最大值规则计算。

Runtime 统一规范化 OpenAI 与 Anthropic endpoint：

- `base_url` 末段没有显式 `vN` 版本时自动补 `/v1`。
- 已以 `/v1`、`/v2` 等版本段结尾时不重复追加版本。
- 已包含完整 `chat/completions` 或 `messages` endpoint 时保持幂等。
- OpenAI 最终追加 `chat/completions`，Anthropic 最终追加 `messages`。

例如，OpenAI 的 `https://example.test/compatible-mode` 与 `https://example.test/compatible-mode/v1` 都解析为 `https://example.test/compatible-mode/v1/chat/completions`；Anthropic 的 `https://example.test/apps/anthropic` 与 `https://example.test/apps/anthropic/v1` 都解析为 `https://example.test/apps/anthropic/v1/messages`。`base_url` 必须是绝对 HTTP(S) URL，不能包含 fragment。

API Provider 后只接受有限的请求级 typed options 与最后一个 quoted prompt：

```bash
sn-cli api-cx --model z-ai/glm-5.1 --temperature 0.2 --max-tokens 2048 "hi"
sn-cli api-cx --stream "hi"
sn-cli api-cx < prompt.md
sn-cli session run --session-id <id> api-cx --temperature 0.2 "hi"
```

支持的 option 是 `--model`、`--max-tokens`、`--temperature`、`--stream|--no-stream`。未知 option、多 positional prompt、命令行 `protocol/base_url/api_key/headers` 都会拒绝。

## runtime.yaml

```yaml
default_project: _default
default_profile: cx
max_concurrency: 1
max_queue: 64
queue_timeout_seconds: 3600
default_deadline_seconds: 300
assets:
  roots:
    project: /srv/runtime-assets
llm:
  mcp_servers:
    - name: local-tools
      command: /usr/local/bin/local-mcp
      args: ["serve"]
      dir: /srv/runtime-assets
      env:
        API_TOKEN: "${LOCAL_MCP_TOKEN}"
      env_passthrough: [HTTP_PROXY, HTTPS_PROXY]
      timeout_seconds: 30
session:
  default_carrier: tmux
  terminal:
    driver: ghostty
```

`assets.roots` 是 LLM Runtime 可读取的宿主机绝对目录映射。请求使用
`asset://project/skills/review/SKILL.md` 这类 URI，不能传绝对路径；Resolver
还会校验目录穿越和 symlink 越界。未配置 root 时仍可使用 inline 资产。

`llm.mcp_servers` 是 stock `sn-server` 与 `sn-cli llm generate` 可选择的 stdio
MCP allowlist。`name` 是请求中 `tools.mcp` 使用的稳定名称；`command`、`args`、
`dir`、`env` 和 `timeout_seconds` 只能由部署配置提供，HTTP 请求不能覆盖。
`dir` 如存在必须是绝对路径。

MCP 子进程默认只继承 `PATH`、`HOME`、`TMPDIR`、`LANG`、`LC_ALL` 中当前已设置
的值。额外继承必须列入 `env_passthrough`；显式 `env` 支持与 profile 相同的
`${VAR}` 展开，引用未设置时 server 启动失败。MCP 只在请求选择对应名称后启动，
请求结束即关闭。

`session.default_carrier` 支持 `tmux|terminal`；`session.terminal.driver` 支持 `ghostty|iterm2`。Runtime home 目录由 layout 固定，额外 asset root 由部署方显式配置。
