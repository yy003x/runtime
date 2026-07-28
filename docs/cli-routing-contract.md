# CLI 路由契约

## 优先级

1. 可选的 leading global `--json`；
2. `-h|--help|help` 与 `--version|version`；
3. 固定 namespace：`profile|session|agent|run|server`；
4. active `commands/<id>.json` 声明的顶层 Profile shortcut；
5. 未命中即失败。

固定 namespace 是保留的顶层 subcommand ID；`profile` 的 `list|show|check` 是
保留 Profile ID。`commands/<id>.json` 必须引用现有、`type=cli` 且
`transport=tty` 的 `configs/<profile>.json`；API、tmux 和 terminal Profile
不能登记为顶层 shortcut。root 只消费 argv 第一项的 `--json`；namespace 或
shortcut 后的同名参数属于目标命令，例如 `sn-cli cx --json` 必须原样透传。

## Profile

```text
sn-cli profile <id> [--effort <level>] [input]
sn-cli profile list
sn-cli profile show <id>
sn-cli profile check [id]
```

不存在 action 层：`exec` 和 `open` 不作为 `profile` 子命令。`type=cli` Profile
的 argv、transport、prompt delivery 完全由 JSON 决定；`type=api` Profile 只做
一次 API call。

source 默认提供 `commit` CLI Profile 和同名 subcommand 映射，因此以下两个入口
使用同一份 Profile：

```text
sn-cli commit <input>
sn-cli profile commit <input>
```

前者是声明式 shortcut；后者明确表达一次性 Profile 调用。二者都不创建 Session
或 durable Run。shortcut 会把 command Profile 中的固定 `args` 与调用方提供的
全部 native args 原样组成最终 argv，不把多个 native args 重新解释为一条
prompt。

`profile <id>` 的自动 delivery 按配置接收零个或一个 input；`manual` 不注入
prompt，其后的 native args 全部原样透传。TTY CLI Profile 会直接替换当前进程，
因此即使使用 leading global `--json`，其 stdout/stderr 和 exit code 仍完全
属于目标 CLI，不会被 Runtime 包装。

`profile <id>` 保留 `--effort low|medium|high|xhigh|max` 作为 Runtime typed
override。CLI Profile 必须显式声明 `effort_adapter=codex-config|claude-flag`；
Runtime 只按 adapter 生成 argv，不读取 `binary` 猜测 Provider。`--` 结束 Runtime
option 解析；其后内容按该 Profile 的 `prompt_delivery` 处理。顶层 shortcut
不解析 `--effort`，始终保持 native argv 透明性。API effort adapter 尚未登记时
明确失败，不把参数静默丢给 HTTP Driver。

## Session

```text
sn-cli session run [options] <profile-id> <input>
sn-cli session submit [options] <profile-id> <input>
sn-cli session list|show|messages|events|logs
sn-cli session tool-result
sn-cli session configure|export|delete|gc
sn-cli session attach|send|interrupt|stop
```

`run` 同步执行并写 Session 文件；`submit` 先创建 durable Run。carrier 操作只支持
active tmux execution；terminal 不伪装为可控 carrier。`session attach` 会替换为
交互式 tmux client，只支持 human 模式；`sn-cli --json session attach ...`
明确失败，不尝试 attach。

Profile ID 位于 options 之后、input 之前：

```text
sn-cli session run [options] <profile-id> <input>
sn-cli session run --session-id <id> api-cx "继续"
sn-cli session run --session-id <id> cx-tmux "启动新的 tmux execution"
```

tmux Profile 必须使用 `transport=tmux` 与 `prompt_delivery=paste|argv`；`manual`
不会接受 Session 自动输入。继续已启动的 tmux 使用 `session send`，而不是再次
`session run`。tmux execution 只承诺 launch handle 和 `transcript_only`。

## Agent 与 Run

```text
sn-cli agent run --profile <model-id> [options] <input>

sn-cli run submit --kind agent|session --profile <id> <input>
sn-cli run get|list|result|events|watch
sn-cli run cancel|resume|retry|reconcile
sn-cli run gc [--older-than 168h] [--limit 100] [--apply]
```

`agent run` 是同步等待的 durable Agent Run。`run submit` 是 queued execution。
Agent 不接受 command profile。

## 输出

- 普通管理面默认输出 action-aware 的紧凑 human 文本；
- Runtime 自己拥有输出的管理 action 使用 `sn-cli --json <namespace> ...`
  输出稳定 JSON，并保留各 action 的业务字段；
- 非流 JSON 失败时 stdout 为空，stderr 只有一个 compact contract v2 error
  document，CLI 返回非零；
- model/Agent `--stream` 输出 NDJSON event，成功时再输出唯一的 compact final
  record；
- `run watch` 输出已提交 event 的 NDJSON，成功 terminal 后输出唯一的 compact
  final record；
- stream 失败不输出 final；已经完成的 NDJSON 行可以保留，stderr 只输出一个
  compact contract v2 error，CLI 返回非零；
- 顶层 command shortcut 和 TTY CLI Profile 完全继承目标进程 stdio 和 exit
  code，leading global `--json` 不改变这一原生边界；
- `session attach` 不提供 machine mode；
- API Profile 非 stream 的 human 输出只返回 assistant text；tool call 或空文本
  返回可诊断摘要，完整结构使用 leading global `--json` 获取。
