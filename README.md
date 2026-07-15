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

已经下载源码时：

```bash
make install
```

兼容入口 `make sn-cli-install` 等价于 `make install`。源码只参与构建，安装后的 `sn-cli` 不依赖源码目录，源码移动或删除后仍可运行。

### 网络源码安装

需要保留源码并在本机编译时：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install-source.sh | bash
```

该方式要求本机已有 Git、Go 1.24+ 和 Make。源码受管 checkout 固定在：

```text
~/.sn/source/sn-runtime
```

安装指定 branch、tag 或 commit：

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install-source.sh | \
  bash -s -- --ref v1.0.0
```

重复执行会先检查 checkout：存在本地修改时停止，干净时更新到指定 ref、重新编译并复用 `install.sh` 完成安装。

### 配置同步

首次安装、再次安装、源码安装和 `sn-cli update` 都执行同一规则：

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
├── source/sn-runtime/       # 仅网络源码安装模式
├── logs/daemon.log
├── cache/
└── tmp/
```

`configs/runtime.yaml` 只配置 `default_project`、`default_profile` 和 `max_concurrency`。所有路径由 `internal/layout` 固定，不接受配置文件改写。

## CLI 使用

### Profile 入口

command CLI profile 根据第一个 profile 参数分流。无参数时启动原生交互程序；`-` 开头的参数直接透传；普通文本或 stdin 作为 managed prompt：

```bash
sn-cli cx                         # Codex interactive
sn-cli cc                         # Claude interactive
sn-cli cx "分析当前仓库"           # managed，自动使用 cx.json 的 exec
sn-cli cc "分析当前仓库"           # managed，自动使用 cc.json 的 -p
printf '分析当前仓库' | sn-cli cx   # stdin managed prompt
sn-cli cx --help                  # 原生 flag 透传
sn-cli cc -p "分析当前仓库"        # 原生 Claude print mode
sn-cli cx -- exec "分析当前仓库"   # -- 后强制原生透传
sn-cli cx "分析当前仓库" -- --skip-git-repo-check
```

direct 调用不创建 run artifact，也不加入 `managed_args`。普通文本调用复用 AgentRun，并由 profile 的 `managed_args` 和 `prompt_delivery` 决定 Codex/Claude 的实际调用方式。命令 stdout 返回 Provider 的真实 final text，run ID 与 artifact 目录写入 stderr；幂等复用时才回退输出 `result.json.summary`。完整规则见 [`docs/cli-routing-contract.md`](docs/cli-routing-contract.md)。

### Managed task

`task/turn` 提供完整 lifecycle。config 统一使用 `-c/--config`，prompt 可以来自 positional、`--prompt-file` 或 stdin，三者互斥：

```bash
sn-cli task run -c fake --mode capture "hello"
sn-cli task run -c cx "处理任务"
sn-cli task run -c cx --prompt-file task.md
printf '处理任务' | sn-cli task run -c cx
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
sn-cli config validate -c fake
sn-cli doctor

sn-cli task status --run-id <id>
sn-cli task logs --run-id <id> --tail 200
sn-cli task watch --run-id <id>
sn-cli task cancel --run-id <id>

sn-cli turn run -c native-fake "继续任务"
sn-cli task block --run-id <id> --reason "等待输入"
sn-cli task continue --run-id <id>
sn-cli task patch-resume --run-id <id> --patch '{"operation":"append","messages":[{"role":"user","content":"继续"}]}'

sn-cli loop run --input "执行计划" --actions-json '[{"type":"respond","content":"完成"}]'
sn-cli loop status --loop-id <id>

sn-cli session start -c cx "分析当前仓库"
sn-cli session start -c cx --prompt-file task.md
sn-cli session start -c cx "分析当前仓库" -- --no-alt-screen
sn-cli session list
sn-cli session send --run-id <id> "继续"
sn-cli session logs --run-id <id> --tail 200
sn-cli session attach --run-id <id>
sn-cli session stop --run-id <id>

sn-cli command start -c cx -- printf 'hello'
sn-cli command status --run-id <id>
sn-cli command stop --run-id <id>

sn-cli clean
sn-cli clean --apply
```

`session start` 复用 `cx.json/cc.json` 的 binary、common args、model 和 env，在 tmux 中启动交互 CLI，不需要 `tcx/tcc`。session 使用 `pipe-pane` 持续写入 `output.log`；CLI 异常退出时最多尝试 5 次、间隔 3 秒，显式 `session stop` 不重启。

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

command profile 的子进程环境按以下顺序生成，direct、managed 和 tmux/session 使用同一规则：

1. 继承 `sn-cli` 当前进程环境。
2. 用 `env_unset` 删除不应传入目标 CLI 的变量。
3. 用 `env_passthrough` 把当前进程变量显式带入 tmux 子进程。
4. 用 `env` 覆盖固定值，值支持 `${HOME}` 等环境变量展开。
5. 最后注入 AgentRun 内部变量。

`env`、`env_passthrough` 与 `env_unset` 的冲突会在配置加载阶段报错。默认 `cx`/`cc` 不固定账号目录，会继承当前 shell 的 `CODEX_HOME` / `CLAUDE_CONFIG_DIR`。需要不依赖 shell 显式选目录时，可以增加 preset：

```json
{
  "presets": {
    "cx-aip": {
      "cli": {
        "command": {
          "env": {"CODEX_HOME": "${HOME}/.codex-aip"}
        }
      }
    },
    "cx-no-api-key": {
      "cli": {
        "command": {
          "env_unset_append": ["OPENAI_API_KEY"]
        }
      }
    }
  }
}
```

Claude profile 使用同样方式设置 `CLAUDE_CONFIG_DIR`。当 `ANTHROPIC_AUTH_TOKEN` 与 `ANTHROPIC_API_KEY` 同时存在时，应按实际认证方式在对应 preset 中用 `env_unset_append` 删除其中一个，避免改变默认账号或计费来源。

切换后可先执行 `sn-cli config validate -c cx` 或 `sn-cli config validate -c cc`。输出会显示实际生效的配置目录和认证环境变量名称，但不会输出 secret 值；Claude 认证变量冲突会出现在 `warnings`。

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
