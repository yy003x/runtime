# Tmux 管理契约

`sn-cli tmux` 是独立的 interactive process manager，不创建或写入 Session。
它只接受 `type=cli` Profile，并固定使用 adapter interactive mode；初始 prompt
是最终 argv token，只有后续 `send` 使用 tmux
paste。

```text
sn-cli tmux start <profile-id> [--model M] [--effort E]
                  [--prompt FILE_OR_TEXT] [--cwd DIR] [input]
sn-cli tmux list
sn-cli tmux show --tmux-id <id>
sn-cli tmux send --tmux-id <id> <input>
sn-cli tmux attach --tmux-id <id>
sn-cli tmux interrupt --tmux-id <id>
sn-cli tmux stop --tmux-id <id>
```

除 `attach` 外，management action 支持 leading global `--json`。`attach` 要求
stdin/stdout 都是 TTY；位于同一专用 server 时 switch，位于其它 tmux server 时
拒绝 nested attach。

## 所有权与隔离

- tmux client 固定使用短 `-S` socket、session `sn-session` 和 active
  `${SN_CLI_HOME}/resources/tmux.conf`，不读取用户 `~/.tmux.conf`。该文件只由
  source/payload `release/tmux.conf` 经 activation 映射产生。
- server marker 绑定完整 Runtime home digest、uid、bootstrap digest、schema 和
  随机 incarnation；所有 window record 绑定同一 incarnation。
- sentinel 仅维持 server，不是用户 window。最后一个 managed window 删除后同时
  清理 sentinel/server。
- Profile secret 只进入 mode=0600、消费即 unlink 的 launch manifest。tmux server
  只继承最小环境，record、argv、日志和 JSON 不保存 resolved secret。
- `list/show` 使用 shared lock；mutation 使用 exclusive lock，并在一个 tmux client
  command queue 内完成 identity conditional 与 action。

## 启动事务

`start` 先创建 UUIDv7-style `tmux_id` 和 blocked helper。helper 写 ready fact 后
等待 go；Runtime 校验 pane/helper/process/executable/manifest，最后写 registered
marker，再释放 helper exec target。marker 前失败删除 window；marker 后失败保留
可查询、可 stop 的 `starting|exited` record，避免调用方误重试。

markerless crash window 以 provisional name 显示为 `orphaned`。只有专用 server
owner、唯一 window/pane/link 和完整 provisional ID 可证明时才能 stop；record
marker 存在但缺字段或版本不支持属于 corruption，不静默忽略。

## 输入与状态

`send` 合并 stdin 和位置 input，要求非空 UTF-8、无 NUL且不超过 1 MiB。实现使用
唯一 buffer、`load-buffer -`、`paste-buffer -dpr` 和单独 Enter；success 只表示
tmux accepted。`interrupt` 发送 `C-c`；`stop` 使用精确 `kill-window`，不按 PID/PGID
发送 raw signal。

window state 为 `starting|running|exited|orphaned`。自然退出由
`remain-on-exit` 保留，`exit_code` 为 nullable；`send/interrupt` 只接受 running，
`stop` 接受全部 state。`list` 在 server 不存在时返回空数组。

Tmux 不保存 transcript、paste、canonical message、Session/Turn/Run ID，也不提供
HTTP route、retention、export 或 GC。`tmux start` 在最终 invocation 构建后会尝试写
best-effort `logs/YYMMDD/cli.jsonl`，但 management action 不写；该日志只是 launch
diagnostic，不是 window registry 或 launch acknowledgement，缺失时不影响 Tmux 状态。
