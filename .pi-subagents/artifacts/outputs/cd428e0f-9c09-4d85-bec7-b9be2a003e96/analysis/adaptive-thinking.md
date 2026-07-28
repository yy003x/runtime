# `pi-adaptive-thinking` 实现与测试证据审计

## 1. 结论摘要

该仓库是一个单包、ESM、TypeScript 的 Pi 扩展，当前版本 `0.1.1`。它在每次启用的 `session_start` 注册一个可配置名称（默认 `set_thinking_level`）的工具，向 `before_agent_start` 的 system prompt 注入动态 guidance，并通过 Pi 原生 `getThinkingLevel` / `setThinkingLevel` / `thinking_level_select` 实现 thinking level 切换。发布面和入口见 `package.json:2-4,22-35,74-84`，用户说明见 `README.md:9-11`。

核心语义是：

- `persist: false`：本 agent turn 临时切换，`agent_end` 恢复记录的 baseline；见 `src/index.ts:296-325`、`src/index.ts:243-255`。
- `persist: true`：仅把目标值记作扩展实例内的 session baseline，不在 turn 末恢复；见 `src/index.ts:317-320`。
- 两种切换都会专门保存并恢复 `~/.pi/agent/settings.json`（或 `PI_CODING_AGENT_DIR/settings.json`）中的 `defaultThinkingLevel`，避免 Pi setter 的副作用变成全局默认；见 `src/index.ts:58-60,101-146`。这是 0.1.1 的主要修复目标（`CHANGELOG.md:3-7`）。

审计发现实现主路径清晰，但“持久”与“手动选择”的组合、同一 turn 多次临时切换、session 非正常结束、settings 恢复失败及并发边界存在重要缺口。现有 22 个测试全部是单元测试，覆盖主成功路径和部分防抖逻辑，但没有覆盖这些高风险分支。

## 2. 审计范围与事实源

已完整读取：

- 全部实现：`src/config.ts`、`src/config-loader.ts`、`src/thinking-levels.ts`、`src/index.ts`。
- 全部测试：`src/config.test.ts`、`src/config-loader.test.ts`、`src/thinking-levels.test.ts`、`src/index.test.ts`。
- 用户/发布文档：`README.md`、`CHANGELOG.md`。
- 包与构建配置：`package.json`、`pnpm-workspace.yaml`、`tsconfig.json`、`tsconfig.build.json`、`rolldown.config.ts`、`.oxfmtrc.json`、`.oxlintrc.json`、`.changeset/config.json`、三份 GitHub Actions workflow；核对了 `pnpm-lock.yaml` 中关键解析版本。
- 历史设计材料：`docs/superpowers/specs/2026-05-26-pi-adaptive-thinking-design.md` 和 `docs/superpowers/plans/2026-05-26-pi-adaptive-thinking.md`。它们是历史意图，不高于当前代码；例如历史材料默认名曾是 `set_reasoning_effort`，当前事实是 `set_thinking_level`（`src/config.ts:10-16`、`README.md:9`）。

仓库是浅克隆/grafted，仅有当前 release commit；CHANGELOG 中链接的历史 commit 无法从本地对象读取。运行环境也未提供任务配置中提及的 `web_search` 子工具，因此没有声称完成外部网络核验；本报告以仓库机器事实为准。

## 3. 用户可见功能

### 3.1 安装、加载与发布面

- npm 安装命令为 `pi install npm:pi-adaptive-thinking`，本地可 build 后用 `pi -e ./dist/index.js`（`README.md:13-25`）。
- npm 只发布 `dist` 与 `README.md`；包为 ESM，默认入口与类型入口均指向 `dist/index.*`（`package.json:22-35`）。
- Pi 扩展清单指向 `./dist/index.js`（`package.json:80-84`）。
- Rolldown 以 `src/index.ts` 为入口、Node/ESM 输出，并把 Pi API 与 TypeBox 保留为外部依赖；`proper-lockfile` 会被 bundle（`rolldown.config.ts:3-10`）。
- peer dependencies 使用 `*`，本地 lock 实际解析到 `@earendil-works/pi-ai` / `pi-coding-agent` 0.75.5、`typebox` 1.1.38；这降低安装冲突但扩大 API 漂移风险。

### 3.2 工具契约

注册项见 `src/index.ts:275-284`：

- 默认名称：`set_thinking_level`；可通过 `toolName` 改名。
- 固定 label：`Set Thinking Level`。
- description：来自 `toolDescription`。
- 固定 prompt snippet：`Set the current Pi thinking level.`。
- prompt guideline 会嵌入配置后的工具名。
- 参数 schema（`src/index.ts:31-47`）：
  - `level`: 必填 string，schema 只要求长度至少 1；执行时还会 `trim()`。
  - `persist`: 可选 boolean，schema default 为 `false`，执行层也用 `?? false`。
  - 禁止额外字段。
- 所有业务成功/失败均返回普通 text tool result，而不是抛出 tool error（`src/index.ts:51-54,285-327`）。

用户可见结果：

- 成功：`Thinking level set to <level>`（`src/index.ts:327`）。
- 已处于目标值：`Thinking level is already <level>; no change made.`（`src/index.ts:297-300`）。
- 非法或模型不支持：列出当前模型的 valid levels（`src/index.ts:288-294`）。
- 连续两次调用：第二次跳过并要求在其它工具调用或新用户输入后重评估（`src/index.ts:302-306`）。
- setter / lock 失败：`Failed to set thinking level: <message>`（`src/index.ts:311-315`）。
- runtime 不存在：`Adaptive Thinking is not enabled for this session.`（`src/index.ts:285-286`）。

### 3.3 System prompt 注入

默认 prompt 强制 agent 主动管理 level：简单任务降档，复杂、调试、风险、多步综合升档，并在 turn 开始、获得新证据和任务变化时重评估（`src/config.ts:4-8`）。

每次 `before_agent_start` 追加：

1. 配置的 `systemPrompt`；
2. 已知 current level；
3. 当前模型 valid levels；
4. 如何调用配置后的工具；
5. 仅在复杂度需要时调用；
6. 同值不调用；
7. 不连续两次调用（`src/index.ts:166-180,221-240`）。

原 system prompt 被保留；两块之间两个换行。空原 prompt 只返回 guidance（`src/index.ts:159-164`）。配置 schema 不允许空 `systemPrompt`，所以用户不能通过空字符串只关闭自定义基础提示而保留固定动态提示。

## 4. 配置语义

### 4.1 字段、默认值与验证

`src/config.ts:10-27` 定义唯一配置 schema：

| 字段 | 默认值 | 约束/影响 |
|---|---|---|
| `enabled` | `true` | false 时本 session 不设置 runtime、不注册工具 |
| `quiet` | `false` | 仅抑制有 config 可用时的 UI notification；不抑制 tool text failure |
| `toolName` | `set_thinking_level` | 非空字符串；决定注册名、guidance、连续调用识别 |
| `toolDescription` | `Set your thinking level` | 非空字符串；工具 description |
| `systemPrompt` | 强制主动管理的完整文本 | 非空字符串；每 turn 注入 |

`parseConfig` 先将单个输入对象浅合并到 built-in defaults，再严格 Parse；额外字段被拒绝（`src/config.ts:31-34`）。它不是 global + project 的字段级层叠。

### 4.2 搜索顺序

候选依次为：

1. `<ctx.cwd>/.pi/adaptive-thinking.json`
2. `<homedir>/.pi/agent/adaptive-thinking.json`
3. built-in defaults

见 `src/config-loader.ts:29-54`、`README.md:50-58`。第一个存在的文件一旦读取即终止搜索：

- 项目文件是 partial config 时，缺失字段来自 built-in defaults，不继承 global 文件。
- 项目文件 JSON/schema 非法时直接失败，也不会回退 global。
- `ENOENT` 才继续；权限、目录、I/O 错误被包装为 structured error（`src/config-loader.ts:38-51`）。

### 4.3 启用、禁用与错误

`session_start` 异步加载配置（`src/index.ts:257-269`）：

- 配置非法/I/O 失败：`runtime = undefined`；有 UI 时 error notification；扩展不注册该 session 工具。
- `enabled: false`：静默把 runtime 置空并返回。
- `quiet` 无法抑制“配置本身非法”的通知，因为失败分支没有可用 config，调用 `notify` 时未传 quiet（`src/index.ts:259-262`）。
- 无 UI 时所有 notification 静默（`src/index.ts:148-157`）。没有日志 fallback。

## 5. Thinking level / effort 解析与切换

### 5.1 词汇和合法值

当前用户契约统一使用 “thinking level”，不是独立的 numeric effort，也没有 `reasoning_effort` 参数。唯一静态全集是：

`off`, `minimal`, `low`, `medium`, `high`, `xhigh`

见 `src/thinking-levels.ts:4-10`。解析行为：

- 工具输入先 trim；大小写敏感；不接受 alias、数字、provider-native 名称或 `auto`（`src/index.ts:288-290`）。
- 若有 `ctx.model`，调用 Pi AI 的 `getSupportedThinkingLevels(model)`，再过滤到上述六值（`src/thinking-levels.ts:12-20`）。
- 无 model 时保守性并不强：直接声称六值全部可用（`src/thinking-levels.ts:15`）。实际 setter 仍可能 clamp 或拒绝。
- model 返回未来新增 level 时会被静默过滤，属于 forward-compatibility 限制。
- 每次 prompt 注入和每次 execute 都重新按当时的 `ctx.model` 求 valid levels，因而能适应 turn/调用间 model 变化（`src/index.ts:234,289`）。

### 5.2 切换顺序

execute 的判定顺序（`src/index.ts:285-327`）：

1. runtime 是否存在；
2. trim level；
3. 静态合法性 + 当前 model 支持性；
4. 计算 `persist`（默认 false）；
5. 从 cache 或 Pi API 取 current；
6. 同值 no-op；
7. 按 toolCallId 判断是否连续调用；
8. 计算 reset baseline；
9. 在 settings lock 下调用 setter 并恢复全局默认；
10. setter 成功后才更新 runtime；
11. 更新 persistent / temporary 状态并返回成功文本。

同值判定先于 back-to-back 判定，因此连续第二次请求相同 current level 会返回“already”，不是“previous tool call was also…”。

## 6. Runtime 状态机

状态全部是 extension factory 闭包中的单个 `runtime`（`src/index.ts:22-29,183-185`）：

- `config`: 当前 session 配置。
- `currentLevel`: cache；初始化来自 `pi.getThinkingLevel()`。
- `persistedLevel`: 当前扩展 session baseline，仅成功 `persist:true` 时写入。
- `temporaryResetLevel`: turn 结束要恢复的 level。
- `lastToolCallWasReasoningTool`: 最近观察到的 tool_call 是否为本工具。
- `reasoningToolCallBackToBackById`: 将每个 toolCallId 绑定到“它出现时，前一个调用是否也是 reasoning tool”。

主要转移：

| 触发 | 前置 | 状态变化 |
|---|---|---|
| enabled `session_start` | config 成功 | 新建 runtime/map，合法 initial level 写入 `currentLevel`（`src/index.ts:271-273`） |
| `thinking_level_select(L)` | runtime 存在 | `currentLevel = L`（`src/index.ts:191-194`） |
| reasoning `tool_call(id)` | runtime 存在 | map[id] = previous-reasoning；last=true（`src/index.ts:196-205`） |
| other `tool_call` | runtime 存在 | last=false（`src/index.ts:206-208`） |
| `before_agent_start` | runtime 存在 | 清连续调用状态；仅在 cache 缺失时读 Pi current（`src/index.ts:225-234`） |
| 成功 persistent set(L) | current != L | current=L；persisted=L；删除 temporary reset（`src/index.ts:317-320`） |
| 成功 temporary set(L) | reset baseline 存在且 != L | current=L；temporaryReset=baseline（`src/index.ts:308-325`） |
| 成功 reset | temporaryReset 存在 | setter(reset)；current=reset；删除 temporary（`src/index.ts:243-251`） |
| reset 失败 | temporaryReset 存在 | 状态保持；可见 error notification（除 quiet/no UI）（`src/index.ts:252-254`） |
| `agent_end` | 任意 runtime | 尝试 reset 后清连续调用状态（`src/index.ts:213-218`） |

`runtimeHandlersRegistered` 保证 `thinking_level_select`、`tool_call`、`before_agent_start`、`agent_end` 只注册一次（`src/index.ts:187-219`）；`session_start` handler 本身在 factory 初始化时立即注册。

## 7. 临时与“持久”语义

### 7.1 正常路径

- 初始 `medium`，临时设 `high`：记录 reset=`medium`，turn 末回到 `medium`。
- 先 persistent 设 `high`，后续 turn 临时设 `low`：reset 优先取 `persistedLevel=high`，turn 末回到 `high`（`src/index.ts:308-322`）。
- 临时后又 persistent：新的 persistent 成为 baseline，并清除待 reset，不会恢复旧值（`src/index.ts:318-320`）。

### 7.2 “持久”的准确边界

这里的 persist 是“extension instance 中的 session baseline”，不是：

- 写入配置文件；
- 写入 Pi 全局 `defaultThinkingLevel`；
- 跨进程/扩展重载持久；
- 明确持久到下一独立 session。

README 正确描述为 session baseline（`README.md:42-48`）；CHANGELOG 0.1.1 明确要求 active session scope（`CHANGELOG.md:3-7`）。名字 `persist` 对迁移设计有误解风险，目标系统应明确命名或文档化 `scope=session`。

### 7.3 组合语义缺陷

1. **高：手动选择不会更新 persistent baseline。** `thinking_level_select` 只更新 `currentLevel`，不更新/清除 `persistedLevel`（`src/index.ts:191-194`）。若 persistent 设 high，用户手动改 low，再临时设 medium，reset 仍优先回 high（`src/index.ts:308-309`），与 README“until … user changes thinking level manually”（`README.md:48`）的自然语义冲突。
2. **高：同一 turn 多次允许的临时切换会丢失 turn 原始 baseline。** 无 persisted baseline 时，第一次 medium→high 记录 medium；经其它工具解锁后第二次 high→low 会把 reset 覆盖为 high，`agent_end` 回 high 而非 turn 开始的 medium。根因是每次都从 current 重新计算并覆盖 `temporaryResetLevel`（`src/index.ts:308-325`）。
3. **高：没有 `session_end` / abort / disposal 恢复。** 临时值只在 `agent_end` 恢复（`src/index.ts:213-218`）。如果 turn 异常中止、session 在 agent_end 前切换，或者扩展宿主未发该事件，runtime 被下次 `session_start` 覆盖，待恢复值丢失。
4. **中：手动选择发生在一个临时切换之后时，待 reset 仍不变。** turn 内用户手动选择是否应取消临时 reset 没有定义；当前实现 turn 末仍会强制恢复旧 baseline。

## 8. 事件生命周期与防连续调用

### 8.1 生命周期

1. 扩展 factory 被加载：只注册 `session_start`。
2. 首次 enabled `session_start`：load config、建立 runtime、注册工具，再首次注册四类 runtime handlers（`src/index.ts:257-332`）。
3. `before_agent_start`：清上一轮/用户输入前的 consecutive 状态，补充 current，并注入 guidance。
4. 每个 `tool_call`：按事件顺序维护 consecutive 标志。
5. tool execute：使用同一 toolCallId 查询当时的 consecutive 事实。
6. `thinking_level_select`：同步 cache（包括 UI 手动选择和 Pi setter 发出的事件）。
7. `agent_end`：恢复临时值，清 consecutive map。

### 8.2 防抖实现的优点与假设

按 ID 存 map 而不是 execute 时读全局 last flag，可正确处理多个 `tool_call` 事件先到、execute 后到的批处理/并行顺序；对应测试在 `src/index.test.ts:125-163`。其它工具调用会重新允许调整（`src/index.test.ts:165-204`），新 user turn 由 `before_agent_start` 清状态。

限制：

- 强依赖 Pi 必须在 execute 前发送带相同 ID 的 `tool_call`。直接调用 execute（现有测试大量这样做）不会被标记为 reasoning call，防连续逻辑不生效。
- map 仅在 before-start / agent-end 清理；单 turn 内按 tool call 数增长。
- tool name 若跨 session 改变，单一 runtime 会立刻按新名识别；旧注册工具是否仍留在宿主 registry 取决于 Pi 生命周期，代码没有 unregister 或去重。
- handlers 只有首次 enabled session 后才存在；随后 disabled/invalid session 会把 runtime 置空，handler 仍存在但成为 no-op。工具注册本身是否按 session 自动清除由宿主保证，扩展没有显式处理。

## 9. 并发锁与 settings 安全

### 9.1 实现

为防止 `pi.setThinkingLevel()` 把 session 切换保存为全局默认，实现会：

1. 解析 agent dir：`PI_CODING_AGENT_DIR` 优先，否则 `~/.pi/agent`（`src/index.ts:58-60`）。
2. 创建 settings 父目录和永久 sidecar `<settings>.adaptive-thinking`（`src/index.ts:87-92`）。
3. 对 sidecar 调用 `proper-lockfile.lockSync(..., {realpath:false})`。
4. 遇到 `ELOCKED` 时每 20ms busy-wait，最多 100 次（约 2 秒）（`src/index.ts:62-85`）。
5. 锁内读取旧 `defaultThinkingLevel`，调用 Pi setter，再读取 setter 后完整 settings，仅还原/删除该字段并完整重写 JSON（`src/index.ts:101-146`）。
6. `finally` release extension lock（`src/index.ts:94-98`）。

这样会保留 setter 同时修改的其它字段；测试专门验证 setter 把 `theme` 从 dark 改 light 时，最终只把 default level 回 medium（`src/index.test.ts:227-268`）。

### 9.2 风险

1. **高：restore 不在 `finally`。** `changeThinkingLevel()` 一旦在修改 settings 后抛错，`restoreDefaultThinkingLevel` 不执行（`src/index.ts:139-145`），可能把 session-only 操作泄漏成全局默认。
2. **高：restore 吞掉所有错误，调用者仍报成功。** settings JSON parse/read/write 失败会在 `restoreDefaultThinkingLevel` 中静默 return（`src/index.ts:121-133`）；外层随后更新 runtime 并返回 `Thinking level set…`。这直接破坏 0.1.1 的安全承诺且不可观测。
3. **高：锁只协调采用同一 sidecar 协议的本扩展实例。** 没有证据表明 Pi 自身或其它 settings writer 使用 `<settings>.adaptive-thinking`。因此它不能阻止外部 writer 在 read-modify-write 窗口更新 settings，完整 `writeFileSync` 可能覆盖别人的并发字段变更。
4. **中：同步 busy-wait 阻塞事件循环最多约 2 秒。** 在并发 session 或 lock 卡住时会冻结 extension host；并且只对精确 `ELOCKED` 重试（`src/index.ts:69-81`）。
5. **中：settings 重写不是原子 rename。** 崩溃可留下截断 JSON；文件权限/metadata 也可能变化。
6. **中：旧默认读取失败被等同于“原来没有该字段”。** malformed/不可读 settings 返回 undefined（`src/index.ts:101-114`）；若 setter 修复/重写文件，restore 会删除 default 字段，而不是报告无法安全保存旧值。
7. **低：sidecar 创建有 check-then-write race 且永久遗留。** 通常内容为空，影响有限（`src/index.ts:89-90`）。
8. **设计耦合：** workaround 假设 Pi setter 会写 `settings.json.defaultThinkingLevel`，并直接操作宿主私有持久化文件。迁移时应优先要求“session-scoped setter”或 setter 的 `persist:false` 参数，而不是复制该补偿事务。

## 10. 安全与失败行为

### 10.1 做得较好的部分

- 配置 schema 严格拒绝错误类型、空关键字符串和额外字段（`src/config.ts:18-33`）。
- 非法/不支持 level 在 setter 前拒绝（`src/index.ts:288-294`）。
- setter 抛错时不提交 runtime 状态（状态更新在 `try` 后，`src/index.ts:311-325`）。
- reset 抛错时保留 `temporaryResetLevel`，不虚报恢复成功（`src/index.ts:248-254`）。
- lock release 使用 `finally`（但 settings restore 自身没有 finally）。
- notification 同时尊重 quiet 和 headless context（`src/index.ts:148-157`）。
- 不将 secret/token 写入结果；error message 会原样回显底层异常，若底层异常包含敏感路径/内容则没有脱敏。

### 10.2 其它失败边界

- `resolveSupportedThinkingLevels` 或 `pi.getThinkingLevel()` 抛错未捕获，可使 prompt/event handler 失败。
- `pi.registerTool` 抛错未捕获；runtime 已建立但 handlers 尚未注册（`src/index.ts:271-331`），会形成部分初始化。
- `thinking_level_select` 未做 runtime `isThinkingLevel` 校验（`src/index.ts:191-194`）；依赖 peer API 的类型保证。未来 level 可污染 current cache。
- reset 失败后只在下一次 `agent_end` 才可能重试；下一 turn 会继续在临时 level 上运行。
- 无 model 时向 agent 暴露全集，可能把非 reasoning model 的 level 误报为可用。
- valid level 数组为空时错误文本退化为 `Valid levels: .`，没有专门的“当前 session 无可用 level”结果。
- `quiet` 不是全面静默：tool result 仍必须可见；非法配置的通知也无法尊重其中的 quiet。

## 11. 测试覆盖审计

### 11.1 数量与已覆盖行为

共 22 个 `test()`：

- `src/config.test.ts:4-28`（5 个）：defaults、partial merge、错误类型、额外字段、默认 prompt 关键语句。
- `src/config-loader.test.ts:17-71`（4 个）：无文件 defaults、global、project precedence、invalid config structured error。
- `src/thinking-levels.test.ts:8-45`（3 个）：六个静态 level、unknown level、无 model fallback、`thinkingLevelMap` null 过滤。
- `src/index.test.ts:42-324`（10 个）：
  - 默认工具与 prompt guidance（43-68）；
  - same-level no-op（70-85）；
  - consecutive blocking（87-123）；
  - per-toolCallId 顺序（125-163）；
  - intervening tool 解锁（165-204）；
  - persistent 不在 agent_end reset（206-225）；
  - persistent 不改变 global default 且保留其它 setter 修改（227-268）；
  - temporary 在 agent_end 恢复（270-288）；
  - manual event 更新 current 并作为无 persisted 时的 reset（290-306）；
  - invalid level 不调 setter（308-323）。

测试均为 mocked Pi API 的单进程单元测试。`createPi` 的 setter 是同步 mock，事件不会由 setter 自动发出；因此没有验证真实 Pi 事件重入/顺序（`src/index.test.ts:9-31`）。

### 11.2 明显缺口

**高优先级未覆盖：**

- persistent baseline 后 manual `thinking_level_select` 的语义。
- 同一 turn 经 intervening tool 的多次 temporary 切换。
- setter 修改 settings 后抛错、restore parse/write 失败。
- lock contention、超时、release failure、跨实例并发、外部 writer 竞争。
- agent abort / session switch / 没有 agent_end。
- reset setter failure、状态保留和后续 turn 行为。

**中优先级未覆盖：**

- disabled config 不注册工具/不注入 prompt（历史设计明确列为应测，但当前缺失）。
- invalid config 在 runtime 层禁用并 notification；quiet/no UI。
- custom `toolName` / `toolDescription` / `systemPrompt` 的完整 runtime 行为。
- malformed JSON、读取权限错误、项目 invalid 时不 fallback global。
- tool parameter schema 本身（额外参数、空字符串、错误 persist 类型）。
- runtime model-specific valid levels 与空列表。
- setter failure 不提交 runtime。
- unknown initial/current level、未来 event level。
- 多 session 的重复工具注册/handler 生命周期。
- prompt 原文为空、空白/换行拼接。
- `PI_CODING_AGENT_DIR` 默认与 override 路径行为、settings 不存在场景。

### 11.3 验证状态

仓库标准验证由 `package.json:40-56` 定义，CI 在 Node 24 上依次执行 install、format、lint、typecheck、test、build（`.github/workflows/ci.yml:23-50`）。本环境没有 `pnpm`，仓库也没有 `node_modules`，所以四次尝试的 `pnpm test`、`pnpm typecheck`、`pnpm lint`、`pnpm format:check` 均在启动前以 exit 127 / `pnpm: command not found` 失败；没有安装依赖，以遵守只读要求。未运行 build，因为既无 pnpm，且 build 会 clean/recreate `dist`。

因此只能报告测试源码覆盖，不能声称测试在本次审计环境通过。

## 12. 文档、实现与历史材料的一致性

- README 当前默认工具名、字段和实现一致（`README.md:29-48,60-68` vs `src/config.ts:10-16`）。
- CHANGELOG 0.1.1 的“active session 而非 global default”由 settings save/restore 实现和一条测试支撑（`CHANGELOG.md:7`、`src/index.test.ts:227-268`），但失败/并发路径未被证明。
- CHANGELOG 0.1.0 仍称 “reasoning-effort tool”（`CHANGELOG.md:13`），是产品概念，不是当前工具名。
- 历史设计/计划中的 `set_reasoning_effort`、Zod 等已经过时；当前是 `set_thinking_level` 与 TypeBox Parse。架构设计应引用当前 src/package，不应复制历史 plan。
- README 没有说明：project partial 不继承 global、persist 不跨重启、consecutive-call guard、settings sidecar/同步锁、失败后的残留行为。

## 13. 可迁移设计原则

1. **把“能力发现、策略、状态、持久化副作用隔离”分层。** 当前 `thinking-levels.ts` 已隔离能力发现，值得保留；settings 补偿事务应进一步成为独立 adapter 并有故障注入测试。
2. **显式定义 scope。** 用 `temporary(turn)`、`session baseline`、`global default` 三个不同概念，避免 boolean `persist` 混淆；状态中保存 `turnEntryLevel`，不要每次临时切换覆盖 baseline。
3. **手动操作是 source-of-truth 事件。** 明确 manual `thinking_level_select` 是更新 session baseline、取消 temporary，还是只改 current；事件最好包含 origin（user/tool/restore），否则无法可靠区分 setter 自发事件与 UI 手动事件。
4. **恢复必须是补偿事务且可观测。** setter 即使抛错也要在 `finally` 尝试恢复；恢复失败不能吞掉，应返回/通知“level 可能已切换且 global default 恢复失败”的复合状态。
5. **避免同步 busy-wait 与私有文件 RMW。** 首选宿主提供真正的 session-scoped setter；退而求其次用 async lock、原子写、共享锁协议和 CAS/version 检测。
6. **所有终止路径都清理临时状态。** 除 `agent_end` 外处理 abort、session_end、extension dispose、下一 session_start 前的补偿；清理应幂等。
7. **能力列表宁可保守。** model 缺失时可返回“unknown”而不是假定全集；空能力列表应给专门错误。对未来 level 应决定透传还是版本化，不应无提示过滤。
8. **状态 cache 要有刷新规则。** 每 turn 从 host 读取并与事件 cache 对账，避免长期只相信初始化值；setter 返回实际应用值比假设请求值更可靠。
9. **防重复策略与执行顺序解耦。** 当前 per-toolCallId 快照是可迁移的好模式，但需定义并行 tool calls 和缺失 `tool_call` 事件时的行为。
10. **配置 precedence 写成机器契约。** 明确是“first existing file wins + built-in defaults”还是“global 字段层 + project 字段层”；为 invalid higher-priority config 是否 fallback 做显式选择。
11. **peer API 要设兼容边界。** `*` 方便集成但不利于稳定发布；至少通过版本矩阵或 capability detection 验证事件和 setter 语义。
12. **以组合状态测试替代只测单步。** 最低矩阵应包含 persistent→manual→temporary→end、temporary→other tool→temporary→end、set throws after write、restore fails、abort/session switch 和双实例竞争。

## 14. 面向后续架构设计的建议实施顺序

1. 先确定三个产品决策：manual selection 如何影响 session baseline；同 turn 多次 temporary 的最终 reset；哪些终止事件保证清理。
2. 把 session state 改为明确的 `sessionBaseline`、`turnEntryLevel`、`currentLevel`、`pendingRestore`，并给转移加纯函数测试。
3. 替换或封装 settings workaround；在无法获得宿主 session-only API 时，至少修复 `finally`、错误传播、原子写和外部并发检测。
4. 补齐上述高优先级组合测试，再做真实 Pi integration smoke test，验证 setter 是否同步写 settings、是否同步发 `thinking_level_select`、事件 origin/order 与工具注册生命周期。
5. 最后更新 README，准确公开 persist scope、配置非层叠语义和恢复限制。

## 15. 审查发现（按严重度）

- **high — `src/index.ts:191-194,308-322`：** manual thinking-level selection 不更新 persistent baseline，后续临时操作可能恢复到用户已经替换掉的旧 baseline。
- **high — `src/index.ts:308-325`：** 同一 turn 第二次 temporary set 会覆盖原 `temporaryResetLevel`，最终不一定恢复 turn 入口 level。
- **high — `src/index.ts:139-145`：** setter 抛错时 settings restore 不执行，可能泄漏全局默认修改。
- **high — `src/index.ts:121-133`：** restore 失败被吞掉而 execute 仍成功，核心 session-only 保证不可观测地失效。
- **high — `src/index.ts:213-218,257-273`：** 只在 `agent_end` reset；abort/session replacement 可丢失 pending restore。
- **high — `src/index.ts:87-145`：** sidecar lock 不与未知外部 settings writer 协调，完整 JSON RMW 可丢并发更新。
- **medium — `src/index.ts:62-81`：** lock 竞争采用同步 busy-wait，最多阻塞事件循环约两秒。
- **medium — `src/thinking-levels.ts:12-20`：** model 缺失时假定全部 level，未来 level 又被静默过滤，能力发现两端都可能失真。
- **medium — `src/index.ts:257-331`：** 多 session 的工具注册/禁用生命周期依赖宿主隐式清理，没有 unregister 或相应测试。
- **medium — 测试缺口：** 关键失败、并发、abort、配置 runtime 分支和组合状态均未覆盖；详见第 11 节。
- **low — `src/index.ts:89-90`：** sidecar check-then-write 竞态且永久遗留。

## 16. 只读性与仓库状态

- 未修改外部仓库任何文件。
- 审计前后 `git status --short` 均为空。
- `git diff --cached --name-only` 为空，无 staged files。
- 唯一写入是任务要求的仓库外报告文件：`/Users/yang/go/run/runtime/.pi-subagents/artifacts/outputs/cd428e0f-9c09-4d85-bec7-b9be2a003e96/analysis/adaptive-thinking.md`。

```acceptance-report
{
  "criteriaSatisfied": [
    {
      "id": "criterion-1",
      "status": "satisfied",
      "evidence": "报告逐项给出用户功能、配置、状态机、事件、level 解析、临时/持久语义、锁、失败行为、测试覆盖、限制和迁移原则，并在第 15 节以 severity + file:line 列出具体发现。"
    }
  ],
  "changedFiles": [
    "/Users/yang/go/run/runtime/.pi-subagents/artifacts/outputs/cd428e0f-9c09-4d85-bec7-b9be2a003e96/analysis/adaptive-thinking.md"
  ],
  "testsAddedOrUpdated": [],
  "commandsRun": [
    {
      "command": "pnpm test",
      "result": "failed",
      "summary": "未启动测试：环境无 pnpm，exit 127；未安装依赖以保持外部仓库只读。"
    },
    {
      "command": "pnpm typecheck",
      "result": "failed",
      "summary": "未启动检查：环境无 pnpm，exit 127。"
    },
    {
      "command": "pnpm lint",
      "result": "failed",
      "summary": "未启动检查：环境无 pnpm，exit 127。"
    },
    {
      "command": "pnpm format:check",
      "result": "failed",
      "summary": "未启动检查：环境无 pnpm，exit 127。"
    },
    {
      "command": "git -C <external-repo> status --short && git -C <external-repo> diff --cached --name-only",
      "result": "passed",
      "summary": "工作树与暂存区均为空。"
    }
  ],
  "validationOutput": [
    "静态审计覆盖全部 8 个 src/测试文件、README、CHANGELOG、package/build/release 配置及历史设计材料。",
    "测试源码共 22 个 test()；本环境因缺少 pnpm 未能执行。",
    "外部仓库 git status 与 staged diff 均为空。"
  ],
  "residualRisks": [
    "未在真实 Pi host 验证 setThinkingLevel 对 settings.json 的副作用和 thinking_level_select 事件顺序。",
    "未执行测试、类型检查、lint 或 format，因为运行环境没有 pnpm/node_modules。",
    "仓库为浅克隆，无法本地展开 CHANGELOG 所链接的历史 commit。",
    "web_search 子工具配置不可用，未进行外部 API 版本核验。"
  ],
  "noStagedFiles": true,
  "diffSummary": "外部仓库零改动；仅生成指定只读审计报告。",
  "reviewFindings": [
    "high: src/index.ts:191-194,308-322 - manual selection 不更新 persistent baseline。",
    "high: src/index.ts:308-325 - 同 turn 多次 temporary 切换覆盖原 reset baseline。",
    "high: src/index.ts:139-145 - setter 抛错时不执行 settings restore。",
    "high: src/index.ts:121-133 - restore 失败被吞掉但工具仍报告成功。",
    "high: src/index.ts:213-218,257-273 - abort/session replacement 可丢 pending restore。",
    "high: src/index.ts:87-145 - sidecar lock 不协调外部 settings writers。"
  ],
  "manualNotes": "本报告以当前代码为最高事实源；历史设计中的 set_reasoning_effort/Zod 已过时。"
}
```
