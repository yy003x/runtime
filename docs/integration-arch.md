# Agent Runtime 整合架构

> 状态：已完成。本文是 `mz-cli`、runtime 与 `agent-arch` 整合后的现行设计和迁移验收基准。

## 1. 目标结果

当前仓库只有一个 Agent Runtime：

- `internal/agentrun` 是 task、turn、loop、session、command lifecycle 与 artifact 的唯一 owner。
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

`cmd/sn-server` 负责：

- 提供 `/healthz` 和 `/v1/runs` HTTP adapter。
- 将执行、状态、日志、结果和控制请求委托给 `agentrun.Service`。
- 不保存独立 agent、session、memory 或 lifecycle。

### 2.2 AgentRun

`internal/agentrun` 负责：

- 生成 run ID 与标准目录。
- 写入 request、status、events、output、result、done。
- 维护公共状态与幂等语义。
- 校验 managed result contract 和 result schema。
- 维护 loop、native resume、tmux session 和 command lifecycle。
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
- interactive command：使用相同 profile common args 和环境，但直接连接终端，不创建 AgentRun。
- `apiProvider`：OpenAI-compatible、Anthropic-compatible、stream 和 mock。
- `tmuxProvider`：通过 daemon RPC 管理 tmux、paste、capture、稳定检测及 `result + done`。
- `nativeProvider`：进程内 LLM loop、persona、snapshot、block/continue/patch-resume/stop/cancel。

### 2.4 Executor 与 Daemon

`internal/executor` 提供 argv、env、cwd、stdin、stream capture、process group、signal forwarding、前台终端切换和 macOS shebang 兼容。

`internal/daemon` 提供：

- owner-only Unix Domain Socket、PID、随机 token。
- binary/version identity、idle exit 和 process registry。
- tmux start/has/capture/send/interrupt/kill。
- dependency lease、ref count、wait TCP/HTTP、restart/optional。
- 按 profile 启用的 audit proxy、upstream proxy、shim 和 DYLD 环境。

daemon 不解析 profile，不创建 run ID，不写 AgentRun artifacts。

## 3. Interactive 与 Managed 分流

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
| `sn-cli cx [raw args...]` | 原生 Codex interactive | 无 |
| `sn-cli cc [raw args...]` | 原生 Claude interactive | 无 |
| `sn-cli prompt -e cx ...` | managed AgentRun | 有 |
| `sn-cli task run --profile cx ...` | managed/capture AgentRun | 有 |
| API/native profile + prompt | AgentRun | 有 |
| tmux session 命令 | AgentRun session + daemon | 有 |

direct command 不解析旧 task flags，raw args 原样追加到目标 CLI argv。terminal、SIGINT/SIGTERM/SIGHUP 和前台 process group 由 executor 处理。

## 4. 目录契约

`SN_CLI_HOME` 默认 `~/.sn`。所有运行数据只能由 `internal/layout` 派生：

```text
~/.sn/
├── bin/sn-cli
├── configs/{runtime.yaml,*.json,personas/,skills/,tools/}
├── runs/<run_type>/<date>/<run_id>/
├── daemon/{runtime.sock,runtime.pid,runtime.token,processes.json,shims/}
├── state/{memory.json,update.json,runs/}
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
| memory | capability | `~/.sn/state/memory.json` |
| run artifacts | AgentRun | `~/.sn/runs` |
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
| `output.log` | AgentRun | stdout/stderr 或 pane capture |
| `result.json` | AgentRun/Provider contract | 结构化最终结果 |
| `done` | tmux Provider | tmux managed task 空完成标记 |
| `native-snapshot.json` | native Provider | native loop snapshot |

tmux managed task 成功条件：

1. `result.json` 已原子写入且可解析。
2. `run_id` 与请求一致。
3. result schema 校验通过。
4. `done` 存在且为空。

stdout、pane 静默、进程退出或单独完成文件都不构成成功。

## 8. HTTP 与 Capability

HTTP 只暴露 run API：

- `GET /healthz`
- `POST /v1/runs`
- `GET /v1/runs/{type}/{id}/status|logs|result`
- `POST /v1/runs/{type}/{id}/cancel|block|stop|continue|patch-resume`

Capability：

- memory：write、recall、forget、sources，持久化到 `state/memory.json`。
- skills：list、route、run、run-auto，从 `configs/skills` 加载。
- tools：schema、call、external description 和 capability guard，从 `configs/tools` 加载。
- workspace：受 root 边界约束的文件访问能力。

## 9. 迁移完成状态

| 阶段 | 状态 | 完成内容 |
| --- | --- | --- |
| P0 | 完成 | 固化现有测试、构建、profile 和 artifact 基线 |
| P1 | 完成 | 统一 `SN_CLI_HOME`、配置 owner 与全部数据路径 |
| P2 | 完成 | interactive `cx/cc` 与 managed prompt 分流，引入 `managed_args` |
| P3 | 完成 | 无源码安装、release update、config 同步、server 更名 |
| P4 | 完成 | README、架构文档和全量验收收口 |

迁移不会自动删除旧 checkout、旧 run 或旧配置目录。它们不再被现行入口读取，可由用户在确认无保留需求后自行归档或删除。

## 10. 验证矩阵

| 类别 | 命令/检查 | 覆盖点 |
| --- | --- | --- |
| 全仓测试 | `go test ./...` | 所有 Go package |
| 静态检查 | `go vet ./...` | Go 静态问题 |
| CLI 回归 | `make sn-cli-test` | AgentRun、Provider、executor、daemon、capability、HTTP、CLI |
| 构建 | `make sn-cli-build && make build` | `sn-cli` 与 `sn-server` |
| Release | `make release` | 四平台 archive/server/checksum |
| 本地安装 | 临时 home 运行 `make install` | config 同步、binary、symlink |
| 网络安装 | 本地 HTTP fixture | 无 Go/Git、checksum、无源码运行 |
| Interactive | `sn-cli cx --help/--version`、`cc --version` | raw args、无 artifact |
| Managed | `sn-cli prompt -e <fixture>` | `managed_args` 与 result artifact |
| Server | `/healthz` | `sn-server` 与共享 home |
| 失败保护 | checksum/config/binary validation fixture | 旧 binary 保留、冲突零部分复制 |
| 补丁质量 | `git diff --check` | 空白与补丁格式 |

## 11. 不变式

1. 只有 AgentRun 拥有 public lifecycle 和 artifacts。
2. active config 只从 `~/.sn/configs` 加载。
3. 发行模板只能补齐缺失配置，不能覆盖或删除本地配置。
4. command direct invocation 不创建 managed artifact。
5. managed prompt 必须显式进入 AgentRun。
6. daemon 只做长期进程和执行环境后端。
7. 普通 profile 不经过 proxy/shim/dylib 路径。
8. Provider 配置不保存 secret。
9. tmux 成功判定始终使用 `result.json + done`。
10. 安装后的 CLI 不依赖源码、Go 或 Git。

## 12. 完成标准

- [x] `sn-cli cx` 与 `mz-cli cx` 一样启动正常 Codex interactive。
- [x] `sn-cli prompt -e cx` 提供 managed AgentRun。
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
