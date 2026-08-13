# SN Runtime

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

SN Runtime 是面向 AI 编程 Agent 与模型调用的自托管执行层。与其把会话状态、重试、
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
- 🤖 **自主 agent 循环**——model + 受控 builtin/MCP tool（默认包含 `web_search`、
  `web_fetch`），带预算限制、durable effect 与流式事件。
- 🪟 **长期 tmux 窗口**——支持默认或隔离 tmux server，可按稳定 ID open / send /
  attach / interrupt / stop。
- 🧪 **严格 JSON Schema 校验**——CLI 与 HTTP 用同一套规则；未知字段与歧义状态 fail closed。
- 🌐 **HTTP / SSE 控制面**——loopback `sn-server` 暴露完整的 Session / Run / Agent /
  Model API，并提供 `/healthz` 与感知执行面的 `/readyz` 探针。

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

源码 `make install` 会先关闭全部 Session-bound native TUI，再停止受管
`sn-server`；安装后不重启 server。任何剩余 tmux carrier 或仍在运行的 Runtime binary
process 都会阻止安装。

确认安装与生效的 Profile：

```bash
sn-cli --version
sn-cli help session      # 查看某个公开命令主题的详细帮助
sn-cli doctor            # 检查 Profile、Tool、Run Store、日志和 tmux
sn-cli profile list      # 列出 active profile 及其类型
sn-cli profile check     # 校验每个 profile 的结构
```

> **Profile 是用户自管的配置文件。** `~/.sn/configs/*.json` 下的内置 profile 是**可用示例**，
> 指向具体 provider 与模型（如 `api-cx` → 阿里百炼 qwen、`api-cc` → GLM）。要真正调用模型，
> 需通过引用的环境变量提供自己的 key（如 `ALIYUN_API_KEY`），或编辑 profile 换成你自己的
> endpoint 与 model。详见下文[配置](#配置)与 [sn-cli 详细使用手册](SN-CLI-USAGE.md)。

### 第一次调用

```bash
# 一次模型 API 调用（需先设置该 profile 引用的环境变量）
sn-cli req api-cx "回复OK"

# 打开 Codex/Claude 交互 TUI
sn-cli cx

# 一次性执行 CLI 并等待退出
sn-cli exec cx "总结当前仓库"
```

### 一次有记录的会话

```bash
# 执行一个有记录的 request，之后跨轮次/API profile 复用同一会话
sn-cli --json session req api-cx "第一轮"      # 从 JSON 取 session_id
sn-cli session req api-cc --session-id <session_id> "第二轮"
sn-cli session messages --session-id <session_id> # 读取历史
```

### 一个 durable 后台 run

`--queue` 只入队——必须有 worker 运行才会出队执行。先启动 server：

```bash
sn-cli --json server start
sn-cli --json session exec cx-deep --queue --task-id analysis --cwd "$PWD" "后台执行"
sn-cli run watch --run-id <run_id>     # 流式追踪事件直到 settled
```

### 一个 tmux-backed Provider 原生 TUI Session

```bash
sn-cli --json session open cx --cwd "$PWD" "分析当前仓库"
sn-cli session send --session-id <session_id> "继续下一步"
sn-cli session attach --session-id <session_id>
sn-cli session close --session-id <session_id>
sn-cli session close-all
```

`session open` 直接在 tmux PTY 中启动 CLI Profile 的 Provider 原生交互模式，发布
`interface=native_tui` Session fact，并保存 opaque tmux binding。默认 detached；需要
立即进入界面时加 `--attach`。`session send` 把 raw input 注入 TUI，`accepted=true`
只表示 tmux 接受了传输操作。`session open` 同时创建一个 running 的
`kind=native_tui` Durable Run 和一个 opaque lifecycle Execution；Provider 退出时先
settle Run，再自动关闭 tmux window；`session close` 则先 settle 为 `cancelled`，再停止
window，向 Provider 转发终止信号，有限宽限期后强制退出，并等待 supervisor 退出后再
返回。TUI 输入输出仍由 Provider 管理，不创建 canonical Turn、Message、Event 或
transcript。

发现 native TUI Session 并查看 lifecycle：

```bash
sn-cli --json session list --interface native_tui
sn-cli --json session show --session-id <session_id>
```

tmux server/window registry 现在只作为私有 PTY carrier，不再提供 public
`sn-cli tmux` namespace 或兼容 alias。所有 mutation 使用 Runtime Session ID；
`session close-all` 只关闭 Session-bound native TUI window，不删除 Session fact。

### 一次自主 agent 循环

```bash
sn-cli agent api-cc \
  "查找 Codex CLI 最新版本，并阅读官方发布页面后总结主要更新。"
```

完整命令、参数与端到端工作流见 [sn-cli 详细使用手册](SN-CLI-USAGE.md)。

## 核心概念

一个 CLI，多个执行边界。每个入口都有明确的作用范围与持久化目标：

```text
sn-cli <cli-id> ────────> Command Bridge ─> 交互 CLI process
sn-cli exec <cli-id> ───> Command Bridge ─> 一次性 CLI process
sn-cli req <api-id> ────> Model Core ─────> 一次 HTTP/SSE request

sn-cli session exec|req ─> Session Service ─> command or model
sn-cli session open ... ─> native_tui Session ─> tmux PTY 中的 Provider TUI
sn-cli agent <api-id> ─> Agent Kernel ─────> model + configured tools
sn-cli run ... ────────> Run Harness ──────> SQLite WAL 控制面
```

| 入口 | 作用 | 持久化 |
|---|---|---|
| `sn-cli <cli-profile-id>` | CLI 交互 direct 调用 | 本地 `cli.jsonl`；无 Session/Run |
| `sn-cli exec <cli-profile-id>` | CLI 非交互一次执行 | 本地 `cli.jsonl`；无 Session/Run |
| `sn-cli req <api-profile-id>` | 一次 API request | 本地 `api.jsonl`；无 Session/Run |
| `sn-cli session exec\|req <profile-id> [--queue]` | Session / Turn / Message / Event / Execution | 文件型 session；本地执行日志；可选 durable run |
| `sn-cli session open\|send\|attach\|interrupt\|close\|close-all` | 带 Runtime identity 的 Provider 原生 TUI | `interface=native_tui` Session + opaque lifecycle Run/Execution 与 tmux binding；无 canonical transcript |
| `sn-cli agent <api-profile-id> [--queue]` | API-only model/tool 循环 | durable run；每轮本地 API 日志（session 可选） |
| `sn-cli run ...` | 查询和控制已有 durable run | SQLite WAL |

几条最重要的边界：

- 一个 **profile** 要么是 `cli`（包裹 CLI），要么是 `api`（调用模型）。namespace
  选择执行契约，`type` 校验 Profile 是否属于该入口。
- **session 永不自动执行 tool call**——模型返回 tool call 时 turn 停在 `requires_action`。
  自主工具循环属于 `agent`。
- Provider 原生长期 TUI 的 public owner 固定为 **`native_tui` Session**；tmux 只作为
  private PTY carrier，不单独创建或拥有 canonical Session lifecycle。
- `session exec|req` 创建的 `managed` Session 与 `session open` 创建的 `native_tui`
  Session 不能共用同一个 Session ID。
- **提交 run 不会自动启动 server**——入队与执行是解耦的。
- **readiness 跟随执行面**——worker 或 reaper 意外退出会让 server 立即 unready 并
  关闭；unready 期间新的 durable submission 返回 `503`，已有 Run 的控制请求仍可收口。

所有诊断都位于 `${SN_CLI_HOME:-~/.sn}/logs`：Profile 执行记录为
`YYMMDD/{cli,api}.jsonl`，脱敏的 CLI/HTTP 控制面审计为 `YYMMDD/audit.jsonl`，server
进程日志为 `sn-server.log`。它们都不是 Session/Run canonical state，也不用于 replay。
审计只记录规范化 action、稳定目标 ID、outcome 与错误/HTTP 状态，不记录 prompt、send
内容或 resolved secret。

精确契约（状态机、crash 恢复、digest/drift 门禁、文件系统安全模型）在
[契约文档](#文档)里；本 README 只停留在使用层面。

## 配置

Profile 是单层配置——每个 profile 一个 JSON 文件：

```text
source/payload configs/<profile-id>.json     → <runtime-home>/configs/<profile-id>.json
source/payload resources/tools/<tool>.json  → <runtime-home>/tools/<tool>.json
source/payload release/runtime.json         → <runtime-home>/runtime.json
source/payload resources/schema/*.json      → <runtime-home>/resources/schema/*.json
source/payload release/tmux.conf            → <runtime-home>/resources/tmux.conf
source/payload release/release.json         → <runtime-home>/resources/release.json
```

仓库 source tree 与每个 release archive 使用左侧同一布局；只有 activation 负责映射到
active home。active home 不是 source 或 archive template。

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
  "driver": "openai",
  "base_url": "https://your-provider/compatible-mode",
  "model": "your-model",
  "headers": { "Authorization": "${YOUR_API_KEY}" },
  "parameters": { "max_tokens": 16384 },
  "timeout": "5m"
}
```

secret 通过 headers 中的 `${VAR}` 引用从环境变量读取，profile 只存引用名不存值；openai
driver 对裸 `Authorization` 自动补 `Bearer` scheme，anthropic 不补。`runtime.json`
选择 Agent tool、预算、scheduler 与 run retention；source `resources/tools/` 默认交付
使用 `Z_AI_API_KEY` 的 `web_search` / `web_fetch` MCP manifest。完整字段、覆盖顺序与示例见
[sn-cli 详细使用手册](SN-CLI-USAGE.md)与[SN Runtime 契约](docs/runtime-contract.md)。

## 目录架构

```text
cmd/             sn-cli、sn-server 入口
pkg/             公开 Go API：agent、command、contract、model、profile、
                 provider、run、session、store、tmux 与 HTTP transport
internal/
  domain/        私有 Runtime 值对象与不变量
  application/   activation、bootstrap、tool/use-case 编排
  infrastructure/配置、文件、日志、进程与 MCP adapter
  interfaces/    入站 CLI adapter
  testkit/       仅测试复用资产
configs/         source CLI/API profile
resources/       schema 与 source tool
release/         runtime.json、tmux.conf 与 release.json payload 模板
```

外部 Go 调用方统一 import `github.com/yy003x/runtime/pkg/...`，不提供旧根 package
兼容 shim。`internal/application/runtimebootstrap` 是 composition root；四个 internal
层按 CLI Runtime + Agent 场景适配，不套用固定 HTTP 服务模板。架构测试对每个公开
`pkg/` package 的直接 module dependency 做 allowlist；新增跨域或 internal adapter
依赖必须显式评审。

## 文档

| 文档 | 范围 |
|---|---|
| [sn-cli 详细使用手册](SN-CLI-USAGE.md) | 完整 CLI 命令、参数、场景与示例参考 |
| [SN Runtime 契约](docs/runtime-contract.md) | 唯一的架构、配置、CLI/HTTP、Session、Run、Agent、Tmux 与激活契约 |

## 构建与验证

```bash
make fmt-check
make test-serial
make test-race
make coverage
make coverage-critical
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
