# Agent Runtime

本仓库实现一个自包含的 Go Agent Runtime。`sn-cli`、HTTP service、CLI/API/tmux/native Provider 和 daemon 共用同一套 profile、生命周期与运行产物契约，不依赖外部 `SINAN_ROOT` 或 Python runtime。

## 架构

```text
cmd/sn-cli                CLI 与 self-update 入口
cmd/runtime-server        HTTP 入口
        │
internal/agentrun         task/turn/loop/session/command 与 artifacts
        │
internal/provider         CLI/API/tmux/native 统一执行接口
        │
internal/executor         短进程执行、流式输出、进程组与信号
internal/daemon           UDS、长期进程、depends、proxy/shim
```

- `agentrun` 是唯一公共 run/session 语义层。
- `Provider` 是唯一执行抽象，包含 command CLI、API、tmux 和 native agent loop。
- daemon 只管理长期进程及其执行环境，不拥有 profile 解析、run 状态或 artifacts。
- `configs/<profile>.json` 是 Provider 配置事实源。

详细架构与迁移结果见 [docs/integration-arch.md](docs/integration-arch.md)。

## Provider

支持以下 Provider：

- `type=cli`：`executor=command|tmux`，支持 Codex、Claude 和 generic CLI。
- `type=api`：OpenAI-compatible、Anthropic-compatible 和本地 mock。
- `type=native`：进程内多轮 agent loop，支持 snapshot、block、continue、patch-resume、stop、cancel。

仓库默认包含 Codex、Claude、百炼、OpenRouter、tmux、`fake` 和 `native-fake` profile。配置只允许引用 secret 环境变量名，不允许内联 token、password、cookie 或 private key。

CLI profile 可按需显式启用 daemon 执行环境：

```json
{
  "type": "cli",
  "depends": [
    {
      "command": "helper --serve",
      "wait_tcp": "127.0.0.1:4141",
      "restart": true,
      "optional": false
    }
  ],
  "execution": {
    "audit_proxy": true,
    "upstream_proxy_env": ["MY_UPSTREAM_PROXY"],
    "bypass": ["localhost", "127.0.0.1"],
    "shim": true,
    "dylib": "${MY_INTERPOSE_DYLIB}"
  },
  "cli": {
    "driver": "generic",
    "executor": "command",
    "command": {"binary": "agent", "args": [], "model": ""},
    "runtime": {"prompt_delivery": "stdin", "result_contract": "optional"}
  }
}
```

`depends` 支持 `wait_tcp`、`wait_http`、`optional`、`restart`。`audit_proxy`、PATH shim 和 DYLD interpose 默认关闭；只有 profile 显式配置时才注入。上游代理地址通过 `upstream_proxy_env` 指定的环境变量读取。

## 构建与安装

```bash
make sn-cli-build
make sn-cli-install
sn-cli --help
```

远程安装：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | bash
```

安装脚本默认使用 `~/.sn-cli/runtime` 作为 managed checkout，构建二进制到 `runs/global/sn-cli/storage/current/bin/sn-cli`，并安装 launcher 到 `~/.local/bin/sn-cli`。

## CLI

按 profile 执行：

```bash
sn-cli fake "hello"
sn-cli cx "分析当前仓库"
sn-cli cx --prompt-file ./prompt.md --image ./screen.png
sn-cli cx-spark --model gpt-5.3-codex-spark "review"
```

生命周期命令：

```bash
sn-cli profiles
sn-cli config choices
sn-cli config validate --profile fake
sn-cli doctor

sn-cli task run --profile fake --mode capture "处理任务"
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
sn-cli session logs <run_id>
sn-cli session attach <run_id>
sn-cli session stop <run_id>

sn-cli command start --profile tcx -- printf 'hello'
sn-cli command status <run_id>
sn-cli command logs <run_id>
sn-cli command stop <run_id>
```

daemon 诊断：

```bash
sn-cli doctor daemon --json
sn-cli daemon start
sn-cli daemon status
sn-cli daemon restart
sn-cli daemon stop
```

doctor 输出包含 daemon 版本、PID、socket、clients/process registry、dependencies 和 proxy 状态。

## 运行产物

```text
runs/global/runtime/<task|turn|session|command>/<YYYY-MM-DD>/<run_id>/
runs/global/runtime/loop/<YYYY-MM-DD>/<loop_id>/
```

标准产物：

- `request.json`
- `status.json`
- `events.jsonl`
- `output.log`
- `result.json`
- `done`，仅 tmux managed task 使用
- `native-snapshot.json`，仅 native Provider 使用

managed tmux task 必须同时存在合法 `result.json` 和空 `done` 文件才算完成。stdout/stderr、pane 静默或单独一个完成文件都不能替代该契约。

## HTTP

```bash
make run
```

默认监听 `:8080`，可通过 `HTTP_ADDR` 修改。HTTP service 只调用 `agentrun`：

- `GET /healthz`
- `POST /v1/runs`
- `GET /v1/runs/{run_type}/{run_id}/status`
- `GET /v1/runs/{run_type}/{run_id}/logs`
- `GET /v1/runs/{run_type}/{run_id}/result`
- `POST /v1/runs/{run_type}/{run_id}/cancel|block|stop|continue|patch-resume`

## 验证

```bash
go test ./...
make sn-cli-test
make sn-cli-build
./cmd/sn-cli-wrapper config validate --profile fake
./cmd/sn-cli-wrapper fake "hello"
./cmd/sn-cli-wrapper doctor daemon --json
git diff --check
```
