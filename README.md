# Agent Runtime

本仓库实现自包含的 Go Agent Runtime。`sn-cli` 是统一终端入口；
`llmruntime` 与 `/v1/llm/generate` 分别提供 Go SDK 和本地 HTTP API。
Runtime 的主要调用入口是 `sn-cli` command、Go SDK 和本地 HTTP API。
CLI Provider、API Provider、Session carrier、Run、Loop、memory、skills、
tools 和 daemon 共用 Provider 与配置契约。

## 架构

```text
cmd/sn-cli             终端入口、Provider 路由、Runtime 控制面、自更新
cmd/sn-server          HTTP /v1/llm/generate、/v1/runs 与 /v1/sessions
runtimeapi             SDK 与 HTTP 共用的结构化 LLM 请求/响应/event
llmruntime             本地 SDK、context compiler、asset/tool/MCP registry
runtimeclient          /v1/llm/generate HTTP client
internal/agentrun      Session、Turn、RunAttempt、Execution 与运行产物
internal/provider      CLI/API Provider 与 carrier 适配
internal/executor      进程、TTY、流式输出与信号
internal/daemon        UDS 与长期进程管理
internal/capability    memory、skills、tools、workspace
internal/layout        ~/.sn 目录契约
internal/installbundle release 解包、checksum 与配置/资源同步
```

- `agentrun` 负责生命周期与运行产物。
- Provider profile 分为 CLI 与 API 两类。
- tmux 和 terminal 是 Session carrier。
- daemon 负责长期进程和执行环境。
- 生效配置从 `${SN_CLI_HOME:-~/.sn}/configs` 读取。
- 仓库 `configs/` 保存发行 profile 与 `runtime.yaml`；`resources/` 保存 persona、skill、tool 和 schema。

详细契约：

- [配置契约](docs/configuration.md)
- [CLI 路由契约](docs/cli-routing-contract.md)
- [整合架构](docs/integration-arch.md)
- [Session、History 与 Context Runtime](docs/session-history-contract.md)
- [LLM Runtime 契约](docs/llm-runtime-contract.md)
- [待实现能力](docs/PENDING.md)

## 安装

### GitHub Release

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
```

指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | \
  bash -s -- --version v1.0.0
```

安装结果：

```text
~/.sn/bin/sn-cli
~/.local/bin/sn-cli -> ~/.sn/bin/sn-cli
~/.sn/configs/*
~/.sn/resources/*
```

### 本地源码

```bash
make install
```

本地安装默认用仓库同名 profile 更新 `~/.sn/configs`，并保留用户额外 profile。只补充缺失配置时：

```bash
make install SN_CLI_OVERWRITE_CONFIGS=0
```

### 网络源码

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install-source.sh | bash
```

需要 Git、Go 1.24+ 和 Make。源码位于 `~/.sn/source/sn-runtime`。指定 branch、tag 或 commit：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install-source.sh | \
  bash -s -- --ref v1.0.0
```

### 安装与更新规则

网络安装、网络源码安装和 `sn-cli system update` 采用以下流程：

1. 校验 archive SHA256 并解包。
2. 在临时 home 中组合本地配置、发行包缺失项与资源。
3. 使用新 binary 执行 `profile list`，校验完整配置。
4. 递归补充缺失的 config 与 resource。
5. 原子替换 `~/.sn/bin/sn-cli`。

文件/目录类型冲突、symlink、特殊文件或配置校验失败都会在替换 binary 前终止。`--overwrite-configs` 只覆盖发行包内同名 config；resource 始终只补缺失项。

## Runtime home

默认 home 是 `~/.sn`，可通过 `SN_CLI_HOME` 覆盖：

```text
~/.sn/
├── bin/sn-cli
├── configs/
│   ├── runtime.yaml
│   └── <profile_id>.json
├── resources/
│   ├── personas/
│   ├── skills/
│   ├── tools/
│   └── schema/
├── runs/
├── sessions/
├── history/
├── daemon/
├── state/
├── memory/
├── logs/
├── cache/
└── tmp/
```

`configs/runtime.yaml`：

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
      env:
        API_TOKEN: "${LOCAL_MCP_TOKEN}"
      timeout_seconds: 30
session:
  default_carrier: tmux
  terminal:
    driver: ghostty
```

`assets.roots` 为可选的 LLM Runtime 资产根目录，供
`asset://project/...` 引用。`llm.mcp_servers` 是 stock server 可选择的 stdio
MCP allowlist；请求只能引用 `name`，不能上传 command 或 env。
`session.default_carrier` 支持 `tmux|terminal`；`session.terminal.driver`
支持 `ghostty|iterm2`。

## CLI

### Provider 入口

CLI profile 的顶层调用等价于原生命令调用。profile 后的 token 按顺序传给目标 CLI，Runtime 不创建记录：

```bash
sn-cli cx
sn-cli cx "hi"
sn-cli cx exec "hi"
sn-cli cc -p "hi"
sn-cli cx --help
```

API profile 执行一次 typed request，也不创建记录：

```bash
sn-cli api-cx --temperature 0.2 --max-tokens 2048 "hi"
sn-cli api-cc < prompt.md
```

显式无记录 batch 使用 `profile exec`：

```bash
sn-cli profile exec cx --ephemeral "hi"
sn-cli profile exec api-cx --temperature 0.2 "hi"
```

CLI 的 managed execution 根据 `command` basename 选择模式：

- Codex 使用 `exec`。
- Claude 使用 `-p`。
- 通用 CLI 使用 stdin。

profile `args`、`effort`、`model` 与调用参数共同组成最终 argv。查看脱敏后的实际命令：

```bash
sn-cli profile command cx
sn-cli profile command cx --mode exec --json
```

### 命令面

```text
run      list|show|logs|result|watch|cancel|reconcile
session  run|submit|open|list|show|messages|events|logs|send|interrupt|stop|attach|configure|export|delete|gc
profile  list|show|validate|command|exec
system   doctor|start|status|stop|restart|update
loop     run|list|show|logs|cancel
skill    list|show|run
tool     list|show|call
memory   list|recall|add|remove|promote
llm      generate
```

公共命令最多两层：namespace + action。完整帮助：

```bash
sn-cli --help
sn-cli --version
```

`--version` 是唯一版本查询入口，不额外提供 `sn-cli version`。正式版本以指向构建提交的 SemVer Git tag（`vMAJOR.MINOR.PATCH`）为事实源；非 tag 源码构建显示 `v0.0.0-dev+<commit>`。tag push 会触发 `.github/workflows/release.yml`，发布前由 `make release-check` 校验 tag 格式及 release binary 的版本注入结果。

### Session 与记录

`session run` 同步执行并跟随输出，`session submit` 异步提交：

```bash
sn-cli session run --session-id <id> cx "继续分析"
sn-cli session submit --session-id <id> cc "后台继续"
sn-cli session run --session-id <id> api-cx --temperature 0.2 "API 继续分析"
sn-cli session run --session-id <id> --prompt-file prompt.md cx --ephemeral
sn-cli session submit --session-id <id> --memory-file route-memory.json cx
```

Runtime options 位于 profile 前；CLI profile 后是目标 CLI 参数，API profile 后是 typed options 与 prompt。只有 `session run|submit` 创建结构化 Turn、message、RunAttempt、Execution 和 `result.json`。

`--memory-file` 接受最大 1 MiB 的 `{id,type?,content,source?}[]` JSON regular file，用于 caller-owned（调用方持有）的只读上下文注入；它不会改写 Session 中保存的原始 user message。每个 Turn 都可以选择不同 profile，Runtime 会从同一 Session 重新投影跨 Provider 上下文。

carrier 会话：

```bash
sn-cli session open --carrier tmux --session-id <id> cx
sn-cli session open --carrier terminal --session-id <id> cc
sn-cli session attach --session-id <id>
sn-cli session send --session-id <id> "继续"
sn-cli session stop --session-id <id>
```

tmux 支持重连；terminal 创建独立终端窗口。`session open` 记录 Execution 与 transcript。

ephemeral Session 只通过显式 GC 回收，默认先预览：

```bash
sn-cli session gc --older-than-hours 24
sn-cli session gc --older-than-hours 24 --limit 100 --apply
```

`--apply` 只把符合条件且非 active 的目录移动到 `history/trash`，不会永久删除。

查询与控制 Run：

```bash
sn-cli run list --active --limit 20
sn-cli run show --run-id <id>
sn-cli run logs --run-id <id> --tail 200
sn-cli run result --run-id <id>
sn-cli run watch --run-id <id>
sn-cli run cancel --run-id <id>
sn-cli run reconcile --dry-run
```

### Capabilities

```bash
sn-cli skill list
sn-cli skill show review
sn-cli skill run review cx

sn-cli tool list
sn-cli tool show shell
sn-cli tool call shell '{"command":"pwd"}'

sn-cli memory list
sn-cli memory recall "runtime"
sn-cli memory add fact "内容"
sn-cli memory promote <candidate-id>
sn-cli memory remove <memory-id>
```

资源目录：

```text
~/.sn/resources/personas/
~/.sn/resources/skills/<skill>/skill.yaml
~/.sn/resources/tools/*.tool.yaml
~/.sn/resources/schema/
```

### Daemon 与更新

```bash
sn-cli system doctor --json
sn-cli system start
sn-cli system status
sn-cli system restart
sn-cli system stop

sn-cli system update --check
sn-cli system update --dry-run
sn-cli system update
sn-cli system update --version v1.0.0
```

## Provider 配置

CLI profile：

```json
{
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "xhigh",
  "args": ["--enable", "multi_agent"],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip",
    "OPENAI_API_KEY": null
  },
  "timeout_seconds": 900
}
```

API profile：

```json
{
  "protocol": "openai",
  "base_url": "https://openrouter.ai/api/v1",
  "model": "z-ai/glm-5.1",
  "api_key": "${OPENROUTER_API_KEY}",
  "headers": {
    "HTTP-Referer": "https://client.example"
  },
  "max_tokens": 16384,
  "timeout_seconds": 300
}
```

OpenAI 与 Anthropic API profile 共用 endpoint 规范化：`base_url` 末段没有 `vN` 时自动补 `/v1`，已有版本段或完整 endpoint 时不重复追加；最终分别调用 `chat/completions` 与 `messages`。

`max_tokens` 是 API profile 的默认最大输出 token 数。direct、Session、Loop、HTTP Run 与 Go LLM Runtime SDK 都会继承；请求级 `--max-tokens` 或 `runtimeapi.Request.max_tokens` 可以覆盖。

CLI/API profile 都可声明 `context_window_tokens`、`reserved_output_tokens`、`keep_recent_turns` 和 `summary_enabled`。`input_budget_tokens` 是唯一硬上限；达到其 70% 时 Runtime 可主动使用已完成 Turn 的确定性截断摘要生成 checkpoint，压缩失败但原始输入仍在预算内时继续使用原始历史。容量未知时采用保守默认值并在 manifest 标记来源。

只有完整 `${VAR}` 引用读取环境变量。未设置的引用会报错；普通字符串保持原值。CLI `env` 的 `null` 值表示从子进程环境删除该变量。Runtime 不读取 `.env` 或 direnv 文件。

## Go LLM Runtime SDK

本地 Go 应用可以直接嵌入 Runtime。调用方需要自行维护业务 Session、Agent
loop 和 tool call 决策：

```go
runtime, err := llmruntime.New(llmruntime.Options{
    ProfileDir: "/srv/sn/configs",
    AssetRoots: map[string]string{"project": "/srv/runtime-assets"},
})
if err != nil {
    return err
}

response, err := runtime.Generate(ctx, runtimeapi.Request{
    Profile: "api-cx",
    Prompt:  "分析当前请求",
    Context: runtimeapi.ContextAssets{
        Skills: []runtimeapi.SkillRef{{
            AssetRef: runtimeapi.AssetRef{
                URI: "asset://project/skills/review/SKILL.md",
            },
        }},
        Memory: []runtimeapi.AssetRef{{
            URI: "asset://project/memory/context.json",
        }},
    },
    Tools: runtimeapi.ToolSelection{
        Inline: toolSchemas,
    },
})
```

默认 `tool_mode=schema_only`，响应中的 `tool_calls` 由上层 Agent 执行并组织下一
轮。若 Runtime 需要自行执行工具，启动时使用 `RegisterTool` 或 `RegisterMCP`，
请求指定 `tool_mode=runtime_execute` 和注册名称。HTTP 调用方使用
`runtimeclient.New`，请求类型完全相同。

动态 memory 通过 `RegisterMemoryProvider` 注册，请求使用
`context.recall[].provider` 选择；文件 memory 继续使用 inline 或 `asset://`。
流式调用使用同一个最终响应契约：

```go
response, err := runtime.GenerateStream(ctx, request, func(event runtimeapi.Event) error {
    // output.delta、tool lifecycle、response.completed
    return consume(event)
})
```

同一结构化请求也可直接交给 command，且不创建 AgentRun Session/Run artifact：

```bash
sn-cli llm generate --request-file request.json
sn-cli llm generate --request-file request.json --stream
cat request.json | sn-cli llm generate --request-file -
```

import path：

```text
github.com/yy003x/runtime/llmruntime
github.com/yy003x/runtime/runtimeapi
github.com/yy003x/runtime/runtimeclient
```

## 运行产物

managed Run 位于：

```text
~/.sn/runs/<task|turn|session|command>/<YYYY-MM-DD>/<run_id>/
~/.sn/runs/loop/<YYYY-MM-DD>/<loop_id>/
```

标准文件包括 `request.json`、`status.json`、`events.jsonl`、`output.log` 和 `result.json`。Session 位于：

```text
~/.sn/sessions/<YYYY-MM-DD>/<session_id>/
```

其中包含 `session.json`、`messages.jsonl`、`events.jsonl`、`turns/`、`executions/` 和 `memory/`。

managed `result.json` 的 optional `assistant_message` 是完整用户可见答复；`contract_version=1` 下 `summary` 继续保存兼容完整内容，不能强制改为短摘要。Session 从 `assistant_message || summary` 确定性派生最多 512 rune 的 Turn 摘要；artifacts 作为脱敏后的引用进入 assistant message metadata。

## 本地 HTTP API

```bash
make build
./bin/sn-server
```

默认监听 `127.0.0.1:8080`。`HTTP_ADDR` 可修改地址；监听非 loopback 地址时必须设置 `SN_SERVER_TOKEN`，请求使用 Bearer token。`GET /healthz` 无需鉴权。

主要端点：

- `GET /healthz`
- `POST /v1/llm/generate`
- `GET|POST /v1/runs`
- `GET /v1/runs/{run_type}/{run_id}/status|logs|result`
- `POST /v1/runs/{run_type}/{run_id}/cancel|block|stop|continue|patch-resume`
- `GET|POST /v1/sessions`
- `POST /v1/sessions/gc`
- `GET /v1/sessions/{session_id}`
- `GET /v1/sessions/{session_id}/messages|events|watch`
- `POST /v1/sessions/{session_id}/turns`

结构化 LLM 请求示例：

```bash
curl -sS http://127.0.0.1:8080/v1/llm/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "profile": "api-cx",
    "prompt": "分析当前请求",
    "context": {
      "skills": [
        {"uri": "asset://project/skills/review/SKILL.md"}
      ]
    }
  }'
```

流式事件使用 SSE：

```bash
curl -N http://127.0.0.1:8080/v1/llm/generate \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d @request.json
```

Session watch 同样在 `Accept: text/event-stream` 时进入 SSE，并支持
`after_seq` 或 `Last-Event-ID` 续读。

HTTP 只能读取 `runtime.yaml` 预配置的 asset root，并只能引用进程启动时已注册
的 tool/MCP；请求不能提交宿主机绝对路径、Go handler 或 MCP command。

## 构建与验证

```bash
make sn-cli-build
make build
make release-check

go test ./...
go vet ./...
make sn-cli-test
make test-serial
make test-race
make coverage COVERAGE_MIN=65.0
git diff --check
```

内部的 `release-check` 会执行格式、测试、vet、目录边界、四平台资产/checksum 与离线 smoke 校验。

## 安装与发布

用户入口固定为三个：

```bash
make install
make release
make publish
```

### install

`make install` 构建并安装当前 checkout，不访问远端、不创建 tag。HEAD 正好有 release tag 时安装正式版本，否则版本为 `v0.0.0-dev+<commit>`。默认覆盖发行包内同名 config，并保留 local-only profile；只补缺失配置时使用 `SN_CLI_OVERWRITE_CONFIGS=0`。

### release

`make release` 要求位于干净的 `main`。它读取本地 tag 与 `origin` 的远端 tag，按稳定 SemVer 取最高版本并自动增加 patch；没有任何 tag 时从 `v0.1.0` 开始。随后执行完整 release gate、生成 `dist/` 并创建本地 annotated tag，不安装、不 push。

```bash
make release
```

需要升级 minor/major 或使用 prerelease 时显式指定：

```bash
make release TAG=v0.2.0
make release TAG=v1.0.0-rc.1
```

### publish

`make publish` 要求位于干净的 `main`，默认选择当前 HEAD 上版本最高的本地稳定 SemVer annotated tag，并验证它确实指向 HEAD。然后读取远端 tag 防止同名冲突，使用 atomic push 同时更新 `origin/main` 与该 tag；它不构建、不测试、不创建 tag、不安装，也不 force。

```bash
make publish
```

也可显式指定当前 HEAD 上的 tag：

```bash
make publish TAG=v0.2.0
```

tag push 会触发 `.github/workflows/release.yml` 创建 GitHub Release。标准发布顺序是：

```bash
make release
make install
make publish
```
