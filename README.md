# Agent Runtime

本仓库实现统一的 Go Agent Runtime。`sn-cli`、`sn-server`、CLI/API/tmux/native Provider、memory、skills、tools 和 daemon 共用同一套配置、生命周期与运行产物契约。

## 当前架构

```text
cmd/sn-cli                 终端入口、交互命令、AgentRun 控制面、自更新
cmd/sn-server              HTTP /v1/runs 与 /v1/sessions adapter
internal/agentrun          Session/Turn/RunAttempt/Execution 与 run artifacts
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

该方式要求仓库已经发布至少一个 GitHub Release；首个 `v*` tag 发布前请使用下文的网络源码安装。

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
├── sessions/
│   └── <YYYY-MM-DD>/<session_id>/{session.json,messages.jsonl,events.jsonl,turns/,executions/,memory/}
├── history/
│   ├── index.json
│   └── trash/
├── daemon/
│   ├── runtime.sock
│   ├── runtime.pid
│   ├── runtime.token
│   ├── processes.json
│   └── shims/
├── state/
│   ├── update.json
│   ├── runs/
│   └── sessions/locks/
├── memory/
│   ├── durable.json
│   └── candidates.json
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

direct 调用不创建 run artifact、逻辑 Session，也不加入 `managed_args`；需要 metadata Session 时显式使用 `sn-cli session exec -c cx -- ...`，该模式不捕获 transcript，因而拒绝 `record_mode=full`。普通文本调用复用 AgentRun，并由 profile 的 `managed_args` 和 `prompt_delivery` 决定实际调用方式。命令 stdout 返回 Provider 的真实 final text，run ID 与 artifact 目录写入 stderr；幂等复用时才回退输出 `result.json.summary`。完整规则见 [`docs/cli-routing-contract.md`](docs/cli-routing-contract.md)。

### Managed task

`task/turn` 提供完整 lifecycle。config 统一使用 `-c/--config`，prompt 可以来自 positional、`--prompt-file` 或 stdin，三者互斥：

```bash
sn-cli task run -c fake --mode capture "hello"
sn-cli task run -c cx "处理任务"
sn-cli task run -c cx --prompt-file task.md
printf '处理任务' | sn-cli task run -c cx
sn-cli session run -c cx "需要保留为 ephemeral Session 的一次性任务"
sn-cli turn run -c cx --session-id <id> "继续"
```

API/native profile 仍通过 prompt 驱动：

```bash
sn-cli bo "hello"
sn-cli native-mock "hello"
```

### 生命周期

```bash
sn-cli profiles
sn-cli config choices
sn-cli config validate -c fake
sn-cli config command -c cx-spark --json
sn-cli doctor

sn-cli task run -c native-mock "同步执行"
sn-cli task submit -c native-mock --queue-timeout-seconds 3600 "后台执行"
sn-cli turn submit -c cx --session-id <id> "后台继续"
sn-cli task status --run-id <id>
sn-cli task logs --run-id <id> --tail 200
sn-cli task result --run-id <id>
sn-cli task watch --run-id <id>
sn-cli task cancel --run-id <id>
sn-cli runs list --active --project <project> --limit 20
sn-cli runs reconcile --dry-run

sn-cli turn run -c native-fake "继续任务"
sn-cli task block --run-id <id> --reason "等待输入"
sn-cli task continue --run-id <id>
sn-cli task patch-resume --run-id <id> --patch '{"operation":"append","messages":[{"role":"user","content":"继续"}]}'

sn-cli loop run --input "执行计划" --actions-json '[{"type":"respond","content":"完成"}]'
sn-cli loop run --session-id <id> --input "协同执行计划" --planner-config ba --capability agent.run
sn-cli loop status --loop-id <id>

sn-cli session start -c cx "分析当前仓库"
sn-cli session start -c cx --prompt-file task.md
sn-cli session start -c cx "分析当前仓库" -- --no-alt-screen
sn-cli session list
sn-cli session send --run-id <id> "继续"
sn-cli session logs --run-id <id> --tail 200
sn-cli session attach --run-id <id>
sn-cli session stop --run-id <id>

sn-cli history create --session-id <id> --project <project> --runtime api --profile ba
sn-cli history list --project <project>
sn-cli history show --session-id <id>
sn-cli history messages --session-id <id> --after-seq 0
sn-cli history events --session-id <id> --after-seq 0
sn-cli history configure --session-id <id> --runtime cli --profile cx --retention pinned
sn-cli history export --session-id <id> --output session.json
sn-cli history delete --session-id <id>
sn-cli history rebuild

sn-cli command start -c cx -- printf 'hello'
sn-cli command status --run-id <id>
sn-cli command stop --run-id <id>

sn-cli clean
sn-cli clean --apply
```

`sn-cli doctor --json` 始终输出数字字段 `contract_version`、可加性演进的 `features` map 与 `scheduler`，供 Workbench 等调用方在执行前校验 CLI/运行产物契约；provider 凭据缺失只影响 doctor 的 `ok` 与对应 provider 状态，不改变 contract version。

managed Run 使用本机持久 FIFO。`task run` 提交后等待终态，`task submit` 返回 `pending`；默认 `max_concurrency=1`、`max_queue=64`、`queue_timeout_seconds=3600`，因此并发提交会排队，不再因单个任务占用 slot 而直接报 `max_concurrency=1 reached`。队列位于 owner-only 的 `state/runs/queue.json`，daemon 重启时会自动 reconcile；也可先运行 `runs reconcile --dry-run` 查看动作。

`session start` 复用 `cx.json/cc.json` 的 binary、common args、model 和 env，在 tmux 中启动交互 CLI，不需要 `tcx/tcc`。自动包装的 tmux session 使用 `sn-agent` 命名空间，不使用旧的 `mz-cli-agent`；session 使用 `pipe-pane` 持续写入 `output.log`；CLI 异常退出时最多尝试 5 次、间隔 3 秒，显式 `session stop` 不重启。

普通 `task run`、顶层 managed profile 和 `POST /v1/runs` 只产生 Run artifact，不隐式创建逻辑 Session；`session run`、`session exec`、`session start`、Session HTTP API 或显式 `--session-id` 才进入 History。
loop 只有显式传入已存在的 `--session-id` 时，planner 与 `run_agent` 子执行才复用该 Session 的跨 Provider 上下文和 Session memory；缺失或跨 project 的 Session 会在 loop 启动阶段拒绝。

### Capabilities

```bash
sn-cli capabilities skills list
sn-cli capabilities tools schemas
sn-cli capabilities memory write note-1 "runtime fact" --session-id <id>
sn-cli capabilities memory recall runtime --session-id <id>
sn-cli capabilities memory candidates --session-id <id>
sn-cli capabilities memory promote candidate-1 --session-id <id>
```

skills/tools 默认位于 `~/.sn/configs`。Agent 自动读取和写入的 Runtime memory 位于当前 Session 的 `memory/{working.json,candidates.json}`；`memory_write` 只生成带 Session/Turn/Run provenance、由 Runtime 分配 ID 的 candidate，必须显式 `promote --session-id` 才进入 working memory。`~/.sn/memory` 只保留 legacy/manual global capability，不被 Session Agent 自动加载。Workbench project/global memory 通过 API `memory[]` 只读注入，Runtime 不扫描或写回 Workbench 目录。

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

`type=api` 默认保持兼容的一次请求/一次响应模式；配置 `api.runtime.enabled=true` 后，同一个 API profile 会使用进程内 Agent loop，支持多轮 `tool_calls`、MCP、skill、memory 和本地 context snapshot。发行模板提供：

- `api-openai-agent`：OpenAI Chat Completions 协议。
- `api-anthropic-agent`：Anthropic Messages 协议。

两种协议共享相同的 Agent 状态机、权限门禁和运行产物，不需要再套一层 `type=native`。示例：

```bash
sn-cli task run -c api-openai-agent --allowed-action echo "调用 echo 后总结"
sn-cli task run -c api-anthropic-agent --allowed-action memory.read "结合本地记忆回答"
sn-cli task run -c api-openai-agent --allowed-action memory.write "把确认后的事实写入 memory"
```

API Agent 的本地上下文写入当前 run 的 `context-snapshot.json`，可沿用公共生命周期继续或修补：

```bash
sn-cli task continue --run-id <id>
sn-cli task patch-resume --run-id <id> --patch '{"operation":"append","messages":[{"role":"user","content":"补充上下文后继续"}]}'
```

`api.runtime` 配置示例：

```json
{
  "enabled": true,
  "system_prompt": "仅调用本次运行明确授权的工具。",
  "max_rounds": 10,
  "token_budget": 128000,
  "llm_timeout_seconds": 120,
  "auto_route_skills": true,
  "skills": ["review"],
  "memory": {"enabled": true, "top_k": 5},
  "mcp_servers": [
    {
      "name": "workspace",
      "transport": "stdio",
      "command": "my-mcp-server",
      "args": ["--stdio"],
      "env_passthrough": ["MCP_ACCESS_TOKEN"],
      "timeout_seconds": 30
    }
  ]
}
```

MCP 当前实现 stdio transport：完成 `initialize` / `notifications/initialized` 协商，分页调用 `tools/list`，再通过 `tools/call` 执行。远端工具以 `mcp__<server>__<tool>` 暴露。建议按具体工具授权；`--allowed-action mcp.<server>` 会授权该 server 的全部工具，`--allowed-action mcp` 会授权全部已配置 MCP server。`--forbidden-action` 优先，`*` 可用于全部拒绝。MCP 子进程默认只继承基础运行环境和 `env_passthrough` 中显式列出的变量。

memory 工具的授权边界为 `memory.read`、`memory.write`、`memory.delete`；其中删除必须单独授权。skill 从 `~/.sn/configs/skills` 加载，可显式列出或按关键词自动路由。启用 memory 且存在显式 Session 时，相关 working memory 会注入首轮 system context；Agent 写入先进入该 Session 的 candidates，经显式晋升后才写 working memory。

发行配置包含 `native-agent`（引用 `oro` OpenAI-compatible API profile）和无需凭据、不会访问网络的 `native-mock`。真实 native adapter 使用 OpenAI-compatible Chat Completions 或 Anthropic Messages 协议，持久化 finish reason 与 token usage；模型无 tool call 时完成，有 tool call 时执行结果会作为 tool message 回填下一轮。达到 `max_rounds` 仍未完成会失败，不再误报成功。

native 默认不向模型暴露任何工具。仅 `--allowed-action <tool-name|capability>` 明确授权的本地 function tool 会进入请求，`--forbidden-action` 优先级更高；external tool 不会由 native 进程内执行。例如：

```bash
sn-cli task run -c native-agent --allowed-action echo "调用 echo 后总结结果"
```

外部 CLI provider 无法在进程内硬拦截模型自己的工具系统，其 `allowed_actions` / `forbidden_actions` 仅作为运行请求审计字段；loop 与 native 的本地 action 边界会强制执行。

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
4. 用 `env` 覆盖固定值；只有 `${HOME}` 形式会读取环境变量。
5. 最后注入 AgentRun 内部变量。

所有支持环境变量替换的 Provider 配置值共用同一规则：`${VAR}` 会读取当前 `sn-cli` 进程环境，`$VAR` 和 `VAR` 都是普通字符串；引用的变量未设置时直接报错，不会替换为空字符串。Runtime 不内置环境文件加载器，不读取项目 `.env` 或 `~/.sn` 下的环境文件；需要的变量应由 shell、系统服务或其他外部环境管理工具在启动 `sn-cli` 前注入。仓库继续忽略 `.env` 文件，仅用于避免本地 secret 被误提交。

`env_passthrough` 与 `env_unset` 只控制子进程环境的继承和删除，不承担配置值替换。`env`、`env_passthrough` 与 `env_unset` 的冲突会在配置加载阶段报错。默认 `cx`/`cc` 不固定账号目录，会继承当前 shell 的 `CODEX_HOME` / `CLAUDE_CONFIG_DIR`。需要不依赖 shell 显式选目录时，可以增加 preset：

managed Provider 还会收到 `AGENTRUN_SESSION_ID`、`AGENTRUN_TURN_ID`，以及 `SN_RUNTIME_CONTEXT_MANIFEST`、`SN_RUNTIME_SKILLS_DIR`、`SN_RUNTIME_TOOLS_DIR`、`SN_RUNTIME_MEMORY_FILE`、`SN_RUNTIME_MEMORY_CANDIDATES_FILE`。这些变量只暴露 Runtime owner 的路径与关联 ID，CLI wrapper/MCP 可以据此复用上下文能力；敏感值不进入 manifest。

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

发行模板已提供 `cx-aip` 和 `cc-aip` preset。基础 `cx`/`cc` 不写固定账号目录、API endpoint 或密钥值；`cc-aip` 只固定账号目录和 endpoint，API Key 仍从父进程环境继承。Provider JSON 对未知字段采用严格校验，字段拼写错误会在 `profiles` 或 `config validate` 阶段直接失败。

切换后可先执行 `sn-cli config validate -c cx` 或 `sn-cli config validate -c cc`。输出会显示实际生效的配置目录和认证环境变量名称，但不会输出 secret 值；Claude 认证变量冲突会出现在 `warnings`。

API profile 的凭据统一写为 `"api_key": "${OPENROUTER_API_KEY}"`；明文、`"OPENROUTER_API_KEY"` 和 `"$OPENROUTER_API_KEY"` 均会被拒绝。audit proxy 的上游值使用 `"upstreams": ["${HTTP_PROXY}"]`，不再通过字段保存环境变量名。旧 `api_key_env` 与 `upstream_proxy_env` 字段不兼容并会被严格 schema 校验拒绝。

`depends`、audit proxy、PATH shim 和 DYLD 注入按 profile 显式启用。secret 只能通过完整 `${VAR}` 占位符引用，不应写入配置、日志或 result。

## 运行产物

managed run 位于：

```text
~/.sn/runs/<task|turn|session|command>/<YYYY-MM-DD>/<run_id>/
~/.sn/runs/loop/<YYYY-MM-DD>/<loop_id>/
```

标准文件包括 `request.json`、`status.json`、`events.jsonl`、`output.log` 和 `result.json`。tmux managed task 还使用空 `done` 文件，native Provider 使用 `native-snapshot.json`，API Agent Runtime 使用 `context-snapshot.json`。

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

默认只监听 `127.0.0.1:8080`，可通过 `HTTP_ADDR` 修改。监听非 loopback 地址时必须设置 `SN_SERVER_TOKEN`，请求使用 `Authorization: Bearer <token>`；配置 token 后，本机请求同样需要鉴权。`GET /healthz` 始终不鉴权。server 还限制请求体、header 和读写超时；HTTP 的 `cwd` 必须是绝对路径，`prompt_file` 必须是 `cwd` 内的相对路径。

```bash
HTTP_ADDR=0.0.0.0:8080 SN_SERVER_TOKEN='<从安全配置注入>' ./bin/sn-server
curl -H "Authorization: Bearer $SN_SERVER_TOKEN" http://127.0.0.1:8080/v1/runs/task/<run_id>/status
```

server 与 CLI 读取同一个 `SN_CLI_HOME`：

- `GET /healthz`
- `GET /v1/runs`
- `POST /v1/runs`
- `GET /v1/runs/{run_type}/{run_id}/status|logs|result`
- `POST /v1/runs/{run_type}/{run_id}/cancel|block|stop|continue|patch-resume`
- `GET|POST /v1/sessions`
- `GET /v1/sessions/{session_id}`
- `GET /v1/sessions/{session_id}/messages|events|watch`
- `POST /v1/sessions/{session_id}/turns`

`POST /v1/runs` 支持 `allowed_actions` 和 `forbidden_actions` 数组，HTTP 发起的 API Agent run 与 CLI 使用同一工具授权规则。默认同步；请求头包含 `Prefer: respond-async` 时返回 `202 Accepted`。`GET /v1/runs` 支持 active/state/type/project/profile/limit 过滤。

Session/History 的完整数据模型、记录策略、Workbench 边界和迁移契约见 [`docs/SESSION_HISTORY_DESIGN.md`](docs/SESSION_HISTORY_DESIGN.md)。

## 构建与验证

```bash
make sn-cli-build
make build
make release
make release-check

go test ./...
go vet ./...
make sn-cli-test
make test-serial
make test-race
make coverage COVERAGE_MIN=65.0
git diff --check
```

仓库 CI 在 push/pull request 上执行格式检查、双入口构建、vet、串行/并行测试、关键 race 和 65% 全仓覆盖率门禁，并单独验证 scheduler 的 FIFO、超时、取消和崩溃恢复。协议与工具回路测试使用本地 fixture，不调用真实或付费 API；真实 Provider smoke 仅在显式提供凭据并启用时运行。

`make release` 生成 darwin/linux、arm64/amd64 的 `sn-cli-<os>-<arch>.tar.gz`、`sn-server-<os>-<arch>` 和 `checksums.txt`。`make release-check` 进一步执行格式、测试、vet、资产/checksum 完整性检查，并在临时 home 中安装当前平台 archive、运行 `native-mock` smoke。推送 `v*` tag 后，GitHub Actions 复用该门禁并发布这些资产。
