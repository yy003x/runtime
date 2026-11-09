# AEP 执行器能力矩阵（P0-4）

三执行器 × AEP capabilities 的实测核验结果。数据来源：2026-10-28 headless spike
（darwin 27，arm64），录制样本位于
`internal/testkit/commandgolden/testdata/streams/`（含 manifest）。

| 执行器 | 版本 | 实测 Profile 环境 |
|---|---|---|
| Codex | codex-cli 0.164.0 | `CODEX_HOME=~/.codex-aip`，model `gpt-6.1-sol` |
| Claude Code | 2.1.308 | `CLAUDE_CONFIG_DIR=~/.claude-aip`，bigmodel Anthropic 兼容端点，model `glm-5.3` |
| PI | 1.4.0（`@earendil-works/pi-coding-agent`） | `~/.pi` 默认配置，默认模型 |

Grok 已在 `pkg/command` 登记 adapter，但不是 AEP MVP 目标执行器（`ExecutorId`
枚举不含 grok）；`executor=acp` 按 D3 在 MVP 直接拒绝。

## 1. Headless 入口与事件流

### Codex

- 入口：`codex exec --ephemeral --json <prompt>`（与现有 canonical selector 相同，
  NDJSON；AEP 只需把既有一次性 Decode 改为增量解码，argv 不变）。
- 实测事件类型：`thread.started`、`turn.started`、`item.started`、
  `item.completed`（`item.type=agent_message|command_execution`）、
  `turn.completed`（携带 usage）。`command_execution` 带 `status=in_progress|
  completed` 与完整 argv。
- 粒度：item 级。无 token delta → AEP 只投 `message.completed` 级事件，
  不伪造 `message.delta`；未识别 item 类型按 `executor.event` 兜底。
- 终态：`turn.completed` / `turn.failed` / `error`；另有 `-o/--output-last-message
  <FILE>` 可直接落终态文本。
- 陷阱（实测确认）：
  - stdin 非 TTY 时 codex 会读取 stdin 追加输入（stderr 提示
    "Reading additional input from stdin..."）；managed exec 必须显式供空 stdin。
  - 非 git cwd 需要 `--skip-git-repo-check`，否则拒绝执行；AEP `cwd` 任意，
    adapter 需处理。
  - stderr 中出现 "Shell cwd was reset …" 属运行环境噪音，不影响 stdout 协议流。

### Claude Code

- 入口：`claude -p --output-format stream-json --verbose
  [--include-partial-messages] <prompt>`。
- **实测确认：不带 `--verbose` 时 CLI 前置校验直接拒绝**（"When using --print,
  --output-format=stream-json requires --verbose"，exit 1，不发起 API 调用）。
  因此 streaming selector 必须内部注入 `--verbose`；这与既有「canonical 模式拒绝
  用户配置 `--verbose`」规则不冲突——streaming 是独立 selector，canonical 拒绝
  规则不放宽（D5）。
- 实测记录类型：`system`（subtype `init|status|thinking_tokens|
  background_tasks_changed|task_started|task_updated|task_notification`）、
  `stream_event`（SSE 透传：`message_start`、`content_block_start`、
  `content_block_delta`（text/thinking delta，token 级）、`content_block_stop`、
  `message_delta`、`message_stop`）、`assistant`（完整 message，content blocks 为
  `thinking|text|tool_use`）、`user`（tool_result）、`result`（终态）。
- 粒度：带 `--include-partial-messages` 时有 token 级 delta（实测 thinking delta），
  可投真 `message.delta`；不带时为 message 级。MVP selector 推荐携带。
- 终态：唯一 `type=result` 记录（`subtype=success`、`is_error`、`stop_reason`、
  `usage`、`modelUsage`、`total_cost_usd`、`num_turns`、`session_id`）。
- 陷阱：variadic flags（如 `--allowedTools <tools...>`）会吞掉后续 prompt，
  必须以 `--` 结束 options（既有 adapter 纪律已覆盖）。

### PI

- 入口（MVP 选定）：`pi -p --mode json --no-session <prompt>`，one-shot JSONL
  事件流，与 managed exec「每 Run 一个进程」模型吻合。
- 备选（记录，MVP 不用）：`--mode rpc` 双向长生命周期 JSONL 命令协议
  （详见安装包 `docs/rpc.md`、`docs/cli-integration.md`）；未来可作为常驻
  enhancement，不改变 AEP 契约。
- 实测事件类型：`session`（header，含 id/cwd）、`entry_appended`、`agent_start`、
  `turn_start`、`message_start|message_end`（role `system|user|assistant`，含
  权威最终消息与 usage）、`message_update`（`assistantMessageEvent`：
  `text_start|text_delta|text_end|thinking_delta|toolcall_*`）、
  `tool_execution_start|update|end`（`toolName/args/partialResult/result/isError/
  durationMs`）、`turn_end`、`agent_end`、`agent_settled`（`aborted`）。
- 粒度：token 级 `text_delta`/`thinking_delta` 原生支持。
- 终态：`message_end.message` 为权威消息；`agent_settled` 标记本轮自动工作结束；
  stopReason（stop/length/error/aborted）在 message 上；`length` 截断时 print 模式
  非零退出 + stderr 警告（文档）。
- 注意：JSON mode 首条 `session` header 只有 JSON mode 有（RPC mode 无）；
  `--no-session` 不落 session 文件；`-nc` 禁用 AGENTS.md/CLAUDE.md 发现。

## 2. AEP capabilities 投影

| capability | codex | claude-code | pi |
|---|---|---|---|
| `streaming` | 部分：item 级，仅 `message.completed` 粒度，无 `message.delta` | 是：token 级（`--include-partial-messages`） | 是：token 级 |
| `cancellation` | 是：TERM 后 5s 内收口 | 是：同 | 是：同 |
| `structuredEvents` | 是（`exec --json` NDJSON） | 是（`stream-json`） | 是（`--mode json`） |
| `sessionResume` | `codex exec resume <id>`（help 验证，未实测） | `claude -p --resume <id>`（help 验证，未实测） | `--continue`/`--resume`/`--session-id`（docs 验证，未实测） |
| `nativeResult` | `turn.completed` + `--output-last-message` | `result` 记录 | `message_end` + `agent_settled` |

- 三家 headless 均具备原生 resume 入口，但 Runtime 维持既有立场：不从 Runtime
  Session ID 推断原生 resume identity。AEP `resumeFromRunId`/`sessionId` 落地为
  同一 managed Session 的新 Turn/Run（P3-4）；`capabilities.sessionResume` 按
  「managed Session 内续跑」语义如实上报。
- Codex resume 与 `--ephemeral` 互斥（ephemeral 不落 session 文件）。

## 3. 取消信号行为（实测）

对整个进程组（`start_new_session`）发 SIGTERM，5 秒宽限后 SIGKILL：

| 执行器 | 行为 | stdout 终态记录 |
|---|---|---|
| codex | 直接死于 signal 15（`killed_by_signal=15`），无需升级 KILL | 无（流停在 `turn.started`） |
| claude | exit 143（128+15） | 无（流停在 delta 中，无 `result`） |
| pi | exit 143 | 无（无 `agent_settled{aborted:true}`） |

结论：**没有执行器在被 SIGTERM 时发出可靠终态记录**；AEP 终态权威只能来自
Runtime 自身的 cancellation reservation + terminal publish barrier（与现有
managed exec 收口设计一致）。原生 graceful cancel（如 pi 的 abort 语义）不在
MVP 范围。

## 4. usage 可信度（实测值）

| 执行器 | 来源 | 字段 |
|---|---|---|
| codex | `turn.completed.usage` | `input_tokens`、`cached_input_tokens`、`cache_write_input_tokens`、`output_tokens`、`reasoning_output_tokens` |
| claude | `result.usage` + `modelUsage` + `total_cost_usd` | `input_tokens`、`cache_read/creation_input_tokens`、`output_tokens`（含 thinking 明细） |
| pi | `message_end.message.usage`（`message_update.usage` 为累计） | `input`、`output`、`reasoning`、`totalTokens`、`cost` |

三家终态均原生上报 usage，可信透传；缺失即省略，不估算（D4）。

## 5. 权限参数映射素材（P3-3 输入）

| AEP `permissions` | codex | claude | pi |
|---|---|---|---|
| `mode=read_only` | `--sandbox read-only` | `--permission-mode plan`/`dontAsk` + 工具裁剪，需 P3 定稿 | `--tools read,grep,find,ls`（allowlist） |
| `mode=workspace_write` | `--sandbox workspace-write` | `--permission-mode acceptEdits` 等 | 无直接等价（工具级 allowlist + `--approve`） |
| `approvalPolicy` | `--approve-for-me` / `--dangerously-bypass-approvals-and-sandbox` | `--permission-prompts host|none` | 无直接等价 |

映射不成立的执行器须显式拒绝参数并声明不支持（D6 原则），不得静默降级。

## 6. 输出与输入硬上限

- 三家 headless 模式均未观察到 CLI 侧 stdout 总量上限（实测最大单次 ~830KB 流）。
- Runtime 侧既有硬上限继续生效：prompt/单 token 128,000 bytes、invocation
  manifest 与 canonical 记录 1 MiB。
- AEP `maxOutputBytes` 处理（P3 定稿）：映射到 managed exec 既有输出上限或显式
  拒绝；`timeoutMs` 走 P3-2 Run 级 deadline。实现前两者均显式拒绝（D6）。

## 7. 对 P1/P2/P3 的直接输入

1. `OutputProtocol` 第三值（streaming）：codex 复用 `exec --ephemeral --json`；
   claude 新 selector `-p --output-format stream-json --verbose
   --include-partial-messages`（`--verbose` 由 selector 内部注入，不放宽用户
   配置的拒绝规则）；pi 新 selector `-p --mode json --no-session`（P3 登记
   `adapter_pi.go` 时实现）。
2. codex adapter 需补 `--skip-git-repo-check` 策略与 stdin 供空纪律（P2/P3）。
3. 归一化映射（P2-3）：`message.delta` 仅 codex 缺失；`tool.*` 三家均有原生
   对应（codex `command_execution` item、claude `tool_use/tool_result`、pi
   `tool_execution_*`）；`system`/`queue_update`/`task_*` 等非映射事件走
   `executor.event` 兜底。
4. 录制样本即 P2-4 回放测试夹具（见 `testdata/streams/manifest.json`）。
