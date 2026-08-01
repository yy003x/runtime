# Runtime vNext

[![CI](https://github.com/yy003x/runtime/actions/workflows/ci.yml/badge.svg)](https://github.com/yy003x/runtime/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yy003x/runtime?include_prereleases&display_name=release)](https://github.com/yy003x/runtime/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/yy003x/runtime)](https://goreportcard.com/report/github.com/yy003x/runtime)
[![Go Version](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**一个本地优先的 Go 运行时，用于运行 AI Agent 与模型调用——把 CLI 包裹、模型 API、
durable 会话、长期 tmux 窗口、自主 agent 循环和后台队列统一在一个 CLI 与 HTTP API 之后。**

[English](README.md) · [简体中文](README.zh-CN.md)

---

## 这是什么

Runtime vNext 是面向 AI 编程 Agent 与模型调用的自托管执行层。与其把会话状态、重试、
取消和工具循环硬塞进临时脚本，它提供了一组边界清晰、可组合的入口：

- 用 typed Profile 包裹 **Codex / Claude** CLI，或直接调用**模型 API**
  （OpenAI-compatible 与 Anthropic-compatible driver）。
- 把多轮**会话**记录到 crash-consistent 的文件型存储。
- 保留一个长期可 attach 的交互式 **tmux** 窗口。
- 运行自主 **agent** 循环（model → tool → tool-result → model）。
- 把 **durable run** 提交到 SQLite 队列，通过 CLI 或 HTTP 控制。

一切都是**本地优先**的：数据落在 `${SN_CLI_HOME:-~/.sn}`（run 用 SQLite WAL，session
用普通文件）。契约**严格、fail-closed**——未知字段、schema 漂移和歧义的 crash 状态都会
被拒绝，而不是被悄悄抹平。

### 给谁用

- 想包裹 Codex/Claude CLI、并需要真正的 session/run 管理而非 shell 一行命令的开发者。
- 想要可脚本化、可持久化、可自托管、替代托管式 Agent 平台的团队。
- 任何在搭建 agent 工作流、需要可恢复 run、取消和干净 HTTP 控制面的人。

## 特性

- 🖥️ **本地优先**——run、会话与状态都留在本机 `~/.sn` 下。
- 🔌 **Provider 中立**——一个 canonical model 契约后接 OpenAI-compatible 与
  Anthropic-compatible driver。
- 🧱 **typed profile**——`type=cli|api` 路由到 Command Bridge 或 Model Core；单层配置，
  没有隐藏映射。
- 💾 **durable 且可恢复的 run**——SQLite WAL 队列，带 cancel / retry / resume / reconcile
  语义，能扛过进程退出。
- 🗂️ **crash-consistent 会话**——atomic、带 journal 的文件存储，恢复时做 identity 核对
  （不做启发式修复）。
- 🤖 **自主 agent 循环**——model + 受控 builtin tool（`read_file`、`list_directory`、
  可选 `write_file`），带预算限制与流式事件。
- 🪟 **长期 tmux 窗口**——专用 tmux server，可按稳定 ID start / send / attach /
  interrupt / stop。
- 🧪 **严格 JSON Schema 校验**——CLI 与 HTTP 用同一套规则；未知字段与歧义状态 fail closed。
- 🌐 **HTTP / SSE 控制面**——loopback `sn-server` 暴露完整的 Session / Run / Agent /
  Model API。

## 快速开始

### 安装

从 GitHub Release 一行安装（无需源码构建）：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
```

或从源码构建（需要 Go 1.25）：

```bash
git clone https://github.com/yy003x/runtime.git
cd runtime
make build sn-cli-build
make install
```

确认安装与生效的 Profile：

```bash
sn-cli --version
sn-cli profile list      # 列出 active profile 及其类型
sn-cli profile check     # 校验每个 profile 的结构
```

> **Profile 是用户自管的配置文件。** `~/.sn/configs/*.json` 下的内置 profile 是**可用示例**，
> 指向具体 provider 与模型（如 `api-cx` → 阿里百炼 qwen、`api-cc` → GLM）。要真正调用模型，
> 需通过引用的环境变量提供自己的 key（如 `ALIYUN_API_KEY`），或编辑 profile 换成你自己的
> endpoint 与 model。详见下文[配置](#配置)与 [sn-cli 详细使用手册](SN-CLI-USAGE.md)。

### 第一次调用

```bash
# 一次模型 API 调用（需先设置该 profile 的 auth 环境变量）
sn-cli api-cx "回复OK"

# 打开 Codex/Claude 交互 TUI
sn-cli cx

# 一次性执行 CLI 并等待退出
sn-cli cx --exec "总结当前仓库"
```

### 一次有记录的会话

```bash
# 执行一个有记录的 turn，之后跨轮次/provider 复用同一会话
sn-cli --json session run api-cx "第一轮"      # 从 JSON 取 session_id
sn-cli session run --session-id <session_id> api-cc "第二轮"
sn-cli session messages --session-id <session_id> # 读取历史
```

### 一个 durable 后台 run

`session submit` / `run submit` 只入队——必须有 worker 运行才会出队执行。先启动 server：

```bash
sn-cli --json server start
sn-cli --json session submit --task-id analysis --cwd "$PWD" cx-deep "后台执行"
sn-cli run watch --run-id <run_id>     # 流式追踪事件直到 settled
```

### 一次自主 agent 循环

```bash
sn-cli agent run --profile api-cx --max-wall-time 20m "审查当前仓库并给出结论"
```

完整命令、参数与端到端工作流见 [sn-cli 详细使用手册](SN-CLI-USAGE.md)。

## 核心概念

一个 CLI，多个执行边界。每个入口都有明确的作用范围与持久化目标：

```text
sn-cli <id> ──────────┐
sn-cli profile <id> ──┴─┬─ type=cli ─> Command Bridge ─> CLI process
                        └─ type=api ─> Model Core ─────> HTTP/SSE

sn-cli session ... ───> Session Service ──> command or model
sn-cli tmux ... ───────> Tmux Service ─────> interactive command window
sn-cli agent run ─────> Agent Kernel ─────> model + configured tools
sn-cli run ... ───────> Run Harness ──────> SQLite WAL
```

| 入口 | 作用 | 持久化 |
|---|---|---|
| `sn-cli <profile-id>` | 一次 CLI/API profile 调用（不记录） | 无 |
| `sn-cli profile <profile-id>` | 与隐式入口完全等价 | 无 |
| `sn-cli session run\|submit` | Session / Turn / Message / Event / Execution | 文件型 session |
| `sn-cli tmux ...` | 专用 tmux 交互窗口 | tmux registry（不存 transcript） |
| `sn-cli agent run` | API-only model/tool 循环 | durable run（session 可选） |
| `sn-cli run ...` | durable run 队列与控制面 | SQLite WAL |

几条最重要的边界：

- 一个 **profile** 要么是 `cli`（包裹 CLI），要么是 `api`（调用模型）。`type` 决定 adapter。
- **session 永不自动执行 tool call**——模型返回 tool call 时 turn 停在 `requires_action`。
  自主工具循环属于 `agent run`。
- **tmux 不创建 session**——它只管理一个交互窗口。
- **提交 run 不会自动启动 server**——入队与执行是解耦的。

精确契约（状态机、crash 恢复、digest/drift 门禁、文件系统安全模型）在
[契约文档](#文档)里；本 README 只停留在使用层面。

## 配置

Profile 是单层配置——每个 profile 一个 JSON 文件：

```text
<runtime-home>/configs/<profile-id>.json   # 源：configs/*.json
<runtime-home>/runtime.json                # 源：configs/runtime/runtime.json
<runtime-home>/resources/                  # JSON schema、tmux.conf、release.json
```

Profile ID 就是文件名去掉 `.json`。CLI profile 包裹一个命令；API profile 指向一个 provider：

```jsonc
// configs/cx.json — 包裹 Codex CLI 的 CLI profile
{
  "type": "cli",
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "xhigh",
  "env": { "CODEX_HOME": "${HOME}/.codex-aip" }
}

// configs/api-cx.json — 调用模型 endpoint 的 API profile
{
  "type": "api",
  "driver": "openai-compatible",
  "base_url": "https://your-provider/compatible-mode",
  "model": "your-model",
  "auth": { "header": "Authorization", "scheme": "Bearer", "from_env": "YOUR_API_KEY" },
  "defaults": { "max_tokens": 16384 },
  "timeout": "5m"
}
```

secret 只从环境变量读取（`auth.from_env`），绝不写入 profile 文件。`runtime.json` 配置
agent 的 builtin tool、预算、scheduler 与 run retention。完整字段、覆盖顺序与示例见
[sn-cli 详细使用手册](SN-CLI-USAGE.md)与[配置契约](docs/configuration.md)。

## 目录架构

```text
agent/       自主 model/tool 循环（Agent Kernel）
command/     CLI Command Bridge 领域
contract/    provider-neutral 的 request / event / error 契约
model/       单次模型调用 + API profile 领域
profile/     command/model catalog 门面
run/         durable run 应用领域（SQLite）
session/     本地 canonical session + context projection
tmux/        专用 tmux server / window 管理
provider/    openai/ + anthropic/ driver
store/sqlite/  run store adapter
transport/   http/（HTTP/SSE）adapter
internal/    cli adapter、runtime bootstrap、config loader、builtin tool
cmd/         sn-cli、sn-server 入口
configs/     源 CLI/API profile + runtime 模板
resources/   严格 JSON schema、tmux.conf、release.json
```

领域包不读 CLI 参数、不打开配置目录、不依赖 HTTP。`internal/runtimebootstrap` 是
composition root；provider、SQLite、CLI、HTTP 和 builtin tool 都是 adapter。

## 文档

| 文档 | 范围 |
|---|---|
| [sn-cli 详细使用手册](SN-CLI-USAGE.md) | 完整 CLI 命令、参数、场景与示例参考 |
| [Runtime vNext 契约](docs/runtime-vnext-contract.md) | 顶层契约与架构 |
| [CLI 路由契约](docs/cli-routing-contract.md) | 命令路由与 ID 规则 |
| [配置契约](docs/configuration.md) | profile / runtime 配置 schema |
| [Session 与 history 契约](docs/session-history-contract.md) | session 状态机与 crash 恢复 |
| [Tmux 管理契约](docs/tmux-contract.md) | tmux 窗口管理 |
| [集成架构](docs/integration-arch.md) | 调用方如何经 CLI / HTTP 集成 |

## 构建与验证

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make release-check SN_CLI_VERSION=v0.1.0
git diff --check
```

测试与 release check 只使用临时 `SN_CLI_HOME`，绝不修改 active `~/.sn`。全部目标见
[Makefile](Makefile)。

## 贡献

本仓库使用 Conventional Commits（中文主题），并保持契约/代码严格锁步：修改公开命令需
同步对应测试、`sn-cli --help`、本 README 与相关契约文档。开发流程、架构边界与验收门禁见
[`AGENTS.md`](AGENTS.md)。

## 许可证

基于 [Apache License, Version 2.0](LICENSE) 授权。

Copyright 2026 yangyang.
