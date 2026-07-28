# CLI 路由契约

## 优先级

1. `-h|--help|help` 与 `--version|version`；
2. 固定 namespace：`profile|session|agent|run|system`；
3. active `commands/<id>.json` 声明的顶层 Profile shortcut；
4. 未命中即失败。

固定 namespace 是保留的顶层 subcommand ID；`profile` 的 `list|show|check` 是
保留 Profile ID。`commands/<id>.json` 必须引用现有 `configs/<profile>.json`。

## Profile

```text
sn-cli profile <id> [input]
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
或 durable Run。

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
active tmux execution；terminal 不伪装为可控 carrier。

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

- 普通管理面输出 JSON；
- model/Agent `--stream` 输出 NDJSON event，再输出 final record；
- `run watch` 输出已提交 event 的 NDJSON，terminal 后输出 final record；
- 顶层 command shortcut 完全继承目标进程 stdio 和 exit code；
- 错误写 stderr，CLI 返回非零。
