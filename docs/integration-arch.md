# Agent Runtime 整合架构

> 状态：已完成。本文记录 `mz-cli`、原 runtime 和 `agent-arch` 能力整合后的最终架构、边界与迁移验收结果。

## 1. 整合结果

整合后的仓库只有一个 Agent Runtime：

- `agentrun` 是唯一 task、turn、loop、session、command 生命周期与 artifact owner。
- `Provider` 是 CLI、API、tmux、native 的唯一执行抽象。
- 原 `agent-arch` 的进程内 LLM loop 已收敛为 native Provider，不再有独立 agent runtime。
- `mz-cli` 的 executor、daemon、depends、audit proxy、PATH shim、DYLD 注入能力已进入 L1 执行底座。
- daemon 是唯一长期进程后端，但不解析 profile，也不读写 run artifacts。
- `cmd/sn-cli` 和 `cmd/runtime-server` 是两个对外入口，二者调用同一个 `agentrun.Service`。
- `configs/<profile>.json` 是唯一 Provider 配置事实源。

已删除的重复架构：

- 原 `internal/agent` 独立 HTTP runtime。
- 原 `internal/config`、`internal/memory`、`internal/token` 第二套 agent state/memory 依赖。
- `legacyruntime.Client`、REPL session store、`sncli/cmd` 和 `sncli/internal` 旧入口。
- `configs/config.yaml` 旧 Provider 配置。
- `/v1/agents`、`/v1/chat`、`/v1/sessions` 旧 HTTP lifecycle。

## 2. 分层架构

```text
┌──────────────────────────────────────────────────────────────┐
│ L4 入口层                                                    │
│ cmd/sn-cli · cmd/runtime-server · self-update                │
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
│ L2 Provider 实现层                                           │
│ CLI command · API · tmux · native                            │
│ internal/provider                                            │
└──────────────────────────────┬───────────────────────────────┘
                               │
┌──────────────────────────────▼───────────────────────────────┐
│ L1 执行底座                                                  │
│ executor · daemon · process registry · depends               │
│ audit proxy/upstream pool · proxy env · PATH shim · DYLD     │
│ internal/executor · internal/daemon                          │
└──────────────────────────────────────────────────────────────┘
```

### 2.1 L4 入口层

`cmd/sn-cli`：

- profile 直接执行。
- task/turn/loop/session/command 控制面。
- profile validate、doctor、daemon doctor。
- capabilities 与 self-update。

`cmd/runtime-server`：

- 提供 `/v1/runs` HTTP adapter。
- 不保存独立 session、memory 或 lifecycle。
- 所有执行和读取都委托给 `agentrun.Service`。

### 2.2 L3 AgentRun

`internal/agentrun` 负责：

- 创建 run ID 与目录。
- 写入 request、status、events、output、result、done。
- 管理公共生命周期和幂等语义。
- 校验 managed result contract 与 result schema。
- 将 Provider detail 写入 `provider_status`，不暴露第二套 lifecycle。

公共状态：

| 状态 | 含义 |
| --- | --- |
| `pending` | run 已创建，等待执行 |
| `running` | Provider 正在准备或执行 |
| `result_pending` | Provider 已结束，正在校验结果 |
| `done` | result contract 已满足 |
| `failed` | Provider、配置或结果校验失败 |
| `blocked` | native Provider 等待人工或外部条件 |
| `cancelled` | run 被取消 |

### 2.3 L2 Provider

统一接口：

```go
type Provider interface {
    Kind() string
    Prepare(context.Context, Config, Request) (PreparedRequest, error)
    Execute(context.Context, PreparedRequest, Sink) (Result, error)
}
```

实现：

- `cliProvider`：调用统一 executor，支持 stdin/arg/none prompt delivery 与实时 stdout/stderr。
- `apiProvider`：OpenAI-compatible、Anthropic-compatible、stream/mock。
- `tmuxProvider`：通过 daemon RPC 启动 session，使用稳定检测、paste-buffer、pane capture 和 `result + done` 协议。
- `nativeProvider`：进程内多轮 LLM loop、snapshot、block/continue/patch-resume/stop/cancel。

`agentrun` 只依赖该接口，不直接分支调用 CLI、API、tmux 或 native 裸执行函数。

### 2.4 L1 Executor

`internal/executor` 提供：

- argv、env、cwd、stdin。
- 按传入 env 解析 PATH。
- stdout/stderr 流式回调与 capture。
- `Setpgid`、SIGINT 到 SIGKILL 的进程组取消。
- 前台终端 process group 处理。
- macOS DYLD + shebang 兼容。

同步 command CLI 由 executor 直接运行。只有 profile 配置了 `depends` 或 execution 注入时，CLI Provider 才向 daemon 申请执行 lease；结束后立即释放。

### 2.5 L1 Daemon

`internal/daemon` 提供：

- Unix Domain Socket 单例。
- owner-only socket、随机 token 鉴权和 PID file。
- client 自动拉起、版本与二进制身份检测；仅无活动进程时自动或显式重启。
- idle exit。
- 持久 process registry；daemon 重启后重新发现存活 tmux session。
- tmux start/has/capture/send/interrupt/kill RPC。
- dependency owner/ref 管理与进程组回收。
- `wait_tcp`、`wait_http`、`optional`、`restart`。
- 按需 audit proxy 与上游 HTTP proxy 轮询池。
- 单入口 proxy env、PATH shim、BROWSER 和 macOS DYLD 注入。

daemon 不负责：

- 解析 `configs/*.json`。
- 创建 run ID。
- 写 request/status/events/result。
- 决定 public lifecycle。
- 保存独立 session 事实源。

`attach-session` 是前台终端连接，不创建或回收长期进程，因此由 CLI 直接连接 tmux；其余 tmux 生命周期操作都由 daemon 执行。

## 3. 配置契约

Provider 只从 `configs/*.json` 加载。支持 `type=cli|api|native`、preset、extends、alias 和 typed overrides。

CLI profile 可选字段：

```json
{
  "depends": [
    {
      "command": "helper --serve",
      "wait_tcp": "127.0.0.1:4141",
      "restart": true,
      "optional": false,
      "silent": false
    }
  ],
  "execution": {
    "audit_proxy": true,
    "upstream_proxy_env": ["UPSTREAM_PROXY_URL"],
    "bypass": ["localhost"],
    "shim": true,
    "dylib": "${INTERPOSE_DYLIB_PATH}"
  }
}
```

约束：

- `depends` 和 `execution` 只允许用于 CLI Provider。
- `wait_tcp` 与 `wait_http` 互斥。
- `upstream_proxy_env` 只保存环境变量名，且要求 `audit_proxy=true`。
- 上游代理只接受 HTTP proxy URL；同一 daemon 生命周期内不允许静默切换代理池。
- audit proxy、shim、dylib 默认关闭。
- dylib 由 profile 显式提供路径；runtime 不隐式修改或签名第三方二进制。
- secret 只从环境读取，不进入 profile、日志或 result artifact。

## 4. 产物契约

标准 run 目录：

```text
runs/global/runtime/<run_type>/<YYYY-MM-DD>/<run_id>/
```

标准文件：

| 文件 | Owner | 说明 |
| --- | --- | --- |
| `request.json` | AgentRun | 不可变执行请求 |
| `status.json` | AgentRun | 公共状态和 Provider detail |
| `events.jsonl` | AgentRun | 追加事件流 |
| `output.log` | AgentRun | Provider stdout/stderr 或 pane capture |
| `result.json` | AgentRun/Provider contract | 结构化最终结果 |
| `done` | tmux Provider | 空文件，tmux managed task 最终完成标记 |
| `native-snapshot.json` | native Provider | native loop snapshot |

tmux managed task 成功条件固定为：

1. `result.json` 已原子写入且可解析。
2. `run_id` 与请求一致。
3. result schema 校验通过。
4. `done` 存在且为空。

stdout/stderr、pane 静默、进程退出或单独一个文件都不构成成功。

## 5. HTTP 契约

HTTP 只暴露 run API：

- `GET /healthz`
- `POST /v1/runs`
- `GET /v1/runs/{type}/{id}/status|logs|result`
- `POST /v1/runs/{type}/{id}/cancel|block|stop|continue|patch-resume`

不存在独立 agent/session HTTP store。

## 6. 迁移记录

| 阶段 | 状态 | 完成内容 |
| --- | --- | --- |
| P0 | 完成 | 固化 managed/capture/API/tmux 与 artifact 基线测试 |
| P1 | 完成 | module 改为 `agent-runtime`；AgentRun 只面向 Provider |
| P2 | 完成 | 统一 executor、流式输出、进程组和 macOS 兼容 |
| P3 | 完成 | native Provider 吸收 agent-arch state/snapshot/loop；HTTP 改用 `/v1/runs` |
| P4 | 完成 | tmux task/session/command 统一到 `provider/tmux` |
| P5 | 完成 | daemon 接管长期进程、registry、depends、proxy/shim；取消清理竞态已覆盖 |
| P6 | 完成 | 统一 `cmd/sn-cli`、`cmd/runtime-server`；删除旧 runtime、REPL 和重复 store |

## 7. 验证矩阵

| 类别 | 命令 | 覆盖点 |
| --- | --- | --- |
| 全仓测试 | `go test ./...` | 所有 Go package |
| CLI 测试 | `make sn-cli-test` | AgentRun、Provider、executor、daemon、capability、HTTP、CLI |
| CLI 构建 | `make sn-cli-build` | `cmd/sn-cli` 唯一入口 |
| Server 构建 | `make build` | `cmd/runtime-server` |
| Profile | `./cmd/sn-cli-wrapper profiles` | JSON profile discovery |
| 配置 | `./cmd/sn-cli-wrapper config validate --profile fake` | schema 与环境 |
| Capture | `./cmd/sn-cli-wrapper fake "hello"` | API mock 与 artifacts |
| Native | `./cmd/sn-cli-wrapper native-fake "hello"` | native loop 与 snapshot |
| Daemon | `./cmd/sn-cli-wrapper doctor daemon --json` | UDS/version/process/dependency/proxy |
| 补丁质量 | `git diff --check` | 空白和补丁格式 |

真实 Codex、Claude、OpenAI、Anthropic smoke 需要对应命令或 secret，不作为无 secret 本地回归门槛。

## 8. 不变式

1. 只有 AgentRun 拥有 public lifecycle 和 artifacts。
2. 新执行方式通过新增 Provider 实现接入，不修改 AgentRun 公共语义。
3. daemon 只做长期进程和执行环境后端。
4. 普通 profile 不经过 proxy/shim/dylib 路径。
5. Provider 配置不保存 secret 或业务路由字段。
6. tmux 成功判定始终使用 `result.json + done`。
7. CLI、HTTP、native、tmux 产生同一套 run artifacts。

## 9. 完成标准

- [x] `sn-cli <profile>` 是唯一推荐终端入口。
- [x] `configs/<profile>.json` 是 Provider 配置事实源。
- [x] `agentrun` 是唯一 run/turn/task/session 语义层。
- [x] Provider 接口统一 CLI/API/tmux/native。
- [x] native Provider 吸收原 agent-arch 能力。
- [x] daemon 是唯一长期进程后端。
- [x] mz-cli 的 executor/daemon/proxy/shim/depends 能力进入 L1。
- [x] 所有入口使用同一套 artifacts。
- [x] README、代码目录和本文档只描述一个 runtime 架构。
