# Agent Runtime 整合架构

> 状态：已完成。本文是 runtime 命令面、Session/carrier、Provider 与 capability 收敛后的现行设计和验收基准。

## 1. 目标结果

当前仓库只有一个 Agent Runtime：

- `internal/agentrun` 是逻辑 Session、Turn、RunAttempt、Execution 以及 task/turn/loop/session/command artifact 的唯一 owner。
- `internal/provider` 对公开配置只暴露 CLI 与 API；tmux/terminal 由 Session carrier 管理。
- 进程内 LLM loop 与旧 tmux Provider 仅保留为内部兼容实现，不属于 profile JSON schema。
- `mz-cli` 的 interactive CLI、executor、daemon 与 tmux 能力已进入统一 runtime；depends/proxy/shim 不再由 profile 配置。
- memory、skills、tools 由 `internal/capability` 提供，数据统一落在 `~/.sn`。
- `cmd/sn-cli` 与 `cmd/sn-server` 调用同一个 `agentrun.Service`。
- active config 只来自 `~/.sn/configs`；persona、skill、tool 和 schema 位于 `~/.sn/resources`，仓库同名目录只属于发行模板。
- 安装和更新不依赖源码 checkout。

## 2. 分层与 Owner

```text
┌──────────────────────────────────────────────────────────────┐
│ L5 发行层                                                    │
│ install.sh · GitHub Release · self-update · config/resource sync│
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
│ CLI command · API · tmux/terminal session carrier             │
│ memory · skills · tools · workspace                          │
│ internal/provider · internal/capability                      │
└──────────────────────────────┬───────────────────────────────┘
                               │
┌──────────────────────────────▼───────────────────────────────┐
│ L1 执行底座                                                  │
│ executor · daemon · process registry · tmux                    │
│ internal/executor · internal/daemon                          │
└──────────────────────────────────────────────────────────────┘
```

### 2.1 入口层

`cmd/sn-cli` 负责：

- command profile 的原生 interactive 启动。
- managed Run、Session、Loop 和 command 控制面。
- profile discovery、validation、doctor，以及 skill/tool/memory namespace。
- daemon 控制与 release self-update。

`system doctor --json` 暴露现有 `agentrun.ContractVersion`、可加性演进的 `features` map 与 `scheduler` 健康信息，调用方据此做兼容门禁；contract 字段独立于 build/release version，也不因单个 Provider 凭据缺失而变化。当前 feature 包括 `durable_queue`、`async_submit`、`run_list`、`run_reconcile` 与 `artifact_durability`，版本均为 1。

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
- 维护 loop、native resume、逻辑 Session、tmux/terminal carrier 和 command lifecycle。
- 维护跨 Provider 的逻辑 Session、规范化消息、Turn/RunAttempt/Execution 关系和可重建 History 索引。
- 维护 `state/runs/queue.json` 的 owner-only 持久 FIFO、queue timeout、原子 claim、run 列表和崩溃恢复。
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
| `blocked` | 内部执行等待输入或外部条件 |
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

- `cliProvider`：`profile exec`、Session、Loop 与 HTTP Run 的 managed CLI 执行；CLI 命令面透传 Provider argv，HTTP/Go 结构化调用可使用 adapter typed overrides，并支持 stdin/arg/none 和实时输出。
- native direct command：使用相同 profile common args 和环境，argv 原样连接当前终端；顶层 direct 不创建 Run artifact 或逻辑 Session。
- `apiProvider`：公开 profile 的 OpenAI-compatible 与 Anthropic-compatible direct/managed 请求。
- `tmuxProvider`、`nativeProvider` 与 API Agent loop：保留为内部兼容执行组件，只能由 Runtime 内部构造，不能从 `configs/*.json` 加载。

`sessionCarrier` 不属于新的 Provider 类型，而是 command CLI 的交互执行载体：`tmux` 提供持久化、输入注入和重新 attach；`terminal` 通过显式 `ghostty|iterm2` driver 新建独立窗口，窗口关闭即结束 Execution。两者都复用同一 profile prepare/env 链和 Session/Execution 关系。

### 2.4 Executor 与 Daemon

`internal/executor` 提供 argv、env、cwd、stdin、stream capture、process group、signal forwarding、前台终端切换和 macOS shebang 兼容。

`internal/daemon` 提供：

- owner-only Unix Domain Socket、PID、随机 token。
- binary/version identity、idle exit 和 process registry。
- tmux start/has/capture/send/interrupt/kill、`pipe-pane` 持久日志与进程监督重启。
- 内部兼容代码仍包含 dependency/proxy/shim 组件，但公开 profile 不再启用这些路径。

daemon 不解析 profile，不创建 run ID，不直接写 AgentRun artifacts。daemon 进程同时托管 AgentRun dispatcher；dispatcher 通过 `agentrun.Service` claim 队列并执行，队列非空或有执行中 run 时禁止 idle exit。默认 `max_concurrency=1`、`max_queue=64`、`queue_timeout_seconds=3600`、`default_deadline_seconds=300`，多进程提交由文件锁串行化；状态、请求、结果、registry 与队列使用临时文件 `fsync`、原子 rename 和父目录 `fsync` 落盘。

## 3. Direct 与 Session 分流

完整的顶层解析、profile 参数归属以 [`cli-routing-contract.md`](cli-routing-contract.md) 为规范源。本节只说明架构边界。

每个 `configs/<profile_id>.json` 只定义一个 profile，ID 由文件名确定；`type`、`cli/api` wrapper、`label` 与 embedded `presets` 不属于当前 schema。command CLI profile 的字段直接放在根对象：

- `args`：native direct 与 managed execution 共用的基础 argv。
- `command`：实际可执行命令；Runtime 按 basename 推导 Codex、Claude 或通用内部适配器。
- `model` / `effort`：可选的模型与推理等级；省略时使用目标 CLI 默认值。Codex/Claude 的 managed mode 只在显式执行 action 中增加。

以 Codex 为例：

```json
{
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "high",
  "args": ["--search"]
}
```

行为契约：

| 调用 | 执行方式 | Artifact |
| --- | --- | --- |
| `sn-cli cx` / `sn-cli cc` | 原生 interactive | 无 run artifact；无逻辑 Session |
| `sn-cli cx "prompt"` / `sn-cli cc "prompt"` | 原生 CLI 携带初始 prompt，进入当前 TTY | 无 run artifact；无逻辑 Session |
| `stdin \| sn-cli cx` | stdin 原样继承，不推导 batch mode | 无 run artifact；无逻辑 Session |
| `sn-cli cx exec ...` | 原生 command passthrough | 无 run artifact；无逻辑 Session |
| `sn-cli cc -p ...` | 原生 flag passthrough | 无 run artifact；无逻辑 Session |
| `sn-cli api-cx [typed-options] "prompt"` | direct typed API request | 无 run artifact；无逻辑 Session |
| `sn-cli profile exec cx "prompt"` | 显式无记录 managed execution | 无 run artifact；无逻辑 Session |
| `sn-cli session run cx ...` | managed result + structured Session/Turn | 有 Run 和逻辑 Session |
| `sn-cli session submit --session-id <id> cc ...` | 跨 Provider 异步续轮 | 有 Run 和逻辑 Session |
| `sn-cli session open --carrier tmux cx ...` | transcript-only tmux Execution | 有 Run 和逻辑 Session |
| `sn-cli session open --carrier terminal cc ...` | transcript-only terminal Execution | 有 Run 和逻辑 Session |

native direct command 不解析 Runtime options；CLI profile 后的每个 argv token 按原顺序传给目标 CLI。它使用 `command + args + configured effort + model + native CLI args`，不自动增加 `exec/-p`，也不根据 stdin 是否为 TTY 切换模式。

`profile exec`、Session、Loop 与 HTTP Run 的 managed argv 按 `command + args + configured effort + model + derived managed mode + provider CLI args` 组装。Codex 增加 `exec`，Claude 增加 `-p`；`args` 已声明对应完整 token 时去重，其他命令不增加厂商参数。CLI Provider 后的 token 保持原顺序，不参与 Runtime option 解析；Session 最后的 prompt 由上下文层取出并通过 Provider prompt delivery 交付。`profile exec` 不写 artifact；`session run` 持久提交并 follow Provider stream，`session submit` 只返回 pending。terminal、SIGINT/SIGTERM/SIGHUP 和前台 process group 由 executor 处理。

CLI 参数契约统一为 `sn-cli <namespace> <action> [arguments] [options]`，最多两个命令词。Provider execution 的 Runtime options 位于 Provider 前，Provider 是 action 后的第一个 positional 参数，例如 `session run --session-id <id> cx "hi"`；Provider 后输入由 CLI/API adapter 拥有。公共语法不使用 `-c|--config`。lifecycle ID 分别使用 `--run-id`、`--session-id`、`--loop-id`，Session prompt 来源为最后一个 positional、Provider 前的 `--prompt-file`、stdin 三选一。

API Provider 后只接受 `--model`、`--max-tokens`、`--temperature`、`--stream|--no-stream` 和最后一个 quoted prompt，映射到协议 payload；未知 option、多 positional prompt，以及命令行 `protocol/base_url/api_key/headers` 都会拒绝。API connection、固定 headers 和 secret 边界仍由 profile 持有，header value 使用统一的 `${VAR}` 展开。

`session run|submit` 是结构化会话路径：Session 拥有规范化 message、Turn、RunAttempt 和 Execution，后续 Turn 可以切换 profile/provider。CLI 的 `result.json` 契约由这个入口注入，不由 profile config 决定；API 的规范结果由 Runtime 根据结构化 Provider 返回生成。`session open` 是 transcript-only 交互路径：保留 command、common args、model 与 env，不注入 managed mode，并创建彼此独立的 Session ID、Run ID 和 Execution ID。未指定 `--carrier` 时读取 `runtime.yaml` 的 `session.default_carrier`，发行默认值为 `tmux`。tmux carrier 使用 `sn-agent` 命名空间与 `pipe-pane` 持久日志；terminal carrier 使用 macOS `script` 捕获 transcript，通过显式 driver 启动窗口，不自动探测应用，也不承诺输入注入或重新 attach。

command 子进程环境由同一套 provider 逻辑生成，direct 与 session 不允许各自实现。顺序固定为：继承当前环境，再应用扁平 `env`；string 展开后设置，`null` 删除；只有记录入口再注入 AgentRun runtime env。多账号配置使用独立 profile 文件。该规则用于切换 `CODEX_HOME`、`CLAUDE_CONFIG_DIR` 等多账号目录，也用于消除父进程中的认证变量冲突；secret 值仍不得写入 profile。

Provider 配置的环境变量替换只有一种语法：`${VAR}`。`$VAR` 与 `VAR` 均为普通字符串，引用未设置时立即报错。API 凭据 schema 为完整的 `api_key: "${VAR}"` 引用；旧 `api_key_env`、`env_passthrough`、`env_unset` 与 proxy/shim 字段不保留在公开 schema。Runtime 不内置环境文件加载器，环境必须在进程启动前注入。

Session 子进程通过 `AGENTRUN_SESSION_ID`、`AGENTRUN_TURN_ID` 和 `SN_RUNTIME_CONTEXT_MANIFEST` 关联逻辑会话；skill、tool、Session working/candidate memory 和外部只读 memory 输入通过对应 `SN_RUNTIME_*` 环境变量提供给 wrapper/MCP。路径暴露不改变 tool capability 的授权与审计边界。

仓库发行 `cx`、`cc`、`cc-bai`、`cc-glm`、`commit`、`cx-image`、`cx-spark`、`api-cx`、`api-cc`、`mcx`、`mcc` 十一个 Provider 模板；其他账号目录、模型和 endpoint 由用户新增独立 profile。`commit`、`cx`、`cx-image` 固定使用 `${HOME}/.codex-aip`，其中 `commit` 使用 Codex Spark、只读 sandbox 和 900 秒 deadline，`cx-image` 接收 `WB_RUNTIME_IMAGE_PATH`；`cx-spark` 固定使用 `${HOME}/.codex-ait`，启用搜索、`danger-full-access`、`never` approval 和 xhigh reasoning/high verbosity。`cc`、`cc-bai`、`cc-glm` 固定使用 `${HOME}/.claude.aip`；`cc-bai` 与 `cc-glm` 都通过 `env` 的 `null` 删除父进程 `ANTHROPIC_AUTH_TOKEN`。API auth 不由 profile 配置。Provider JSON 严格拒绝未知字段；安装和更新会在校验新 binary 前显式迁移旧 schema，常规 loader 保持只读。

`profile validate <profile>` 只暴露最终配置目录与已生效的认证变量名称，不暴露变量值；Claude 同时生效 `ANTHROPIC_API_KEY` 和 `ANTHROPIC_AUTH_TOKEN` 时返回 warning，具体保留哪一种认证由本地 profile 决定。`profile command <profile> [--json]` 默认只读输出 native direct argv；`--mode exec` 输出 `profile exec`/Session 使用的 managed argv。两种模式都脱敏、不启动 Provider，也不返回 profile env 值。

进程内 native/API Agent/MCP 代码仍作为内部兼容组件接受单元测试，但不构成公开 profile 配置契约，也不能通过 `configs/*.json` 启用。后续若重新开放，必须另行设计独立、最小且可迁移的 schema。

## 4. 目录契约

`SN_CLI_HOME` 默认 `~/.sn`。所有运行数据只能由 `internal/layout` 派生：

```text
~/.sn/
├── bin/sn-cli
├── configs/{runtime.yaml,*.json}
├── resources/{personas/,skills/,tools/,schema/}
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
| persona | Runtime 内部扩展 | `~/.sn/resources/personas` |
| skills/tools | capability | `~/.sn/resources/skills`、`~/.sn/resources/tools` |
| config schema | 文档/配置工具 | `~/.sn/resources/schema` |
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
max_queue: 64
queue_timeout_seconds: 3600
default_deadline_seconds: 300
session:
  default_carrier: tmux
```

未知字段会被严格拒绝；路径不由配置控制。JSON/YAML 工具约束分别见 `resources/schema/provider-profile.schema.json` 与 `resources/schema/runtime.schema.json`。

## 5. 配置发行与本地 Ownership

active config、runtime resource 与 release template 严格分离：

- active config：`~/.sn/configs`。
- runtime resource：`~/.sn/resources/{personas,skills,tools,schema}`。
- release template：仓库或 archive 中的 `configs/` 与 `resources/`。
- runtime 不回退读取仓库配置，也不内嵌默认配置。

安装和更新的同步算法：

1. 递归预检 source 和 target。
2. symlink、特殊文件或文件/目录类型冲突立即失败。
3. 只创建 target 中不存在的目录和文件。
4. 已存在文件不比较内容、不覆盖。
5. target 中多出的文件不删除。
6. source 后续删除的模板不删除 target 文件。

同步前，新 binary 会在临时 home 中迁移旧 profile schema，把旧 `configs/{personas,skills,tools,schema}` 的缺失项复制到 `resources/`，并使用“现有本地配置/资源 + 新增模板”执行 `profile list` 验证。旧 `type/cli/api` wrapper、`label/presets`、嵌套 CLI 字段、环境控制字段、`api.auth` 和 `result_contract` 只由显式迁移入口处理；无法等价表达的 native、profile tmux、depends/execution 与 API 高级运行字段会明确报错。旧资源目录保留，普通加载保持只读严格校验。验证通过后才写 active home 和 binary。

常规 schema 演进保持向后兼容，新增字段必须有代码默认值。明确批准的不兼容 schema 收口由版本化、幂等 migrator 处理；迁移先在临时合并配置中通过严格校验，失败时不安装新 binary。

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
resources/
```

同一 release 还提供 `sn-server-<os>-<arch>` 和 `checksums.txt`。GitHub Actions 在 `v*` tag 上执行测试、交叉编译、checksum 和 release 发布。

发布前统一运行 `make release-check`：除生成四平台资产外，还校验资产与 checksum 完整性，并在临时 `SN_CLI_HOME` 中安装当前平台 archive、验证 `contract_version`，再注入不会进入发行包的 test-only CLI fixture 执行 Session/queue smoke。首个 GitHub Release 创建前，网络 binary 安装不可用，应使用 `install-source.sh`。

### 6.2 网络安装

`install.sh`：

- 检测 OS/ARCH。
- 下载 archive 与 `checksums.txt`。
- 校验 SHA256 和 tar entry。
- 在 `~/.sn/tmp` 解包和验证。
- 迁移旧资源目录，默认同步缺失 config 与 resource；显式 `--overwrite-configs` 只覆盖包内同名 config，不删除本地额外文件。
- 原子替换 `~/.sn/bin/sn-cli`。
- 原子更新 `~/.local/bin/sn-cli` symlink。

不调用 Go、Git，不 clone 或保留源码。

### 6.3 本地安装

`make install` 构建 `bin/sn-cli` 后调用同一安装契约。本地源码可以位于任意目录，安装结果不引用源码路径。为缩短本地配置调试闭环，`SN_CLI_OVERWRITE_CONFIGS` 默认是 `1`，会传入 `--overwrite-configs`；使用 `make install SN_CLI_OVERWRITE_CONFIGS=0` 可恢复为只补缺失。该开关不删除本地额外 profile、不覆盖 resources，也不改变网络安装、网络源码安装或 self-update 的默认安全语义。

### 6.4 网络源码安装

`install-source.sh` 通过 Git 下载源码到 `~/.sn/source/sn-runtime`，checkout 指定 branch、tag 或 commit，在本机执行 `make sn-cli-build`，再调用 `install.sh --binary --configs --resources`。

- 依赖 Git、Go 1.24+ 和 Make。
- 首次 clone 使用临时目录，完成 checkout 后再移动到正式源码目录。
- 再次安装要求受管 checkout 无本地修改，避免覆盖用户源码。
- binary 安装和配置/资源同步仍复用统一安装契约。
- 源码保留不改变 active config owner；runtime 仍只读取 `~/.sn/configs`。

### 6.5 Self-update

`sn-cli system update` 使用 GitHub Release API 与 release asset，不执行 `git fetch/pull/checkout`。支持：

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
| `output.log` | AgentRun | Provider 启动后单调追加的 stdout/stderr；同步 run 从 stream marker 后增量 follow；tmux 使用 `pipe-pane`，terminal 使用 transcript capture |
| `result.json` | AgentRun/Provider contract | 结构化最终结果 |
| `context-manifest.json` | Session | 当前 Turn 的消息、配置、策略与资源 digest |

`session open` 的 tmux/terminal Execution 只记录 transcript；结构化 Run 的成功仍以合法 `result.json` 为准，stdout 或 pane 静默不能替代结果契约。

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

`schema_version` 是数字，`validation.passed` 是布尔值，`artifacts`/`errors` 是 object 数组。`outcome` 只接受 `succeeded|failed|blocked|partial|cancelled`。Session result contract 的示例必须直接由 Go `Result` 类型序列化生成，避免提示、结构体和校验器漂移。

## 8. HTTP 与 Capability

HTTP 暴露 run 与 Session/History API：

- `GET /healthz`
- `GET /v1/runs`
- `POST /v1/runs`
- `GET /v1/runs/{type}/{id}/status|logs|result`
- `POST /v1/runs/{type}/{id}/cancel|block|stop|continue|patch-resume`
- `GET|POST /v1/sessions`
- `GET /v1/sessions/{id}`
- `GET /v1/sessions/{id}/messages|events|watch`
- `POST /v1/sessions/{id}/turns`

`sn-server` 默认地址为 `127.0.0.1:8080`。非 loopback 地址必须通过 `SN_SERVER_TOKEN` 开启 Bearer 鉴权；`/healthz` 保持无鉴权以供存活探针使用。HTTP adapter 的 JSON body 上限默认为 1 MiB，拒绝未知字段和非 JSON 写请求，并限制 `prompt_file` 只能引用 `cwd` 内的相对路径。

`POST /v1/runs` 默认保持同步兼容；请求头包含 `Prefer: respond-async` 时返回 `202 Accepted` 与 `Preference-Applied: respond-async`。`GET /v1/runs` 支持 `active`、`state`、`run_type`、`project_id`、`profile` 和 `limit` 过滤。

Capability 由一个 registry 统一装配，CLI、loop、native 与 API runtime 不再各自决定目录：

- memory：公共 CLI 为 `list|recall|add|remove|promote`；working 与 candidate 分离持久化。
- skills：公共 CLI 为 `list|show|run`，从 `resources/skills` 加载；内部仍支持关键词 route。
- tools：公共 CLI 为 `list|show|call`，从 `resources/tools` 加载，并与内置 function、external description、MCP stdio 和 capability guard 共享模型。
- workspace：受 root 边界约束的文件访问能力。

## 9. 迁移完成状态

| 阶段 | 状态 | 完成内容 |
| --- | --- | --- |
| P0 | 完成 | 固化现有测试、构建、profile 和 artifact 基线 |
| P1 | 完成 | 统一 `SN_CLI_HOME`、配置 owner 与全部数据路径 |
| P2 | 完成 | native direct、显式无记录 exec 与 Session 分流，managed mode 只由明确 action 推导 |
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
| 覆盖率 | `make coverage COVERAGE_MIN=65.0` | 全仓 atomic coverage 门禁；scheduler 另由故障恢复与 release smoke 覆盖 |
| 静态检查 | `go vet ./...` | Go 静态问题 |
| CLI 回归 | `make sn-cli-test` | AgentRun、Provider、executor、daemon、capability、HTTP、CLI |
| 构建 | `make sn-cli-build && make build` | `sn-cli` 与 `sn-server` |
| Release | `make release-check` | 四平台 archive/server/checksum、临时安装与 test-only CLI smoke |
| 本地安装 | 临时 home 运行 `make install` | config 同步、binary、symlink |
| 网络安装 | 本地 HTTP fixture | 无 Go/Git、checksum、无源码运行 |
| Native direct | `sn-cli <fixture>`、`sn-cli <fixture> "prompt"`、`stdin \| sn-cli <fixture>` | 原生 argv/stdin/TTY、无自动 managed mode、无 artifact |
| Profile exec | `sn-cli profile exec <fixture> "prompt"` | 显式 managed mode 与去重、无 artifact |
| Session | `sn-cli session run [runtime-options] <fixture>`、`session open --carrier tmux|terminal <fixture>` | 结构化跨 Provider Turn、独立 ID、carrier transcript 与 lifecycle |
| Server | `/healthz` | `sn-server` 与共享 home |
| Scheduler | 并发 submit、daemon restart、`run reconcile --dry-run` | FIFO、queue timeout、终态不重跑、队列可观察性 |
| 失败保护 | checksum/config/binary validation fixture | 旧 binary 保留、冲突零部分复制 |
| 补丁质量 | `git diff --check` | 空白与补丁格式 |

## 11. 不变式

1. 只有 Session、Loop 等显式状态 owner 创建 lifecycle 和 artifacts；顶层 profile 与 `skill run` 不记录。
2. active config 只从 `~/.sn/configs` 加载。
3. 发行模板只能补齐缺失配置和资源，不能覆盖或删除本地文件。
4. command direct invocation 不创建 managed artifact。
5. 顶层 CLI profile 始终 native direct；`profile exec` 明确选择无记录 batch；只有 `session run|submit` 注入 Provider 结果契约并创建会话记录。
6. daemon 只做长期进程和执行环境后端。
7. 普通 profile 不经过 proxy/shim/dylib 路径。
8. Provider 配置不保存 secret；引用 secret 只使用完整 `${VAR}` 占位符。
9. `session open` 只保证 transcript，不产生伪结构化 assistant final。
10. 安装后的 CLI 不依赖源码、Go 或 Git。
11. 公开 profile schema 只有扁平 CLI/API；内部兼容 Provider 不得通过配置启用。
12. memory 和本地 function tool 在未明确授权时不会执行；删除 memory 需要独立权限。

## 12. 完成标准

- [x] `sn-cli cx` 与 `mz-cli cx` 一样启动正常 Codex interactive。
- [x] `sn-cli cx "prompt"` 与 `sn-cli cc "prompt"` 进入原生 CLI TTY，不增加 `exec/-p` 且不创建 artifact。
- [x] `sn-cli profile exec <profile> "prompt"` 明确执行无记录 batch。
- [x] `sn-cli session run|submit <profile>` 形成结构化 Session/Turn，并允许后续 Turn 切换 Provider。
- [x] `sn-cli session open --carrier tmux|terminal <profile>` 使用同一 config 创建独立 Execution 与 transcript。
- [x] session 支持 list/show/messages/events/logs/send/interrupt/stop/attach/configure/export/delete；carrier 不支持的能力明确报错。
- [x] `agentrun` 统一 task/turn/loop/session/command。
- [x] 公开 Provider profile 收口为扁平 CLI/API，tmux/terminal 由 Session carrier 统一管理。
- [x] memory、skills、tools 接入统一 home 和统一 capability registry。
- [x] daemon 吸收 executor 周边长期进程能力。
- [x] `~/.sn/configs` 是唯一 active config source。
- [x] `~/.sn/resources` 是 persona、skill、tool 和 schema 的统一目录。
- [x] 本地与网络安装都执行不覆盖 config/resource 同步及旧资源目录复制迁移。
- [x] 网络源码安装将受管 checkout 保留在 `~/.sn/source/sn-runtime`。
- [x] self-update 不依赖源码 checkout。
- [x] `cmd/sn-cli` 与 `cmd/sn-server` 是唯一对外入口。
- [x] CLI、HTTP 与 tmux/terminal Session carrier 使用同一套 artifacts。
- [x] CI 覆盖构建、vet、串并行测试、race 与覆盖率门禁。
