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
| `timeout_seconds` | managed execution 与 direct API/native request 的 deadline；native direct CLI 由目标程序管理生命周期 | 否 |

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

API profile 固定为四个必填字段，另有可选 headers 与超时：

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
  "timeout_seconds": 300
}
```

| 字段 | 作用 |
| --- | --- |
| `protocol` | `openai` 或 `anthropic`；必须显式提供 |
| `base_url` | compatible endpoint 的基础 URL |
| `model` | 请求模型 ID |
| `api_key` | 只能是完整 `${ENV_VAR}` 引用，禁止明文 |
| `headers` | 可选固定请求 headers；value 支持 `${VAR}` 展开 |
| `timeout_seconds` | 可选的 profile deadline |

认证 header 由 Runtime 根据协议与 endpoint 选择，不配置 `auth`。`headers` 不能覆盖 `Authorization`、`Proxy-Authorization` 或 `x-api-key`，这些认证字段只由 `api_key` 生成。公开 profile 不接受 `stream`、`mock` 或 `runtime`；单次请求参数通过调用入口的 typed options 提供。

API Provider 后只接受有限的请求级 typed options 与最后一个 quoted prompt：

```bash
sn-cli api-cx --model z-ai/glm-5.1 --temperature 0.2 --max-tokens 2048 "hi"
sn-cli api-cx --stream "hi"
sn-cli api-cx < prompt.md
sn-cli session run --session-id <id> api-cx --temperature 0.2 "hi"
```

支持的 option 是 `--model`、`--max-tokens`、`--temperature`、`--stream|--no-stream`。未知 option、多 positional prompt、命令行 `protocol/base_url/api_key/headers` 都会拒绝。

## 不再属于 profile 的字段

以下能力不进入公开 JSON schema：

- `type`、`cli`、`api`：由 `command` 或 API 四字段自动识别，不再使用 wrapper。
- `native`：不再是可加载的 profile family。
- `executor`、`tmux`：tmux 是 Session carrier，使用 `sn-cli session open --carrier tmux <profile>`。
- `prompt_delivery`、`prompt_args`：由内部适配器固定管理。
- `env_passthrough`、`env_unset`：分别改为 `env.NAME="${NAME}"` 与 `env.NAME=null`。
- `override_policy`：profile 不再决定调用方权限；CLI profile 尾参数原样透传，API CLI 入口和 HTTP/Go 结构化调用按 adapter 的固定字段校验。
- `depends`、`execution`：依赖进程、proxy/shim 不再由 profile 启动。

## 显式迁移

普通加载严格且只读，不会修改文件。旧配置使用：

```bash
sn-cli system migrate-config
```

迁移会把旧 `type/cli/api` wrapper、`driver/binary`、嵌套 `command/runtime`、默认 `managed_args`、`api.auth` 和 embedded presets 转为扁平独立文件，并把 `env_passthrough/env_unset` 合并进 `env`。旧 `native`、profile 内 tmux、`depends/execution`、非默认 prompt 或 API 高级运行字段无法等价表达时会明确报错，不会静默改变行为。

迁移同时删除旧 `runtime.yaml` 的路径字段，并把旧 `configs/{personas,skills,tools,schema}` 中缺失的资源复制到 `resources/`；旧资源不删除，已有目标不覆盖。

## runtime.yaml

```yaml
default_project: _default
default_profile: cx
max_concurrency: 1
max_queue: 64
queue_timeout_seconds: 3600
default_deadline_seconds: 300
session:
  default_carrier: tmux
  terminal:
    driver: ghostty
```

`session.default_carrier` 支持 `tmux|terminal`；`session.terminal.driver` 支持 `ghostty|iterm2`。路径由 Runtime layout 固定，不接受配置覆盖。
