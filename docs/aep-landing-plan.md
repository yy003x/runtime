# AEP（Agent Execution Protocol）落地计划

**文档性质：** 实施计划，不是契约。实施过程中所有语义变更必须同步进入唯一契约
`docs/runtime-contract.md` 并 bump `contract_version`；本文与契约冲突时以契约为准。

**上游规范：** AEP 规范 `0.1.0-draft`（stdio + JSON-RPC 2.0 + NDJSON）。规范原文在
阶段 0 落入 `docs/aep/spec.md`（见 P0-1）；在此之前不得开始 P1 编码。

**执行会话开工须知：**

1. 先读 `AGENTS.md` 与 `docs/runtime-contract.md` 全文，遵守同步门禁与验证要求。
2. 按阶段顺序执行，P0 交付物不齐不进入 P1；每阶段末尾运行本文「验证门禁」一节命令。
3. 阶段内任务可并行，阶段间不可。
4. 决策 D1–D11 已带推荐项；标「需用户确认」的必须先取得用户确认。

---

## 0. 背景结论（已完成的能力核验）

sn-runtime 已实现 AEP 要求的大部分执行层基础设施，AEP 落地定位是**新增第三个
inbound adapter（协议投影层）+ 少量执行器扩展**，不是新建平行 Runtime：

| AEP 要求 | sn-runtime 现状 | 判定 |
|---|---|---|
| 多执行器 adapter 框架 | `pkg/command/` 按 basename 登记 Codex/Claude/Grok | ✅ 缺 PI |
| 统一输入/参数/cwd | CLI Profile + `Overrides{Model,Effort,CWD}` | ✅ |
| 状态查询 | durable Run 状态机 + `pkg/run/query_service.go` | ✅ |
| 事件持久化与续读 | `events(run_id, sequence, event_json)` + `after_seq` | ✅ 直接复用 |
| 取消/进程树 | cancellation reservation + TERM→KILL + 进程组 | ✅ |
| 终态 result/error | terminal publish barrier + retry | ✅ |
| stdio JSON-RPC 入站 | 仅 CLI + HTTP（`pkg/transport/http/`） | ❌ 新增 |
| 流式事件（`message.delta` 等） | CLI 子进程中间过程 opaque，仅终态 canonical decode | ❌ **最大缺口** |
| PI 执行器 | 未登记 | ❌ 新增 |
| ACP Adapter / TS SDK | 无 | ❌ 后置 |

## 1. 目标与非目标

**目标：** 在 sn-runtime 内实现 AEP 规范的服务端：客户端以 stdio JSON-RPC 调起、
统一启动/查询/取消/收结果/补事件，覆盖 Codex、Claude Code、PI 三个执行器。

**非目标：** 不新建第二套 Run/Session 状态机；不改动 canonical Store 语义；
ACP Adapter 与 TypeScript Client SDK 后置（P5）；不做 ACP 之外的第二种兼容协议。

## 2. 关键设计决策

### D1 实现语言（已确认 2026-10-28）

AEP 规范原文写「实现语言：TypeScript」，本仓库是 Go。**已确认：服务端用 Go 落在
本仓库；TS 仅用于后置的 Client SDK（独立仓库）**。协议线上格式是 JSON，语言无关；
`docs/aep/spec.md` 实现语言条目已同步修订。理由：复用 10 万行已验证基础设施，
避免双状态 owner。

### D2 AEP 入口形态（已确认 2026-10-28）

**已确认：新增 CLI namespace `sn-cli aep`**，前台进程，stdin/stdout 承载 NDJSON
JSON-RPC，经 `internal/application/runtimebootstrap` 组装并**启动自有 scheduler
worker**（同 sn-server 的执行面，但不监听 HTTP）。备选：独立二进制 `cmd/aep-server`
（交付面 +1，activation/install/release 均需扩展），不推荐 MVP 采用。

理由：AEP 客户端期望「拉起即用」；queued session Run 需要 worker 执行
（契约：单次 `sn-cli` 命令不启动后台 worker，因此 `aep` 必须显式启动 worker）。

AEP server 生命周期语义（写入契约）：

- stdin 关闭或收到 shutdown 方法 → 对本进程 owner 的非终态 Run 走既有取消路径
  graceful 收口后退出；
- 进程异常退出 → 沿用既有启动恢复（running → `needs_reconciliation`），
  不新增第二条恢复通道；
- 与常驻 `sn-server` 并存：多个 worker 共享同一 SQLite 队列，原子领取语义
  需验证（P4-3）。

### D3 Executor ↔ Profile 解析（已确认 2026-10-28）

AEP `RunInput.executor ∈ {codex, claude-code, pi, acp}`，但本仓库的执行配置单元是
Profile（`configs/*.json`）。**已确认：AEP 请求增加必填 `profile` 字段（Profile ID），
`executor` 由该 Profile 的 `command` basename 推导并校验一致**；`executor=acp`
在 MVP 直接拒绝。

理由：Profile 是唯一配置面（AGENTS.md）；显式引用不引入第二层映射。

### D4 状态与字段映射

| AEP `RunStatus` | sn-runtime Run state | 说明 |
|---|---|---|
| `pending` | `queued` | |
| `running` | `running` | |
| `waiting` | `paused` | 仅 agent 类暂停；CLI run 实际不会出现 |
| `cancelling` | `cancel_requested=1` 且非终态 | 投影态，不落新状态 |
| `succeeded` | `completed` | |
| `failed` | `failed` | |
| `cancelled` | `cancelled` | |
| （无） | `needs_reconciliation` | 投影为 `failed` + `error.code=reconciliation_required`；不新建 AEP 状态 |

其余映射：AEP `seq` ↔ `events.sequence`（每 Run 单调，直接复用）；
AEP `eventId` 由 `run_id:sequence` 确定性派生，不新增存储列；
AEP `run.events`+`afterSeq` ↔ 既有 query service 的 `after_seq` 语义；
AEP `usage` 仅在可信时填写（终态 result 提供则透传，否则省略）；
AEP `artifacts` MVP 恒为 `[]`（协议字段保留，执行器不产 `artifact.created`）。

### D5 流式事件归一化（本计划最大开发量）

`pkg/command.Adapter` 现状：`Build` 产 invocation、`Decode([]byte)` 在进程结束后
一次性解析终态。扩展方案：

1. `OutputProtocol` 新增第三值（如 `canonical_stream`，命名实施时定），
   各 adapter 翻译为原生流式 selector：
   - Codex：`exec --ephemeral --json` 本就是 NDJSON，改为增量解码；
   - Claude：`-p --output-format stream-json`（当前契约拒绝 `--verbose` 的规则
     针对 `--output-format json`，stream 模式需按 P0 实测结果修订，保持
     fail-closed：新 selector 独立登记，不放宽既有 canonical 规则）；
   - PI / Grok：按 P0 能力核验结果登记或声明不支持。
2. `Adapter` 增加可选 `StreamDecoder` 接口（逐行解析原生事件 → 归一化事件），
   未实现的 adapter 在 AEP 层把 `capabilities.streaming` 置 `false`。
3. 事件粒度以实测为准、能力如实上报：原生只有 item 级（无 token delta）时，
   只发 `message.completed` 级事件，不发伪造的 `message.delta`；
   识别不了的原生事件包 `executor.event`，不映射为成功/失败。
4. `pkg/session/managed_exec.go` 增量消费 stdout → 归一化 → 写 `events` 表
   （先落盘后推送）→ 推送订阅者（复用既有 watch 订阅机制）。

### D6 RunInput options 处理

MVP 支持：`model`/`reasoningEffort` → `Overrides`；`cwd` → typed CWD。
**已确认（2026-10-28）：MVP 仅支持 `model`/`reasoningEffort`/`cwd`，
`timeoutMs`/`maxOutputBytes`/`permissions.*` 在 P3 实现前一律显式拒绝。**
`timeoutMs` → 新增 Run 级 deadline（executor context 到期走既有取消路径，
settle 为 timeout error），在 P3 实现。
`maxOutputBytes` → 映射到既有硬上限或拒绝（按 P0 实测上限值决定）。
`permissions.mode/approvalPolicy` → P3 按 adapter grammar 扩展。
原则（AEP 规范自带）：不支持即拒绝，不静默降级。

### D7 方法集（MVP）

`initialize`、`executor.list`、`executor.get`、`run.start`、`run.cancel`、
`run.get`、`run.result`、`run.events`、`shutdown`。事件通知方法 `run.event`。
AEP 规范 §6 之后内容补全后（P0-1）若与该集合冲突，以规范为准并回来修订本节。

### D8 幂等与重复

`run.start` 无幂等键，重复调用创建新 Run（对齐本仓库「不提供第二套入口/兼容
shim」立场）；AEP 规范侧补写「不支持幂等」。请求 `id` 唯一性、在途并发按
JSON-RPC 常规处理。

### D9 AEP 规范补全项（P0 由用户提供全文后核对）

已知缺口：§6 方法清单截断、错误码表、`permission.requested` 的回应方法、
session 方法、`initialize` 版本协商（`EventEnvelope.version:"1.0"` vs 文档
`0.1.0-draft` 的关系需写死）。若规范最终稿与 D4/D5/D6 冲突，先改规范再编码。

### D10 TS Client SDK（后置）

独立仓库（如 `aep-client-ts`），只依赖协议 JSON 格式，不依赖本仓库构建。
含去重与 `seq` 缺口自动补查。P5 交付。

### D11 ACP Adapter（后置）

独立兼容入口，纯翻译层（ACP 会话/通知/取消 → AEP 方法）。协议稳定后（P4 完成）
再启动。不阻塞 MVP。

## 3. 阶段计划

### P0 规范补全与能力核验（无产品代码）

| # | 任务 | 交付物 |
|---|---|---|
| P0-1 | 向用户取得 AEP 规范全文（含 §6 之后），修订 D1/D7/D9 涉及条目后落入 `docs/aep/spec.md` | `docs/aep/spec.md` |
| P0-2 | 能力核验 spike（临时脚本，不入 `pkg/`）：三执行器 headless 实测——启动 argv、原生事件类型清单与粒度、取消信号行为、resume 能力、终态结构、usage 可信度、输出硬上限 | 核验记录 + 录制样本（沿 `internal/testkit/commandgolden` 金样模式存放） |
| P0-3 | PI 专项：确认 PI 的 headless/流式入口（用户自有运行时，位于 `~/.pi`），确定 `adapter_pi.go` 的 grammar 输入 | PI 能力记录 |
| P0-4 | 产出 `docs/aep/capability-matrix.md`（三执行器 × AEP capabilities） | 能力矩阵 |
| P0-5 | D1/D2/D3/D6 用户确认 | 决策记录（更新本文对应小节状态） |

**验收：** 能力矩阵齐备且与录制样本一致；规范全文入库；四项决策已确认。
**契约影响：** 无。

### P1 传输层与协议壳（非流式垂直切片）

| # | 任务 | 触达位置 |
|---|---|---|
| P1-1 | AEP DTO 与 JSON-RPC/NDJSON 编解码、方法分发、错误映射（strict decode，沿用本仓库 JSON 纪律：拒绝未知字段/重复 key/trailing data） | 新增 `pkg/transport/aep/` |
| P1-2 | `sn-cli aep` 入口：stdin/stdout 传输、stderr 诊断、bootstrap 组装（含自有 scheduler worker）、graceful shutdown | `cmd/sn-cli`、`internal/interfaces/cli/`、`internal/application/runtimebootstrap/` |
| P1-3 | D7 方法映射到既有 service：`run.start` → 创建/复用 managed Session + 提交 queued session Run（`session exec --queue` 等价语义）；`run.cancel` → 既有 cancellation reservation；`run.get/result/events` → `pkg/run` query service | `pkg/transport/aep/` |
| P1-4 | `executor.list/get` 能力探测（PATH 解析、adapter 登记、streaming/sessionResume 按能力矩阵静态登记，不从协议宣称） | `pkg/transport/aep/` |
| P1-5 | 端到端驱动测试：Go 测试以子进程方式 stdio 驱动 `sn-cli aep`，跑真实 claude/codex Profile，断言事件序、终态、`afterSeq` 补洞、取消路径 | `pkg/transport/aep/` 测试 |

**验收：** 一个执行器全链路真实跑通（start → lifecycle 事件 → result → events 补查 → cancel）；
AEP server stdout 只有协议消息。
**契约影响：** 契约 §1 owner 表新增 inbound adapter 行；§2 执行矩阵新增 `sn-cli aep` 行；
§9 CLI namespace + `contract_version` bump；`--json help`、README ×2、SN-CLI-USAGE.md 同步。
**状态：** 已完成（2026-10-28，用户确认落地）。codex 真实 Profile 全链路通过（start → run.completed →
settled → result → events 回查 → shutdown）；取消路径由 `run.cancel` 单测覆盖；
全套验证门禁（fmt-check / test-serial / test-race / vet / release-check / git diff --check）
通过；`contract_version` 18→19。

### P2 流式事件归一化

| # | 任务 | 触达位置 |
|---|---|---|
| P2-1 | `OutputProtocol` 第三值 + Codex/Claude 流式 selector + `StreamDecoder` 可选接口（按 P0 实测粒度实现） | `pkg/command/adapter.go`、`adapter_codex.go`、`adapter_claude.go` |
| P2-2 | managed exec 增量消费 stdout → 归一化事件 → `events` 表先落盘后推送 | `pkg/session/managed_exec.go`、`pkg/run/` |
| P2-3 | AEP 事件投影：`EventEnvelope` 组装（`eventId` 派生、`seq` 复用、`source.nativeEventType`）+ `executor.event` 兜底 | `pkg/transport/aep/` |
| P2-4 | 归一化单测：P0 录制样本回放，断言映射与不可映射事件的兜底行为 | `pkg/command/` 测试 |

**验收：** 支持流式的执行器在 AEP 侧收到有序 `message.*`/`tool.*` 事件；
不支持的能力位为 `false` 且不发对应事件；`run.events` 能完整重放。
**契约影响：** §5 CLI executor grammar 修订（新增 streaming selector，不放宽既有
canonical 拒绝规则）；事件 kind 清单与 shape 说明；`contract_version` bump。
**状态：** 已完成（2026-11-02）。P0 录制样本回放（codex ok/tool/cancel、claude
ok/tool/cancel）+ selector/拒绝组合单测全绿；session 层 3 个流式测试覆盖事件顺序、
终态等价与流损坏收口；AEP projection 单测覆盖 5 类新映射，e2e 断言
`message.completed` 先于 `run.completed` 到达且 `run.events` 完整重放；能力位
`streaming/structuredEvents` 按 adapter `StreamDecoder` 能力如实上报
（codex/claude=true，pi/acp=false）。`contract_version` 19→20。

### P3 PI adapter 与 options 补全

| # | 任务 | 触达位置 |
|---|---|---|
| P3-1 | `adapter_pi.go` + `Resolve` 登记 basename `pi`（grammar 按 P0-3） | `pkg/command/` |
| P3-2 | Run 级 deadline（`timeoutMs`）：executor context 到期 → 取消路径 → timeout error settle | `pkg/run/session_executor.go`、`pkg/session/` |
| P3-3 | `permissions.mode` 映射到各 adapter 显式 flag（不存在则该执行器声明不支持并拒绝参数） | `pkg/command/adapter_*.go` |
| P3-4 | resume 语义：AEP `resumeFromRunId`/`sessionId` → 同一 managed Session 新 Turn/Run；原生 resume identity 不推断（维持契约既有立场）；`capabilities.sessionResume` 如实上报 | `pkg/transport/aep/`、`pkg/session/` |

**验收：** PI 真实跑通端到端；`timeoutMs` 生效且进程树被清理；不支持参数显式报错。
**契约影响：** §2 adapter 登记表、§5 grammar、执行矩阵、`contract_version` bump。

**状态：** 已完成（2026-11-02）。P3-1 pi adapter（canonical/streaming 共用
`-p --mode json --no-session`，唯一 `agent_settled` 终态纪律，effort 直接映射
`--thinking`）；P3-2 `timeout_ms` 为公开 Run request 字段，`run.Service.execute` 到期
取消执行 context，非用户取消的到期取消以 `timeout`/`phase=run` settle 为 `failed`；
P3-3 `permissions`（`read_only|workspace_write`）冻结进 CLI snapshot 并纳入
`request_digest`，adapter flag 注入 codex `--sandbox`/claude `--permission-mode`/
pi read_only `--tools read,grep,find,ls`，Grok 与 pi workspace_write 显式拒绝，
Profile 配置等价/互斥选项时 fail-closed；P3-4 `input.resumeFromRunId` 复用源 Run
的 managed Session 开新 Turn（`sessionId` 同时给出必须一致），
`capabilities.sessionResume` 对已登记执行器为 `true`。真实 pi（v1.4.0，
`/opt/homebrew/bin/pi`）经 `sn-cli aep` 端到端通过（`run.result` `succeeded`/
summary `ok`/`complete`，事件 delta→message→run.completed）。`contract_version`
20→21。

### P4 硬化与一致性

| # | 任务 | 说明 |
|---|---|---|
| P4-1 | 共享 conformance 测试套件 | 三 adapter 同一断言集：`seq` 从 1 单调无缺口、三种终态与 `exitCode` 一致、取消三段语义、stdout 纯净、畸形 JSON/未知方法/缺 `id`、超时截断 |
| P4-2 | 断连/关停语义验证 | graceful cancel、crash 后 `needs_reconciliation` 投影、重启用 `run.events` 补洞 |
| P4-3 | 多 worker 共存验证 | `sn-cli aep` 与 `sn-server` 同时领取同一 SQLite 队列的原子性与饥饿行为 |
| P4-4 | redaction 核查 | 事件/结果/错误不含 resolved secret、完整 argv/env（复用既有脱敏测试模式） |
| P4-5 | 交付面评估 | `make release-check`；确认无新 binary、install 脚本无需变更 |

**验收：** 全部 conformance 通过；验证门禁全绿。

### P5 后置项（独立立项，不阻塞 MVP）

ACP Adapter（D11）、TypeScript Client SDK（D10）、AEP daemon 形态（如有长驻需求）。

## 4. 验证门禁（每阶段末尾）

普通变更最小相关测试；触及契约/架构/发布时完整执行（AGENTS.md 要求）：

```bash
make fmt-check
make test-serial
make test-race
go vet ./...
make release-check SN_CLI_VERSION=<valid-semver>   # 仅契约/发布影响时
git diff --check
```

测试只用临时 `SN_CLI_HOME`，不得修改 active `~/.sn`。

## 5. 风险与开放问题

| 风险 | 影响 | 缓解 |
|---|---|---|
| AEP 规范 §6 之后缺失（P0-1 依赖用户提供全文） | 方法集/错误码/permission 回路定不了 | P0 强制前置；缺则按 D7 最小集先行并在规范补全后回修 |
| Claude stream-json 与既有 `--verbose` 拒绝规则表面冲突 | P2 契约修订复杂 | 按实测把 streaming 做成独立 selector，不放宽既有 canonical 规则 |
| 双 worker 共享队列未验证 | 并发下 Run 重复执行或饥饿 | P4-3 专项验证；必要时 MVP 约束「aep 与 sn-server 不同时开 worker」并写入契约 |
| `needs_reconciliation` 无 AEP 对应态 | 上层误判为普通失败 | 投影 `failed` + 显式 `error.code`；建议规范后续版本增加 reconcile 方法 |
| PI 能力未知 | D5/P3 范围不确定 | P0-3 专项核验；PI 不支持流式则能力位如实为 `false` |
| 事件 `event_json` shape 若需结构化校验 | 可能触发 Store schema 变更 | P1 保持 opaque 复用；需要校验时单独评估 `user_version` bump |

## 6. 决策状态跟踪

| 决策 | 状态 |
|---|---|
| D1 语言：服务端 Go / SDK TS | 已确认（2026-10-28，按推荐） |
| D2 入口：`sn-cli aep` + 自有 worker | 已确认（2026-10-28，按推荐） |
| D3 Executor↔Profile：请求带显式 `profile` | 已确认（2026-10-28，按推荐；`executor=acp` MVP 拒绝） |
| D4 状态/字段映射 | 已定（按本表执行） |
| D5 流式归一化方案 | 已定（细节按 P0 实测） |
| D6 options 处理 | 已确认（2026-10-28；MVP 仅 `model`/`reasoningEffort`/`cwd`，其余显式拒绝） |
| D7 方法集（MVP） | 依 P0-1 规范全文核对 |
| D8 无幂等 | 已定 |
| D9 规范补全 | P0-1 |
| D10 TS SDK 后置 | 已定 |
| D11 ACP 后置 | 已定 |
