# sn-cli

`sn-cli` 是司南的本地终端入口，提供 profile runtime 调用、个人 AI 会话和原生 Codex / Claude 会话接管。

## 定位

- `sn-cli <profile> "文本"` 读取仓库内 `configs/<profile>.yaml`，执行单次 runtime，并把结果输出到 stdout。
- 当前 P0 内置 `fake` provider，不访问远端模型，用于验证配置加载、状态机、事件和产物链路。
- 默认进入司南自研 REPL，普通输入走结构化 runtime。
- `cx` / `cc` 通过配置化 command、args、env 直接启动 Codex / Claude Code。
- `/runtime` 命令和 `run` 子命令保留旧迁移链路，仍调用 `apps/runtime/cmd/runtime-provider`。
- `native` 命令通过 `wb.runtime` 启动并 attach 原生 Codex / Claude TUI；tmux 只由 runtime 直接操作。
- Go 只负责终端交互、session、工具别名和 runtime CLI 调用，不复制 `wb.runtime` 实现。
- `update` 命令负责版本检查和升级；交互入口会按配置间隔提示可用更新。

## 常用命令

```bash
make sn-cli-build
make sn-cli-install
cmd/sn-cli-wrapper --help
cmd/sn-cli-wrapper cx
cmd/sn-cli-wrapper cc
cmd/sn-cli-wrapper tools
cmd/sn-cli-wrapper providers
cmd/sn-cli-wrapper fake "hello"
cmd/sn-cli-wrapper fake --session-id local-dev "hello again"
cmd/sn-cli-wrapper native codex
cmd/sn-cli-wrapper doctor --json
cmd/sn-cli-wrapper update --check
```

## Runtime Profile

单次执行入口：

```bash
sn-cli <profile> [--session-id SESSION_ID] "文本"
```

profile 文件放在仓库 `configs/` 下，例如 `configs/fake.yaml`：

```yaml
name: fake
provider:
  type: fake
runtime:
  timeout_seconds: 30
artifacts:
  root: runs/global/runtime
```

`artifacts.root` 必须是 `runs/` 下的相对路径，避免 profile 把产物写到仓库外。

一次执行会生成独立 run 目录：

```text
runs/global/runtime/runs/<YYYY-MM-DD>/<run_id>/
```

当前固定产物包括 `request.json`、`resolved_config.json`、`events.jsonl`、`stdout.log`、`stderr.log`、`output.txt`、`result.json` 和 `artifacts/`。`tools`、`skills`、`mcp`、`memory context`、API provider、tmux provider 和跨次 `session_id` 上下文管理仍是后续阶段预留能力。

## 安装

本地仓库安装：

```bash
bash scripts/install-sn-cli.sh
```

远程安装：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/agent-arch/main/scripts/install-sn-cli.sh | bash
```

默认安装到 `~/.local/bin/sn-cli`。脚本会优先使用当前仓库；如果不是在仓库内执行，会把仓库 clone/update 到 `~/.sn-cli/agent-arch`，构建 `runs/global/sn-cli/storage/current/bin/sn-cli`，再安装 launcher。

可选覆盖：

```bash
SN_CLI_INSTALL_DIR=/usr/local/bin bash scripts/install-sn-cli.sh
SN_CLI_REF=main curl -fsSL https://raw.githubusercontent.com/yy003x/agent-arch/main/scripts/install-sn-cli.sh | bash
SN_CLI_REPO_DIR="$HOME/.local/share/agent-arch" bash scripts/install-sn-cli.sh
bash scripts/install-sn-cli.sh --dry-run
```

## 工具别名

默认工具别名在 `sncli/conf/default.json` 的 `tools` 中定义：

- `sn-cli cx`：启动 `codex`，默认带司南推荐的 Codex 参数。
- `sn-cli cc`：启动 `claude`，默认带 Claude Code 权限、超时和输出环境变量。

本机覆盖放 `sncli/conf/local.json`，例如：

```json
{
  "tools": {
    "cc-sonnet": {
      "command": "claude",
      "args": ["--model", "sonnet"],
      "env": {
        "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000"
      }
    }
  }
}
```

## 原生会话

`sn-cli native <provider>` 不直接操作 tmux；它读取 `native.profiles`，把 provider 映射到 `wb.runtime` 的 tmux profile，然后通过 `python3 -m wb.runtime.cli session start/attach` 启动和接入会话。

默认映射：

- `codex` -> `tmux-codex`
- `claude` -> `tmux-claude`

## 更新

检查更新：

```bash
sn-cli update --check
sn-cli update --check --json
```

升级：

```bash
sn-cli update
sn-cli update --install-dir "$HOME/.local/bin"
```

`update` 会比较当前仓库 commit 与配置的远端 ref，默认是 `main`；有新版本时执行 `git pull --ff-only` 并复用 `scripts/install-sn-cli.sh` 重建二进制和 launcher。若当前仓库有未提交的 tracked 变更，会拒绝升级，避免覆盖本地开发现场。

进入 REPL：

```bash
cmd/sn-cli-wrapper
```

REPL 命令：

```text
/provider codex|claude|fake
/runtime <prompt>
/native codex|claude
/session
/logs
/help
/exit
```

## 落位

- 源码：`sncli/cmd/`、`sncli/internal/`
- 配置：`sncli/conf/default.json`
- 本机覆盖：`sncli/conf/local.json`
- 二进制：`runs/global/sn-cli/storage/current/bin/sn-cli`
- 安装脚本：`scripts/install-sn-cli.sh`
- 更新状态：`runs/global/sn-cli/state/current/update-check.json`
- session：`runs/global/sn-cli/session/<YYYY-MM-DD>/<session-id>/`
