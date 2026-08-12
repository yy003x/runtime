# Runtime 集成架构

## 调用方边界

Runtime 是执行基础设施，不拥有业务 workflow、业务 Session、skill 路由、长期
memory、model fallback 或项目上下文策略。Workbench 等调用方只能通过公开
CLI/HTTP 传入 prompt、correlation ID 和有限 labels，不直接读写
`${SN_CLI_HOME:-~/.sn}`。

```text
Business Orchestrator
  ├─ CLI direct ─────> <cli-id>
  ├─ one shot ───────> exec <cli-id> / req <api-id>
  ├─ recorded turn ──> session exec|req [--queue]
  ├─ durable console > session open|send|attach|interrupt|close
  ├─ raw TUI ────────> tmux start|send|attach
  └─ tool loop ──────> agent <api-id> [--queue]

Runtime
  ├─ Command Adapter
  ├─ Model Core
  ├─ Session Service
  ├─ Tmux Service
  ├─ Agent Kernel
  ├─ Tool Catalog / MCP Adapter
  └─ Run Harness
```

业务 Session 可保存 `runtime_session_id`，业务 Run 可保存 `runtime_run_id`。原始
Tmux window 只暴露 `tmux_id`；由 `session open` 创建的 window 额外保存 opaque
Session binding，但不读取 Session/Run Store，也不创建第二份 identity 或 history。

## 公开入口边界

- `req <api-id>` ↔ `POST /v1/model/generate`：一次 API model call；
- `<cli-id>` / `exec <cli-id>`：分别是 interactive direct 与 non-interactive
  本机 process replacement，无 HTTP 等价入口；
- `session exec|req <profile> [--queue]` ↔ Session Turn/Run HTTP：记录 canonical
  Turn；
- `session open|send|attach|interrupt|close`：本机 tmux-backed Session console，
  每个已消费 prompt 创建 durable Session Run，不暴露 HTTP；
- `tmux ...`：本机 human/management CLI，不暴露 HTTP；
- `agent <api-id> [--queue]` ↔ `POST /v1/agent/run` / `POST /v1/runs`：同步或
  queued durable API-only Agent；
- `run ...` 只查询或控制已经创建的 Durable Run。

机器调用 CLI 管理面必须用 leading global `sn-cli --json ...`。bare CLI direct 与
`exec` 都继承目标进程 stdout/stderr/exit，不为获取结构化结果而包装输出。Session
CLI executor 则由 Runtime 自己启动 canonical managed subprocess。Profile ID 紧跟
拥有它的执行 namespace，option 位于其后，input 必须最后。

HTTP 不接受 command upload、env、任意 Provider payload 或 tool handler。Server
只能选择启动时加载的 Profile 和注册 tool；CLI executor 的 HTTP cwd 必须来自
absolute request/Profile 配置，不能使用 server 启动 cwd。

`${SN_CLI_HOME}/logs/YYMMDD/{cli,api}.jsonl` 是 Runtime 自用的 best-effort 本地
execution diagnostics，不是公开集成入口、canonical state 或 completion signal。
业务调用方不得直接读取它来驱动 workflow；应继续使用 CLI/HTTP result、Session
facts、Run records 和 event sequence。queue submit 本身不写日志，worker 的实际
Profile execution 才写；日志丢失不改变执行状态。
MCP HTTP 不是 Profile Provider attempt，不写 `api.jsonl`；Agent tool effect/event
才是它的 durable evidence。

## Durable Agent execution identity

Agent Run 在进入 SQLite queue 前冻结 private non-secret execution snapshot，而不是
只记录一个可重新解释的 Profile ID：

```text
agent or session --queue / retry
  → freeze or verify Agent + model/Provider + tool snapshot
  → persist combined request_digest/config_digest
  → create queued Run

new Session/model/tool side effect
  → compare current loaded snapshot with frozen snapshot
  → equal: advance
  → drift: fail closed or preserve the active pause

terminal / cancel / reconcile
  → frozen snapshot + durable journal only
```

model snapshot 保存完整 API Profile、Profile digest 和 concrete driver semantic
identity；tool snapshot 保存 composite/builtin/MCP implementation/version、
canonical non-secret manifest/roots/cwd configuration 和 definitions。MCP header
只冻结 `${VAR}` 引用，不冻结 resolved secret。绑定 Session 时另存 Session 自己的
`session_request_digest/session_config_digest`，不把 combined Agent digest 混入
Session facts。headers 只冻结 `${VAR}` 引用名，resolved secret value 不冻结；相同引用名
下的 secret rotation 允许在下一次 Provider call 生效。

Retry 保留原 private snapshot，不按当前配置 re-freeze；只有完整 current snapshot
仍匹配时才创建新 Run。恢复 durable completed/failed/started effect、已知 terminal
projection、cancel 和 reconcile 不要求 current Profile、Provider 或 tool 仍可加载，
因此配置删除或升级不会把已经可证明的结果卡死。

composition 按动作最小化：

- `run get|list|result|trace|events|watch` 只加载 Run Store；
- `run gc` 只加载 Run Store；省略 `--older-than` 时额外只读取 retention 配置；
- `run cancel|reconcile` 只加载 Run Store 与 Session maintenance service；
- `run resume|retry`、带 `--queue` 的 `session exec|req`、`agent [--queue]` 和 worker
  execution 才加载 current Profile/Provider/tool/runtime execution dependencies。

private 仅表示不经公共 DTO、event、log 或 error 输出，不表示加密。snapshot equality
用于阻断已表示的语义漂移，不是 binary attestation、数字签名或 OS sandbox；同 UID
恶意进程、SQLite 篡改和未 bump semantic version 的实现变化不在保证范围内。

## 故障语义

- Provider 单次调用失败：typed Runtime error，Driver 不 retry；
- CLI Session 的非零 exit、signal、协议失败或输出超限：Turn failed，不追加
  partial assistant；
- Session child 已可能执行但结果未知：Session blocked，durable Run
  `needs_reconciliation`，不自动重放；
- consumer stream 断开：ephemeral model call 随 context 取消；
- durable Agent SSE 断开：Run 继续，可按 event sequence 续读；
- 有副作用 tool effect 已 started 且结果未知：`needs_reconciliation`，不自动重放；
  显式 `run reconcile` 保留 effect evidence 并以 failed 收口；绑定的 Session
  在此之前保持 blocked；
- MCP manifest 支持 `read_only`/`write_local`/`write_external` 三档 effect；
  transport/HTTP/protocol/remote failure
  作为 `ToolResult{is_error=true}` 确定闭合，不自动 retry；
- worker 单 Run 失败：结案后继续领队列；Store/claim 错误才停止 worker；
- Tmux window 的 pane transcript 不进入 Session，也不伪装 canonical result。
- `session send` accepted 只表示 console 接收；调用方仍以 Session/Run terminal fact
  判断完成。

## 发布、安装与激活

一个 platform archive 原子交付：

```text
sn-cli
sn-server
configs/*.json
resources/schema/*.json
resources/tools/*.json
release/runtime.json
release/tmux.conf
release/release.json
```

archive payload 与仓库 source 使用同一配置布局。activation 将 `configs/` 映射到
active `configs/`、`resources/tools/` 映射到 active `tools/`、
`release/runtime.json` 映射到 active 根 `runtime.json`、`resources/schema/`
映射到 active `resources/schema/`，并将
`release/{tmux.conf,release.json}` 映射到 active
`resources/{tmux.conf,release.json}`。payload `release/release.json` 声明
activation epoch 4、CLI contract 和 state schema。旧 active-shaped archive 不再
读取，也没有 migration 或兼容 reader。
installer/updater 必须让 staged candidate 在 active-home maintenance lock 内执行
preflight 与激活，不能先分段替换 resources/configs/binary。

激活前至少证明：

- `sn-server` 未运行且 lifecycle identity 无歧义；
- 专用 Tmux server/socket 不存在；
- 无 active/unknown Session execution；
- 无 queued/running/paused/needs-reconciliation Run；
- 无其它进程正在执行目标 home 的 `sn-cli|sn-server`；
- active Profile、Tool Catalog、Session fact 和 SQLite schema 与 candidate 的当前
  contract 精确一致。

默认保留合法 active configs/tools，只补缺失模板；`--overwrite-configs` 显式替换
Profile、Tool Catalog 和 runtime config，但不绕过运行态或 schema 门禁。unsupported state
必须先停服并整体移到可恢复备份；Runtime 不做自动 migration。

根目录 `make install` 是仅面向本地源码调试的例外策略：固定使用完整 source
bundle，校验 candidate 后自动停止受管 server，按上述映射全量覆盖 source
`configs/`、`resources/tools/` 和 `release/runtime.json`，并在新
artifact 已完整提交且 activation guard 仍生效时丢弃 Session/Run 状态；成功后不
重启。该 local-source 授权不进入 archive/network installer 或 `server update`，
也不绕过 Tmux、目标 binary process、路径和 journal 门禁。

candidate preflight 只读取 payload 自身的 `release/release.json`，不接受环境 token
绕过。
任何 target mutation 或停服前，candidate 先对 payload 执行完整 Profile 语义检查，
并 required/no-follow 校验 `release/runtime.json`、`resources/tools/`、
`release/tmux.conf` 和固定
identity/root shape 的可编译 Schema；staged home 再做二次检查。activation 先持久化
journal/state guard，再以 no-replace regular file
暂时占用 active `bin/`、`configs/`、`tools/`；二次 quiescence 使用原 binary inode 和
coordinator PID/start-token。journal 自身持续作为入口 barrier；
`committed|rolled_back` terminal phase、stage/rename/guard/journal 的目录 fsync
和 crash recovery 均完成后才放行，歧义状态保留 barrier 等待恢复。installer 的
外部 command link 在 activation mutation 前通过稳定 directory FD、durable owner
sidecar 与 no-clobber `symlinkat` 预留；owner `flock` 串行化协作 installer。
失败路径保留 exact owner/link，不删除最终名称；retry 只有在 owner 内容、inode
identity 和 target 均未变化时才复用，绝不覆盖已有入口。

release gate 包括 Go format、serial/race tests、vet、跨平台 build/checksum、临时
home install/upgrade、Profile/Session/Tmux schema preflight、各执行 namespace 与
Profile type 配对 smoke 和 server lifecycle smoke；不得修改 active `~/.sn`。
