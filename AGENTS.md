# Runtime 协作约定

## 项目定位

本仓库是 SN Runtime 公开 CLI、HTTP、canonical model contract、Session 和
durable Run 的权威实现。Workbench 等调用方只能通过公开入口集成，不直接读写
`${SN_CLI_HOME:-~/.sn}`。

执行面固定为：

- `sn-cli <cli-profile-id>`：CLI Profile 的 direct 交互调用，不创建 Session/Run；
- `sn-cli exec <cli-profile-id>`：CLI Profile 的非交互任务调用，不创建 Session/Run；
- `sn-cli req <api-profile-id>`：API Profile 的单次请求，不创建 Session/Run；
- `sn-cli profile list|show|check`：Profile 只读管理面，不执行 Profile；
- `sn-cli session exec|req ...`：文件型本地执行会话，`--queue` 时进入 durable
  Run；Session 不自动执行 tool；
- `sn-cli tmux ...`：固定专用 tmux server/window 的交互进程管理，不进入 Session；
- `sn-cli agent <api-profile-id>`：唯一 API-only Agent Kernel，`--queue` 时排队；
- `sn-cli run ...`：SQLite durable Run 查询与控制面，不负责提交新 Run。

所有有效 CLI/API Profile 都通过同一 Profile ID 路由，并由 `type=cli|api` 选择
执行 adapter。公开入口、配置、持久化事实和 machine output 只认当前 contract 的
完整 schema，不提供 alias、自动 migration、第二套 reader 或兼容 shim。

## 架构边界

- `command/`、`model/`、`session/`、`tmux/`、`agent/`、`run/` 是独立领域；
- `contract/` 保存 Provider-neutral request/event/error；
- `provider/*`、`store/sqlite/`、`transport/*` 是 adapter；
- `internal/executionlog/` 只保存 best-effort 本地执行诊断，不是 canonical Store；
- `internal/runtimebootstrap/` 是 composition root；
- `internal/cli/` 只做 decode/call/encode，不建立第二套状态机；
- Agent Kernel 不读取 profile/config、不打开数据库；
- Provider driver 只做一次 HTTP attempt，不执行 tool、fallback 或持久化；
- `internal/toolconfig/` 只加载严格 tool manifest；`internal/toolmcp/` 每次只执行
  一个只读 MCP tool call，不 retry、不持久化；
- Session 遇到 tool call 只进入 `requires_action`；
- Run terminal state、result/error、terminal event 和 `run.settled` 必须同事务提交。

新增源码优先落入现有 owner。不得在根目录新增运行态目录、临时兼容 package 或
职责不明的共享层。

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
```

activation 显式映射 `configs/ → configs/`、`resources/tools/ → tools/`、
`release/runtime.json → runtime.json`、`resources/schema/ → resources/schema/`，
以及 `release/{tmux.conf,release.json} → resources/{tmux.conf,release.json}`；不得把
source/payload 路径当成 active home 路径，也不得反向从 active home 生成 source。
未来 `skills/`、`mcp/` 等可交付资产只能扩展在 source/payload `resources/` 下，
不得重新增加仓库或 archive 顶层配置根。

`configs/*.json` 是唯一 Profile 配置面，必须用 `type=cli|api` 显式分流到独立
执行领域；不存在第二层 command ID 映射。args 一字符串一 argv token，secret
只从环境变量解析。loader 严格拒绝当前 schema 之外的字段和缺失的领域标识。

source/payload `resources/tools/*.json` 经 activation 映射到 active
`tools/*.json`，后者是 Agent 外部工具的唯一运行配置面；文件 basename 必须等于
tool `name`。当前只支持 `effect=read_only`、`executor.type=mcp`，secret 只保留
`${VAR}` 引用并在实际 tool call 时解析。`req` 和 Session 不自动执行这里的工具。

只有携带 Profile ID 并进入真实 CLI launch/API Provider call 的执行才写本地日志；
查询、校验和 queue submit 不写。API key 只保留 `${VAR}` 引用，resolved secret 必须
脱敏。日志写入、锁冲突或路径错误不得改变执行结果，也不与 Session/Run 建事务；当前
不自动 GC，也不迁移旧 flat log。

## 同步门禁

修改公开命令、profile 分流、Session/Agent/Run 语义、HTTP route、event 或 Store
barrier 时，同步：

- 对应领域测试；
- `sn-cli --help`；
- `README.md`；
- `docs/runtime-contract.md` 及相关专题契约；
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
