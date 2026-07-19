# Agent Runtime 整合架构

> 状态：已完成。本文是 `mz-cli`、runtime 与 `agent-arch` 整合后的现行设计和迁移验收基准。

## 1. 目标结果

当前仓库只有一个 Agent Runtime：

- `internal/agentrun` 是逻辑 Session、Turn、RunAttempt、Execution 以及 task/turn/loop/session/command artifact 的唯一 owner。
- `internal/provider` 是 CLI、API、tmux、native 的唯一执行抽象。
- 原 `agent-arch` 的进程内 LLM loop 已收敛为 native Provider。
- `mz-cli` 的 interactive CLI、executor、daemon、depends、audit proxy、PATH shim、DYLD 注入能力已进入统一 runtime。
- memory、skills、tools 由 `internal/capability` 提供，数据统一落在 `~/.sn`。
- `cmd/sn-cli` 与 `cmd/sn-server` 调用同一个 `agentrun.Service`。
- active config 只来自 `~/.sn/configs`，仓库 `configs/` 只属于发行模板。
- 安装和更新不依赖源码 checkout。

## 2. 分层与 Owner

```text
┌──────────────────────────────────────────────────────────────┐
│ L5 发行层                                                    │
│ install.sh · GitHub Release · self-update · config sync      │
│ internal/installbundle                                       │
└──────────────────────────────┬───────────────────────────────┘
                               │
┌──────────────────────────────▼───────────────────────────────┐
│ L4 入口层                                                    │
│ cmd/sn-cli · cmd/sn-server                                   │
└──────────────────────────────┬───────────────────────────────┘
                               │
┌──────────────────────────────▼───────────────────────────────┐
│ L3 AgentRun 语义层                                           │
│ task · turn · loop · session · command                       │
│ request/status/events/output/result/done                     │
│ internal/agentrun                                            │
└──────────────────────────────┬───────────────────────────────┘
                               │ Provider
┌──────────────────────────────▼───────────────────────────────┐
│ L2 Provider 与 Capability                                    │
│ CLI command · API · tmux · native                            │
│ memory · skills · tools · workspace                          │
│ internal/provider · internal/capability                      │
└──────────────────────────────┬───────────────────────────────┘
                               │
┌──────────────────────────────▼───────────────────────────────┐
│ L1 执行底座                                                  │
│ executor · daemon · process registry · depends               │
│ audit proxy · proxy env · PATH shim · DYLD                    │
│ internal/executor · internal/daemon                          │
└──────────────────────────────────────────────────────────────┘
```

### 2.1 入口层

`cmd/sn-cli` 负责：

- command profile 的原生 interactive 启动。
- managed prompt 和 task/turn/loop/session/command 控制面。
- profile discovery、validation、doctor、capabilities。
- daemon 控制与 release self-update。

`doctor --json` 暴露现有 `agentrun.ContractVersion`，调用方据此做兼容门禁；该字段独立于 build/release version，也不因单个 provider 凭据缺失而变化。

`cmd/sn-server` 负责：

- 提供 `/healthz`、`/v1/runs` 和 `/v1/sessions` HTTP adapter。
- 将执行、状态、日志、结果和控制请求委托给 `agentrun.Service`。
- 默认仅监听 loopback；非 loopback 必须配置 Bearer Token，并限制请求体、header、读写超时和文件路径输入。
- 不保存第二套 agent、session、memory 或 lifecycle。

### 2.2 AgentRun

`internal/agentrun` 负责：

- 生成 run ID 与标准目录。
- 写入 request、status、events、output、result、done。
- 维护公共状态与幂等语义。
- 校验 managed result contract 和 result schema。
- 维护 loop、native resume、tmux session 和 command lifecycle。
- 维护跨 Provider 的逻辑 Session、规范化消息、Turn/RunAttempt/Execution 关系和可重建 History 索引。
- 通过 `result_ref` 引用 run 的不可变 `result.json`，不复制 result 事实。
- 将 Provider detail 写入 `provider_status`，不产生第二套状态机。

公共状态：

| 状态 | 含义 |
| --- | --- |
| `pending` | run 已创建 |
| `running` | Provider 正在准备或执行 |
| `result_pending` | Provider 已结束，正在校验结果 |
| `done` | result contract 已满足 |
| `failed` | 配置、Provider 或结果校验失败 |
| `blocked` | native Provider 等待输入或外部条件 |
| `cancelled` | run 已取消 |

### 2.3 Provider

统一接口：

```go
type Provider interface {
    Kind() string
    Prepare(context.Context, Config, Request) (PreparedRequest, error)
    Execute(context.Context, PreparedRequest, Sink) (Result, error)
}
```

实现边界：

- `cliProvider`：managed command CLI，支持 typed overrides、stdin/arg/none 和实时输出。
- interactive command：使用相同 profile common args 和环境，直接连接终端；顶层 direct 不创建 run artifact 或逻辑 Session，显式 `session exec` 才创建 `metadata_only` Session/Execution。
- `apiProvider`：OpenAI-compatible、Anthropic-compatible、stream 和 mock；`api.runtime.enabled=true` 时复用进程内 Agent loop，增加 tool call、MCP、skill、memory 与本地 context 生命周期。
- `tmuxProvider`：通过 daemon RPC 管理 tmux、paste、capture、稳定检测及 `result + done`。
- `nativeProvider`：进程内 LLM loop、persona、snapshot、block/continue/patch-resume/stop/cancel、finish reason、token usage 和授权 tool-call 回路。

### 2.4 Executor 与 Daemon

`internal/executor` 提供 argv、env、cwd、stdin、stream capture、process group、signal forwarding、前台终端切换和 macOS shebang 兼容。

`internal/daemon` 提供：

- owner-only Unix Domain Socket、PID、随机 token。
- binary/version identity、idle exit 和 process registry。
- tmux start/has/capture/send/interrupt/kill、`pipe-pane` 持久日志与进程监督重启。
- dependency lease、ref count、wait TCP/HTTP、restart/optional。
- 按 profile 启用的 audit proxy、upstream proxy、shim 和 DYLD 环境。

daemon 不解析 profile，不创建 run ID，不写 AgentRun artifacts。

## 3. Interactive 与 Managed 分流

完整的顶层解析、profile 参数和 `--` 语义以 [`cli-routing-contract.md`](cli-routing-contract.md) 为规范源。本节只说明架构边界。

command CLI profile 有两类参数：

- `cli.command.args`：interactive 与 managed 都使用的 common args。
- `cli.runtime.managed_args`：只在 AgentRun managed/capture 调用中使用。

以 Codex 为例：

```json
{
  "command": {
    "binary": "codex",
    "args": ["--search"],
    "model": "gpt-5.6-sol"
  },
  "runtime": {
    "prompt_delivery": "stdin",
    "managed_args": ["exec"],
    "result_contract": "required"
  }
}
```

行为契约：

| 调用 | 执行方式 | Artifact |
| --- | --- | --- |
| `sn-cli cx` / `sn-cli cc` | 原生 interactive | 无 run artifact；无逻辑 Session |
| `sn-cli cx --help` / `sn-cli cc -p ...` | 原生 flag passthrough | 无 run artifact；无逻辑 Session |
| `sn-cli cx -- exec ...` | 移除 `--` 后原生 command passthrough | 无 run artifact；无逻辑 Session |
| `sn-cli session exec -c cx -- ...` | 显式 direct Session | metadata Session/Execution |
| `sn-cli cx "prompt"` / `sn-cli cc "prompt"` | managed AgentRun | 有 |
| `stdin \| sn-cli cx` | managed AgentRun | 有 |
| `sn-cli task run -c cx ...` | managed/capture AgentRun | 有 |
| `sn-cli session run -c cx ...` | managed Run + ephemeral Session | 有 |
| API/native profile + prompt | AgentRun | 有 |
| `sn-cli session start -c cx ...` | 同 config 的 tmux session + daemon | 有 |

direct command 不解析 runtime task flags；首个 `--` 仅作为 sn-cli 强制透传分隔符并在执行前移除。managed argv 固定按 `binary + command.args + model + raw-cli-args + managed_args` 组装。普通文本必须复用 AgentRun managed 链路。顶层 profile prompt 的 stdout 使用本次 Provider final text，只有本次未执行 Provider 时才回退到 result summary；run 信息单独写 stderr。terminal、SIGINT/SIGTERM/SIGHUP 和前台 process group 由 executor 处理。

CLI 参数契约统一为 `sn-cli <namespace> <action> [named options] [prompt] [-- raw-cli-args]`。config 只使用 `-c/--config`，lifecycle ID 只使用 `--run-id`/`--loop-id`，prompt 来源 positional、`--prompt-file`、stdin 三选一。旧 `prompt`/`prune` 命令及旧参数不保留兼容。

session 运行时将 command config 包装为 tmux config，保留 binary、common args、model、env 和 preset 结果，移除一次性执行专用 `managed_args`。自动包装时 tmux session 基础名固定为 `sn-agent`，禁止回退到旧的 `mz-cli-agent` 命名空间。首个 prompt 只有在 tmux buffer 粘贴和 Enter 成功后才记录 `prompt.submitted`；这表示提交成功，不表示模型完成。session 的 pane 输出持续写入 `output.log`，CLI 非零退出时最多尝试 5 次、间隔 3 秒，显式 stop 直接终止 tmux，不触发重启。

command 子进程环境由同一套 provider 逻辑生成，direct、managed 和 session 不允许各自实现。顺序固定为：继承当前环境、`env_unset` 删除、`env_passthrough` 显式传递、`env` 覆盖、AgentRun runtime env 注入。profile preset 可以用 `env_unset_append` 追加清理项。该规则用于切换 `CODEX_HOME`、`CLAUDE_CONFIG_DIR` 等多账号目录，也用于消除父进程中的认证变量冲突；secret 值仍不得写入 profile。

managed 子进程通过 `AGENTRUN_SESSION_ID`、`AGENTRUN_TURN_ID` 和 `SN_RUNTIME_CONTEXT_MANIFEST` 关联逻辑会话；skill、tool、Session working/candidate memory 和外部只读 memory 输入通过对应 `SN_RUNTIME_*` 环境变量提供给 wrapper/MCP。路径暴露不改变 tool capability 的授权与审计边界。

基础 `cx`/`cc` 继承父进程账号与认证环境；发行模板中的 `cx-aip`/`cc-aip` 承担固定账号目录或 endpoint 的显式选择。Provider JSON 使用严格字段解码，根对象、嵌套对象和 preset 中的未知字段均拒绝加载，避免拼写错误静默失效。

`config validate` 只暴露最终配置目录与已生效的认证变量名称，不暴露变量值；Claude 同时生效 `ANTHROPIC_API_KEY` 和 `ANTHROPIC_AUTH_TOKEN` 时返回 warning，具体保留哪一种认证由本地 profile 决定。`config command -c <config> [--json]` 复用同一份 profile 解析链，只读输出 managed argv 并脱敏，不启动 Provider，也不返回 profile env 值。

native 的 OpenAI-compatible adapter 与 direct API 一致使用 `/chat/completions`，Anthropic-compatible adapter 使用 `/messages` 并避免 base URL 已含 `/v1` 时重复拼接。两者共享 tool、tool result、finish reason 和 token usage 模型。没有 tool call 的响应结束本次运行；tool call 结果写回 snapshot 上下文后进入下一轮；未在 `allowed_actions` 中授权、命中 `forbidden_actions` 或属于 external kind 的工具不会执行。`max_rounds` 耗尽是明确失败，不是成功完成。

API profile 默认保持 one-shot 兼容。启用 `api.runtime` 后直接使用同一套进程内 Agent 状态机，OpenAI Chat Completions 的 `tool_calls` 和 Anthropic Messages 的 `tool_use/tool_result` 会统一映射到内部消息模型。API Agent context 持久化为 `context-snapshot.json`，支持 block/continue/patch-resume/stop/cancel；native 继续使用 `native-snapshot.json`。

API Agent capability 装配顺序：加载 profile system prompt；从 `configs/skills` 显式加载或自动路由 skill；召回当前 Session working memory；加入 Workbench/API 只读注入 memory；装配本地 function、memory 和 MCP 工具；最后按 `allowed_actions`/`forbidden_actions` 过滤后发送模型。每个 Turn 固化实际使用的 message/skill/tool/memory/config/policy digest；API/native 使用结构化历史 messages，managed CLI 使用规范化历史块。memory 的读、候选写入、删除分别使用 `memory.read`、`memory.write`、`memory.delete` 权限；Agent 写入先进入当前 Session candidates，显式 promote 后才进入 working memory。

MCP client 当前支持官方 stdio transport，使用单行 UTF-8 JSON-RPC 2.0，按 `initialize → notifications/initialized → tools/list → tools/call` 生命周期运行，并处理 `tools/list` 分页。模型侧工具名规范化为 `mcp__<server>__<tool>`；可以精确授权工具，也可以使用 `mcp.<server>` 或 `mcp` 扩大范围。server annotation 不作为可信授权依据，`forbidden_actions` 始终优先。

## 4. 目录契约

`SN_CLI_HOME` 默认 `~/.sn`。所有运行数据只能由 `internal/layout` 派生：

```text
~/.sn/
├── bin/sn-cli
├── configs/{runtime.yaml,*.json,personas/,skills/,tools/}
├── runs/<run_type>/<date>/<run_id>/
├── sessions/<date>/<session_id>/{turns/,executions/,memory/}
├── history/{index.json,trash/}
├── daemon/{runtime.sock,runtime.pid,runtime.token,processes.json,shims/}
├── state/{update.json,runs/,sessions/locks/}
├── memory/{durable.json,candidates.json}
├── logs/daemon.log
├── cache/
└── tmp/
```

路径 owner：

| 数据 | Owner | 路径 |
| --- | --- | --- |
| Provider profile | provider loader | `~/.sn/configs/*.json` |
| runtime settings | AgentRun | `~/.sn/configs/runtime.yaml` |
| persona | native Provider | `~/.sn/configs/personas` |
| skills/tools | capability | `~/.sn/configs/skills`、`tools` |
| Session working/candidate memory | AgentRun/capability | `~/.sn/sessions/<date>/<id>/memory/{working.json,candidates.json}` |
| legacy/manual global memory | capability | `~/.sn/memory/{durable.json,candidates.json}` |
| run artifacts | AgentRun | `~/.sn/runs` |
| session facts | AgentRun | `~/.sn/sessions` |
| history read model/trash | AgentRun | `~/.sn/history` |
| run registry | AgentRun | `~/.sn/state/runs` |
| daemon identity/registry | daemon | `~/.sn/daemon` |
| update state | updater | `~/.sn/state/update.json` |
| daemon log | daemon | `~/.sn/logs/daemon.log` |

`runtime.yaml` 只允许运行策略字段：

```yaml
default_project: _default
default_profile: cx
max_concurrency: 1
```

旧文件中的历史路径字段会被 YAML decoder 忽略，路径不再由配置控制。

## 5. 配置发行与本地 Ownership

active config 与 release template 严格分离：

- active config：`~/.sn/configs`。
- release template：仓库或 archive 中的 `configs/`。
- runtime 不回退读取仓库配置，也不内嵌默认配置。

安装和更新的同步算法：

1. 递归预检 source 和 target。
2. symlink、特殊文件或文件/目录类型冲突立即失败。
3. 只创建 target 中不存在的目录和文件。
4. 已存在文件不比较内容、不覆盖。
5. target 中多出的文件不删除。
6. source 后续删除的模板不删除 target 文件。

同步前，新 binary 会在临时目录中使用“现有本地配置 + 新增模板”执行 `profiles` 验证。验证通过后才写 active config 和 binary。

该策略要求 schema 演进保持向后兼容。新增字段必须有代码默认值，不能依赖覆盖用户已有配置完成迁移。

## 6. 发行、安装与更新

### 6.1 Release

每个平台 archive 名固定为：

```text
sn-cli-<darwin|linux>-<arm64|amd64>.tar.gz
```

archive 包含：

```text
sn-cli
configs/
```

同一 release 还提供 `sn-server-<os>-<arch>` 和 `checksums.txt`。GitHub Actions 在 `v*` tag 上执行测试、交叉编译、checksum 和 release 发布。

发布前统一运行 `make release-check`：除生成四平台资产外，还校验资产与 checksum 完整性，并在临时 `SN_CLI_HOME` 中安装当前平台 archive、验证 `contract_version` 和执行 `native-mock` task。首个 GitHub Release 创建前，网络 binary 安装不可用，应使用 `install-source.sh`。

### 6.2 网络安装

`install.sh`：

- 检测 OS/ARCH。
- 下载 archive 与 `checksums.txt`。
- 校验 SHA256 和 tar entry。
- 在 `~/.sn/tmp` 解包和验证。
- 同步缺失 config。
- 原子替换 `~/.sn/bin/sn-cli`。
- 原子更新 `~/.local/bin/sn-cli` symlink。

不调用 Go、Git，不 clone 或保留源码。

### 6.3 本地安装

`make install` 构建 `bin/sn-cli` 后调用同一安装契约。本地源码可以位于任意目录，安装结果不引用源码路径。

### 6.4 网络源码安装

`install-source.sh` 通过 Git 下载源码到 `~/.sn/source/sn-runtime`，checkout 指定 branch、tag 或 commit，在本机执行 `make sn-cli-build`，再调用 `install.sh --binary --configs`。

- 依赖 Git、Go 1.24+ 和 Make。
- 首次 clone 使用临时目录，完成 checkout 后再移动到正式源码目录。
- 再次安装要求受管 checkout 无本地修改，避免覆盖用户源码。
- binary 安装和配置同步仍复用统一安装契约。
- 源码保留不改变 active config owner；runtime 仍只读取 `~/.sn/configs`。

### 6.5 Self-update

`sn-cli update` 使用 GitHub Release API 与 release asset，不执行 `git fetch/pull/checkout`。支持：

- `--check`
- `--dry-run`
- `--version <tag>`

下载、checksum、解包和临时合并配置都位于 `~/.sn/tmp`。binary 通过本地配置验证后，使用同目录 rename 原子替换。任一下载、checksum、解包、配置或 binary 验证失败时，旧 binary 保留。

## 7. Artifact 契约

标准 run 目录：

```text
~/.sn/runs/<run_type>/<YYYY-MM-DD>/<run_id>/
```

| 文件 | Owner | 说明 |
| --- | --- | --- |
| `request.json` | AgentRun | 不可变执行请求 |
| `status.json` | AgentRun | 公共状态与 Provider detail |
| `events.jsonl` | AgentRun | 追加事件流 |
| `output.log` | AgentRun | stdout/stderr；session 使用 `pipe-pane` 持久终端日志 |
| `result.json` | AgentRun/Provider contract | 结构化最终结果 |
| `done` | tmux Provider | tmux managed task 空完成标记 |
| `native-snapshot.json` | native Provider | native loop snapshot |
| `context-snapshot.json` | API Agent Provider | API Agent 多轮消息、tool result、token usage 与可恢复上下文 |

native snapshot 额外保存每轮 message/tool message、累计 input/output tokens 和最后的 finish reason。tool 参数、结果及错误仅落在当前 run 目录，不写入 profile；provider status 和事件只记录工具名、状态与计数，不记录凭据。

tmux managed task 成功条件：

1. `result.json` 已原子写入且可解析。
2. `run_id` 与请求一致。
3. result schema 校验通过。
4. `done` 存在且为空。

stdout、pane 静默、进程退出或单独完成文件都不构成成功。

内置 `result.json` 的最小结构为：

```json
{
  "schema_version": 1,
  "run_id": "<request.run_id>",
  "outcome": "succeeded",
  "summary": "任务结果摘要",
  "artifacts": [],
  "errors": [],
  "validation": {"commands": [], "passed": true}
}
```

`schema_version` 是数字，`validation.passed` 是布尔值，`artifacts`/`errors` 是 object 数组。`outcome` 只接受 `succeeded|failed|blocked|partial|cancelled`。managed prompt 的示例必须直接由 Go `Result` 类型序列化生成，避免提示、结构体和校验器漂移。

## 8. HTTP 与 Capability

HTTP 暴露 run 与 Session/History API：

- `GET /healthz`
- `POST /v1/runs`
- `GET /v1/runs/{type}/{id}/status|logs|result`
- `POST /v1/runs/{type}/{id}/cancel|block|stop|continue|patch-resume`
- `GET|POST /v1/sessions`
- `GET /v1/sessions/{id}`
- `GET /v1/sessions/{id}/messages|events|watch`
- `POST /v1/sessions/{id}/turns`

`sn-server` 默认地址为 `127.0.0.1:8080`。非 loopback 地址必须通过 `SN_SERVER_TOKEN` 开启 Bearer 鉴权；`/healthz` 保持无鉴权以供存活探针使用。HTTP adapter 的 JSON body 上限默认为 1 MiB，拒绝未知字段和非 JSON 写请求，并限制 `prompt_file` 只能引用 `cwd` 内的相对路径。

Capability：

- memory：write、recall、forget、sources、candidates、promote；durable 与 candidate 分离持久化。
- skills：list、route、run、run-auto，从 `configs/skills` 加载。
- tools：schema、call、external description、MCP stdio 和 capability guard，从 `configs/tools` 与 `api.runtime.mcp_servers` 加载。
- workspace：受 root 边界约束的文件访问能力。

## 9. 迁移完成状态

| 阶段 | 状态 | 完成内容 |
| --- | --- | --- |
| P0 | 完成 | 固化现有测试、构建、profile 和 artifact 基线 |
| P1 | 完成 | 统一 `SN_CLI_HOME`、配置 owner 与全部数据路径 |
| P2 | 完成 | interactive `cx/cc` 与 managed prompt 分流，引入 `managed_args` |
| P3 | 完成 | 无源码安装、release update、config 同步、server 更名 |
| P4 | 完成 | CLI 统一参数契约、同 config session、日志与异常重启 |
| P5 | 完成 | README、架构文档和全量验收收口 |

迁移不会自动删除旧 checkout、旧 run 或旧配置目录。它们不再被现行入口读取，可由用户在确认无保留需求后自行归档或删除。

## 10. 验证矩阵

| 类别 | 命令/检查 | 覆盖点 |
| --- | --- | --- |
| 全仓测试 | `go test ./...` | 所有 Go package |
| 串行回归 | `make test-serial` | 文件锁、artifact 与生命周期顺序 |
| 并行重复 | `go test ./... -count=5` | tmux、daemon、run 并发稳定性 |
| Race | `make test-race` | AgentRun、memory、native/API agent、HTTP 关键并发路径 |
| 覆盖率 | `make coverage COVERAGE_MIN=65.0` | 全仓 atomic coverage 门禁 |
| 静态检查 | `go vet ./...` | Go 静态问题 |
| CLI 回归 | `make sn-cli-test` | AgentRun、Provider、executor、daemon、capability、HTTP、CLI |
| 构建 | `make sn-cli-build && make build` | `sn-cli` 与 `sn-server` |
| Release | `make release-check` | 四平台 archive/server/checksum、临时安装与 native-mock smoke |
| 本地安装 | 临时 home 运行 `make install` | config 同步、binary、symlink |
| 网络安装 | 本地 HTTP fixture | 无 Go/Git、checksum、无源码运行 |
| Interactive | `sn-cli cx --help/--version`、`cc --version` | raw args、无 artifact |
| Managed | `sn-cli <fixture> "prompt"`、`stdin \| sn-cli <fixture>` | prompt 来源、raw args、`managed_args` 与 result artifact |
| Session | `sn-cli session start -c <fixture> "prompt"` | 同 config tmux、提交事件、持久日志、异常重启 |
| Server | `/healthz` | `sn-server` 与共享 home |
| 失败保护 | checksum/config/binary validation fixture | 旧 binary 保留、冲突零部分复制 |
| 补丁质量 | `git diff --check` | 空白与补丁格式 |

## 11. 不变式

1. 只有 AgentRun 拥有 public lifecycle 和 artifacts。
2. active config 只从 `~/.sn/configs` 加载。
3. 发行模板只能补齐缺失配置，不能覆盖或删除本地配置。
4. command direct invocation 不创建 managed artifact。
5. 所有 managed prompt（包括 profile 普通文本简写）必须进入 AgentRun。
6. daemon 只做长期进程和执行环境后端。
7. 普通 profile 不经过 proxy/shim/dylib 路径。
8. Provider 配置不保存 secret。
9. tmux managed task 成功判定使用 `result.json + done`；session start 成功判定使用 pane ready 和可选的 `prompt.submitted`。
10. 安装后的 CLI 不依赖源码、Go 或 Git。
11. API one-shot 与 API Agent Runtime 共用 profile schema 和协议 adapter；是否进入 Agent loop 只由 `api.runtime.enabled` 决定。
12. MCP、memory 和本地 function tool 在未明确授权时不会暴露给模型；删除 memory 需要独立权限。

## 12. 完成标准

- [x] `sn-cli cx` 与 `mz-cli cx` 一样启动正常 Codex interactive。
- [x] `sn-cli cx "prompt"` 与 `sn-cli cc "prompt"` 按 profile 配置进入 managed AgentRun。
- [x] `sn-cli session start -c cx "prompt"` 使用同一 config 完成 tmux 启动和首个 prompt 提交。
- [x] session 支持 list/status/logs/send/interrupt/stop/attach、持久日志和异常重启。
- [x] `agentrun` 统一 task/turn/loop/session/command。
- [x] Provider 统一 CLI/API/tmux/native。
- [x] native Provider 吸收 agent-arch loop、persona 和 snapshot。
- [x] memory、skills、tools 接入统一 home。
- [x] daemon 吸收 executor 周边长期进程能力。
- [x] `~/.sn/configs` 是唯一 active config source。
- [x] 本地与网络安装都执行不覆盖 config 同步。
- [x] 网络源码安装将受管 checkout 保留在 `~/.sn/source/sn-runtime`。
- [x] self-update 不依赖源码 checkout。
- [x] `cmd/sn-cli` 与 `cmd/sn-server` 是唯一对外入口。
- [x] CLI、HTTP、native、tmux 使用同一套 artifacts。
- [x] API Provider 支持 OpenAI/Anthropic Agent tool call、MCP、skill、memory 和本地 context 生命周期。
- [x] CI 覆盖构建、vet、串并行测试、race 与覆盖率门禁。
