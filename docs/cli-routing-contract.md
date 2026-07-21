# sn-cli 路由契约

本文是 `sn-cli` 命令语法、profile 分流、Session 与 carrier（执行载体）边界的规范源。入口实现、帮助文案、README、架构文档和测试不得与本文冲突。

## 1. 命令层级与统一语法

`sn-cli` 最多只有两层命令词：第一层是 namespace 或动态 profile ID，第二层是 action。action 之后只能是业务参数；需要 profile 的 namespace 把 profile 放在 action 后的第一个 positional 参数。

```text
sn-cli <profile>
sn-cli <profile> [prompt...] [-- raw-cli-args...]
sn-cli <profile> -- [raw-cli-args...]
sn-cli <namespace> <action> [arguments...] [options...]
```

- 全局参数只有 `-h|--help` 和 `--version`。
- `prompt` 是唯一允许不带 key 的执行数据，多个 token 以空格连接。
- 顶层 profile prompt 来源为 positional 或 stdin 二选一；`session run|submit` 额外支持 `--prompt-file`。同时提供多个来源必须报错。
- namespace 中的 profile 使用第三个参数，例如 `session run cx`、`profile show cx`；公共语法不使用 `-c|--config`。
- Run 使用 `--run-id`，逻辑 Session 使用 `--session-id`，Loop 使用 `--loop-id`；ID 不接受 positional 简写。
- `--` 之后全部属于目标原生 CLI，Runtime 不再解析；prompt 后的 raw 参数始终追加在 profile 生成的参数之后。
- 不保留 legacy alias、旧命令名、旧参数名或隐式兼容入口。

## 2. 顶层解析优先级

第一个参数按以下顺序解析：

1. `-h|--help`、`--version`。
2. 固定 namespace：`run`、`session`、`profile`、`system`、`loop`、`skill`、`tool`、`memory`。
3. 从 active config `~/.sn/configs/*.json` 解析精确 profile/preset ID。
4. 均未命中时返回 `unknown command`，不得猜测 Provider 或静默降级。

profile/preset ID 不得与固定 namespace、`help` 或 `version` 重名。配置不再定义 alias；出现 `aliases` 字段必须按未知字段拒绝。

## 3. 动态 Profile 分流

Profile 路由只区分交互、prompt 和显式 `--`，不创建持久 Run 或逻辑 Session：

| 调用形式 | 路由 | 参数处理 | Run artifact | 逻辑 Session |
| --- | --- | --- | --- | --- |
| `sn-cli <profile>` + TTY stdin | direct interactive | 无额外参数 | 无 | 无 |
| `stdin \| sn-cli <profile>` | direct one-shot | stdin 作为 prompt | 无 | 无 |
| `sn-cli <profile> "prompt"` | direct one-shot | positional 作为 prompt | 无 | 无 |
| `sn-cli <profile> -- ...` | direct passthrough | 移除 `--` 后原样传递 | 无 | 无 |

interactive 与 passthrough 只适用于 `type=cli` 且 `cli.executor=command` 的 profile。one-shot prompt 支持 command CLI、API 和 native profile；tmux profile 必须使用 `session run` 或 `session open`。

CLI one-shot 与 `session run|submit` 共用同一份 Provider argv 规则：

```text
binary + command.args + typed-overrides + model + managed_args + prompt-args + raw-cli-args
```

`managed_args` 只表达 Provider 的非交互入口差异，例如 Codex `exec` 与 Claude `-p`；它不表示是否记录。重复原生参数的覆盖语义由目标 CLI 决定。direct one-shot 直接转发 Provider stdout/stderr，不打印 Run ID、状态或 Runtime result，也不注入 `AGENTRUN_*` 环境变量。

Provider 差异只能由 profile config 表达，不得在入口硬编码 `cx -> exec` 或 `cc -> -p`。

```bash
sn-cli cx
sn-cli cc
sn-cli cx "hi"
sn-cli cc "hi"
printf 'hi' | sn-cli cx
sn-cli cx "hi" -- --skip-git-repo-check
sn-cli cx -- exec "hi"
sn-cli cc -- -p "hi"
```

`sn-cli cx run "hi"`、`sn-cli cx submit "hi"` 和 `sn-cli cx --help` 必须报错。需要记录时使用 `session run|submit`；原生参数必须放在 `--` 后。

### 3.1 结果契约与记录 owner

- profile config 不得出现 `result_contract`；普通加载遇到该字段时按未知字段拒绝。安装/更新流程只在 schema 升级阶段一次性删除旧字段，然后再执行严格校验。
- direct interactive、direct passthrough、direct one-shot 和 `skill run` 不创建 Run/Session artifact，也不注入结果文件契约。
- `session run|submit` 是 Provider 会话记录的唯一创建入口。command CLI 由 Runtime 注入 `result.json` 契约；API/native 由 Runtime 根据结构化 Provider 结果生成同一规范结果。
- `session open` 记录 carrier Execution 与 transcript，不要求交互式 Provider 写 `result.json`。
- `run` namespace 只查询和控制已有记录，不创建新记录。
- `loop` namespace 可以持久化循环恢复、状态和取消所需的编排数据；这些数据不等同于逻辑 Session。

### 3.2 环境配置

direct、session、tmux 和 terminal carrier 必须使用同一份 `cli.command` 环境配置。子进程环境顺序固定为：继承当前环境、应用 `env_unset`、应用 `env_passthrough`、应用 `env`、注入当前入口允许的 Runtime 环境；后写入的值优先。

- 只有完整的 `${VAR}` 会读取环境变量；`$VAR` 和 `VAR` 都是普通字符串。
- `${VAR}` 未设置时立即报错，不得替换为空字符串。
- 同一变量不得同时出现在 `env_unset` 与 `env|env_passthrough`。
- API 凭据使用 `api_key: "${VAR}"`；配置、日志和 Session 记录不得保存 secret。
- Runtime 不加载 `.env` 或 direnv 文件；环境由启动 `sn-cli` 的外部进程注入。

## 4. Namespace 契约

公共 command surface 固定为：

```text
run      list|show|logs|result|watch|cancel|reconcile
session  run|submit|open|list|show|messages|events|logs|send|interrupt|stop|attach|configure|export|delete
profile  list|show|validate|command
system   doctor|start|status|stop|restart|update
loop     run|list|show|logs|cancel
skill    list|show|run
tool     list|show|call
memory   list|recall|add|remove|promote
```

namespace 可以有多个二级 action，但不得继续创建 action 下的子命令层。

### 4.1 Run

```bash
sn-cli run list --active --project <project> --limit 20
sn-cli run show --run-id <id>
sn-cli run logs --run-id <id> --tail 200
sn-cli run result --run-id <id>
sn-cli run watch --run-id <id>
sn-cli run cancel --run-id <id>
sn-cli run reconcile --dry-run
```

`run` 是 Session Turn、carrier Execution 和已有内部 Run 的统一查询控制面。Run 类型必须从持久 registry/request 解析，不得依赖 ID 前缀猜测。Loop 仍由 `loop` namespace 管理。

### 4.2 Session

Session 是跨 API、native、CLI、tmux 和 terminal 的逻辑会话 owner，不等于某个 tmux window 或某次 Run。一个 Session 可以包含多个 Turn、RunAttempt 和 Execution；每个 Turn 可以切换 profile/provider，为 GUI 展示和后续上下文迁移保留稳定底层关系。

```bash
sn-cli session run cx --session-id <id> "继续分析"
sn-cli session submit cc --session-id <id> "后台继续"
sn-cli session open cx --carrier tmux --session-id <id> -- --no-alt-screen
sn-cli session open cc --carrier terminal --session-id <id>
sn-cli session list
sn-cli session show --session-id <id>
sn-cli session messages --session-id <id>
sn-cli session events --session-id <id>
sn-cli session logs --session-id <id> --tail 200
sn-cli session send --session-id <id> "继续"
sn-cli session interrupt --session-id <id>
sn-cli session stop --session-id <id>
sn-cli session attach --session-id <id>
sn-cli session configure --session-id <id> --retention pinned
sn-cli session export --session-id <id> --output session.json
sn-cli session delete --session-id <id>
```

- `session run|submit` 创建或复用 logical Session，并产生结构化 Turn、规范化 user/assistant message、RunAttempt 和 Execution；默认 `record_mode=full`、`retention=standard`、`capture_quality=structured`。
- `session run` 提交成功后立即输出 run 信息并 follow Provider stream，终态输出 `RunSummary`；`session submit` 只返回 pending `RunSummary`。
- 顶层 profile 与 `skill run` 始终 direct；需要会话记录必须显式使用 `session` namespace。
- `session open` 创建或复用 logical Session，并新增独立 Execution 和 Run artifact；Session ID、Run ID、Execution ID 不得复用为同一个 ID。
- `tmux` 是可重连的持久 carrier；`terminal` 是独立 terminal window carrier，关闭 window 即结束其 Execution，不能重连。
- `terminal` driver 只从 `configs/runtime.yaml` 的 `session.terminal.driver` 读取，支持 `iterm2|ghostty`，不得自动探测应用。
- `session open` 只保证 transcript，`capture_quality=transcript_only`，不能将 terminal 文本伪装成结构化 assistant final。
- `session send|attach` 只在 carrier 支持时执行；terminal 不支持输入注入或重新 attach 时必须明确报错。
- `session logs|interrupt|stop|attach|send` 使用逻辑 `--session-id` 定位当前 active carrier Execution；Run 级控制仍使用 `run ... --run-id`。

### 4.3 Profile 与 System

```bash
sn-cli profile list
sn-cli profile show cx
sn-cli profile validate cx
sn-cli profile command cx --json

sn-cli system doctor --json
sn-cli system start
sn-cli system status
sn-cli system stop
sn-cli system restart
sn-cli system update --check
```

`profile command` 只读解析 one-shot/Session argv，不启动 Provider，也不输出 profile env 值；文本和 JSON 输出都必须脱敏。`system serve` 仅供 daemon 子进程内部启动，不列入公共 help。

## 5. 不兼容清理

以下旧入口全部移除且不提供 alias：`task`、`turn`、`runs`、`history`、`config`、`profiles`、`providers`、`doctor`、`daemon`、`command`、`capabilities`、`tools`、`clean`、`update`、`upgrade`、`version`、`help`。旧 `<profile> run|submit`、`run command`、`session start|exec|status|watch|history`、`loop start|step|status` 和 capability 旧 action 同样不保留。CLI 不再接受 `--mode`，结果契约只由入口语义决定。

## 6. 变更门禁

修改顶层命令、profile 分流、参数名称、`--` 语义、prompt 来源或 direct/session/carrier 边界时，必须在同一变更中：

1. 更新本文并明确不兼容影响。
2. 更新 CLI 路由、parser、artifact 和 Session 关系测试。
3. 更新 `sn-cli --help` 与 README。
4. 同步更新 `docs/integration-arch.md`。
5. 运行 `go test ./...`、`go vet ./...` 和 `make sn-cli-test`。

同步 `run` 的 follow 还必须保持 `output.log` 在 Provider 启动后单调追加；follow 只能转发 stream marker 之后的内容，不得输出可能包含原生参数的日志 header。终态前最后一个无换行 chunk 必须 drain，follow 断开不得取消已持久提交的 Run。

只修改实现而不更新契约和测试，视为未完成。
