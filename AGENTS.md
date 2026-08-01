# Runtime 协作约定

## 项目定位

本仓库是 Runtime vNext 公开 CLI、HTTP、canonical model contract、Session 和
durable Run 的权威实现。Workbench 等调用方只能通过公开入口集成，不直接读写
`${SN_CLI_HOME:-~/.sn}`。

执行面固定为：

- `sn-cli <profile-id>`：隐式 Profile 一次调用，不记录；
- `sn-cli profile <profile-id>`：与隐式入口完全等价的一次 command 或 model
  调用，不记录；
- `sn-cli session ...`：文件型本地执行会话，不自动执行 tool；
- `sn-cli tmux ...`：固定专用 tmux server/window 的交互进程管理，不进入 Session；
- `sn-cli agent run`：唯一 API-only Agent Kernel；
- `sn-cli run ...`：SQLite durable Run 控制面。

所有有效 CLI/API Profile 都通过同一 Profile ID 路由，并由 `type=cli|api` 选择
执行 adapter。公开入口、配置、持久化事实和 machine output 只认当前 contract 的
完整 schema，不提供 alias、自动 migration、第二套 reader 或兼容 shim。

## 架构边界

- `command/`、`model/`、`session/`、`tmux/`、`agent/`、`run/` 是独立领域；
- `contract/` 保存 Provider-neutral request/event/error；
- `provider/*`、`store/sqlite/`、`transport/*` 是 adapter；
- `internal/runtimebootstrap/` 是 composition root；
- `internal/cli/` 只做 decode/call/encode，不建立第二套状态机；
- Agent Kernel 不读取 profile/config、不打开数据库；
- Provider driver 只做一次 HTTP attempt，不执行 tool、fallback 或持久化；
- Session 遇到 tool call 只进入 `requires_action`；
- Run terminal state、result/error、terminal event 和 `run.settled` 必须同事务提交。

新增源码优先落入现有 owner。不得在根目录新增运行态目录、临时兼容 package 或
职责不明的共享层。

## 配置与目录

source：

```text
configs/*.json
configs/runtime/runtime.json
resources/schema/*.json
```

active home：

```text
configs/
runtime.json
resources/
sessions/
state/runtime.db
```

`configs/*.json` 是唯一 Profile 配置面，必须用 `type=cli|api` 显式分流到独立
执行领域；不存在第二层 command ID 映射。args 一字符串一 argv token，secret
只从环境变量解析。loader 严格拒绝当前 schema 之外的字段和缺失的领域标识。

## 同步门禁

修改公开命令、profile 分流、Session/Agent/Run 语义、HTTP route、event 或 Store
barrier 时，同步：

- 对应领域测试；
- `sn-cli --help`；
- `README.md`；
- `docs/runtime-vnext-contract.md` 及相关专题契约；
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
