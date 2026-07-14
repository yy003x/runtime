# Agent Runtime

本仓库实现统一的 Go Agent Runtime。`sn-cli`、`sn-server`、CLI/API/tmux/native Provider、memory、skills、tools 和 daemon 共用同一套配置、生命周期与运行产物契约。

## 当前架构

```text
cmd/sn-cli                 终端入口、交互命令、AgentRun 控制面、自更新
cmd/sn-server              HTTP /v1/runs adapter
internal/agentrun          task/turn/loop/session/command 与 artifacts
internal/provider          CLI/API/tmux/native Provider
internal/executor          进程执行、流式输出、终端与信号
internal/daemon            UDS、tmux、depends、proxy/shim
internal/capability        memory、skills、tools、workspace
internal/layout            ~/.sn 唯一路径契约
internal/installbundle     release 解包、checksum、配置同步
```

- `agentrun` 是唯一公共 lifecycle 与 artifact owner。
- `Provider` 是 CLI、API、tmux、native 的唯一执行抽象。
- daemon 只管理长期进程和执行环境，不拥有 profile 或 run 状态。
- 本地生效配置只从 `~/.sn/configs` 读取。
- 仓库 `configs/` 只作为安装包中的配置模板源。

完整设计见 [docs/integration-arch.md](docs/integration-arch.md)。

## 安装

### 网络安装

网络安装下载 GitHub Release 中已编译的 binary、`configs/` 和 `checksums.txt`，不下载源码，也不要求 Go 或 Git：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
```

安装结果：

```text
~/.sn/bin/sn-cli
~/.local/bin/sn-cli -> ~/.sn/bin/sn-cli
~/.sn/configs/*
```

安装指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | \
  bash -s -- --version v1.0.0
```

### 本地源码安装

```bash
make install
```

兼容入口 `make sn-cli-install` 等价于 `make install`。源码只参与构建，安装后的 `sn-cli` 不依赖源码目录，源码移动或删除后仍可运行。

### 配置同步

首次安装、再次安装和 `sn-cli update` 都执行同一规则：

1. 递归复制发行包 `configs/` 中本地缺失的目录和文件。
2. 已存在的本地文件永不覆盖。
3. 发行包删除的模板不会删除本地文件。
4. 用户新增的本地文件会保留。
5. 同一路径发生文件/目录类型冲突时，安装在复制前失败并报告路径。

因此本地配置始终由用户拥有。新增模板可以自动补齐，模板更新不会改写本地配置。

## 本地目录

默认 runtime home 是 `~/.sn`，可通过 `SN_CLI_HOME` 覆盖：

```text
~/.sn/
├── bin/sn-cli
├── configs/
│   ├── runtime.yaml
│   ├── *.json
│   ├── personas/
│   ├── skills/
│   └── tools/
├── runs/
│   └── <task|turn|loop|session|command>/<YYYY-MM-DD>/<run_id>/
├── daemon/
│   ├── runtime.sock
│   ├── runtime.pid
│   ├── runtime.token
│   ├── processes.json
│   └── shims/
├── state/
│   ├── memory.json
│   ├── update.json
│   └── runs/
├── logs/daemon.log
├── cache/
└── tmp/
```

`configs/runtime.yaml` 只配置 `default_project`、`default_profile` 和 `max_concurrency`。所有路径由 `internal/layout` 固定，不接受配置文件改写。

## CLI 使用

### 交互命令

command CLI profile 默认启动原生交互程序，参数原样传给目标命令，不创建 managed run artifact：

```bash
sn-cli cx
sn-cli cx --help
sn-cli cx --version
sn-cli cc
```

`cx` 启动正常 Codex TUI，`cc` 启动正常 Claude Code。profile 的 common args 和 model 仍然生效，`managed_args` 不会进入交互命令。

### Managed prompt

需要 AgentRun lifecycle、结果文件和审计产物时，显式使用 `prompt` 或 `task run`：

```bash
sn-cli prompt -e cx "分析当前仓库"
printf '分析当前仓库' | sn-cli prompt -e cx

sn-cli task run --profile fake --mode capture "hello"
sn-cli task run --profile cx "处理任务"
```

API/native profile 仍通过 prompt 驱动：

```bash
sn-cli fake "hello"
sn-cli native-fake "hello"
```

### 生命周期

```bash
sn-cli profiles
sn-cli config choices
sn-cli config validate --profile fake
sn-cli doctor

sn-cli task status <run_id>
sn-cli task logs <run_id>
sn-cli task watch <run_id>
sn-cli task cancel <run_id>

sn-cli turn run --profile native-fake "继续任务"
sn-cli task block <run_id> --reason "等待输入"
sn-cli task continue <run_id>
sn-cli task patch-resume <run_id> --patch '{"operation":"append","messages":[{"role":"user","content":"继续"}]}'

sn-cli loop run --input "执行计划" --actions '[{"type":"respond","content":"完成"}]'

sn-cli session start --profile tcx
sn-cli session send <run_id> --text "继续"
sn-cli session attach <run_id>
sn-cli session stop <run_id>

sn-cli command start --profile tcx -- printf 'hello'
sn-cli command status <run_id>
sn-cli command stop <run_id>
```

### Capabilities

```bash
sn-cli capabilities skills list
sn-cli capabilities tools schemas
sn-cli capabilities memory write note-1 "runtime fact"
sn-cli capabilities memory recall runtime
```

默认路径分别是 `~/.sn/configs/skills`、`~/.sn/configs/tools` 和 `~/.sn/state/memory.json`。

### Daemon

```bash
sn-cli doctor daemon --json
sn-cli daemon start
sn-cli daemon status
sn-cli daemon restart
sn-cli daemon stop
```

daemon 使用 owner-only socket 和 token，日志写入 `~/.sn/logs/daemon.log`。

### 更新

```bash
sn-cli update --check
sn-cli update --dry-run
sn-cli update
sn-cli update --version v1.0.0
```

更新从 GitHub Release 下载当前平台 archive 并校验 SHA256。新 binary 会先使用“本地配置 + 新增模板”的临时合并配置完成验证，再同步缺失配置并原子替换 `~/.sn/bin/sn-cli`。失败时保留旧 binary。

## Provider

支持：

- `type=cli`、`executor=command|tmux`：Codex、Claude 和 generic CLI。
- `type=api`：OpenAI-compatible、Anthropic-compatible 和 mock。
- `type=native`：进程内多轮 agent loop、snapshot、block、continue、patch-resume、stop、cancel。

command profile 可区分 common args 与 managed-only args：

```json
{
  "type": "cli",
  "cli": {
    "driver": "codex",
    "executor": "command",
    "command": {"binary": "codex", "args": ["--search"], "model": "gpt-5.6-sol"},
    "runtime": {
      "prompt_delivery": "stdin",
      "managed_args": ["exec"],
      "result_contract": "required"
    }
  }
}
```

`depends`、audit proxy、PATH shim 和 DYLD 注入按 profile 显式启用。secret 只能引用环境变量名，不应写入配置、日志或 result。

## 运行产物

managed run 位于：

```text
~/.sn/runs/<task|turn|session|command>/<YYYY-MM-DD>/<run_id>/
~/.sn/runs/loop/<YYYY-MM-DD>/<loop_id>/
```

标准文件包括 `request.json`、`status.json`、`events.jsonl`、`output.log` 和 `result.json`。tmux managed task 还使用空 `done` 文件，native Provider 使用 `native-snapshot.json`。

tmux managed task 只有在合法 `result.json` 和空 `done` 同时存在时才算完成。stdout、pane 静默或单独完成标记都不能替代该契约。

## HTTP Server

```bash
make install
make run
```

或构建后直接运行：

```bash
make build
./bin/sn-server
```

默认监听 `:8080`，可通过 `HTTP_ADDR` 修改。server 与 CLI 读取同一个 `SN_CLI_HOME`：

- `GET /healthz`
- `POST /v1/runs`
- `GET /v1/runs/{run_type}/{run_id}/status|logs|result`
- `POST /v1/runs/{run_type}/{run_id}/cancel|block|stop|continue|patch-resume`

## 构建与验证

```bash
make sn-cli-build
make build
make release

go test ./...
go vet ./...
make sn-cli-test
git diff --check
```

`make release` 生成 darwin/linux、arm64/amd64 的 `sn-cli-<os>-<arch>.tar.gz`、`sn-server-<os>-<arch>` 和 `checksums.txt`。推送 `v*` tag 后，GitHub Actions 执行测试并发布这些资产。
