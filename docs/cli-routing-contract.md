# sn-cli 路由契约

本文是 `sn-cli` 命令语法、profile 分流、Session 与 carrier（执行载体）边界的规范源。入口实现、帮助文案、README、架构文档和测试不得与本文冲突。

## 1. 命令层级与统一语法

`sn-cli` 最多只有两层命令词：第一层是 namespace 或动态 profile ID，第二层是 action。Provider 执行 namespace 的 Runtime options 位于 Provider 前，Provider 是 action 后的第一个 positional 参数；Provider 后的输入由 profile 类型拥有。

```text
sn-cli <profile>
sn-cli <profile> [native-cli-args...]
sn-cli <namespace> <action> [arguments...] [options...]
sn-cli session <run|submit|open> [runtime-options...] <provider> [provider-input...]
```

- 全局参数只有 `-h|--help` 和 `--version`。
- 顶层 CLI profile 不解析 prompt；profile 后的每个 argv token 和 stdin 按原生 CLI 语义传递，不连接 positional token，也不根据 stdin 是否为 TTY 改变执行模式。
- API profile 后只接受 `--model`、`--max-tokens`、`--temperature`、`--stream|--no-stream` 和最后一个 quoted prompt；未知 option、多 positional prompt 与空 prompt 报错。prompt 也可来自 stdin。
- `session run|submit` 的 Runtime options 必须位于 Provider 前；CLI Provider 后除最后一个 Session prompt 外都是原生 managed command 参数，API Provider 后是 typed API options 与 prompt。`--prompt-file` 位于 Provider 前；positional、prompt file 与 stdin 三选一。CLI 使用 `--prompt-file` 或非空 stdin 时，Provider 后全部 token 都按原生 argv 处理，Runtime 不再猜测哪个 token 是另一个 prompt，调用方不得混用。
- namespace 中的 Provider 是 action 后的第一个 positional 参数；它前面可以有 Runtime options，例如 `session run --session-id <id> cx "hi"`。公共语法不使用 `-c|--config`。
- Run 使用 `--run-id`，逻辑 Session 使用 `--session-id`，Loop 使用 `--loop-id`；ID 不接受 positional 简写。
- 不保留 alias、旧命令名、旧参数名或隐式兼容入口。

## 2. 顶层解析优先级

第一个参数按以下顺序解析：

1. `-h|--help`、`--version`。
2. 固定 namespace：`run`、`session`、`profile`、`system`、`loop`、`skill`、`tool`、`memory`。
3. 从 active config `~/.sn/configs/<profile_id>.json` 解析精确 profile ID。
4. 均未命中时返回 `unknown command`，不得猜测 Provider 或静默降级。

profile ID 由文件名确定，不得与固定 namespace、`help` 或 `version` 重名。一个 JSON 文件只定义一个 profile；配置不再定义 `id`、`label`、`presets` 或 alias，这些字段必须按未知字段拒绝。

## 3. 动态 Profile 分流

动态 profile 不创建持久 Run 或逻辑 Session：

| 调用形式 | 路由 | 参数处理 | Run artifact | 逻辑 Session |
| --- | --- | --- | --- | --- |
| `sn-cli <cli-profile> [args...]` | native direct | argv 原样追加，继承当前 stdin/stdout/stderr 与 TTY | 无 | 无 |
| `sn-cli <api-profile> [typed-options] "prompt"` | direct API request | typed options 映射 request payload，最后一个 positional 是 prompt | 无 | 无 |
| `stdin \| sn-cli <api-or-native-profile>` | direct request | stdin 作为 prompt | 无 | 无 |
| `sn-cli profile exec <cli-profile> [args...]` | explicit unrecorded exec | 选择 managed selector，随后原样追加 CLI argv | 无 | 无 |
| `sn-cli profile exec <api-profile> [typed-options] "prompt"` | explicit unrecorded request | typed API request，不创建记录 | 无 | 无 |

包含 `command` 的 CLI profile 永远使用 native direct 路由。`sn-cli cx "hi"` 与 `codex "hi"` 具有相同的原生交互语义；`sn-cli cc "hi"` 同理。Runtime 不因 positional 参数或 piped stdin 自动增加子命令、切换执行器或捕获输出。tmux 不是 profile 类型，只能由 `session open --carrier tmux` 创建。

native direct argv 规则：

```text
command + args + configured-effort + model + native-cli-args
```

`profile exec`、`session run|submit`、Loop 和 HTTP Run 等显式 managed execution 共用 Provider argv 规则：

```text
command + args + configured-effort + model + derived-managed-mode + provider-cli-args
```

`command` 是实际执行的命令或可执行文件路径。只有显式 managed execution 才按 basename 选择内部适配器：Codex 自动增加 `exec`，Claude 自动增加 `-p`，其他命令不增加厂商参数。若 `args` 已包含 Codex 的 `exec`，或 Claude 的 `-p` / `--print` 完整 token，Runtime 不再重复增加。Provider 后的 CLI 参数不参与 Runtime typed override 解析，保持 token 与顺序；Session 最后一个 positional prompt 由上下文层取出，组合历史、memory 与 result contract 后通过 Provider prompt delivery 交付。公开配置不提供第二套 managed 参数字段。

`profile exec` 是明确的无记录、Provider-neutral batch 入口：CLI 根据 command 选择 `exec/-p` 后透传 Provider argv，API 解析 typed request options。它直接转发 Provider stdout/stderr，不打印 Run ID、状态或 Runtime result，也不注入 `AGENTRUN_*` 环境变量。`session run|submit` 才注入结果契约并创建记录。

“无记录”只约束 Runtime 自己的 Run、Session、Turn、message、events、logs 与 result；native direct 下 Codex/Claude 自身是否保存会话由目标 CLI 管理。

Provider 差异由 `command` 推导出的内部适配器和 profile config 表达，不得在入口硬编码 `cx -> exec` 或 `cc -> -p` 等 profile ID 规则。

```bash
sn-cli cx
sn-cli cc
sn-cli cx "hi"
sn-cli cc "hi"
printf 'hi' | sn-cli cx
sn-cli cx --help
sn-cli cx --no-alt-screen "hi"
sn-cli cx exec "hi"
sn-cli cc -p "hi"
printf 'hi' | sn-cli profile exec cx
sn-cli profile exec cx --skip-git-repo-check "hi"
sn-cli api-cx --temperature 0.2 --max-tokens 2048 "hi"
```

`sn-cli cx --help` 查看 Codex 原生帮助；`sn-cli --help` 查看 Runtime 帮助。顶层 CLI profile 的 `run`、`submit` 等 token 也由目标 CLI 自行解释，Runtime 不再把它们识别为 profile action。需要无记录 batch 时使用 `profile exec` 或显式原生 `exec/-p`；需要记录时使用 `session run|submit`。

### 3.1 结果契约与记录 owner

- profile config 只有两种扁平结构：以 `command` 识别 CLI，或以 `protocol/base_url/model/api_key` 识别 API；API 可选配置固定 `headers`。不得出现旧 `type/cli/api/native` wrapper，以及 `label`、`presets`、`driver`、`managed_args`、`executor/tmux`、`prompt_*`、`depends/execution`、`override_policy`、`env_passthrough/env_unset`、`auth`、`stream/mock/runtime` 或 `result_contract`。普通加载遇到这些字段时拒绝。
- `system doctor --json` 发现旧 schema 时会返回 migration 提示：`migration.required: true`、`migration.action: "system migrate-config"`，以及 `migration.configs`（受影响文件与字段路径）。迁移会把旧嵌套字段扁平化，并把 embedded preset 拆成独立 JSON；同名独立文件优先。
- native direct、direct API/native request、`profile exec` 和 `skill run` 不创建 Run/Session artifact，也不注入结果文件契约。
- `session run|submit` 是 Provider 会话记录的唯一创建入口。CLI 由 Runtime 注入 `result.json` 契约；API 由 Runtime 根据结构化 Provider 结果生成同一规范结果。
- `session open` 记录 carrier Execution 与 transcript，不要求交互式 Provider 写 `result.json`。
- `run` namespace 只查询和控制已有记录，不创建新记录。
- `loop` namespace 可以持久化循环恢复、状态和取消所需的编排数据；这些数据不等同于逻辑 Session。

### 3.2 环境配置

direct、session、tmux 和 terminal carrier 必须使用同一份扁平 `env` 配置。子进程先继承当前环境，再按 `env` 逐项处理：字符串值展开后写入；`null` 删除变量；最后注入当前入口允许的 Runtime 环境，后写入的值优先。

- 只有完整的 `${VAR}` 会读取环境变量；`$VAR` 和 `VAR` 都是普通字符串。
- `${VAR}` 未设置时立即报错，不得替换为空字符串。
- 需要显式透传变量时写成 `"NAME": "${NAME}"`；需要删除时写成 `"NAME": null`。
- API 凭据使用 `api_key: "${VAR}"`；配置、日志和 Session 记录不得保存 secret。
- API `headers` 只来自 profile，value 使用同一套 `${VAR}` 展开；命令行不能覆盖，认证 header 仍由 `api_key` 与协议推导。
- Runtime 不加载 `.env` 或 direnv 文件；环境由启动 `sn-cli` 的外部进程注入。

## 4. Namespace 契约

公共 command surface 固定为：

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

Session 是跨 API、CLI、tmux 和 terminal 的逻辑会话 owner，不等于某个 tmux window 或某次 Run。一个 Session 可以包含多个 Turn、RunAttempt 和 Execution；每个 Turn 可以切换 profile/provider，为 GUI 展示和后续上下文迁移保留稳定底层关系。

```bash
sn-cli session run --session-id <id> cx "继续分析"
sn-cli session submit --session-id <id> cc "后台继续"
sn-cli session run --session-id <id> api-cx --temperature 0.2 "API 继续分析"
sn-cli session run --session-id <id> --prompt-file prompt.md cx --skip-git-repo-check
sn-cli session open --carrier tmux --session-id <id> cx --no-alt-screen
sn-cli session open --carrier terminal --session-id <id> cc
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
- 顶层 CLI profile 与 CLI `skill run` 始终 native direct；API/native profile 执行 direct request。它们都不创建 Runtime 记录；需要会话记录必须显式使用 `session` namespace。
- `session open` 创建或复用 logical Session，并新增独立 Execution 和 Run artifact；Session ID、Run ID、Execution ID 不得复用为同一个 ID。
- `session open <provider>` 未指定 `--carrier` 时读取 `configs/runtime.yaml` 的 `session.default_carrier`；发行默认值是 `tmux`。
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
sn-cli profile command cx --mode exec --json
sn-cli profile exec cx "无记录批处理"

sn-cli system doctor --json
sn-cli system migrate-config
sn-cli system start
sn-cli system status
sn-cli system stop
sn-cli system restart
sn-cli system update --check
```

`profile command` 默认只读解析 native direct argv；`--mode exec` 解析 `profile exec` 与 Session 共用的 managed argv。它不启动 Provider，也不输出 profile env 值；文本和 JSON 输出都必须脱敏。

`profile exec` 显式执行无记录 batch：CLI 使用 managed adapter，API/native 执行 direct request；不创建 Run、Session 或结果文件契约。需要结构化结果、异步执行或 GUI 会话时使用 `session run|submit`。`system serve` 仅供 daemon 子进程内部启动，不列入公共 help。

## 5. 不兼容清理

不再保留旧命令、旧参数名、alias 与隐式兼容入口；需变更时以版本变更说明和变更日志为准。

## 6. 变更门禁

修改顶层命令、profile 分流、Provider 参数归属、参数名称、prompt 来源或 direct/session/carrier 边界时，必须在同一变更中：

1. 更新本文并明确不兼容影响。
2. 更新 CLI 路由、parser、artifact 和 Session 关系测试。
3. 更新 `sn-cli --help` 与 README。
4. 同步更新 `docs/integration-arch.md`。
5. 运行 `go test ./...`、`go vet ./...` 和 `make sn-cli-test`。

同步 `run` 的 follow 还必须保持 `output.log` 在 Provider 启动后单调追加；follow 只能转发 stream marker 之后的内容，不得输出可能包含原生参数的日志 header。终态前最后一个无换行 chunk 必须 drain，follow 断开不得取消已持久提交的 Run。

只修改实现而不更新契约和测试，视为未完成。
