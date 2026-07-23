# Agent Runtime

本仓库实现统一的 Go Agent Runtime。`sn-cli`、`sn-server`、CLI/API Provider、Session carrier、memory、skills、tools 和 daemon 共用同一套生命周期与运行产物契约。

## 当前架构

```text
cmd/sn-cli                 终端入口、交互命令、AgentRun 控制面、自更新
cmd/sn-server              HTTP /v1/runs 与 /v1/sessions adapter
internal/agentrun          Session/Turn/RunAttempt/Execution 与 run artifacts
internal/provider          CLI/API Provider 与 carrier 适配
internal/executor          进程执行、流式输出、终端与信号
internal/daemon            UDS、tmux 与长期进程注册
internal/capability        memory、skills、tools、workspace
internal/layout            ~/.sn 唯一路径契约
internal/installbundle     release 解包、checksum、配置/资源同步与迁移
```

- `agentrun` 是唯一公共 lifecycle 与 artifact owner。
- `Provider` 公开配置只区分 CLI 与 API；tmux/terminal 是 Session carrier。
- daemon 只管理长期进程和执行环境，不拥有 profile 或 run 状态。
- 本地生效配置只从 `~/.sn/configs` 读取。
- 仓库 `configs/` 只放会影响 Runtime 的 Provider profile 与 `runtime.yaml`；当前十一个基础 profile 必须保留，其他 profile 可按需新增。persona、skill、tool 和 schema 位于 `resources/`。

配置字段见 [docs/configuration.md](docs/configuration.md)，完整设计见 [docs/integration-arch.md](docs/integration-arch.md)。

## 安装

### 网络安装

网络安装下载 GitHub Release 中已编译的 binary、`configs/`、`resources/` 和 `checksums.txt`，不下载源码，也不要求 Go 或 Git：

该方式要求仓库已经发布至少一个 GitHub Release；首个 `v*` tag 发布前请使用下文的网络源码安装。

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
```

安装结果：

```text
~/.sn/bin/sn-cli
~/.local/bin/sn-cli -> ~/.sn/bin/sn-cli
~/.sn/configs/*
~/.sn/resources/*
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

本地调试默认把仓库 `configs/` 中所有同名文件覆盖到 `~/.sn/configs`，但不会删除本地额外 profile。需要恢复为只补缺失配置时使用：

```bash
make install SN_CLI_OVERWRITE_CONFIGS=0
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

### 配置与资源同步

网络安装、网络源码安装和 `sn-cli system update` 默认执行安全的补缺失规则；本地仓库 `make install` 为方便调试，默认显式开启同名 config 覆盖：

1. 在临时副本中把旧 `type/cli/api` wrapper、`label/presets`、嵌套 CLI 字段、`api.auth`、旧环境控制字段以及历史 runtime path 字段迁移为当前扁平 schema。
2. 把旧 `configs/{personas,skills,tools,schema}` 中本地缺失的文件复制到同名 `resources/` 目录；旧目录保留，不自动删除。
3. 默认递归复制发行包 `configs/` 与 `resources/` 中本地缺失的目录和文件。
4. `make install` 默认以 `--overwrite-configs` 覆盖仓库中存在的同名 config；设置 `SN_CLI_OVERWRITE_CONFIGS=0` 可关闭。
5. resources 始终只补缺失，发行包删除的模板不会删除本地文件，用户新增 profile 和资源都会保留。
6. 同一路径发生 symlink、特殊文件或文件/目录类型冲突时，安装在复制前失败并报告路径。

因此正式安装仍优先保护本地配置；本地开发者只有通过默认开启的 `make install` 调试开关才覆盖同名 config，且不会做镜像删除。

## 本地目录

默认 runtime home 是 `~/.sn`，可通过 `SN_CLI_HOME` 覆盖：

```text
~/.sn/
├── bin/sn-cli
├── configs/
│   ├── runtime.yaml
│   └── *.json
├── resources/
│   ├── personas/
│   ├── skills/
│   ├── tools/
│   └── schema/
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

`configs/runtime.yaml` 配置默认 project/profile、执行 deadline、队列上限和 Session carrier。所有路径由 `internal/layout` 固定，不接受配置文件改写。

```yaml
default_project: _default
default_profile: cx
max_concurrency: 1
max_queue: 64
queue_timeout_seconds: 3600
default_deadline_seconds: 300
session:
  default_carrier: tmux       # tmux|terminal
  terminal:
    driver: ghostty           # ghostty|iterm2，显式配置，不自动探测
```

## CLI 使用

### Profile 入口

CLI profile 入口始终是 native direct：继承当前 stdin/stdout/stderr 与 TTY，profile 后的参数按原生 argv 传递。Runtime 不连接 positional token，也不因 prompt 或管道输入自动增加 `exec`/`-p`：

```bash
sn-cli cx                         # Codex interactive
sn-cli cc                         # Claude interactive
sn-cli cx "分析当前仓库"           # 原生 codex "分析当前仓库"，进入 TTY
sn-cli cc "分析当前仓库"           # 原生 claude "分析当前仓库"，进入 TTY
sn-cli cx --help                   # Codex 原生帮助
sn-cli cx --no-alt-screen "分析当前仓库"
printf '分析当前仓库' | sn-cli cx  # stdin 原样继承，不自动切换 batch
sn-cli cx exec "分析当前仓库"      # 显式原生 Codex batch
sn-cli cc -p "分析当前仓库"        # 显式原生 Claude print mode
```

CLI profile 被识别后，后面的每个 token 都属于原生 command，并按原顺序传递。上述 native direct 调用都不创建 Run artifact 或逻辑 Session。通用脚本只知道 profile、但不知道背后是 Codex、Claude 还是 API 时，使用显式、无记录的 `profile exec`：

```bash
sn-cli profile exec cx "分析当前仓库"
printf '分析当前仓库' | sn-cli profile exec cc
sn-cli profile exec cx --skip-git-repo-check --ephemeral "分析当前仓库"
```

“无记录”只约束 sn-cli Runtime；Codex/Claude 原生客户端是否保存自身会话，仍由对应客户端及其配置决定。

只有这个显式 action 以及 `session run|submit`、Loop/HTTP Run 等 managed execution 会按 `command` basename 选择适配器：Codex 增加 `exec`，Claude 增加 `-p`；`args` 已包含完整 token 时不重复增加。`profile exec` 原样转发 Provider stdout/stderr，不创建 Runtime 记录，也不注入 `result.json` 契约。

API profile 支持无记录 direct request。需要结构化记录或异步执行时，显式使用 `session run|submit`：

```bash
sn-cli api-cx "直接调用 OpenAI-compatible API"
sn-cli api-cc "直接调用 Anthropic-compatible API"
sn-cli api-cx --temperature 0.2 --max-tokens 2048 "使用请求级参数"
sn-cli profile exec api-cx "显式无记录调用"
sn-cli session run cx "记录本次任务"
sn-cli session submit api-cx "后台调用"
```

API Provider 后不是原生 argv，而是 Runtime API adapter 的有限 typed options：`--model`、`--max-tokens`、`--temperature`、`--stream|--no-stream`，最后一个 quoted positional 是 prompt；未知 option 和多个 positional prompt 会报错。`protocol/base_url/api_key/headers` 仍只来自 profile。文件 prompt 直接使用 stdin，例如 `sn-cli api-cx < prompt.md`。

API 的固定 `headers` 也只能写在 profile 中，value 使用与 `env` 相同的 `${VAR}` 展开；命令行不能覆盖，认证 header 由 `api_key` 与协议统一生成。

profile 采用扁平 CLI/API schema；结果文件契约只由 `session run|submit` 启用。顶层 CLI profile 后的所有 token 由目标 CLI 解释，`sn-cli cx --help` 因此查看 Codex 帮助，而 `sn-cli --help` 查看 Runtime 帮助。安装或更新会先显式迁移旧 schema，再执行严格校验；普通配置加载不会隐式改写文件。

完整规则见 [`docs/cli-routing-contract.md`](docs/cli-routing-contract.md)。

### 命令面

公共命令最多两层：第一层是 namespace，第二层是 action。Provider 执行命令把 Runtime options 放在 Provider 前，Provider 是 action 后的第一个 positional 参数；Provider 后面的输入由 CLI/API 类型拥有。公共语法不再使用 `-c|--config`。

```text
run      list|show|logs|result|watch|cancel|reconcile
session  run|submit|open|list|show|messages|events|logs|send|interrupt|stop|attach|configure|export|delete
profile  list|show|validate|command|exec
system   doctor|start|status|stop|restart|migrate-config|update
loop     run|list|show|logs|cancel
skill    list|show|run
tool     list|show|call
memory   list|recall|add|remove|promote
```

Run 的统一查询控制面：

```bash
sn-cli run list --active --project <project> --limit 20
sn-cli run show --run-id <id>
sn-cli run logs --run-id <id> --tail 200
sn-cli run result --run-id <id>
sn-cli run watch --run-id <id>
sn-cli run cancel --run-id <id>
sn-cli run reconcile --dry-run
```

`run` 只查询和控制 Session、Loop 等 owner 已创建的记录，会从持久 registry/request 识别真实类型，不依赖 ID 前缀。Loop 仍由 `loop` namespace 管理。

### 生命周期

```bash
sn-cli profile list
sn-cli profile show cx
sn-cli profile validate cx
sn-cli profile command cx --json
sn-cli profile command cx --mode exec --json
sn-cli profile exec cx "无记录批处理"
sn-cli system doctor --json
sn-cli system migrate-config

sn-cli session run cx "创建逻辑会话并同步执行"
sn-cli session submit --session-id <id> cc "切换 Provider 后台继续"
sn-cli session run --session-id <id> api-cx --temperature 0.2 "API 继续分析"
sn-cli session run --session-id <id> --prompt-file prompt.md cx --skip-git-repo-check
sn-cli session list --project <project>
sn-cli session show --session-id <id>
sn-cli session messages --session-id <id> --after-seq 0
sn-cli session events --session-id <id> --after-seq 0
sn-cli session configure --session-id <id> --retention pinned
sn-cli session export --session-id <id> --output session.json
sn-cli session delete --session-id <id>

sn-cli loop run --input "执行计划" --actions-json '[{"type":"respond","content":"完成"}]'
sn-cli loop run --session-id <id> --input "协同执行计划" --planner-config api-cx --capability agent.run
sn-cli loop list --active
sn-cli loop show --loop-id <id>
sn-cli loop logs --loop-id <id> --tail 200
sn-cli loop cancel --loop-id <id>
```

`sn-cli system doctor --json` 始终输出数字字段 `contract_version`、可加性演进的 `features` map 与 `scheduler`，供 Workbench 等调用方在执行前校验 CLI/运行产物契约；Provider 凭据缺失只影响 doctor 的 `ok` 与对应 Provider 状态，不改变 contract version。

Session Run 使用本机持久 FIFO。`session run` 在显示 run ID 后 follow 至终态，`session submit` 返回 `pending`；默认 `max_concurrency=1`、`max_queue=64`、`queue_timeout_seconds=3600`、`default_deadline_seconds=300`。队列位于 owner-only 的 `state/runs/queue.json`，daemon 重启时自动 reconcile，也可运行 `sn-cli run reconcile --dry-run` 预览动作。

### Session 与 carrier

Session 是跨 API、CLI、tmux 和 terminal 的逻辑会话 owner，不等于某个 tmux window 或某次 Run。一个 Session 可以包含多个 Turn、RunAttempt 和 Execution，每个 Turn 可以切换 profile/provider。

```bash
sn-cli session open cx
sn-cli session open --carrier tmux --session-id <id> cx --no-alt-screen
sn-cli session open --carrier terminal --session-id <id> cc
sn-cli session logs --session-id <id> --tail 200
sn-cli session send --session-id <id> "继续"
sn-cli session interrupt --session-id <id>
sn-cli session attach --session-id <id>
sn-cli session stop --session-id <id>
```

发行 `runtime.yaml` 默认 `session.default_carrier: tmux`，因此第一条就是最简 tmux 打开方式。顶层 profile 永远使用当前 TTY，不读取 carrier 配置；创建新 terminal、可记录或可重连的交互会话仍需 `session open`。

- `session run|submit` 形成结构化 Turn、message、RunAttempt 和 Execution，适合 GUI 展示及后续上下文迁移。
- 顶层 CLI profile 与 CLI `skill run` 始终 native direct；API/native profile 执行 direct request。它们都不创建 Run 或 Session；需要记录必须显式进入 `session` namespace。
- `session open` 创建独立 carrier Execution，只保证 transcript，不把终端文本伪装成结构化 assistant final。
- `tmux` 可持久化和重新 attach；`terminal` 新开 Ghostty/iTerm2 window，关闭 window 即结束 Execution，当前不支持 `send|attach`。
- Session ID、Run ID、Execution ID 相互独立；carrier 控制命令统一通过逻辑 `--session-id` 定位当前 Execution。

Loop 只有显式传入已有 `--session-id` 时，planner 与 `run_agent` 子执行才复用该 Session 的跨 Provider 上下文和 Session memory；缺失或跨 project 的 Session 会在启动阶段拒绝。

### Capabilities

```bash
sn-cli skill list
sn-cli skill show <name>
sn-cli skill run <name> --input "检查当前变更"
sn-cli skill run <name> --input "检查当前变更" --search

sn-cli tool list
sn-cli tool show echo
sn-cli tool call echo --args '{"text":"hello"}'

sn-cli memory list --session-id <id> --state all
sn-cli memory recall runtime --session-id <id> --top-k 5
sn-cli memory add note-1 "runtime fact" --session-id <id>
sn-cli memory promote candidate-1 --session-id <id>
sn-cli memory remove note-1 --session-id <id>
```

`skill run` 只渲染 skill prompt 并 direct 调用其 `default_profile`：CLI profile 把 prompt 作为原生最后一个 positional 参数带入当前 TTY，API/native profile 执行 direct request。`--input|--input-file|--query|--vars` 等 skill options 必须写在 Provider 参数前；遇到第一个其他 token 后，其余 token 都交给 `default_profile`。它不接受 Run/Session 记录参数；需要持久会话时由 `session run` 进入记录链路。

非配置资源共用同一个 capability registry：

- `resources/personas/<id>.yaml`：保留给 Runtime 内部能力扩展；当前公开 profile schema 不引用 persona。
- `resources/skills/*.skill.yaml` 或 `resources/skills/<name>/skill.yaml`：声明 prompt 模板、关键词和 `default_profile`，由 `skill run` 使用。
- `resources/tools/*.tool.yaml` 或 `resources/tools/<name>/tool.yaml`：声明 external tool 的 schema、capability 和 request 模板；内置 function tool 与目录工具在同一 registry 中展示。
- Session memory 位于 `sessions/.../<session_id>/memory/{working.json,candidates.json}`；不带 `--session-id` 的 memory 命令操作 `~/.sn/memory` 全局手工存储。

Agent 的 `memory.write` 先产生带 Session/Turn/Run provenance 的 candidate，只有显式 `memory promote --session-id` 才进入 working memory。Workbench project/global memory 仍通过 API `memory[]` 只读注入，Runtime 不扫描或写回 Workbench 目录。

### Daemon

```bash
sn-cli system doctor --json
sn-cli system migrate-config
sn-cli system start
sn-cli system status
sn-cli system restart
sn-cli system stop
```

daemon 使用 owner-only socket 和 token，日志写入 `~/.sn/logs/daemon.log`。

### 更新

```bash
sn-cli system update --check
sn-cli system update --dry-run
sn-cli system update
sn-cli system update --version v1.0.0
sn-cli --version
```

版本由 exact Git tag 和构建 metadata 注入；非 tag 构建显示 commit 与 dirty 状态。更新从 GitHub Release 下载当前平台 archive 并校验 SHA256。新 binary 会先使用“本地配置/资源 + 新增模板”的临时合并 home 完成迁移与验证，再同步缺失项并原子替换 `~/.sn/bin/sn-cli`。失败时保留旧 binary。

`system doctor --json` 发现旧 profile schema 时会返回 `migration` 字段；执行 `sn-cli system migrate-config` 会幂等转换旧字段、把 embedded preset 拆成独立 JSON，并把旧 `configs/{personas,skills,tools,schema}` 的缺失项复制到 `resources/`。同名目标文件始终优先，旧资源目录不会自动删除。

## Provider

公开配置只支持两种扁平 profile：`command` 表示 CLI；`protocol/base_url/model/api_key` 表示 API。每个 `configs/<profile_id>.json` 只定义一个 profile，ID 由文件名确定；不再配置 `type`、`cli/api` wrapper、`id`、`label` 或 `presets`。正式约束见 [Provider JSON Schema](resources/schema/provider-profile.schema.json) 与 [Runtime Settings Schema](resources/schema/runtime.schema.json)。

最小 CLI profile 只需实际命令；需要固定模型或推理等级时再配置 `model`、`effort`。顶层 profile 不增加任何子命令；只有显式 managed execution 才为 Codex 增加 `exec`、为 Claude 增加 `-p`：

```json
{
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "high"
}
```

`generic` 不是配置值：`command` basename 为 `codex` 或 `claude` 时启用对应 managed adapter，其他命令自动进入内部通用兜底，不增加厂商专属参数，也不接受 `effort`。省略 `model` 时不生成 `--model` 参数，由目标 CLI 使用自身默认模型；更完整的字段说明见 [`docs/configuration.md`](docs/configuration.md)。

最小 API profile 需要显式协议、endpoint、模型和环境变量形式的凭据：

```json
{
  "protocol": "openai",
  "base_url": "https://example.test/v1",
  "model": "model-id",
  "api_key": "${API_KEY}"
}
```

API 认证由 Runtime 根据协议和 endpoint 选择，不配置 `auth`：OpenAI-compatible 使用 Bearer；Anthropic-compatible 默认使用 `x-api-key`，OpenRouter 的 Anthropic Messages endpoint 自动切换为 Bearer。

仓库发行十一个 Provider 模板：

- `cx`：Codex CLI，启用搜索、`danger-full-access`、`never` approval、high reasoning/verbosity，固定使用 `${HOME}/.codex-aip`。
- `cc`：Claude CLI，固定使用 `${HOME}/.claude.aip`，模型与推理等级由 Claude 默认配置决定。
- `cc-bai`：Claude CLI 经百炼 Anthropic-compatible endpoint 使用 Qwen，凭据读取 `${BAILIAN_API_KEY}`，固定使用 `${HOME}/.claude.aip`，并移除父进程的 `ANTHROPIC_AUTH_TOKEN`。
- `cc-glm`：Claude CLI 经 OpenRouter 使用 GLM 模型映射，固定使用 `${HOME}/.claude.aip`，并移除父进程的 `ANTHROPIC_AUTH_TOKEN`。
- `cx-image`：Codex 图片理解，接收 `WB_RUNTIME_IMAGE_PATH`。
- `cx-spark`：启用搜索、`danger-full-access`、`never` approval 和 xhigh reasoning/high verbosity，模型固定为 Codex Spark，使用 `${HOME}/.codex-ait`。
- `commit`：Git 提交批次的只读规划 profile，使用 Codex Spark，默认设置 `sandbox_mode=read-only` 与 `approval_policy=never`，deadline 为 900 秒；真实 Git 写入由调用方确定性执行。
- `mcx`：Mozi Codex，使用 `gpt-5.6-sol`。
- `mcc`：Mozi Claude，保留 CLI 超时环境变量。
- `api-cx`：OpenAI-compatible API。
- `api-cc`：Anthropic-compatible API。

其他账号、模型或 endpoint 由用户新增独立 JSON。公开 API profile 固定为一次请求/一次响应，不再通过 profile 开启 `native`、`mock`、MCP 或进程内 Agent runtime；需要多轮编排时由 Session、Loop 或外部 CLI 自身负责。

CLI provider 无法在进程内硬拦截目标程序自己的工具系统；`allowed_actions` / `forbidden_actions` 仅作为运行请求审计字段。Runtime 自身的 `tool`、`skill`、`memory` 与 `loop` namespace 仍按各自权限边界执行。

需要覆盖默认值时，CLI 字段直接放在顶层：

```json
{
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "high",
  "args": ["--search"]
}
```

command profile 的子进程环境按以下顺序生成，direct 与 tmux/session 使用同一规则：

1. 继承 `sn-cli` 当前进程环境。
2. 应用 `env`：string 展开后设置，`null` 删除。
3. 只有 Session/Loop 等记录入口才注入 AgentRun 内部变量。

所有支持环境变量替换的 Provider 配置值共用同一规则：`${VAR}` 会读取当前 `sn-cli` 进程环境，`$VAR` 和 `VAR` 都是普通字符串；引用的变量未设置时直接报错，不会替换为空字符串。Runtime 不内置环境文件加载器，不读取项目 `.env` 或 `~/.sn` 下的环境文件；需要的变量应由 shell、系统服务或其他外部环境管理工具在启动 `sn-cli` 前注入。仓库继续忽略 `.env` 文件，仅用于避免本地 secret 被误提交。

发行的 `commit`、`cx`、`cx-image` 固定 `CODEX_HOME=${HOME}/.codex-aip`，`cx-spark` 固定 `CODEX_HOME=${HOME}/.codex-ait`；`cc`、`cc-bai`、`cc-glm` 固定使用 `CLAUDE_CONFIG_DIR=${HOME}/.claude.aip`。需要切换其他账号目录时，用户可新增独立 profile 文件，例如 `cx-custom.json`：

Session Provider 还会收到 `AGENTRUN_SESSION_ID`、`AGENTRUN_TURN_ID`，以及 `SN_RUNTIME_CONTEXT_MANIFEST`、`SN_RUNTIME_SKILLS_DIR`、`SN_RUNTIME_TOOLS_DIR`、`SN_RUNTIME_MEMORY_FILE`、`SN_RUNTIME_MEMORY_CANDIDATES_FILE`。这些变量只暴露 Runtime owner 的路径与关联 ID，CLI wrapper/MCP 可以据此复用上下文能力；敏感值不进入 manifest。

```json
{
  "command": "codex",
  "model": "gpt-5.6-sol",
  "effort": "high",
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip",
    "OPENAI_API_KEY": null
  }
}
```

Claude profile 使用同样方式设置 `CLAUDE_CONFIG_DIR`。当 `ANTHROPIC_AUTH_TOKEN` 与 `ANTHROPIC_API_KEY` 同时存在时，应按实际认证方式在对应独立 profile 中将不用的变量设置为 `null`，避免改变默认账号或计费来源。

仓库额外发行 `cx-image`、`cx-spark` 和 `commit` 三个明确用途的 Codex 变体，以及 `mcx`、`mcc`。`cx` 保持最小原生配置；`cx-spark` 固定搜索、sandbox、approval 和 reasoning 参数；`cx-image` 在 `args` 中显式声明 `exec`，因此是有意设计的 batch-only profile；`commit` 只负责只读批次规划，不授予 Git 写权限。其他账号或模型变体由用户新增。Provider JSON 对未知字段采用严格校验，字段拼写错误会在 `profile list` 或 `profile validate` 阶段直接失败。

切换后可先执行 `sn-cli profile validate cx` 或 `sn-cli profile validate cc`。输出会显示实际生效的配置目录和认证环境变量名称，但不会输出 secret 值；Claude 认证变量冲突会出现在 `warnings`。

API profile 的凭据统一写为 `"api_key": "${OPENROUTER_API_KEY}"`；明文、`"OPENROUTER_API_KEY"` 和 `"$OPENROUTER_API_KEY"` 均会被拒绝。`depends`、`execution`、proxy/shim 等执行编排字段不再属于 profile。secret 只能通过完整 `${VAR}` 占位符引用，不应写入配置、日志或 result。

## 运行产物

managed run 位于：

```text
~/.sn/runs/<task|turn|session|command>/<YYYY-MM-DD>/<run_id>/
~/.sn/runs/loop/<YYYY-MM-DD>/<loop_id>/
```

标准文件包括 `request.json`、`status.json`、`events.jsonl`、`output.log` 和 `result.json`。Provider 启动后的 `output.log` 只追加；同步 `run` 只跟随 stream marker 后的新增内容，日志 header 不进入终端。`session open` 的 tmux/terminal carrier 记录 transcript，但不把终端文本伪装成结构化 assistant final。

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

`POST /v1/runs` 支持 `allowed_actions` 和 `forbidden_actions` 审计数组。默认同步；请求头包含 `Prefer: respond-async` 时返回 `202 Accepted`。`GET /v1/runs` 支持 active/state/type/project/profile/limit 过滤。

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

`make release` 生成 darwin/linux、arm64/amd64 的 `sn-cli-<os>-<arch>.tar.gz`、`sn-server-<os>-<arch>` 和 `checksums.txt`；CLI archive 固定包含 `sn-cli + configs/ + resources/`。`make release-check` 进一步执行格式、测试、vet、目录边界、旧资源迁移、资产/checksum 完整性检查，并在临时 home 中使用 test-only CLI fixture 运行离线 smoke；该 fixture 不进入发行 `configs/`。推送 `v*` tag 后，GitHub Actions 复用该门禁并发布这些资产。
