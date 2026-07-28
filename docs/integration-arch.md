# Runtime 集成架构

## 调用方边界

Runtime 是执行基础设施，不拥有业务 workflow、业务 Task/Session、skill 路由、
长期 memory、model fallback 或项目上下文策略。Workbench 等调用方通过公开
CLI/HTTP 传入 prompt、correlation ID 和有限 labels，不直接读写 `~/.sn`。

```text
Business Orchestrator
  ├─ one shot ─────> profile <id>
  ├─ local history ─> session run|submit
  └─ tool loop ─────> agent run / run submit

Runtime
  ├─ Command Bridge
  ├─ Model Core
  ├─ Session Service
  ├─ Agent Kernel
  └─ Run Harness
```

Workbench 业务 Session 可以保存 `runtime_session_id`，业务 Run 可以保存
`runtime_run_id`；两者不共享主键或文件。

## CLI 与 HTTP 的等价边界

CLI 和 HTTP 复用相同 application service：

- `profile <model-id>` ↔ `POST /v1/model/generate`：一次 model call；
- `session run` ↔ `POST /v1/sessions/{id}/turns`：一次记录型 Turn；
- `agent run` ↔ `POST /v1/agent/run`：一个 durable Agent Run 并同步等待；
- `run submit` ↔ `POST /v1/runs`：queued Run。

机器调用 CLI 管理面时必须使用 leading global `sn-cli --json ...`；默认 human
输出不是协议输入。顶层 command shortcut 和 CLI Profile 仍是透明进程边界，不
得为获取结构化结果而包装其 stdout/stderr。

HTTP 不暴露 command upload，因此远程调用不能注入 binary、argv、env、tool handler
或任意 Provider payload。Server 只能选择启动时已加载的 model/profile/tool。

## 故障语义

- Provider 一次调用失败：typed `RuntimeError`，Driver 不自动 retry；
- command 非零退出：Session Turn failed，保留 exit code；
- consumer stream 断开：ephemeral model call 随 request context 取消；
- durable Agent SSE 断开：Run 继续执行，调用方通过 event sequence 续读；
- tool effect 已 started 且结果未知：`needs_reconciliation`；
- queued/paused cancel：直接在 terminal transaction 中 cancelled；
- running cancel：传播 context；未知副作用不能伪装为安全 cancelled；
- worker 中单个 Run 失败：记录 Run 失败后继续领取队列。

## 发布与运行

一个 platform archive 原子交付：

```text
sn-cli
sn-server
configs/
commands/
runtime.json
resources/
```

installer 默认保留 active command/model/runtime config，只补缺失模板；
`--overwrite-configs` 只以发行模板替换这些用户配置。`resources/` 是 Runtime
管理资产，每次安装都会整体刷新。self-update 同时更新 `sn-cli` 与 `sn-server`，
不会形成不同版本的控制面和服务进程。

release gate 包括 Go format、serial tests、race tests、vet、跨平台 build、
checksum、临时 home install、profile validation、硬兼容 command smoke 和
`sn-server` start/status/stop 及并发生命周期 smoke；不修改 active `~/.sn`。
