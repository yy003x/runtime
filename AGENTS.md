# Runtime 协作约定

## 项目定位

本仓库是 SN Runtime 公开 CLI、HTTP、canonical model contract、Session 和
durable Run 的权威实现。Workbench 等调用方只能通过公开入口集成，不直接读写
`${SN_CLI_HOME:-~/.sn}`。

执行面固定为：

- `sn-cli <cli-profile-id>`：CLI Profile 的交互 TUI 调用，不创建 Session/Run；
- `sn-cli exec <cli-profile-id>`：CLI Profile 的非交互任务调用，不创建 Session/Run；
- `sn-cli call <api-profile-id>`：API Profile 的单次请求，不创建 Session/Run；
- `sn-cli profile list|show|check`：Profile 只读管理面，不执行 Profile；
- `sn-cli auth`：认证 TUI，支持 `/login`、`/logout`、`/status`、`/help`、`/exit`；
  无 provider 的 `auth login|logout` 直接进入对应向导，仅用于交互终端；
- `sn-cli auth login|status|logout openai-codex`：Codex OAuth 直接命令管理面；
  凭证仅供 `openai-codex-responses` 单次调用使用，`status` 只读本地缓存；
- `sn-cli doctor`：本机就绪检查，不调用 Provider 或 MCP；
- `sn-cli session exec|call ...`：`interface=managed` 的文件型本地执行会话，
  `--queue` 时进入 durable Run；Session 不自动执行 tool；
- `sn-cli session open|send|attach|interrupt|close|close-all ...`：`interface=native_tui`，
  在 tmux PTY 中直接运行 Provider 原生交互 TUI；`open` 创建 opaque lifecycle
  Run/Execution，Provider 退出或 `close` 时收口；输入输出不创建 canonical
  Turn/Message/Event 或 transcript；公开发现和控制只使用 Session ID，tmux 仅是私有
  PTY carrier；
- `sn-cli agent <api-profile-id>`：唯一 API-only Agent Kernel，`--queue` 时排队；
- `sn-cli job ...`：SQLite durable Run 查询与控制面，不负责提交新 Run；
- `sn-cli server info|start|status|stop`：HTTP 与 durable worker 生命周期；
- `sn-cli update ...`：检查/下载/激活 Runtime release，不执行 Profile。

另有独立二进制 `sn-code`（`make sn-code-build` 源码构建，不进入 release payload）：
交互编程 TUI 与 `sn-code acp`（Agent Client Protocol v1 的 stdio JSON-RPC 接入，
见 `docs/ACP.md`）复用同一 AgentSession 内核，均不创建 canonical Session/Run。

所有有效 CLI/API Profile 都通过同一 Profile ID 路由，并由 `type=cli|api` 选择
执行 adapter。公开入口、配置、持久化事实和 machine output 只认当前 contract 的
完整 schema，不提供 alias、自动 migration、第二套 reader 或兼容 shim。

## 架构边界

- `pkg/{command,model,session,agent,run}` 是公开 Runtime 领域能力；
- `pkg/contract` 保存 Provider-neutral request/event/error；
- `pkg/provider/*`、`pkg/store/sqlite/`、`pkg/transport/http/` 是公开 adapter；
- `internal/domain/` 只保存私有值对象与不变量，不依赖其它层；
- `internal/application/` 保存激活、启动和 tool/use-case 编排；
- `internal/infrastructure/` 保存配置、文件、日志、进程、private tmux carrier、
  Provider HTTP helper 与 MCP adapter，不反向依赖 application/interfaces；
- `internal/interfaces/cli/` 只做 decode/call/encode，不建立第二套状态机；
- `internal/interfaces/codingacp/` 是 sn-code 的 ACP v1 stdio 协议投影：
  复用 codingagent 内核与确认门（映射 `session/request_permission`），不启动
  子进程、不建立第二套状态机；与 `executor=acp`（ACP client adapter）和 AEP
  是互不相干的协议；
- Auth TUI 的短生命周期流程由 `internal/application/authruntime` 编排，CLI 只做
  命令补全、选择和渲染；`internal/infrastructure/terminalinput` 管理可取消的 raw
  terminal I/O，不创建 Session/Run，不保存输入历史或认证日志；
- `internal/application/runtimebootstrap/` 只装配 Runtime Session/Agent/Run；
  `internal/application/codingbootstrap/` 只装配 sn-code 编程产品；两者依赖
  `internal/application/modelruntime/` 统一装配 Profile、Provider、认证与日志，
  共享底座不依赖任何产品专属模块，重试由产品装配层选择；
- `internal/application/serverruntime/` 编排 server 生命周期，
  `internal/infrastructure/serverprocess/` 管理进程身份、lease 与锁；CLI 仅解析和渲染；
- `internal/infrastructure/buildinfo/` 保存共享构建信息，产品名称由入口提供；
  sn-code 不依赖 CLI 界面包或 Runtime Session/Run 装配，Runtime 不依赖编程模块；
- `internal/infrastructure/executionlog/` 只保存 best-effort 本地执行诊断，不是
  canonical Store；
- Agent Kernel 不读取 profile/config、不打开数据库；
- Provider driver 只做一次 HTTP attempt，不执行 tool、fallback 或持久化；
- `internal/infrastructure/toolconfig/` 只加载严格 tool manifest；
  `internal/infrastructure/toolmcp/` 每次只执行一个声明 effect/risk 的 MCP tool call，
  不 retry、不持久化；
- Session 遇到 tool call 只进入 `requires_action`；
- `managed` 与 `native_tui` Session ID 不混用；Runtime Session ID 不推断
  Provider 原生 resume identity；
- Run terminal state、result/error、terminal event 和 `run.settled` 必须同事务提交。

外部 Go API 统一落在 `pkg/`；旧根 package 不提供兼容 shim。新增私有源码优先落入
`internal/{domain,application,infrastructure,interfaces}` 的现有 owner；测试复用资产
只进入 `internal/testkit/`。`pkg/` 下的每个 package 都必须是明确登记的公开 API 或
adapter，不得在其中嵌套仅供仓内使用的 `internal` package。不得在根目录新增运行态
目录、临时兼容 package 或职责不明的共享层。

## 配置与目录

source 与 release archive payload 使用同一配置布局：

```text
configs/*.json
resources/schema/*.json
resources/tools/*.json
release/runtime.json
release/tmux.conf
release/release.json
```

active home：

```text
configs/*.json
tools/*.json
runtime.json
resources/schema/*.json
resources/tmux.conf
resources/release.json
sessions/
logs/YYMMDD/{api,cli}.jsonl
state/runtime.db
state/auth.json
state/auth.lock
```

activation 显式映射 `configs/ → configs/`、`resources/tools/ → tools/`、
`release/runtime.json → runtime.json`、`resources/schema/ → resources/schema/`，
以及 `release/{tmux.conf,release.json} → resources/{tmux.conf,release.json}`；不得把
source/payload 路径当成 active home 路径，也不得反向从 active home 生成 source。
未来 `skills/`、`mcp/` 等可交付资产只能扩展在 source/payload `resources/` 下，
不得重新增加仓库或 archive 顶层配置根。

`configs/*.json` 是唯一 Profile 配置面，必须用 `type=cli|api` 显式分流到独立
执行领域；不存在第二层 command ID 映射。args 一字符串一 argv token，API key
只从环境变量解析。Codex OAuth 由 `sn-cli auth` 独立管理，在单次调用时注入；凭证
存于 active `state/auth.json`（0600，目录 0700），不进入 Profile、日志、Store 或发布包，
安装和升级保留；不读取 Codex CLI/Pi 的凭证。loader 严格拒绝当前 schema 之外的字段和缺失的领域标识。

source/payload `resources/tools/*.json` 经 activation 映射到 active
`tools/*.json`，后者是 Agent 外部工具的唯一运行配置面；文件 basename 必须等于
tool `name`。`effect` 为 `read_only`/`write_local`/`write_external` 三档之一
（写副作用必须显式声明 `risk`），`executor.type=mcp`，secret 只保留
`${VAR}` 引用并在实际 tool call 时解析。`web_search` 与 `web_fetch` 是
`write_external` / `high`，不在默认工具列表中；启用后未确认不得发起外部请求。
`call` 和 Session 不自动执行这里的工具。

只有携带 Profile ID 并进入真实 CLI launch/API Provider call 的执行才写本地日志；
查询、校验和 queue submit 不写。API key 只保留 `${VAR}` 引用；resolved secret 只
存在于 HTTP 请求层，事件、结果与返回错误面向消费者原样传递，仅本地日志与诊断
observer 面做结构感知脱敏（只替换字符串值，不改写 JSON key）。日志写入、锁冲突或
路径错误不得改变执行结果，也不与 Session/Run 建事务；当前不自动 GC，也不迁移旧
flat log。

## 同步门禁

修改公开命令、profile 分流、Session/Agent/Run 语义、HTTP route、event 或 Store
barrier 时，同步：

- 对应领域测试；
- `sn-cli --help`；
- `README.md` 与 `README.zh-CN.md`；
- `SN-CLI-USAGE.md`；
- 唯一契约文档 `docs/runtime-contract.md`；
- install/self-update/release payload（如影响交付物）。

## 验证

普通变更运行最小相关测试。架构、契约、安装或发布变更至少运行：

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make release-check SN_CLI_VERSION=<valid-semver>
git diff --check
```

测试和 release check 只使用临时 `SN_CLI_HOME`，不得修改 active `~/.sn`。Git
提交与 push 只在用户明确要求时执行。
