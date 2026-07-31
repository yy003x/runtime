# Runtime vNext

Runtime vNext 是一个本地优先的 Go Agent Runtime。它把一次命令/模型调用、
canonical Session、长期 Tmux TUI、Agent loop 和 durable Run 分成独立边界。

完整命令、参数、场景和示例请直接阅读
[sn-cli 详细使用手册](SN-CLI-USAGE.md)。

```text
sn-cli <id> ──────────┐
sn-cli profile <id> ──┴─┬─ type=cli ─> Command Bridge ─> CLI process
                        └─ type=api ─> Model Core ─────> HTTP/SSE

sn-cli session ... ───> Session Service ──> command or model
sn-cli tmux ... ───────> Tmux Service ─────> interactive command window
sn-cli agent run ─────> Agent Kernel ─────> model + configured tools
sn-cli run ... ───────> Run Harness ──────> SQLite WAL
```

## 快速开始

构建并覆盖安装当前源码：

```bash
make build sn-cli-build
make install
```

确认 active Profile：

```bash
sn-cli --version
sn-cli profile list
sn-cli profile show cx-remote
sn-cli profile check
```

常用调用：

```bash
sn-cli cx
sn-cli cx --exec --effort high "分析当前仓库"
sn-cli api-cx "只调用一次模型"
sn-cli session run api-cx "保留这次会话"
sn-cli tmux start cx "打开长期交互窗口"
sn-cli agent run --profile api-cx "完成这个任务"
```

要让后台队列实际出队执行，需要启动 `sn-server`：

```bash
sn-cli --json server start
sn-cli session submit --task-id lark-remote --cwd "$PWD" cx-remote "执行远程任务"
sn-cli run list --state queued
```

管理命令需要稳定 machine output 时，把 `--json` 放在整个命令的第一项：

```bash
sn-cli --json server status
```

## 执行边界

| 入口 | 作用 | 持久化 |
|---|---|---|
| `sn-cli <profile-id>` | 一次 CLI/API Profile 调用 | 无 |
| `sn-cli profile <profile-id>` | 与隐式 Profile 完全等价 | 无 |
| `sn-cli session run|submit` | Session、Turn、Message、Event、Execution | 文件型 Session |
| `sn-cli tmux ...` | 专用 Tmux interactive window | Tmux registry，不保存 transcript |
| `sn-cli agent run` | API-only model/tool loop | Durable Run，Session 可选 |
| `sn-cli run ...` | Durable Run 队列和控制面 | SQLite WAL |

Profile 的 `type=cli|api` 决定 command adapter 或 Provider adapter。Session
不自动执行 tool call；Tmux 不创建 Session；`session submit` 和 `run submit`
不会自动启动 server。Session 内部保留 `requires_action` projection，但 stock
CLI/HTTP 不发布 tool-result 写入口。

Agent tool effect 的结果无法确认时，Run 进入 `needs_reconciliation`，不自动
重放。显式 `run reconcile` 保留 effect evidence 并以 failed 收口；如果 Agent
绑定了 Session，该 Session 在收口前保持 blocked。Agent `paused` 只通过
`run resume` 恢复。Tool 参数和 Resume input 使用完整 JSON Schema 校验；
Resume body 是携带 `pause_id` 与 `input` 的 strict envelope。Agent checkpoint
使用 LoopState schema 2 的 `base_message_count` 重建完整 model/tool/resume
message journal，并核对 durable model request/result digest、usage 与 budget；
任何无 durable evidence 或不在 provider-safe closed prefix 内的消息都会 fail
closed。

Agent request 不接受 per-Run `cwd`；tool workspace 来自 `runtime.json`。
pause/resume 是 Kernel extension，底层 `run resume` CLI/API 保留，但 stock
capability 不宣称默认工具会产生 Pause。builtin 只提供 `read_file`、
`list_directory` 和显式启用的 `write_file`，不提供不具备 OS sandbox 的
`exec_command`。server-owned running Run 通过 SQLite cancellation polling 接收
独立进程发出的 `run cancel`。

Durable Agent 在创建 Run 前冻结 private non-secret execution snapshot，绑定
Agent contract、完整 API Profile、Provider driver semantic identity、tool
implementation/config/definitions，以及可选的独立 Session digests。公开 Run 只
暴露 combined `request_digest/config_digest`；resolved `auth.from_env` value 不
持久化且允许在同一变量名下轮换。每个新的 model/tool side effect 都要求 current
loaded snapshot 匹配；已持久化 terminal/effect 的恢复、cancel 和 reconcile 不依赖
current Profile、Provider 或 tool 仍存在。

不存在 `profile exec|open`、`launch/transport/prompt_delivery`、无 `type` 的旧
Profile reader 或 Tmux/Session compatibility shim。

## 目录架构

源码按领域和依赖方向组织：

```text
agent/                  Agent Kernel 领域
command/                CLI Command Bridge 领域
contract/               Provider-neutral request/event/error 契约
model/                  一次模型调用与 profile 领域
profile/                command/model catalog 门面
run/                    durable Run application domain
session/                本地会话与 context projection
tmux/                   专用 tmux server/window 管理
provider/
  openai/               OpenAI-compatible driver
  anthropic/            Anthropic-compatible driver
store/sqlite/           Run Store adapter
transport/
  http/                 HTTP/SSE adapter
internal/
  cli/                  只负责 decode/call/encode 的 CLI adapter
  runtimebootstrap/     dependency wiring
  runtimeconfig/        runtime.json loader
  toolbuiltin/          受 root 约束的内置工具 adapter
cmd/
  sn-cli/
  sn-server/
configs/
  *.json                source CLI/API Profile
  runtime/runtime.json  source runtime template
resources/
  schema/               严格 JSON Schema
  tmux.conf             固定 tmux bootstrap
  release.json          activation/schema epoch
runtimetest/            faux provider、PTY、golden、scenario
```

领域包不读取 CLI 参数、不打开配置目录，也不依赖 HTTP。`internal/runtimebootstrap`
是 composition root；Provider、SQLite、CLI、HTTP 和 builtin tool 都是 adapter。

## Runtime Home

默认 `${SN_CLI_HOME:-~/.sn}`：

```text
bin/
  sn-cli
  sn-server
configs/
resources/schema/
resources/tmux.conf
resources/release.json
runtime.json
sessions/
state/
  session-locks/
  session-invocations/
  session-mutations/
  session-trash-moves/
  runtime.db
  sn-server.pid
  sn-server.log
  sn-server.lease.lock
  sn-server.lifecycle.lock
  runtime.maintenance.lock
  tmux.lock
  update.json
tmp/
```

`sessions/` 保存可读、可重建的会话事实；`state/session-mutations/` 保存 private
`mutation_version=3` undo journal；`state/session-trash-moves/` 保存
`version=1` 的 delete/GC rename journal；`state/session-invocations/` 保存 managed
CLI helper 消费即删除的私有 invocation manifest。Session 文件操作使用
directory-FD relative、no-follow 和 device/inode identity 门禁；JSONL 追加通过
atomic full-file rewrite 发布，prepared rollback 只有在当前文件仍属于该 mutation，
且原始 `size + prefix_digest` 匹配时才恢复原前缀。新 Session root 通过随机 nonce
owner marker 与 journal 绑定。这些 private recovery/transport 文件不属于
canonical Session fact，canonical Session `schema_version` 仍为 2。

`state/runtime.db` 是 durable Run 的唯一事实源。调用方不应直接读取或改写
`sessions/` 或上述 `state/` 内容，应使用公开 CLI/HTTP。

## 构建与安装

```bash
make check
make build sn-cli-build
make V=1 sn-cli-build
make install
```

`make install` 是本地源码调试的覆盖入口：校验 candidate，停止受管
`sn-server`，用当前源码的 Profiles、runtime config 和 resources 更新 active
home，并清理旧 `sessions/`、`state/session-locks/`、
`state/session-invocations/`、`state/session-mutations/`、
`state/session-trash-moves/` 和 `state/runtime.db*`；成功后不自动重启 server。
安装前必须先停止全部由 `sn-cli tmux` 管理的 live window；preflight 发现仍在运行
的 managed Tmux 时会拒绝安装，且不会自动关闭交互窗口。

测试和 release check 只使用临时 `SN_CLI_HOME`，不修改 active `~/.sn`。临时安装、
network/archive installer、升级 schema 和 activation 安全规则见
[配置契约](docs/configuration.md)与
[Runtime vNext 契约](docs/runtime-vnext-contract.md)。

## 配置入口

Source：

```text
configs/*.json
configs/runtime/runtime.json
resources/
```

Active：

```text
${SN_CLI_HOME:-~/.sn}/configs/*.json
${SN_CLI_HOME:-~/.sn}/runtime.json
${SN_CLI_HOME:-~/.sn}/resources/
```

`configs/*.json` 是唯一 Profile 配置层，必须通过 `type=cli|api` 显式分流。
Profile ID 来自文件名；不存在额外 command shortcut 或 `commands/*.json`。
参数、字段、覆盖顺序和示例见
[sn-cli 详细使用手册](SN-CLI-USAGE.md)与
[配置契约](docs/configuration.md)。

## 文档

- [sn-cli 详细使用手册](SN-CLI-USAGE.md)
- [Runtime vNext 契约](docs/runtime-vnext-contract.md)
- [CLI 路由契约](docs/cli-routing-contract.md)
- [配置契约](docs/configuration.md)
- [Session 与 history 契约](docs/session-history-contract.md)
- [Tmux 管理契约](docs/tmux-contract.md)
- [集成架构](docs/integration-arch.md)

## 验证

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make make-step-contract-test
make release-check SN_CLI_VERSION=v0.1.0
git diff --check
```
