# Agent Runtime 整合架构

## 1. 目标

Runtime 提供一套稳定的本地 Agent 执行底座：

- `sn-cli` 负责命令路由、Provider 调用、Session 与 Run 控制。
- `sn-server` 将同一运行能力暴露为 HTTP API。
- CLI/API Provider 使用统一的 request、status、events、logs、output 与 result 契约。
- Session 统一管理 CLI、API、tmux 和 terminal 的会话关系。
- daemon 管理异步队列、长期进程与本地 IPC。
- memory、skills 和 tools 通过 capability registry 装配。

## 2. 分层与 Owner

```text
┌─────────────────────────────────────────────────────────┐
│ Entry                                                   │
│ sn-cli · sn-server                                      │
└──────────────────────────┬──────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────┐
│ AgentRun                                                │
│ Session · Turn · RunAttempt · Execution · Loop          │
│ registry · queue · artifacts · result contract          │
└───────────────┬──────────────────────────┬──────────────┘
                │                          │
┌───────────────▼──────────────┐  ┌────────▼──────────────┐
│ Provider                     │  │ Capability            │
│ CLI · API · carrier adapter  │  │ memory · skill · tool │
└───────────────┬──────────────┘  └───────────────────────┘
                │
┌───────────────▼─────────────────────────────────────────┐
│ Executor · Daemon · Layout                              │
│ process · TTY · stream · signal · UDS · filesystem      │
└─────────────────────────────────────────────────────────┘
```

Owner 规则：

| 领域 | Owner |
| --- | --- |
| profile 解析、请求构建、协议适配 | `internal/provider` |
| Session/Turn/Run/Execution/Loop | `internal/agentrun` |
| argv、env、cwd、stdin、TTY、signal | `internal/executor` |
| 队列、UDS、长期进程 | `internal/daemon` |
| memory、skill、tool、workspace | `internal/capability` |
| home 与全部路径 | `internal/layout` |
| CLI 路由与呈现 | `internal/cli` |
| HTTP 传输 | `internal/transport` |

## 3. CLI 路由

第一个参数按以下顺序解析：

1. `-h|--help`、`--version`
2. 固定 namespace
3. `configs/<profile_id>.json`
4. `unknown command`

`-h|--help` 与 `--version` 在配置加载前处理，不创建 Runtime home。正式版本由构建提交上的 `vMAJOR.MINOR.PATCH` Git tag 注入；非 tag 构建显示 `v0.0.0-dev+<commit>`。不额外提供 `sn-cli version` 命令。

固定 namespace：

```text
run session profile system loop skill tool memory
```

公共命令最多两层：namespace + action。Provider profile 是顶层动态命令，也可作为 `profile exec`、`session run|submit|open` 的执行目标。

### 3.1 Direct

```bash
sn-cli cx [native-cli-args...]
sn-cli api-cx [typed-options] "prompt"
```

CLI direct：

- 最终 argv 为 `command + args + configured-effort + model + native-cli-args`。
- 继承当前 stdin/stdout/stderr 与 TTY。
- 生命周期由目标 CLI 管理。
- Runtime 不创建 Run 或 Session。

API direct：

- typed options 映射为 request payload。
- prompt 来自最后一个 positional 或 stdin。
- Runtime 直接打印 Provider 结果。
- Runtime 不创建 Run 或 Session。

### 3.2 Managed

```bash
sn-cli profile exec <profile> [provider-input...]
sn-cli session run <runtime-options...> <profile> [provider-input...]
sn-cli session submit <runtime-options...> <profile> [provider-input...]
```

managed CLI argv：

```text
command + args + configured-effort + model + managed-selector + provider-cli-args
```

- Codex selector 是 `exec`。
- Claude selector 是 `-p`。
- 通用 CLI 使用 stdin，不增加 selector。
- profile `args` 已提供 selector 时不重复增加。

`profile exec` 直接转发 stdout/stderr，不创建记录。`session run|submit` 创建结构化 Session 数据与 Run artifacts，并启用 `result.json` 契约。

### 3.3 Carrier

```bash
sn-cli session open --carrier tmux <profile> [native-cli-args...]
sn-cli session open --carrier terminal <profile> [native-cli-args...]
```

carrier 负责承载交互式 CLI：

- tmux 支持 attach、send、interrupt、stop 与重连。
- terminal 通过 `ghostty|iterm2` 创建独立窗口。
- carrier Execution 记录 transcript 和生命周期事件。
- Session、Run 与 Execution 使用独立 ID。

详细语法由 [CLI 路由契约](cli-routing-contract.md) 定义。

## 4. Provider

### 4.1 Profile schema

每个 `configs/<profile_id>.json` 定义一个 profile，ID 取文件名。

CLI 字段：

```text
command model effort args env timeout_seconds
```

API 字段：

```text
protocol base_url model api_key headers timeout_seconds
```

loader 使用严格 JSON 解码并拒绝未知字段。配置读取是只读操作。

### 4.2 CLI adapter

`command` basename 用于选择 `codex`、`claude` 或 `generic` adapter。adapter 负责：

- `effort` 到原生 argv 的映射。
- managed selector。
- prompt delivery。
- 支持的 request override。
- Provider 输出解析。

CLI direct 的尾参数原样透传。Session 的 Runtime options 位于 profile 前；Runtime 从 profile 后输入中提取本轮 prompt，其余 token 交给 CLI adapter。

### 4.3 API adapter

支持：

- OpenAI-compatible
- Anthropic-compatible

API CLI 入口支持：

```text
--model
--max-tokens
--temperature
--stream
--no-stream
```

`protocol`、`base_url`、`api_key` 和 `headers` 由 profile 固定。认证 header 根据 protocol 与 endpoint 生成。direct provider 与进程内 LLM client 共用同一个 endpoint resolver：`base_url` 末段没有 `vN` 时补 `/v1`，已有版本段或完整 endpoint 时不重复追加，再按 OpenAI/Anthropic 分别定位 `chat/completions` 或 `messages`。

### 4.4 环境

CLI 子进程先继承当前环境，再应用 profile `env`：

- string：展开后设置。
- `null`：删除变量。

完整 `${VAR}` 是唯一环境变量引用语法。引用未设置时返回错误。API 的 `api_key` 必须是完整环境变量引用，`headers` value 使用相同展开规则。

## 5. Session 与 Run

关系：

```text
Session
├── Turn
│   └── RunAttempt
│       └── Execution
└── carrier Execution
```

- Session 是跨 Provider 的逻辑会话。
- Turn 表示一轮 user/assistant 交互。
- RunAttempt 表示一次可重试运行。
- Execution 表示一次具体 Provider 或 carrier 执行。
- 一个 Session 可在不同 Turn 使用不同 profile。

### 5.1 记录入口

| 入口 | Session | Run artifact | result contract |
| --- | --- | --- | --- |
| `sn-cli <cli-profile>` | 无 | 无 | 无 |
| `sn-cli <api-profile>` | 无 | 无 | 无 |
| `profile exec` | 无 | 无 | 无 |
| `session run` | 有 | 有 | 有 |
| `session submit` | 有 | 有 | 有 |
| `session open` | 有 | 有 | transcript |
| `loop run` | 独立编排 | 有 | 有 |

### 5.2 Context

Session message 是跨 Provider 上下文的事实源。Runtime 根据规范化 user/assistant messages、Session memory、当前 prompt 与结果约束编译 Provider 输入。

Provider 切换只改变 adapter 与模型配置，不改变 Session/Turn/Execution 的关系。该结构可直接支持 GUI 会话列表、当前对话续轮、运行状态与跨模型上下文。

## 6. 目录与产物

```text
~/.sn/
├── configs/
├── resources/
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

Run：

```text
runs/<run_type>/<YYYY-MM-DD>/<run_id>/
├── request.json
├── status.json
├── events.jsonl
├── output.log
└── result.json
```

Session：

```text
sessions/<YYYY-MM-DD>/<session_id>/
├── session.json
├── messages.jsonl
├── events.jsonl
├── turns/
├── executions/
└── memory/
```

写入约束：

- `status.json` 和 `result.json` 原子替换。
- `events.jsonl`、`messages.jsonl` 与 `output.log` 只追加。
- Run registry 支持 list、active 过滤与 reconcile。
- Session lock 位于 `state/sessions/locks`。
- secret 不进入配置输出、日志或运行产物。

## 7. 安装与更新

release 包含：

```text
sn-cli
configs/
resources/
```

支持 darwin/linux 与 arm64/amd64。每个 archive 和 server binary 都进入 `checksums.txt`。

安装与 self-update：

1. 下载或读取本地 payload。
2. 校验 checksum、文件类型与目录边界。
3. 在临时 home 合并本地配置和发行包缺失项。
4. 使用 payload binary 执行 `profile list`。
5. 同步 active home。
6. 原子替换 binary。

默认同步只补缺失项。开发安装可通过 `--overwrite-configs` 更新发行包内同名 profile；本地额外 profile 与 resource 保持不变。

## 8. Daemon、HTTP 与 Capability

daemon 通过 UDS 提供状态、启动、停止和队列调度。异步 submit 写入持久队列，dispatcher 根据并发上限执行，启动时可恢复待处理任务。

HTTP 与 CLI 共用 `agentrun.Service`：

- `/v1/runs` 创建、查询、日志、结果与控制。
- `/v1/sessions` 创建、查询、消息、事件、watch 与续轮。
- `Prefer: respond-async` 提交异步 Run。
- `system doctor --json` 提供 `contract_version`、features 与 scheduler 健康状态。

Capability registry 统一装配：

- `resources/skills`
- `resources/tools`
- `memory`
- workspace

## 9. 不变式

1. `SN_CLI_HOME` 是 Runtime home 的唯一覆盖入口。
2. profile ID 来自 JSON 文件名。
3. profile loader 严格、只读。
4. CLI profile 后的原生参数保持 token 边界与顺序。
5. Runtime options 位于 Session profile 前。
6. 只有 Session 与 Loop 入口写 Runtime 记录。
7. `result.json` 是 managed Run 的结构化终态结果。
8. stdout 用于最终文本或 JSON；过程跟随输出使用 stderr。
9. Session、Run、Turn 与 Execution ID 各自表达独立实体。
10. 安装先验证新 binary 与完整配置，再替换 active binary。
11. config、日志、命令预览和 artifacts 不保存 secret。

## 10. 验证

```bash
make fmt-check
go test ./...
go vet ./...
make sn-cli-test
make test-serial
make test-race
make coverage COVERAGE_MIN=65.0
make release-check
```

`release-check` 校验 source、配置/资源目录、跨平台资产、checksum、安装、更新、doctor、profile validate、同步 Session Run、异步 submit、Run 查询与 reconcile。
