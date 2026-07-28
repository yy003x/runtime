# Runtime vNext

Runtime vNext 是一个本地优先的 Go Agent Runtime。它用一个 `sn-cli` 明确提供
三种执行语义，并把一次命令调用、一次模型调用、会话延续、Agent loop 和 durable
Run 分成独立边界。

```text
sn-cli profile <id> ──┬─ command ─> Command Bridge ─> CLI process
                      └─ model ───> Model Core ─────> HTTP/SSE

sn-cli session ... ───> Session Service ──> command or model
sn-cli agent run ─────> Agent Kernel ─────> model + configured tools
sn-cli run ... ───────> Run Harness ──────> SQLite WAL
```

## 执行语义

- `sn-cli <command-id> [args...]`：`commands/*.json` 声明的顶层快捷入口，只能
  映射 CLI/TTY Profile；Profile 固定参数后直接追加调用方 native args，并保留
  原生 TTY、stdio、signal、exit code 和 argv 顺序。
- `sn-cli profile <profile-id> [--effort <level>] [input]`：一次性调用。
  `type=cli` 启动一次 CLI，`type=api` 发起一次 API 请求；不创建 Session 或
  durable Run，也不处理 tool loop。`--effort` 是 Runtime typed override，
  只有显式声明 `effort_adapter` 的 CLI Profile 才接受。
- `sn-cli session run|submit ...`：维护本地 Session、Turn、Message、Event 和
  Execution。model history 投影为结构化 `messages[]`；command history 投影为
  有边界的文本。Session 遇到 tool call 只返回 `requires_action`，不执行工具。
- `sn-cli agent run --profile <model-id> ...`：唯一的 Agent harness，循环执行
  model/tool/tool-result，只接受 API model profile。只有显式 `--session-id` 才
  同步投影到 Runtime Session。
- `sn-cli run ...`：durable Run 的队列、事件、checkpoint、pause/resume/cancel、
  retry 和 reconciliation 控制面，不是第四种执行语义。

不存在 `profile exec`、`profile open`、无 `type` 的旧 Profile reader 或根据
binary 名称推断 Codex/Claude 参数的兼容层。

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
provider/
  openai/               OpenAI-compatible driver
  anthropic/            Anthropic-compatible driver
store/sqlite/           Run Store adapter
transport/
  cli/                  CLI event encoder
  http/                 HTTP/SSE adapter
internal/
  cli/                  composition root 的 CLI adapter
  runtimebootstrap/     dependency wiring
  runtimeconfig/        runtime.json loader
  toolbuiltin/          受 root 约束的内置工具 adapter
cmd/
  sn-cli/
  sn-server/
configs/
  *.json                source CLI/API Profile
  commands/             source 顶层子命令映射
  runtime/runtime.json  source runtime template
resources/schema/       严格 JSON Schema
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
commands/
resources/schema/
runtime.json
sessions/
state/
  runtime.db
  sn-server.pid
  sn-server.log
  sn-server.lease.lock
  sn-server.lifecycle.lock
  update.json
tmp/
```

`sessions/` 保存可读、可重建的会话事实；`state/runtime.db` 是 durable Run 的唯一
事实源。调用方不应直接读取或改写这些文件，应使用公开 CLI/HTTP。

## 构建与临时安装

```bash
make check
make build sn-cli-build
make V=1 sn-cli-build

runtime_home="$(mktemp -d)"
install_dir="$(mktemp -d)"
bash install.sh \
  --binary ./bin/sn-cli \
  --server ./bin/sn-server \
  --configs ./configs \
  --commands ./configs/commands \
  --runtime-config ./configs/runtime/runtime.json \
  --resources ./resources \
  --home "$runtime_home" \
  --install-dir "$install_dir"
```

Make 默认只报告 stage、state、result、elapsed 和关键路径；有限任务成功时隐藏
底层噪声，失败时完整回放。`V=1` 显示安全转义后的真实 argv 并实时透传子命令
输出；`run`、`dev`、安装及 release/check/publish 编排入口实时输出。

正式 `make install` 会写入 `${SN_CLI_HOME:-~/.sn}`，应由用户显式执行。本仓实现
不会在测试或 release check 中修改 active `~/.sn`。

## 常用入口

```bash
sn-cli profile list
sn-cli --json profile list
sn-cli profile show cx
sn-cli profile check

sn-cli commit "为当前改动生成提交计划"
sn-cli profile commit "只执行一次提交规划"
sn-cli profile api-cx "只调用一次模型"
sn-cli session run api-cx "保留这次会话"
sn-cli session submit cx-deep "后台执行并记录"
sn-cli agent run --profile api-cx "完成这个任务"

sn-cli run list --state queued
sn-cli run watch --run-id <run_id>
sn-cli run gc
sn-cli server start
sn-cli server status
sn-cli server stop
```

管理命令默认输出面向人的紧凑文本；需要稳定机器结果时，把全局 `--json` 放在
namespace 前，例如 `sn-cli --json server status`。`--json` 不会从 namespace
后的参数中截获，因此 `sn-cli cx --json` 仍把该参数原样交给目标 CLI。顶层
shortcut 与 TTY CLI Profile 始终继承目标进程的原生输出，即使写了 leading
global `--json` 也不伪装成 Runtime JSON；交互式 `session attach` 明确不支持
machine mode。

顶层 `sn-cli <command-id>` 只从 active `commands/` 加载映射，再到
`configs/` 解析对应 Profile。source 默认提供 `commit`、`cx`、`cc` 和 `cx-*`；
其中 `cx|cc|cx-*` 是硬兼容入口。CLI Profile 中的 `exec`、`-p`、`-m`、`-c`、
`--skip-git-repo-check` 等全部是普通 argv token，Runtime 不理解也不补写。

API token limit 使用 Driver 对应的协议名：OpenAI-compatible Chat Completions
使用 `--max-completion-tokens`，Anthropic-compatible 使用 `--max-tokens`。
Session 的调用顺序是 `session run [options] <profile-id> <input>`。tmux 由
`transport=tmux` 的独立 Profile 启动，当前 carrier 用 `session send|attach`
继续和控制；Runtime 只记录 `transcript_only`，不会伪造结构化 assistant 输出。

## 文档

- [Runtime vNext 契约](docs/runtime-vnext-contract.md)
- [CLI 路由契约](docs/cli-routing-contract.md)
- [配置契约](docs/configuration.md)
- [Session 与 history 契约](docs/session-history-contract.md)
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
