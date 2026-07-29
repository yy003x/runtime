# Runtime vNext

Runtime vNext 是一个本地优先的 Go Agent Runtime。它把一次命令/模型调用、
canonical Session、长期 Tmux TUI、Agent loop 和 durable Run 分成独立边界。

```text
sn-cli profile <id> ──┬─ command ─> Command Bridge ─> CLI process
                      └─ model ───> Model Core ─────> HTTP/SSE

sn-cli session ... ───> Session Service ──> command or model
sn-cli tmux ... ───────> Tmux Service ─────> interactive command window
sn-cli agent run ─────> Agent Kernel ─────> model + configured tools
sn-cli run ... ───────> Run Harness ──────> SQLite WAL
```

## 执行语义

- `sn-cli <command-id> [args...]`：顶层 shortcut。使用 Profile 的 typed 固定配置和
  `exec` mode，但调用方 native args 不由 Runtime 解析；保留原生 stdin/stdout/
  stderr、signal、exit code 和 argv 兼容。
- `sn-cli profile <profile-id> [typed-options] [input]`：一次性调用。
  CLI Profile 支持 `--model`、`--effort`、`--prompt`、`--exec[=true|false]` 和
  `--cwd`；Runtime 通过 command adapter 生成确定 argv 后 process replacement。
  API Profile 仍发起一次 HTTP model call。两者都不创建 Session 或 durable Run。
- `sn-cli session run|submit ...`：维护本地 Session、Turn、Message、Event 和
  Execution。API history 投影为结构化 `messages[]`；CLI history 由 Session 自己
  投影，并固定在 managed subprocess 中以 `exec=true` 捕获机器协议、exit 和
  stdout/stderr 事实。Session 遇到 canonical tool call 只返回
  `requires_action`，不执行工具。
- `sn-cli tmux start|list|show|send|attach|interrupt|stop`：只管理专用 tmux
  server 的 interactive window，固定 `exec=false`。它不创建 Runtime Session，
  也不把 pane transcript 或 paste 内容写入 Session history。
- `sn-cli agent run --profile <model-id> ...`：唯一的 Agent harness，循环执行
  model/tool/tool-result，只接受 API model profile。只有显式 `--session-id` 才
  同步投影到 Runtime Session。
- `sn-cli run ...`：durable Run 的队列、事件、checkpoint、pause/resume/cancel、
  retry 和 reconciliation 控制面，不是第四种执行语义。

不存在 `profile exec`、`profile open`、`launch/transport/prompt_delivery`、无
`type` 的旧 Profile reader 或 Tmux/Session 兼容 shim。CLI adapter 只按 `command`
basename 选择；未登记 command 明确失败。

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
  cli/                  CLI event encoder
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
  commands/             source 顶层子命令映射
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
commands/
resources/schema/
resources/tmux.conf
resources/release.json
runtime.json
sessions/
state/
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

正式 `make install` 会写入 `${SN_CLI_HOME:-~/.sn}`，应由用户显式执行。它是本地
源码调试的固定覆盖入口：先校验当前 checkout 的完整 candidate，再安全停止受管
`sn-server`，用 source profiles、commands、runtime config 和 resources 替换
active 内容，并丢弃旧 `sessions/`、Session 私有状态和
`state/runtime.db*`。成功后不会自动重启 server。本仓测试和 release check 只在
临时 `SN_CLI_HOME` 验证该流程，不修改 active `~/.sn`。

普通 network/archive installer 默认保留已符合当前 schema 的 active configs；旧字段会在任何
binary/resource 替换前失败。确认已备份后可用 `--overwrite-configs` 显式替换。
`--install-dir` 必须位于 Runtime home 外；home 与 install-dir 都按最近已存在祖先
解析 canonical path。为避免 case-insensitive filesystem 在创建前无法判定的
Unicode alias，尚不存在的路径组件只接受 printable ASCII；已存在的 Unicode 父目录
可以正常使用。命令链接只接受已经精确指向该 home 的 symlink，或在激活后以
no-clobber 方式创建，且必须位于 home 外，绝不覆盖已有文件、目录或其它 symlink。
激活由 staged candidate 在 maintenance/server/Tmux lock 下完成：journal 先落盘，
state guard 与短生命周期的 `bin/configs` 双 barrier 阻止新旧 binary 并发写入；
journal 本身也是入口 barrier；只有完整提交或完整回滚、校验 digest 并 durable
清理 journal 后才解除。

## 常用入口

```bash
sn-cli profile list
sn-cli --json profile list
sn-cli profile show cx
sn-cli profile check

sn-cli commit "为当前改动生成提交计划"
sn-cli profile commit --effort high "只执行一次提交规划"
sn-cli profile api-cx "只调用一次模型"
sn-cli session run api-cx "保留这次会话"
sn-cli session submit cx-deep "后台执行并记录"
sn-cli tmux start cx "打开长期交互窗口"
sn-cli tmux list
sn-cli tmux attach --tmux-id <tmux_id>
sn-cli agent run --profile api-cx "完成这个任务"

sn-cli run list --state queued
sn-cli run watch --run-id <run_id>
sn-cli run gc
sn-cli server start
sn-cli --json server start
sn-cli server status
sn-cli server stop
sn-cli server upgrade-check
```

管理命令默认输出面向人的紧凑文本；需要稳定机器结果时，把全局 `--json` 放在
namespace 前，例如 `sn-cli --json server status`。`--json` 不会从 namespace
后的参数中截获，因此 `sn-cli cx --json` 仍把该参数原样交给目标 CLI。顶层
shortcut 与 CLI Profile 始终继承目标进程的原生输出，即使写了 leading global
`--json` 也不伪装成 Runtime JSON；`tmux attach` 是 human-only。
`server start` 首次启动和已运行的幂等分支都会返回相同的正整数 `pid`；第三方托管
应使用 `sn-cli --json server start` 读取该 PID。

顶层 `sn-cli <command-id>` 只从 active `commands/` 加载映射，再到
`configs/` 解析对应 Profile。source 默认提供 `commit`、`cx`、`cc` 和 `cx-*`；
其中 `cx|cc|cx-*` 是硬兼容入口。shortcut 的调用方参数保持 native；显式
`profile <id>` 则只接受 Runtime typed option 和最终 input，由 Codex/Claude adapter
处理 model、effort、mode selector 与 command/subcommand 参数顺序。

API token limit 使用 Driver 对应的协议名：OpenAI-compatible Chat Completions
使用 `--max-completion-tokens`，Anthropic-compatible 使用 `--max-tokens`。
Session 的调用顺序是 `session run [options] <profile-id> <input>`。
`--prompt-file`、`--terminal-driver`、`--command-arg` 以及
`session send|attach|interrupt|stop` 已移除；交互进程统一由独立
`sn-cli tmux` 管理。

本次 schema epoch 不读取旧 `binary/transport/prompt_delivery` Profile、Session
fact schema=1 或 SQLite user_version=1。需要保留数据时，先用旧版本导出/停服，
将 `sessions/` 与 `state/runtime.db` 整体移动到可恢复备份，再使用普通
`install.sh`；本地源码调试可直接执行 destructive `make install` 丢弃旧数据。
legacy v0.1.1 self-updater 会在替换前被 activation gate 拒绝。

## 文档

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
