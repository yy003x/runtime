# CLI 路由契约

## 根路由

路由优先级固定为：

1. 可选且只允许位于 argv 第一项的 global `--json`；
2. `-h|--help|help` 与 `--version|version`；
3. 固定 namespace：`exec|req|profile|session|tmux|agent|run|server`；
4. active `configs/<id>.json` 声明的 CLI Profile；
5. 未命中或命中 API Profile 即失败。

namespace 或 Profile ID 后的 `--json` 不属于 root。例如 `sn-cli exec cx --json`
进入 `exec` typed parser，并因未知 option 失败。固定根 namespace
`exec|req|profile|session|tmux|agent|run|server|help|version` 和 Profile 管理 action
`list|show|check` 都是保留 Profile ID，不能被配置覆盖。

执行入口使用语义 namespace 决定领域，Profile `type` 做严格配对校验，不再由一个
通用 Profile 入口隐式选择 adapter：

```text
sn-cli <cli-profile-id> [options] [input]       # CLI interactive direct
sn-cli <cli-profile-id> resume [session-id]     # 续接底层 CLI 既有会话
sn-cli exec <cli-profile-id> [options] [input]  # CLI non-interactive exec
sn-cli req <api-profile-id> [options] [input]   # one API request
```

除 bare CLI direct 外，Profile ID 必须紧跟拥有它的 namespace/action；所有 option
位于 Profile ID 之后，input 至多一个且必须是最后一个参数。`--` 可用于终止 option
解析，但不改变该顺序。

## Profile 与直接执行

```text
sn-cli <cli-profile-id>
  [--model M] [--effort E] [--prompt FILE_OR_TEXT] [--cwd DIR] [input]

sn-cli <cli-profile-id> resume [session-id]
  [--model M] [--effort E]

sn-cli exec <cli-profile-id>
  [--model M] [--effort E] [--prompt FILE_OR_TEXT] [--cwd DIR] [input]

sn-cli req <api-profile-id>
  [--system TEXT] [--max-tokens N] [--temperature N]
  [--stream] [--request-file PATH|-] [input]

sn-cli profile list
sn-cli profile show <profile-id>
sn-cli profile check [profile-id]
```

bare Profile 只接受 `type=cli`，固定使用 interactive/direct mode；`exec` 只接受
`type=cli`，固定使用 non-interactive mode；`req` 只接受 `type=api`，只做一次
Provider request。类型不匹配在任何进程启动或网络请求前失败。三者均不创建
Session 或 durable Run；实际 CLI launch/API Provider call 会写 best-effort 本地
execution log。

CLI prompt 按 Profile `prompt`、`--prompt`、piped stdin、位置 input 合并。bare
direct 允许空 prompt；`exec` 的最终 prompt 必须非空。Runtime 不把 bare direct 或
`exec` 的原生 stdout/stderr/exit 包装成 JSON。

`<cli-profile-id> resume [session-id]` 续接底层 CLI（claude/codex）的既有会话，
仅 interactive direct 可用（`exec` 模式拒绝）。`session-id` 是底层 CLI 的**原生**
session id（claude/codex 自行存储会话，Runtime 不为其建立 Session/Run），透传给
adapter 翻译：claude→`--resume <id>`、codex→`resume <id>`。缺省 id 为 bare resume
（claude 恢复最近、codex 进入交互 picker）。`resume` 后可跟 `--model`/`--effort`，
但不再接受位置 input（继续输入用 `--prompt` 或进入会话后 stdin）。

API `--request-file` 读取 canonical `ModelRequest`，不是把本地路径发送给 Provider；
普通 input 与 `--system` 也只作为请求内容。`req` 不接受 `--cwd`，不会读取或写入
模型请求正文提到的本地路径，结果只同步返回给调用方。

`profile` 只拥有 `list|show|check` 配置管理动作，不执行 Profile。
当前 contract 不提供 legacy ingress、alias 或兼容 shim。

本地 execution log 固定写入 `logs/YYMMDD/cli.jsonl` 或 `api.jsonl`。它不改变上述
路由和 canonical persistence：Profile 管理、纯查询、控制动作与 queue submit 不写，
worker 真正执行后才写；日志失败允许丢失且不得影响执行。完整 schema、脱敏和文件
安全语义见 `docs/runtime-contract.md` 的“本地 Profile 执行日志”。

## Session

```text
sn-cli session exec <cli-profile-id> [options] [input]
sn-cli session req <api-profile-id> [options] [input]
sn-cli session list|show|messages|events|logs
sn-cli session executions --session-id <id>
sn-cli session execution --session-id <id> --execution-id <id>
sn-cli session open <cli-profile-id> [--session-id <id>]
                    [--retention R] [--model M] [--effort E]
                    [--cwd DIR] [input]
sn-cli session send --session-id <id> <input>
sn-cli session attach|interrupt|close --session-id <id>
sn-cli session reconcile --session-id <id>
                         [--terminate|--acknowledge-unknown]
sn-cli session configure|export|delete|gc
```

`session exec` 与 `session req` 默认同步等待一个 terminal Turn；在 Profile ID 后加入
`--queue` 时创建 durable Session Run 并只入队。两条入口进入同一个 Provider-neutral
Session service，底层 Session、Turn、Message、Event、Execution 和文件 schema
不因 CLI namespace 改名而变化。

`session exec` 只接受 CLI Profile，固定 non-interactive managed subprocess，捕获
canonical stdout、stderr 和 exit；它不支持 CLI direct/TUI，也不使用 Tmux。
`session req` 只接受 API Profile，执行一次 Provider request。同一 Session 可在
Turn 边界通过 `session exec` 或 `session req` 显式切换 executor/Profile；active
或 `requires_action` Turn 未闭合时仍禁止切换。

`session open` 只接受 CLI Profile，并创建或绑定一个 idle Session。它启动一个带
opaque Session binding 的 tmux console；`send` 和 attach 后直接输入的每个 prompt
都由 console 通过 durable `RunNow(kind=session)` 执行。Profile、base prompt、model、
effort 与 cwd 在 open 时冻结并在每轮 spawn 前复核；漂移时拒绝该轮并要求重新 open。
`interrupt` 先写 canonical Run cancel；`close` 若有 active Run，必须等 Run terminal
且 Session active Turn 清空后才能删除 window。Session 本身不会随 `close` 删除。

CLI Turn override 只支持 `--model`、`--effort`、`--cwd`；API request options 只适用
于 `session req`。输入来自 piped stdin 和可选的最后一个位置参数，两者以换行合并，
合并后必须非空。

Session 领域内部保留 `requires_action`/tool-result projection，但 stock CLI 和 HTTP
不发布 tool-result 写入口；当前公开 Session request 也不声明 tools。自动
model/tool/tool-result loop 只属于 Agent。

## Tmux

```text
sn-cli tmux start <cli-profile-id>
                  [--model M] [--effort E] [--prompt FILE_OR_TEXT]
                  [--cwd DIR] [input]
sn-cli tmux list
sn-cli tmux show --tmux-id <id>
sn-cli tmux send --tmux-id <id> <input>
sn-cli tmux attach --tmux-id <id>
sn-cli tmux interrupt --tmux-id <id>
sn-cli tmux stop --tmux-id <id>
```

原始 `tmux` namespace 是不创建 Runtime Session 的 interactive process manager。`start` 只接受 CLI
Profile，固定 interactive mode；每次在专用 server 的 `sn-session` 中新增 window。
初始 prompt 是最终 argv token，只有后续 `send` 使用安全 paste。`attach` 是
human-only，要求 stdin/stdout TTY。

## Agent 与 Run

```text
sn-cli agent <api-profile-id> [options] [input]
sn-cli agent <api-profile-id> --queue [options] [input]

sn-cli run get|list|result|trace|events|watch
sn-cli run cancel|resume|retry|reconcile
sn-cli run gc [--older-than 168h] [--limit 100] [--apply]
```

`agent` 只接受 API Profile，是 Runtime-owned model/tool loop。默认创建 durable Run 并
同步执行到 terminal；`--queue` 只创建并入队，由 worker 执行。Agent request 不接受
`cwd`。位置 input 存在时不读取 stdin；省略位置 input 时读取非 TTY stdin，最终输入
必须非空。

`run` 不接受 fresh submission，只查询或控制已有 Durable Run；`retry` 仍可从已有
终态 Run 创建关联 Run。fresh 创建入口归执行语义 owner：
带 `--queue` 的 `session exec`、`session req` 和 `agent`。`resume` 是 Kernel
extension：保留底层 CLI/API/Store state，但 stock server capability 不宣称默认
builtin tool 集可产生 Pause。

以上只重构 CLI ingress。`POST /v1/model/generate`、Session/Agent/Run HTTP routes、
Session 文件 schema 与 SQLite Run schema 保持不变；`POST /v1/runs` 仍是 HTTP
调用方的 queued Run 创建接口。

## 输出

- Runtime 管理 action 默认输出 compact human 文本；
- leading global `--json` 选择 `schema_version=1`、`contract_version=4` machine
  contract；
- 非流失败 stdout 为空，stderr 只有一个 compact v3 error document；
- stream/watch 输出 NDJSON，成功时最后一行是唯一 final record；失败不输出 final；
- bare CLI direct 和 `exec` 始终继承目标进程 stdout/stderr/exit；
- `tmux attach` 不支持 machine mode；
- `tmux list` 在专用 server 不存在时成功返回空集合。
