# Session 与 History 契约

## 文件布局与 schema

```text
sessions/
  <session_id>/
    session.json
    messages.jsonl
    events.jsonl
    turns/<turn_id>/turn.json
    turns/<turn_id>/context-manifest.json
    executions/<execution_id>.json
    context/current.json
  _system/
    index.json
    trash/<timestamp>/<session_id>/
state/
  session-locks/
  session-invocations/.invocation-<random>.json
  session-mutations/<session_id>.json
  session-trash-moves/<session_id>.json
  runtime.db
```

Session fact 固定 `schema_version=2`，SQLite 固定 `PRAGMA user_version=4`。缺失、
不相等或混合状态 fail closed，不提供版本推断、字段补齐或自动 migration。

`logs/YYMMDD/{cli,api}.jsonl` 不属于本布局或 Session fact。`session logs` 读取的是
canonical Session activity；本地 execution log 只是可丢失诊断，Session CLI/API
真正执行时会另写一条，但恢复、replay、完成判定和 GC 都不依赖它。

写入使用 Session-scoped `flock`、大小限制和 directory-FD relative filesystem
操作。已有路径逐组件以 `O_NOFOLLOW` 打开；`sessions/`、`state/`、Session root
及其私有子目录的 device/inode 在 Store 生命周期内固定，directory replacement
直接 fail closed。canonical fact、journal、lock 和 invocation manifest 必须是
single-link regular file。

JSON replace 通过同目录临时文件、file/dir `fsync` 和 identity-checked atomic
publication 完成：新目标使用 no-replace rename，已有目标使用 atomic exchange
并验证被替换 inode 后再按 identity 删除。新 Session root 先在随机临时目录完成
owner 与 root identity 准备，再通过 no-replace rename 发布。JSONL sequence 在单
Session 内单调递增；`_system/index.json` 可重建，不是原始事实。

### Filesystem threat model

该门禁用于抵抗 symlink/hardlink、路径或 directory replacement、可预测 target
名称上的并发 swap，以及任意 crash point。删除 regular file 或空目录时，Store
不会直接 unlink 可见名称，而是先以 no-replace rename 移到 128-bit random private
quarantine，再复核类型、link count 和 device/inode；若不匹配则尝试恢复原名并
fail closed，只有匹配时才 unlink quarantine。

这不是对同 UID hostile process 的绝对隔离。POSIX 没有 compare-by-inode unlink；
如果攻击者已获得同 UID 任意代码执行，能够持续枚举并替换不可预测 quarantine
名称，或使用 ptrace/kill 干预进程，则超出本 Store 的威胁模型。权限边界仍必须由
OS account、Runtime Home mode 和进程托管层提供。

同一次 Session mutation 涉及 JSONL append 或多个 fact 时，Store 使用私有
per-Session undo journal：

1. journal 携带随机 nonce、Session root identity 和目标 identity；创建新 Session
   root 时，在任何 canonical fact 写入前 durable 写入与 journal 精确匹配的
   `.runtime-mutation-owner.json`，再以 no-replace rename 发布该 root；
2. 每个目标第一次写入前，先把 replace 的完整 preimage，或 JSONL 的原始
   `size + SHA-256 prefix_digest` 写入
   `state/session-mutations/<session_id>.json`，并同步文件和目录；
3. journal 为 `prepared` 时才允许修改 Session fact。JSON fact 使用 atomic
   replace；JSONL 不执行原地 append，而是读取已有内容、追加一个完整 JSON line，
   再 atomic full-file rewrite。每次 publication 的新 inode 都追加到 journal 的
   owned identity 集合；
4. mutation 全部成功后，先把 journal 原子改为 `committed`，再按
   owner marker → journal 顺序清理并分别同步目录；
5. commit persist 报错时必须 strict/no-follow 重读 journal 并比较完整 identity；
   可证明 `committed` 时绝不回滚，可证明 `prepared` 时才允许回滚，缺失、损坏或
   identity 漂移一律保留 facts/journal 并 fail closed；
6. 进程在 `prepared` 阶段退出时，下一次 `NewStore` 或同 Session 操作在持有同一
   `flock` 后恢复 replace preimage。JSONL 只有在当前 inode 属于该 mutation、
   当前长度不小于原始 `size` 且原始长度范围的 digest 等于 `prefix_digest` 时，
   才 atomic rewrite 回原前缀；在 `committed` 阶段退出时保留已提交 fact，只清理
   journal；
7. `SessionExisted=false` 只有在 owner marker 的 session ID 与随机 nonce 都匹配，
   且完整 scope 校验通过后才允许回滚；缺 marker、nonce 不符或有未登记内容时，
   不删除任何 fact、temp 或 forensic journal；
8. recovery 不调用 Provider、不执行 command/tool，也不重放外部 effect；无
   durable journal 的未知 partial state 不做启发式修复。

journal 使用独立的私有 `mutation_version=3`，不属于 canonical fact；owner marker
只在未清理 mutation 中短暂存在，也不是 canonical fact。因此
Session `schema_version` 仍为 2。启动校验必须逐 Session 持同一把锁，先 recovery
再执行 schema、layout、sequence 和 count 校验，避免另一个活跃 writer 的中间态
被误判为 Store 损坏。该协议只覆盖文件型 Session Store，不与 SQLite durable Run
建立伪 transaction。

## Managed invocation manifest

CLI Session 不把 resolved argv/env 直接放入进程参数。Runtime 在
`state/session-invocations/` 创建随机命名、mode `0600`、single-link 的 strict
JSON manifest；payload 只包含 resolved `path/argv/environment/cwd`，可能含当前
环境解析出的 secret，因此是不可导出、不可完整记录日志的短生命周期 private state。
CLI execution log 只保存可读 command，Profile env 保留 `${VAR}` 引用，不保存该
manifest 中的 resolved argv/environment。
写入并同步后，Runtime 把 manifest absolute path、directory identity 和 file
identity 传给 private helper。

helper 先等待 marker-before-exec handshake；收到 go 后逐组件 no-follow 打开
manifest directory，核对 directory/file device+inode、mode、link count 和大小，
strict decode 后按同一 identity 删除 manifest，最后执行 `syscall.Exec`。因此路径
替换、symlink、hardlink 或 manifest 内容不完整会在上述威胁模型内 fail closed。

正常 manifest 消费即删除。Session execution service 和 maintenance service
初始化时只会清理超过 24 小时、名称/mode/size/strict payload 和 identity 都符合
Runtime manifest 约束的遗留文件，每次最多 1000 个；无法证明 ownership 的 entry
保持原状。

## Trash move journal

`session delete` 和 `session gc --apply` 使用
`state/session-trash-moves/<session_id>.json` 的 private `version=1` journal。
journal 固定 source Session root device/inode，target 只能是
`_system/trash/<timestamp>/<session_id>`。

rename 前先 durable 写入 journal。恢复或正常执行只接受以下可证明状态：

- source 存在且 target 不存在：source identity 必须匹配 journal，然后执行
  no-replace rename，重新核对 target identity，再 `fsync` target parent 和
  `sessions/`；
- source 不存在且 target 存在：target identity 必须匹配 journal，视为 rename
  已完成；
- source/target 同时存在、同时缺失、类型错误或 identity 漂移：保留 journal 并
  fail closed。

`NewStore` 在 mutation/schema 校验前恢复 pending trash move；move 可证明完成后
重建 `_system/index.json`，最后删除 move journal。Delete/GC 在普通路径中遵循同一
顺序。恢复不物理删除 trash 中的 Session，也不提供公开 restore action。

## Identity

```text
session_id   Runtime conversation
turn_id      一次用户输入及其 terminal result
run_id       一次 durable execution
execution_id 一次具体 API attempt 或 managed process
task_id      caller-owned correlation label
```

ID 不复用。每个 Turn 可选择 API 或 CLI Profile；executor/provider 只能在 Turn
边界切换，active 或 `requires_action` 未闭合时禁止切换。

## Context projection

每个 Turn 先追加原始 user message，再从原始 messages 构造唯一 projection：

- API：保留结构化 role/content/tool_calls/tool_call_id；
- CLI：生成转义、定界、有界的 history/current-input prompt，再在最前面合并
  Profile base prompt；
- manifest 记录 profile、request/config/base-prompt digest、sequence range、
  token estimate、capacity、pressure 和 correlation；
- API Profile 未声明 `context.window_tokens` 时使用 `32768` 保守窗口并记录
  `capacity_source=conservative_default`；显式窗口记录为 `profile`；
- request options 使用 Provider-neutral `max_output_tokens`、`temperature`、`top_p`
  和 `stop_sequences`；Profile 的统一 `parameters.max_tokens` 由 adapter 转为 wire 字段；
- 有效输出预留取 `reserved_output_tokens`、Profile 默认输出上限和请求级输出上限的
  最大值；默认预留为 `8192`，输入预算至少保留 `2` 个 Token；
- overflow Turn 仍记录 user input、failed 状态和 typed error；
- projection 不改写或删除原始 messages。

Profile base prompt 是 invocation 前缀，不另存为本轮 user Message。最终 CLI
prompt 仍受 128,000-byte argv-token 门禁。

## CLI Execution

`session exec` 对 CLI Profile 固定 non-interactive managed mode，也不使用 Tmux。
command adapter 构建 canonical invocation，managed helper 在 marker-before-exec
handshake 后启动 Provider。`session req` 则对 API Profile 执行一次 Provider
request；两者共用本节的 Session/Turn/Execution schema。

Execution 保存：

- executor kind、request/config digest 和 absolute cwd；
- `spawn_intent|running|settled` lifecycle；
- owner/helper PID、PGID 和 process-start identity；
- exit code/signal、typed result/error；
- stdout/stderr observed bytes、prefix digest、truncated、limit-exceeded 和有限
  Runtime summary。

不保存原始 provider stderr、resolved env secret、完整 argv/env 或 partial
assistant。OS exit=0 和 canonical protocol terminal 双门禁通过后，才追加唯一
assistant Message。

Runtime 进程崩溃后，已经可能运行的 child 不自动重放。stale Turn 保持 blocked；
durable Session Run 进入 `needs_reconciliation`。

## Tool result

API Session 返回 canonical tool call 后：

1. Turn 为 `requires_action`，Session 为 blocked；
2. 外部提交原始 `tool_call_id`、content、is_error、idempotency key；
3. 同 key 同 payload 重放返回相同 receipt；
4. key 或 call ID 冲突 fail closed；
5. pending tool 全满足后 Session 回到 idle，下一 Turn 才能继续。

Session 不执行 canonical tool。CLI Provider 进程内部的工具行为是 opaque executor
行为，不进入上述协议。`is_error` 同时属于 canonical tool Message，下一 Turn
投影到 anthropic driver 时必须保留为 `tool_result.is_error`。

## Reconciliation

```text
session reconcile --session-id <id>
session reconcile --session-id <id> --terminate
session reconcile --session-id <id> --acknowledge-unknown
```

默认只探测 process identity；`--terminate` 只有 PID、PGID、start token 和 group
leader 全匹配时才发送信号；`--acknowledge-unknown` 用于操作者已经在外部确认没有
存活副作用进程的情况，两 flag 互斥。结案后，durable Run 再通过 `run reconcile`
读取 Session terminal result。所有动作幂等，digest 不匹配为 conflict。

Agent 绑定 Session 后，`paused` 只允许通过对应 durable Run 的 `run resume`
恢复；unknown tool effect 则投影为 Session `blocked`、active Turn `running` 和
Execution `settled/unknown`。它不是 pending tool input；stock Session 不发布
tool-result 写入口，也不能通过公开 `session reconcile` 收口。唯一恢复入口是：

```text
run reconcile --run-id <agent-run-id>
```

该入口显式确认 effect outcome 仍然 unknown，不重放 Agent 或 tool；它先幂等地把
Session Turn 结案为 failed，再在 SQLite terminal barrier 中把 Run 结案为 failed。
checkpoint、Run/Session event 和 `tool_effects` 证据均保留。两个 Store 不建立伪
transaction；任一步写入失败后重复同一命令会收敛到相同终态。
若进程在 unknown projection 写入前退出，Turn 上预先原子持久化的
`agent_owned=true` marker、精确 `run_id` correlation 和 SQLite Run 状态共同
识别 owner；此时 Execution 可以仍是 `running`，恢复入口仍相同。

## Private durable payload

`session exec|req <profile> --queue` 在 ingress 冻结 caller-relative prompt file、
typed override、non-secret Profile snapshot 和 digest。公开 Run request 只保留 digest/ref；
snapshot 与解析后的 base prompt 存入 SQLite `private_request_json`，对应 Go 字段
强制 `json:"-"`。

private payload 不得出现在 Run query/result/event、Session export、human output、
日志或 error。worker 不重新读取 prompt file，也不用自己的 cwd 解释输入；Profile
删除或 non-secret config 漂移在 spawn 前 conflict。env secret 只在 worker 执行时
从当前环境解析。

Agent Run 复用同一 Store-private `private_request_json` 列保存自己的 versioned
execution snapshot，但不复用 CLI Session snapshot shape。Agent payload 冻结
non-secret model/Provider/tool identity 和可选 Session digests；headers 只保存
`${VAR}` 引用名，resolved secret value 同样不持久化。两种 private payload 都必须 strict decode、
canonical/digest 自校验且受大小限制，也都由 Go 字段 `json:"-"` 阻止公开序列化。

同一 `session_id` 同时只允许一个 queued/running/paused/needs-reconciliation
durable Session Run，由 SQLite 原子约束保证。

## Agent projection 与 retention

Agent 默认不创建 Session。显式 `--session-id` 时，Agent 读取结构化历史并以稳定
correlation 幂等投影新增 assistant/tool messages。SQLite Run 和文件 Session 不建
伪跨 Store transaction。reconciliation 完成前 Session 保持 blocked，不能创建
新 Turn；完成后恢复 idle。

Session Turn/Execution 的 `request_digest/config_digest` 继续使用 Session 自己的
profile-only 算法。Agent private snapshot 另存
`session_request_digest/session_config_digest` 并与这些 Session facts 精确比较；
它们与 Run 中包含 Agent contract、Provider 和 tool identity 的 combined digest
不是同一个值，也不得相互替代。恢复已知 terminal projection、取消或
`run reconcile` 只消费 frozen Session digests 和 durable evidence，即使 current
Profile 已删除或执行配置已漂移也必须能够收口；只有推进新的 model/tool side
effect 才要求 current execution snapshot 匹配。

Agent Session projection 只发布 provider-safe 的最长闭合前缀：从
`base_message_count` 开始，每个 assistant tool-call message 必须立即由按声明顺序、
`tool_call_id` 精确匹配的一组 tool messages 完整闭合，才推进 safe boundary。允许
单独识别一个尚未闭合的 assistant boundary 供 paused 状态校验，但不会把 partial
tool round 当作下一次 Provider 可消费历史。只有当前 Turn 为 completed 时，结果
Message 才取当前 Turn `[base_message_count:safe_end)` 中最后一条 assistant
Message；其他状态的结果 Message 固定为空。provider/executor 只能在该边界闭合后
切换。

Retention：

```text
ephemeral | standard | pinned
```

`session gc` 默认 dry-run，只选择超过 cutoff、非 active/blocked 的 ephemeral
Session；`--apply` 在锁内复核后移动到 `_system/trash`。`session delete` 同样是
可恢复移动。

GC 结果区分：

- `candidates`：锁外扫描时符合条件的 Session；
- `moved`：`--apply` 持锁复核后实际移入 trash 的 Session；
- `skipped`：扫描后 retention、state、`updated_at` 已变化，或已被其他操作移动，
  因而安全跳过的 Session。

持锁复核必须同时满足 `retention=ephemeral`、`state=idle` 和
`updated_at < cutoff`。单个候选失效不使整批 GC 失败。
