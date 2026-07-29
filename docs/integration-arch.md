# Runtime 集成架构

## 调用方边界

Runtime 是执行基础设施，不拥有业务 workflow、业务 Session、skill 路由、长期
memory、model fallback 或项目上下文策略。Workbench 等调用方只能通过公开
CLI/HTTP 传入 prompt、correlation ID 和有限 labels，不直接读写
`${SN_CLI_HOME:-~/.sn}`。

```text
Business Orchestrator
  ├─ one shot ───────> profile <id>
  ├─ recorded turn ──> session run|submit
  ├─ interactive TUI > tmux start|send|attach
  └─ tool loop ──────> agent run / run submit

Runtime
  ├─ Command Adapter
  ├─ Model Core
  ├─ Session Service
  ├─ Tmux Service
  ├─ Agent Kernel
  └─ Run Harness
```

业务 Session 可保存 `runtime_session_id`，业务 Run 可保存 `runtime_run_id`；
Runtime Tmux window 只暴露 `tmux_id`，三者不共享 identity 或 storage。

## 公开入口边界

- `profile <api-id>` ↔ `POST /v1/model/generate`：一次 API model call；
- `profile <cli-id>`：本机 process replacement，无 HTTP 等价入口；
- `session run|submit` ↔ Session Turn/Run HTTP：记录 canonical Turn；
- `tmux ...`：本机 human/management CLI，不暴露 HTTP；
- `agent run` ↔ `POST /v1/agent/run`：durable API-only Agent；
- `run submit` ↔ `POST /v1/runs`：queued Run。

机器调用 CLI 管理面必须用 leading global `sn-cli --json ...`。CLI Profile 和
shortcut 是透明目标进程边界，不为获取结构化结果而包装 stdout/stderr。Session
CLI executor 则由 Runtime 自己启动 canonical managed subprocess。

HTTP 不接受 command upload、env、任意 Provider payload 或 tool handler。Server
只能选择启动时加载的 Profile 和注册 tool；CLI executor 的 HTTP cwd 必须来自
absolute request/Profile 配置，不能使用 server 启动 cwd。

## 故障语义

- Provider 单次调用失败：typed Runtime error，Driver 不 retry；
- CLI Session 的非零 exit、signal、协议失败或输出超限：Turn failed，不追加
  partial assistant；
- Session child 已可能执行但结果未知：Session blocked，durable Run
  `needs_reconciliation`，不自动重放；
- consumer stream 断开：ephemeral model call 随 context 取消；
- durable Agent SSE 断开：Run 继续，可按 event sequence 续读；
- tool effect 已 started 且结果未知：`needs_reconciliation`；
- worker 单 Run 失败：结案后继续领队列；Store/claim 错误才停止 worker；
- Tmux window 的 pane transcript 不进入 Session，也不伪装 canonical result。

## 发布、安装与激活

一个 platform archive 原子交付：

```text
sn-cli
sn-server
configs/
commands/
runtime.json
resources/
```

`resources/release.json` 声明 activation epoch、CLI contract 和 state schema。
installer/updater 必须让 staged candidate 在 active-home maintenance lock 内执行
preflight 与激活，不能先分段替换 resources/configs/binary。

激活前至少证明：

- `sn-server` 未运行且 lifecycle identity 无歧义；
- 专用 Tmux server/socket 不存在；
- 无 active/unknown Session execution；
- 无 queued/running/paused/needs-reconciliation Run；
- 无其它进程正在执行目标 home 的 `sn-cli|sn-server`；
- active Profile、Session fact 和 SQLite schema 与 candidate 兼容。

默认保留合法 active configs，只补缺失模板；`--overwrite-configs` 显式替换
Profile、subcommand 和 runtime config，但不绕过运行态或 schema 门禁。schema 1
必须先用旧版 export，再停服、备份/reset；不自动 migration。

根目录 `make install` 是仅面向本地源码调试的例外策略：固定使用完整 source
bundle，校验 candidate 后自动停止受管 server，全量覆盖 source configs，并在新
artifact 已完整提交且 activation guard 仍生效时丢弃 Session/Run 状态；成功后不
重启。该 local-source 授权不进入 archive/network installer 或 `server update`，
也不绕过 Tmux、目标 binary process、路径和 journal 门禁。

legacy v0.1.1 updater 的 staged validation 会被 contract-v3 candidate 在任何
release payload、binary、配置或受管 resource file mutation 前拒绝；旧版自身的
layout bootstrap 只能留下固定空 legacy directory。升级 v0.1.1 必须使用当前
release 的 `install.sh`；若恢复一键 self-update，需先发布兼容 schema 1 的 bridge
updater。

staged gate 只读取 candidate payload 自身的 manifest，不接受环境 token 绕过。
新版 activation 先持久化 journal/state guard，再以 no-replace regular file
暂时占用 active `bin/`、`configs/`；二次 quiescence 使用原 binary inode 和
coordinator PID/start-token。journal 自身持续作为入口 barrier；
`committed|rolled_back` terminal phase、stage/rename/guard/journal 的目录 fsync
和 crash recovery 均完成后才放行，歧义状态保留 barrier 等待恢复。installer 的
外部 command link 在成功激活后通过稳定 directory FD 与 no-clobber `symlinkat`
创建，失败只报告链接未创建，绝不覆盖现有入口。

release gate 包括 Go format、serial/race tests、vet、跨平台 build/checksum、临时
home install/upgrade、Profile/Session/Tmux schema preflight、hard-compatible
shortcut smoke 和 server lifecycle smoke；不得修改 active `~/.sn`。
