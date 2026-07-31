# Runtime vNext 仓库治理清单

## 1. 文档定位

本文不是新的产品契约，也不替代 `SN-CLI-USAGE.md`。它只用于追踪本轮整仓治理：

- 哪些能力已经实现并应保留；
- 哪些能力只实现了底层状态机，公开入口仍不可用；
- 哪些旧能力已经废弃，应继续拒绝或清理残留；
- 哪些代码虽然包含 legacy/obsolete 命名，但仍承担升级安全职责；
- 每一批治理的修改范围、验收条件和交付顺序。

事实冲突时按以下顺序处理：

1. 当前源码、loader、Store schema 和测试；
2. `docs/runtime-vnext-contract.md` 及对应专题契约；
3. `SN-CLI-USAGE.md`；
4. `documents/designs/contracts/` 下的设计记录；
5. `outputs/design/` 下的历史材料。

本文是执行清单，不是事实来源。治理完成后，功能事实必须回写到正式契约和
`SN-CLI-USAGE.md`，不能只更新本文。

## 2. 审计基线

审计基线：

```text
HEAD: c8c3fde
Target: 当前未提交工作树
```

当前总状态：治理修改仍在未提交工作树中；实现与 release gate 已完成，
Git 交付等待用户另行授权。

当前工作树包含尚未进入 HEAD 的正确性修复和新增文档、配置。治理过程中必须保留并
整体交付，不能以 HEAD 覆盖：

- Run startup recovery 只由 `sn-server` 执行；
- activation preflight 读取完整 Session/Execution fact；
- Session Store 忽略自身 atomic-write 临时文件；
- durable CLI Session snapshot 保存 effective model/effort；
- durable Agent private execution snapshot、combined digest 与 pre-effect drift gate；
- `SN-CLI-USAGE.md`；
- `configs/cx-remote.json` 及配套测试、release payload。

2026-07-31 当前工作树重新通过：

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make release-check SN_CLI_VERSION=v0.0.0-governance.2
git diff --check
```

另已执行 `go test ./...` 作为整仓快速复核。以上结果属于当前未提交工作树；
不等于已经 commit、push 或安装到 active `~/.sn`。

## 3. 当前领域状态

| 领域 | 当前判定 | 治理方向 |
|---|---|---|
| Profile / Command | 主链完成 | 门禁通过 |
| Model / Provider | semantic identity 与 execution snapshot 已闭合 | 门禁通过 |
| Session | blocked/reconciliation、public tool 边界和 GC 已闭合 | 门禁通过 |
| Tmux | 主链完成 | 门禁通过，保持独立 interactive process manager |
| Agent Kernel | frozen validation、pre-effect gate 和 pause 边界已闭合 | 门禁通过 |
| Durable Run | private snapshot、跨进程 cancel 和 agent cwd 已闭合 | 门禁通过 |
| HTTP | validation、not-found、route 和 capability 已同步 | 门禁通过 |
| Server / Activation | 原子交付和 release 验证已闭合 | 门禁通过 |
| Schema / 文档 | 权威层级与 schema/loader parity 已闭合 | 门禁通过 |

## 4. P0：发布前必须关闭

### GOV-P0-01 Agent Session reconciliation 死锁

状态：已完成并通过整仓 release gate。

已采用第一种设计并完成：

- Agent 执行前在 Turn 原子保存 `agent_owned=true` owner marker；
- `needs_reconciliation` 保留 active Turn，Execution outcome 为 unknown；
- 唯一公开恢复入口为 `run reconcile`，不 replay Agent 或 tool；
- Session projection 先按精确 `run_id` 幂等收口，SQLite terminal publish 后置；
- SQLite publish 失败时可重试，且不覆盖后来创建的 active/blocked Turn；
- unknown `tool_effects`、checkpoint 和 event evidence 均保留；
- `paused` 仍只由 `run resume` 恢复；
- 普通 terminal Agent Run 不伪装成 reconciliation 幂等结果。

已增加 crash-before-projection、terminal publish fail、later active/blocked Turn、
pause/resume 和重复 reconcile 回归测试。

### GOV-P0-02 形成一致的可交付基线

状态：已完成并通过整仓 release gate。

- 当前工作树含多项关键正确性修复；
- `SN-CLI-USAGE.md` 和 `configs/cx-remote.json` 尚未跟踪；
- HEAD、工作树和未来 release payload 的行为不一致。

已完成：

- 保留现有工作树修改；
- 将代码、测试、文档、配置和 release payload 作为同一批次验证；
- 当前工作树完成第 11 节完整门禁；HEAD 仍未包含这些修改，不把 HEAD 误标为已交付。

本轮 activation 门禁已额外收紧：完整 payload Profile 语义、
required/no-follow runtime、Tmux resource 和具有固定 identity/root shape 的可编译
Schema 均在任何 target mutation/lock/stage/停服前验证；merged staged home 再
二次复检。非法 payload 不会创建 target state，也不会停止现有 server。

验收：

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make release-check SN_CLI_VERSION=<valid-semver>
git diff --check
```

本项不授权自动 commit 或 push。

### GOV-P0-03 Session JSONL 与 fact crash consistency

状态：已完成并通过整仓 release gate。

已增加私有 per-Session `mutation_version=3` undo journal，不改变 canonical
Session `schema_version=2`：

- replace fact 首次写入前保存原始 bytes；JSONL rewrite 前保存原始
  `size + SHA-256 prefix_digest`；journal 先 fsync，目标后写；
- JSONL 不做原地 append，而是追加完整 line 后 atomic full-file rewrite；每次
  publication 的新 inode 都持久登记为 mutation-owned identity；
- 全部 fact 成功后先持久化 committed marker，再删除 journal 并同步目录；
- prepared crash 在同一 Session `flock` 内恢复 replace preimage；JSONL 只有在
  当前 inode 属于 mutation、当前长度不少于原始 size 且 prefix digest 匹配时，
  才 atomic rewrite 回原前缀；committed crash 保留结果并清理 journal；
- 新 Session atomic write 遗留的 Runtime-owned temp 只在已登记 replace target
  目录内、且完整 scope 校验通过后清理；任何额外或不安全内容均保留 facts 和
  journal 并 fail closed；
- 新 Session journal 使用随机 nonce 与 root owner marker durable 绑定；缺 marker、
  nonce 不匹配或 forged journal 即使枚举全部 facts 也不能触发删除；
- committed journal rename 成功但目录 fsync 报错时，先 strict/no-follow 重读并
  比较完整 mutation identity；可证明 committed 时永不回滚，prepared 才回滚，
  歧义状态保留证据并 fail closed；
- `NewStore` 在 strict schema/layout/count 校验前逐 Session 持锁 recovery，普通
  Session 操作在开始新 mutation 前也会恢复遗留 journal；
- recovery 不调用 Provider、不执行 command/tool，不伪造与 SQLite durable Run
  的 transaction；
- Session filesystem 使用 pinned directory FD、逐组件 `O_NOFOLLOW`、single-link
  regular-file 和 device/inode 门禁；删除前移入随机 private quarantine 并复核
  inode，不匹配时尝试恢复原名并 fail closed；
- delete/GC 使用 `state/session-trash-moves/` private `version=1` journal，绑定
  source root identity，以 no-replace rename 移入固定 trash shape；恢复后先重建
  index，再清理 journal；
- managed CLI invocation 使用 `state/session-invocations/` 中 identity-bound、
  mode `0600`、消费即删除的 private manifest；只清理超过 24 小时且可证明
  Runtime-owned 的遗留文件；
- 本地源码 activation reset 同步清理 `sessions/`、`state/session-locks/`、
  `state/session-invocations/`、`state/session-mutations/`、
  `state/session-trash-moves/` 和 `state/runtime.db*`。

safe-fs 威胁模型覆盖 symlink/hardlink、路径替换、确定性并发 swap 和 crash，不
宣称抵抗已获同 UID 任意代码执行、可持续枚举随机 quarantine 名称或使用
ptrace/kill 的攻击者；POSIX 不提供 compare-by-inode unlink。

长驻进程内的 finalization 写入失败也已收口：若 committed facts 已存在，返回事实
终态；若精确相关 execution 仍为 running，则原子转为 `SessionBlocked` 并保留
active Turn/Execution，durable Session Run 进入 `needs_reconciliation`，避免既
伪报 failed 又永久卡在 active。

已增加真实 helper subprocess exit failpoint，覆盖 JSONL append 后 crash、
JSONL 半行写入后 crash、committed-marker 后 crash、新 Session 首个 fact 后
crash、owner marker 已清理但 journal 尚未清理时 crash、多 fact 中途 crash、
callback error rollback、committed journal rename 前错误回滚、rename 后 durability
ack 丢失时保留事实、重复 recovery no-op、并发 `NewStore` 等待 active writer 和
恶意越界 journal fail-closed。

### GOV-P0-04 Agent durable execution snapshot

状态：当前工作树已实现，等待本轮验证与整仓 release gate。

本轮不再把 durable Agent 的 `profile_id` 当作可在恢复时重新解释的完整执行配置。
`AgentExecutor.Prepare` 在 Run 创建前冻结 Store-private、canonical、non-secret
execution snapshot：

```text
execution_contract_version
model_execution_snapshot
tool_execution_snapshot
tool_execution_digest
session_request_digest       # optional
session_config_digest        # optional
config_digest
request_digest
```

落地约束：

- model snapshot 包含完整 API Profile、Profile digest 和 concrete Provider driver
  implementation/version；tool snapshot 包含 implementation/version、canonical
  non-secret config 和 definitions；
- `config_digest` 绑定 Agent contract、model/Provider、tool 和可选 Session config；
  `request_digest` 再绑定 immutable public Agent request 和可选 Session request；
- Agent-bound Session 保留独立的 profile-only request/config digest，不能与
  combined Run digest 混用；
- `auth.from_env` 只冻结变量名，不保存 resolved secret value；同名 secret rotation
  不构成 drift；
- fresh submit 与 Retry 在创建 Run 前比较 current snapshot；每个新
  Session/model/tool side effect 和 active pause Resume 推进前再次比较；
- Retry byte-for-byte 保留原 private payload，不 re-freeze；drift 时不创建新 Run；
- durable completed/failed/started effect、pause closure、known terminal
  projection、cancel 和 reconcile 只消费 frozen snapshot 与 durable evidence，
  不要求 current Profile/Provider/tool；
- private payload strict decode、canonical/digest/size 自校验，并以 `json:"-"`
  排除在公共 DTO、event、log 和 error 之外。

该门禁只检测 snapshot 已表示的配置和 semantic identity 漂移。private payload
不是密文；semantic version 需要人工正确 bump；binary provenance、OS sandbox、
同 UID hostile process 和 SQLite 篡改不在保证范围内。

验收要求：

- Profile、driver identity、tool definitions/enabled set、roots/cwd 和 tool
  implementation drift 在任何新副作用前 fail closed；
- secret value rotation 继续执行，`from_env` 名称变化拒绝；
- terminal recovery、cancel、reconcile 和 Session terminal projection 在 current
  execution dependencies 缺失时仍收敛；
- public Run/Session/HTTP/CLI output 不泄露 private snapshot；
- Retry 保留原 snapshot，drift 不产生新 Run。

## 5. P1：功能与契约闭环

### 已确认并执行的五项公开决策

用户已于 2026-07-31 整体确认“1–5 全部按候选执行”。当前工作树已实施：

1. 移除 public `session tool-result`，内部保留 `requires_action` 状态机；
2. pause/resume 只作为 Kernel extension，移除 stock binary 的 resume capability
   claim，但保留底层 `run resume` CLI/API/state；
3. server-owned `running` Run 的跨进程 `run cancel` 使用 SQLite polling；
4. 拒绝 `run submit --kind agent --cwd`，不再接受后静默忽略；
5. 移除 builtin `exec_command`，因为当前 workspace-root/path 门禁不是真实 sandbox。

代码、测试、schema、README、使用手册和正式契约已经同步，并通过本轮完整
release gate。

### GOV-P1-01 Session tool workflow

状态：已完成并通过整仓 release gate。

- Session 内部存在 `requires_action` 和 `tool-result` 状态机；
- 公开 Session CLI、HTTP DTO 和 `RunRequest` 没有 tools；
- 真实 Provider 通常不会在未声明 tools 时返回合法 tool call；
- `tool-result --error` 的 `IsError` 历史投影问题已修复：canonical Message 保留
  `is_error`，并映射到 Anthropic `tool_result.is_error`；
- CLI `session tool-result` 与 HTTP
  `/v1/sessions/{id}/turns/{turn_id}/tool-results` 已删除；
- 内部 Session service/state projection 保留，供领域测试和未来显式 extension；
- 使用手册明确 stock Session 不是手工 tool loop，自动 tool loop 属于 Agent。

验收：

- CLI/HTTP 已删除路由返回 unknown/not-found 且不创建或修改 Session；
- canonical internal tool result 的 `is_error` Provider 投影测试继续保留；
- help、README、使用手册和正式契约不再发布该入口。

### GOV-P1-02 Agent pause/resume 产品边界

状态：已完成并通过整仓 release gate。

- Kernel、Run Store 和 `run resume` 支持 pause/resume；
- 当前内置工具不会返回 `Pause`；
- 没有公开自定义 ToolExecutor 注册入口；
- `server info` 已移除 stock `run.resume` capability；
- 底层 `run resume` CLI/API/state、strict envelope、validator 与已有测试保留。

验收：

- 文档和 `server info` capability 不得超出公开可达能力；
- Kernel/Run 的 pause/resume 领域和 CLI/HTTP control plane 测试继续通过。

### GOV-P1-03 Session GC 原子复检

状态：已完成。

修复前：

- GC 在锁外选择 ephemeral/idle/expired 候选；
- apply 时调用 `Delete`，锁内只复检 active/blocked；
- 并发改为 pinned 或更新时间变化后仍可能被移动到 trash。

目标：

- apply 时在 Session lock 内重新检查 retention、state 和 cutoff；
- 候选已变化时跳过，不应使整批 GC 失败；
- dry-run 与 apply 输出应区分 candidate、moved、skipped。

验收：

- 增加 configure/GC 并发回归测试；
- pinned、active、blocked、更新时间已刷新的 Session 均不得被移动。

落地结果：

- apply 在 Session lock 内重新检查 `retention=ephemeral`、`state=idle` 和
  `updated_at < cutoff`；
- 候选变化或已被其他操作移动时记入 `skipped`，继续处理整批；
- `GCResult.skipped` 使用 `omitempty`，保持既有 JSON consumer 兼容；
- 回归测试覆盖 pinned、active、blocked、`updated_at` 刷新和仍有效候选。

### GOV-P1-04 Run 控制面语义

状态：已完成并通过整仓 release gate。

- server-owned running Run 的 owner worker 轮询 SQLite cancellation reservation，
  独立 CLI/HTTP 进程均可中断 execution context；
- ordinary terminal/reconciliation publish 拒绝已有 reservation，只有
  cancellation-owned finalizer 可以消费，关闭 poll 与 terminal publish 竞态；
- `run submit --kind agent --cwd`、HTTP Agent Run `cwd` 和内部 Agent Request
  `cwd` 均在副作用前拒绝；Session `cwd` 保持不变；
- Agent 显式 reconciliation executor 已实现：保留 unknown tool-effect evidence，
  如绑定 Session 则先幂等收口 Session projection，再将 Run 结案为 failed。

已完成项：

- Agent `Reconcile` 已接入 Run control plane，覆盖无 Session、绑定 Session、崩溃后
  Session projection 恢复和重复调用幂等；本节剩余目标不再包含 Agent
  reconciliation executor；
- queued/paused cancellation 已通过 durable reservation、kind-specific
  finalizer、terminal barrier 和 startup keyset drain 闭环；
- 双 SQLite Store/Service 回归测试覆盖 server worker 执行、独立 control
  Service 取消以及最终 terminal barrier。

### GOV-P1-05 Builtin exec sandbox 边界

状态：已完成并通过整仓 release gate。

- builtin registry 和 handler 已移除 `exec_command`；
- runtime loader 与 `runtime.schema.json` 只允许
  `read_file|list_directory|write_file`；
- 旧配置包含 `exec_command` 时 fail closed，不静默忽略；
- tool implementation semantic version 已 bump，旧 frozen snapshot 不会错误绑定到
  新 builtin 实现。

### GOV-P1-06 CLI / HTTP validation parity

状态：已完成并通过整仓 release gate。

统一以下规则：

- Agent budget 上限；
- Run/Session state、kind、limit filter；
- ID 不存在时的 `404/not_found`；
- request size、UTF-8、NUL；
- strict JSON：未知字段、尾随 JSON、EOF；
- duration 和数字边界；
- RuntimeError 到 HTTP status 的映射。

验收：

- 为同一非法输入建立 CLI/HTTP table test；
- 不存在的 Session/Run 查询不能一部分返回 404、一部分返回空集合；
- `--request-file` 复用 `internal/strictjson`。

落地结果：

- `internal/strictjson` 统一限制大小、单一 JSON document、重复字段和 UTF-8；
  固定 Runtime DTO 使用 non-null object 且拒绝全部显式 `null`，model generate
  使用 strict object + canonical validation；run resume 使用只含 `pause_id` 与
  `input` 的 strict envelope，只有 `input` 由 pause schema 决定 shape/null；
- canonical text 统一拒绝 invalid UTF-8/NUL；
- CLI 与 HTTP 对 Session/Run ID、filter、budget、duration、GC limit 和 request
  body 使用同一边界；
- CLI 参数、ID 和未知 option 通过显式 validation marker 输出
  `invalid_request/request`，未标记的 Store/I/O 错误仍保持 `internal`；
- 新增 canonical `not_found`，HTTP 使用 `400/404/409/500` 区分非法 ID、不存在、
  状态冲突和 Store 故障；
- Run cancel body 固定为 strict `{}`，GC omission 与显式 `0` 不再混淆；
- HTTP Session 子路由严格匹配 path arity；Run cancel/reconcile 的空 object body
  拒绝 `null`、array 和任何字段，且校验失败不会产生 mutation；
- Bearer auth 失败返回 canonical RuntimeError envelope。

### GOV-P1-07 Schema 与 runtime loader 一致性

状态：已完成并通过整仓 release gate。

已明确双层规范：

- JSON Schema 是结构、枚举、字符串 grammar 和可表达数值边界的权威层；
- Go loader 是 filesystem、URL 语义、清理后重复值和 duration 总量等语义规则的
  权威层；
- Profile/runtime 使用共享 valid/invalid fixture 同时验证 Schema 与 loader，
  只允许显式标注的 semantic-only rule 在 Schema 通过后由 loader 拒绝；
- command basename、endpoint、header/ref、effort、duration、int64 和 context
  arithmetic 已收紧；Profile load 直接执行 command semantic check；
- activation 会 strict decode、编译并复检 release 中的两个 Schema。

## 6. P2：架构解耦与一致性

### GOV-P2-01 Bootstrap 分层

状态：已完成并通过整仓 release gate。

已拆分：

```text
ProfileServices
SessionMaintenanceServices
SessionServices
RunQueryServices
RunMaintenanceServices
Services
```

Profile 查询与直接调用不加载 `runtime.json`；Session 查询、导出、删除、GC 和
reconcile 不加载 Profile/Provider/SQLite。Run query 只加载 SQLite；Run
cancel/reconcile 只加载 Run Store 与 Session maintenance，且通过 private snapshot
恢复，不要求 current Profile/Provider/tool。`run submit|resume|retry`、Session
execution 和 server worker 才加载各自所需的完整执行依赖。

### GOV-P2-02 Doctor 使用真实 command resolver

状态：已完成。`server doctor` 复用 command invocation 的 cwd/env/PATH/ref
resolver，并区分 command 不存在与 Profile 配置无效。

### GOV-P2-03 Machine output 稳定性

状态：已完成并通过整仓 release gate。

已固定 `profile.Entry` 等嵌套 DTO 的 JSON 字段名，删除测试专用输出分支，统一
human/machine resource error；`server info` 不再宣称 stock resume capability。

## 7. 已弃用能力

以下能力继续保持“不兼容、不恢复”：

```text
profile exec
profile open
session send
session attach
session interrupt
session stop
legacy commands/*.json 配置层
legacy command-ID shortcut
legacy runtime.yaml
legacy 无 type Profile
legacy Profile.binary
legacy Profile.transport
legacy Profile.prompt_delivery
legacy Profile.effort_adapter
legacy Profile.launch
legacy Profile --interactive option
legacy Session --prompt-file/--session-file/--terminal-driver/--command-arg/--launch options
legacy namespace
legacy artifact reader
legacy compatibility shim
```

这里的 `command-ID shortcut` 专指第二层 command ID 映射，不包括正式入口
`sn-cli <profile-id>`；`Profile.transport` 专指已删除的 Profile 字段，不包括当前
`transport/http` adapter。相关 CLI 应返回明确错误；不增加 fallback。

## 8. 清理候选

### 已删除或 internalize

- `model.LoadProfileDir`
- `model.DecodeProfile`
- `model.LoadProfileFile`
- 仅测试使用的 `runUpdateVNext`
- 无调用方的 `Service.LatestExecution`
- `outputs/design/profile-session-command-protocol.md` 历史讨论文件
- 无生产 owner 的 `transport/cli/generate.go`
- 从未成为合法 Store fact 的 `TurnPending`

以下字段经复核继续保留：

- `SessionArchived` 已参与 Store、reconcile、HTTP 和 activation 校验；
- `ContextManifest.CheckpointRef/CheckpointDigest` 属于 `schema_version=2`，本轮不
  以 dead-code 清理破坏持久化兼容边界。

### 不能按旧名称直接删除

- `RequireLegacyProfileListGate`：阻断旧 updater 激活 contract-v3 candidate；
- activation `commands` tombstone：负责旧目录迁移、回滚和 journal recovery；
- `__sn_tmux_helper`：Tmux ready/go launch protocol；
- `server upgrade-activate`：安装和升级内部入口。

这些代码不是公开旧功能，而是当前安全协议的一部分。

## 9. 文档治理

### 正式契约

保留：

```text
docs/runtime-vnext-contract.md
docs/cli-routing-contract.md
docs/configuration.md
docs/session-history-contract.md
docs/tmux-contract.md
docs/integration-arch.md
```

专题契约只描述自身 owner，不重复整套 CLI 使用说明。

### 用户手册

`SN-CLI-USAGE.md` 只描述当前二进制可从公开入口实际使用的能力。上文固定五项公开
决策在用户确认前必须继续按当前实现描述，并明确候选方向不是当前行为。已稳定的
resume envelope、queued/paused cancellation、Session/CLI input 上限、状态枚举、
Agent reconciliation、HTTP validation 与 not-found 语义应直接与源码和正式契约
同步，不再列为开放问题。

### 历史设计

`outputs/design/` 不作为当前契约。需要保留的设计决策迁移为明确 ADR；其余历史讨论
可以删除，不在 README 或正式契约中引用。

## 10. 执行批次

### G0：交付基线

- 保留当前工作树修复；
- 收拢手册、`cx-remote`、tests 和 release payload；
- 执行完整 release gate。

### G1：状态机安全

- GOV-P0-01 Agent Session reconciliation；
- GOV-P0-04 Agent durable execution snapshot；
- GOV-P1-03 Session GC（已完成）；
- GOV-P1-04 Agent reconciliation 部分。

### G2：公开能力闭环

- GOV-P1-01 Session tools（已完成）；
- GOV-P1-02 Agent pause/resume（已完成）；
- GOV-P1-04 cancel 和 agent cwd（已完成）；
- GOV-P1-05 builtin `exec_command`（已完成）。

### G3：契约一致性

- GOV-P1-06 CLI/HTTP validation（已完成）；
- GOV-P1-07 Schema parity（已完成）；
- machine output 和 capability（已完成）。

### G4：架构与清理

- Bootstrap 分层（已完成）；
- Doctor resolver（已完成）；
- 删除已确认的 dead/unreachable surface（已完成）；
- 收拢正式文档和历史设计（已完成，公开决策内容除外）。

每批独立修改、独立验证。不得以“大清理”为由同时改动未确认的状态机语义。

## 11. 完成定义

整仓治理完成必须同时满足：

- 所有公开命令在实现、`--help`、README、`SN-CLI-USAGE.md` 和契约中一致；
- 所有 advertised capability 都能从 stock CLI 或 HTTP 实际触达；
- 任意非 terminal 状态都有唯一、幂等、可测试的恢复或终止路径；
- Durable Agent 的新 side effect 必须通过 frozen/current snapshot gate，已知
  terminal、cancel 和 reconcile 不得依赖 current execution config；
- Agent Run combined digest 与 Session profile-only digest 保持独立，private
  snapshot 不进入任何公共输出；
- CLI、HTTP 和 Store 对相同输入使用相同 validation 规则；
- JSON Schema 与 Go loader 使用共享 fixture 验证；
- 无生产调用链的 package、helper 和状态字段已删除或明确标记扩展接口；
- legacy activation 安全门禁仍完整；
- 完整 release gate 通过；
- 没有自动 commit 或 push，除非用户另行明确授权。
