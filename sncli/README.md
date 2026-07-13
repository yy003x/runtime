# sn-cli

`sn-cli` 是当前仓库 Go Agent Runtime 的统一终端入口。它直接加载仓库 `configs/*.json`，不再委托外部 `sinan` / `wb.runtime`。

## 安装

本地仓库：

```bash
bash scripts/install-sn-cli.sh
```

远程安装：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | bash
```

默认行为：

- 管理 checkout：`~/.sn-cli/runtime`
- Go binary：`<repo>/runs/global/sn-cli/storage/current/bin/sn-cli`
- launcher：`~/.local/bin/sn-cli`
- launcher 设置 `SN_CLI_ROOT`，因此可以在任意工作目录运行；任务 `cwd` 仍是调用命令时的目录。
- 安装完成前执行 profile 加载和 `fake` 配置验证。

可选参数：

```bash
bash scripts/install-sn-cli.sh --dry-run
bash scripts/install-sn-cli.sh --install-dir /usr/local/bin
bash scripts/install-sn-cli.sh --local-repo /path/to/runtime
bash scripts/install-sn-cli.sh --repo-dir "$HOME/.local/share/runtime"
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | \
  SN_CLI_REF=main bash
```

## Profile 入口

```bash
sn-cli <profile_id> "prompt"
sn-cli <profile_id> --prompt-file ./prompt.md
sn-cli <profile_id> --prompt_file ./prompt.md --image ./screen.png
```

Provider 只读取 `configs/<profile_id>.json`，支持 preset 展开。profile 命令优先于 `sncli/conf/default.json` 的工具 alias。

常用 profile：

```bash
sn-cli fake "hello"
sn-cli cx "分析当前仓库"
sn-cli cx-spark "快速 review"
sn-cli cc "分析当前仓库"
sn-cli oro "调用 OpenRouter OpenAI-compatible API"
```

`--provider-overrides` 接收 JSON object。Codex 支持 `model`、`reasoning_effort`、`sandbox_mode`、`approval_policy`、`service_tier`、`verbosity`、`images`；Claude 支持 `model`、`effort`、`permission_mode`、`append_system_prompt`、`allowed_tools`、`disallowed_tools`。

## Runtime 命令面

```text
profiles
config choices|validate
doctor
task run|status|logs|watch|cancel
turn run|status|logs|watch|cancel
loop run|start|step|status|logs|cancel
session start|status|logs|watch|send|interrupt|stop|attach
command start|status|logs|watch|interrupt|stop|attach
capabilities skills|tools|memory
prune [--apply]
```

示例：

```bash
sn-cli task run --profile fake --mode capture "hello"
sn-cli task run --profile cx --prompt-file ./request.md --result-schema ./result.schema.yaml
sn-cli loop run --input "demo" --actions '[{"type":"respond","content":"done"}]'
sn-cli session start --profile tcx --cwd "$PWD"
sn-cli command start --profile tcx --cwd "$PWD" -- printf '%s\n' hello
```

direct CLI 的 `managed` 模式下，`result_contract=required` 的 provider 必须写入 `AGENTRUN_RESULT_FILE`；`capture` 模式或 optional/none contract 可由 runtime 合成 `result.json`。tmux task 始终使用 `result.json` + 空 `done` 文件的联合完成信号，不根据 stdout/stderr 或屏幕静默猜测完成。

## REPL 与 native

不带参数启动 REPL：

```bash
sn-cli
```

REPL 的普通输入通过仓库内 Go runtime 执行。`/native codex|claude` 和顶层 `native` 只是 `tcx` / `tcc` 的便捷映射，仍由本仓库 tmux lifecycle 实现。

## 更新

```bash
sn-cli update --check
sn-cli update
sn-cli update --install-dir "$HOME/.local/bin"
```

`update` 比较当前 checkout 与配置的远端 `main`，有更新时执行 fast-forward pull 并复用安装脚本。当前 checkout 有未提交 tracked 改动时会拒绝升级。

## 文件落位

- CLI 源码：`sncli/cmd/`、`sncli/internal/`
- CLI 配置：`sncli/conf/default.json`
- Provider 配置：`configs/*.json`
- Runtime settings：`configs/runtime.yaml`
- Runtime runs：`runs/global/runtime/`
- REPL sessions：`runs/global/sn-cli/session/`
- Update state：`runs/global/sn-cli/state/current/update-check.json`
