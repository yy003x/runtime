# `wb-chat` 当前真实实现梳理（只读）

## 结论

`wb-chat` 已经不是纯文档约定：生产 API 在每个 turn 提交前实际执行“显式 skill / 完整短语确定性路由 -> 本地连续性与相似度 lane -> 固定 Model Stage 的语义模型 lane”，并把结果作为 `sn-cli` memory 注入 provider。它同时有 scope/load surface、模型输出 schema、阈值/风险门禁、离线评测和 TaskRecord route trace。

但当前并不存在按任务动态选择 effort/reasoning/profile 的机制：

- 动态的只有 `complexity=simple|complex`、local-vs-model lane、owner/interaction gate、失败 fallback；`complexity` 不影响 Model Stage、profile 或 reasoning。
- 生产语义路由永远调用配置中的单一 Stage `wb-chat.semantic_routing`（`wb/agent/chat_router.py:1370-1382`；`apps/agent/conf/skill-routing.yaml:19-30`）。
- 该 Stage 的共享 profile 固定为 `cx-adv`（`configs/skills-execution.yaml:19-27`），且 runtime 明确禁用 fallback（`apps/agent/conf/skill-runtime.yaml:17-25`）。本机只能通过 `configs/local/skills-execution.yaml` 覆盖这个已登记 Stage 的 profile；不能新增 Stage（`wb/agent/skill_execution.py:261-295`）。
- model/provider/reasoning 的实际值不在仓库策略中，而由所选 `sn-cli` profile 管理；dispatcher 只执行 `sn-cli profile exec <profile>`（`skills/wb-model/scripts/dispatch.py:142-168`）。`skill_execution` 的人配置字段只允许 `profile`，机器字段只允许 `fallback_profile/live_progress`（`wb/agent/skill_execution.py:18-26,146-174`）。

因此，若目标是“按请求动态 effort/reasoning”，不能把 reasoning 参数直接塞进现有路由结果或 dispatcher；应先决定是否通过多个稳定 Stage/profile 表达档位，或扩展 execution policy。现有明确契约是 reasoning 归 `sn-cli profile`，业务路由归 `wb-chat`，执行解析归 `wb-model/skill_execution`。

## 1. 真实调用链

### 1.1 API 生产入口

`wb/api/workbench.py:_route_message` 是当前可确认的生产接线点：

1. `load_skill_routing_config()`；
2. 按 project 和 develop mode 计算 `available_skills`；
3. 先 `route_text()`；
4. **只有** `match_type == "fallback"` 才调用 `route_semantically()`（`wb/api/workbench.py:450-470`）。

`create_session` 和 `add_message` 都在提交 provider 前调用它（`wb/api/workbench.py:196-227,495-530`）。会话自身的 provider/profile 由 API 用户选择或 workbench config 决定（`wb/api/workbench.py:209-225,503-529`），与内部 `wb-chat.semantic_routing` 的 `cx-adv` 是两次独立模型选择：前者回答/执行最终 turn，后者只做 owner 判定。

路由结果经 `_route_memory()` 作为 `type=workbench_route` memory 注入；provider 收到原始用户文本，不是 route envelope（`wb/api/workbench.py:103-113,217-226,522-530`）。`_route_context()` 还附加指令：direct 时读取目标 skill，answer/clarify 时读取 `wb-chat`（`wb/api/workbench.py:52-90`）。

### 1.2 确定性层

`route_text()` 的顺序和行为：

- Unicode NFKC、casefold、空白压缩、仅移除句末标点（`wb/agent/skill_router.py:31-34`）。
- 从 `$name` 中只保留已注册 skill；一个显式且非 internal/in-scope 则 direct，多个则 `wb-chat clarify`（`wb/agent/skill_router.py:363-413`）。
- 没有显式 skill 后，完整归一化输入查全局唯一 alias；不是子串匹配（`wb/agent/skill_router.py:415-450`）。
- project mismatch、skill disabled、当前 Agent load surface 缺失均回 `wb-chat clarify`（`wb/agent/skill_router.py:322-358`）。

配置加载会强制：fallback 必须是 global skill；internal skill 不可成为 route target；归一化短语全局唯一；所有 enabled 用户 skill 必须有 exact route 或列入 semantic-only（`wb/agent/skill_router.py:103-155,232-288`）。当前 fallback 是 `wb-chat`，126 个 exact phrases / 40 routes（实测 check）。

### 1.3 load surface / index

`configs/skills.yaml` 是加载事实源：`wb-chat` 是 enabled/global（`configs/skills.yaml:253-255`），`wb-model` 是 enabled/internal（`configs/skills.yaml:310-312`），`wb-repo/wb-session/wb-work` 是 develop（`configs/skills.yaml:316-319,338-343`）。生成的 `skills/index.json:4-23` 分别记录 global/develop/internal surface，`wb-chat` 记录在 `skills/index.json:305-309`。

`configured_available_skills()` 组合 global + 可选 develop + 当前 project skills（`wb/agent/skill_router.py:300-319`）。index 由 `wb/agent/skill_index.py` 从 skill frontmatter、`agents/openai.yaml` 和 `configs/skills.yaml` 生成；不是手写路由清单。`skill_router.load_config()` 又以该 index 校验所有目标和加载面（`wb/agent/skill_router.py:37-100,116-128`）。

### 1.4 `wb-chat` 本地 lane

`route_semantically()` 先构造 context，再 `_self_route()`；只有返回 `None` 才调用模型（`wb/agent/chat_router.py:1328-1353`）。本地 lane 包括：

- 从最近一个带 routing 的 **user message** 恢复 `active_owner/pending_handoff/interaction_mode`（`wb/agent/chat_router.py:218-250`）。
- 轻量聊天、一般概念问答、Workbench skill 路径说明直接 `wb-chat answer`，不调用模型（`wb/agent/chat_router.py:1129-1141`）。
- active owner continuation / pending handoff gate 通过时 direct；高风险 `wb-git/wb-session` 需要当前轮显式领域证据，写 owner 需要执行词（`wb/agent/chat_router.py:858-904,1054-1127`）。
- 对 exact phrases 做 `SequenceMatcher` 相似度，按 skill 取最高分；高风险 skill 被排除。本地仅在 `complexity=simple`、score >= 0.92、margin >= 0.15 时接受（`wb/agent/chat_router.py:991-1051,1143-1171`）。

复杂度是启发式：多 owner 信号、多步词、proposal+execution、governance 词任一命中即 complex（`wb/agent/chat_router.py:991-1012`）。它仅阻止 local similarity direct，并进入结果/评测；**不动态选择执行资源**。

### 1.5 上下文与 interaction mode

上下文最多 8 条消息、3 个同 project TaskRecord、30 天、单消息 2000 字符、总 24KB（`apps/agent/conf/skill-routing.yaml:25-30`）。TaskRecord 只抽取 expectation、最后路由、结果、反馈、learning 摘要；不读取 `original_inputs`（`wb/agent/chat_router.py:320-427`）。消息和 task 字段经 `redact_text`，总量超限先丢旧 task，再丢最旧 message（`wb/agent/chat_router.py:210-215,430-445`）。

`infer_interaction_mode()` 是确定性词表并继承上一轮模式（`wb/agent/chat_router.py:253-291`）：只读/review 优先，discovery 次之，条件式“是否需要”默认为 review，proposal 词且无强执行词为 proposal，执行词为 execution。模型可补充 mode，但本地已识别的 mode 优先，模型不能升级它（`wb/agent/chat_router.py:1240-1248`）。

模型 route 还会拒绝：score/margin 不足、有 missing context/clarification、非 execution 却指向 `wb-work` 或写 mode、当前输入缺少高风险证据（`wb/agent/chat_router.py:1249-1263`）。拒绝后生成 `pending_handoff` 和一个 clarification question（`wb/agent/chat_router.py:1200-1222,1303-1324`）。

### 1.6 模型 lane

Prompt 是 `skills/wb-chat/prompts/semantic-routing.md`，由动态 skill catalog、bounded context、阈值、高风险 skill、用户输入和 JSON schema 渲染（`wb/agent/chat_router.py:520-551`）。模型输出必须精确包含 schema v1 的 decision/complexity/interaction/candidates/clarification/handoff/reasons，candidate 必须在当前加载面且不能选 internal skill；领域 `mode` 不能冒充 interaction mode（`wb/agent/chat_router.py:605-640,643-780`；schema：`apps/agent/conf/schemas/chat-routing-result.schema.json`）。

执行通过 `bash skills/wb-model/scripts/run.sh --stage <stage>`，父进程执行 60 秒 timeout 和进程组 TERM/KILL；非零/空输出/非法 JSON 安全回退 `wb-chat triage`（`wb/agent/chat_router.py:554-584,1370-1405`）。

`wb-model` 只负责解析已登记 Stage、profile/fallback/progress 并执行 profile。未知 Stage fail closed（`wb/agent/skill_execution.py:59-99`）；共享配置可被本机同名 profile 覆盖，但本机不能注册新 Stage（`wb/agent/skill_execution.py:261-295`）。当前 `wb-chat.semantic_routing` 和离线 `wb-chat.routing_evaluation` 都固定 `cx-adv`，都无 fallback。

## 2. Owner 边界

### 路由 owner：`wb-chat`

持有业务 owner 判定、prompt、候选 schema、上下文选择/脱敏、score/margin、interaction mode、高风险和 handoff gate；输出 answer/direct/clarify，但不执行目标任务。证据：`skills/wb-chat/SKILL.md` 及真实实现 `wb/agent/chat_router.py`。

### 确定性入口 owner：`skill_router`

持有显式 skill、exact phrase、scope/load surface、配置完整性；它不看对话历史、不做模型推断（`wb/agent/skill_router.py:363-450`）。

### 执行策略 owner：`skill_execution` + `wb-model`

持有 Stage 注册、profile/fallback/progress 解析与 `sn-cli profile exec`；不判断业务 owner，也不持有 wb-chat prompt（`wb/agent/skill_execution.py:18-99`；`skills/wb-model/scripts/dispatch.py:253-290`）。

### 领域 owner

direct 后 provider 被指示读取目标 skill 并做边界自检（`wb/api/workbench.py:75-79`）。路由不是执行授权本身；interaction mode 约束仍须目标 skill 遵守。

### TaskRecord owner：`wb-finalize` / `task_history`

route envelope 只是一份进入 owner 的证据，不是 execution step。`wb-finalize` 解包、去重、补 null、规范化为 route entries（`wb/agent/finalize.py:391-489,795-815`）；`TaskRecordCodec` 强制 routing trace 的精确字段、sequence、decision、score 范围、target/handoff shape（`wb/agent/task_history.py:637-762`；schema：`apps/agent/conf/task-record/v2.schema.json:216-276`）。

## 3. Route envelope 与 TaskRecord

协议仅定义：

```text
[[WORKBENCH_ROUTE_V1]]\n
<JSON>
[[USER_INPUT]]\n
<visible input>
```

解码器只检查 prefix/separator、JSON object，不直接做完整 route schema 校验（`wb/agent/route_envelope.py:9-26`）；严格规范化发生在 finalize。

API 有 `_routed_content()` 构造器（`wb/api/workbench.py:93-100`），但生产的 `create_session/add_message` 当前传的是原始 `content` + route memory，不调用它（`wb/api/workbench.py:217-226,522-530`）。`_normalize_history_message()` 只有当历史 content 本身是 envelope 时才恢复 `message["routing"]`（`wb/api/workbench.py:116-126`）。

`wb-finalize --routed-input` 要求合法 envelope；普通 `--input` 若恰带 envelope也会提取。最终 TaskRecord 中 `routing_trace={status,entries,final_owner}`，entry 精确包含 decision/target/route_id/source/score/complexity/interaction/continuity/clarification/reasons。

## 4. effort / reasoning / profile 的当前事实

| 层 | 当前决定点 | 是否动态 |
|---|---|---|
| API 会话回答 profile | 用户参数 / `workbench_config.chat_profile`，`wb/api/workbench.py:196-225,495-529` | 可按会话/turn 选择，但与 wb-chat route complexity 无关 |
| wb-chat 路由 Stage | `apps/agent/conf/skill-routing.yaml:19-30` 固定 `wb-chat.semantic_routing` | 否 |
| Stage primary profile | `configs/skills-execution.yaml:19-27` 固定 `cx-adv`；local 可覆盖同 Stage | 部署级覆盖，不是请求级动态 |
| fallback/progress | `apps/agent/conf/skill-runtime.yaml:17-25`；wb-chat fallback 为 null | 仅失败驱动；wb-chat 当前禁用 |
| model/provider/reasoning/effort | 对应 `sn-cli` profile 的外部配置 | 仓库内不决策；随 profile 静态绑定 |
| `complexity` | `wb/agent/chat_router.py:991-1012` | 动态，但不驱动 profile/reasoning |

额外事实：通用 runtime API schema 仍暴露 `reasoning_effort`，`wb/api/workbench.py:365-390` 会把它塞进 `provider_overrides`；但 `SnCLIClient.task_run_command()` 对任何非空 `provider_overrides/model/reasoning_effort` 都明确报错，要求改用 profile（`wb/task/sn_cli.py:216-230`）。因此这不是可用的动态 effort 机制，而是当前公共 API 与 transport contract 的不一致/遗留面。

## 5. 可插入点（若后续设计动态 effort/profile）

1. **最小且符合现契约：新增稳定 Stage 档位。** 在 `wb-chat` owner 内根据模型调用前已知的 `complexity/context/risk` 选择例如两个已登记 Stage；各 Stage 在 `configs/skills-execution.yaml` 绑定不同 profile，reasoning 仍由 profile 管理。需同步 routing config 的 stage 约束、execution config/runtime、schema/tests/evaluation。优点是不向 route result 泄漏 provider 参数；缺点是 Stage 语义是否应代表“业务阶段”而非“算力档位”需先定 contract。
2. **execution policy 增加受控 selector。** 由 `skill_execution` 接收结构化 tier（不是任意 profile/reasoning）并映射到预登记 profile。这样 wb-chat 只输出/选择业务 tier，wb-model 仍拥有执行解析。需要扩展 `ExecutionSettings`、schema、配置分层和 fail-closed tests；风险比方案 1 大。
3. **不建议：在 chat routing schema/envelope 中加入 raw `reasoning_effort` 或 provider 参数。** 这会打破“路由证据不等于执行配置”、`HUMAN_SETTING_KEYS={profile}` 和 `sn-cli profile` 事实源边界，也增加 TaskRecord 长期耦合。
4. **若只需 API 最终助手动态 profile**，插入点在 `_profile_for_selection`/会话 runtime，而不是 wb-chat semantic router；但这会改变用户可见执行 provider，必须与内部语义路由 profile 分开命名和审计。

## 6. 不可破坏契约

- 显式单 skill 和全局唯一完整短语优先，且不额外调用语义模型；仅 fallback 进入 semantic lane（`wb/api/workbench.py:450-470`）。
- internal `wb-model` 永远不能成为用户路由目标（`wb/agent/skill_router.py:141-145,249-253,379-385`）。
- project/load surface 必须在 local/model 两侧都校验；模型候选只能来自当前 available skill catalog。
- 每轮最多一个主 owner；interaction mode 是授权，不可写入 candidate domain `mode`。
- 当前输入优先于模型 interaction mode；模型不得升级只读授权。
- `wb-git/wb-session` 零误报；高风险 direct 必须有当前输入证据。
- score + margin + context + clarification + write gate 全部通过才可 direct；失败必须回 chat，不随机 fallback owner。
- 路由上下文必须有界、脱敏，TaskRecord context 不得复制 `original_inputs`、代码、diff、provider transcript。
- Stage 必须预登记，未知 Stage 在调用 provider 前失败；reasoning/model/provider 由 profile 管理。
- route envelope 与 TaskRecord V2 字段 shape 是长期审计契约；新增字段不能只改 router，必须同步 API context、chat schema、finalize normalizer、TaskRecord schema/codec/tests。
- 路由 trace 不得记录成 execution step；无 envelope 只能 `unavailable/not_applicable`，不能从最终 skill 反推。

## 7. 风险与发现

### high — API 多轮连续性在真实持久化路径上可能丢失

`chat_router` 只从历史 user message 的 `routing` 字段恢复连续性（`wb/agent/chat_router.py:218-250`）；API 只会从 content envelope 解出该字段（`wb/api/workbench.py:116-126`）。但生产提交传原始 content + memory，不传 `_routed_content()`（`wb/api/workbench.py:217-226,522-530`）。现有 API test 通过人工构造 prior envelope 验证恢复，并未证明 `sn-cli` 会把 route memory 回写为 user content envelope。若 history view 不把 memory 合并到 message，下一 turn 的 `active_owner/pending_handoff/interaction_mode` 会消失。应以真实 `sn-cli show` fixture/integration test 确认；若确认丢失，这是文档宣称“上下文连续”的核心缺口。

### high — interaction mode 没有覆盖确定性 direct lane

`infer_interaction_mode()` 只在 semantic route context 内运行；显式 skill/exact phrase direct 后 API 不再调用 `route_semantically()`（`wb/api/workbench.py:456-469`），而 `route_text()` 输出没有 interaction mode（`wb/agent/skill_router.py:399-405,434-442`）。例如 exact design/repair/validate 请求依赖目标 skill 自检，route envelope 本身没有“interaction mode 先于 owner”的证据。若要强化统一授权，必须在不破坏 deterministic no-model fast path 的前提下给确定性结果补本地 interaction inference。

### high — API route memory 与 TaskRecord envelope 不是一条自动贯通链

生产 API route 作为 memory 注入，但 `_routed_content()` 没有生产调用；`wb-finalize --routed-input` 又只接受 `WORKBENCH_ROUTE_V1` envelope（`wb/agent/finalize.py:804-811`）。因此“本轮 API 已路由”不自动等于“finalize 可取得可审计 envelope”。需要确认 provider/runtime 是否会把 memory 转成 finalize 可用 routed input；本仓代码未显示这条桥。否则 TaskRecord routing coverage 会是 `unavailable` 或依赖 agent 手工重建，后者违背“不得反推”。

### medium — 审计 source 硬编码成 `cx-adv`

模型结果和模型失败都写 `router_source="cx-adv"`（`wb/agent/chat_router.py:1265-1269,1394-1403`），但本机允许把该 Stage profile 覆盖成其它值。这样 route trace 的 source 可能与实际 profile 不符。更稳妥的是 source 表示 lane（如 `semantic-model`），另以 execution resolution 提供实际 profile；或从 resolver 返回实际 profile，但不能泄漏私有配置。

### medium — `reasoning_effort` API 面是不可用遗留契约

`wb/api/workbench.py:365-390` 接受并传递 overrides，`wb/task/sn_cli.py:225-230` 立即拒绝。调用方传非空 model/reasoning/provider_overrides 会失败，不是动态机制。应在后续工作中要么从 API schema 移除/明确 4xx，要么在更上层直接拒绝并说明 profile owner；不要绕过 sn-cli profile 契约。

### medium — interaction heuristic 有优先级误判边界

只读/review 词在执行词之前直接返回；例如“确认下并修改”会因“确认下”得到 `review-only`（`wb/agent/chat_router.py:253-266`），不会考虑后面的明确写词。保守性避免误写，但可能制造错误澄清。应增加冲突表达标注集，而不是简单调词序。

### medium — 完整 95% 质量门禁当前未在本轮实测

无 semantic prediction 文件时实测仅 `PARTIAL`：59 case 中 17 measured，coverage 25.42%；不能据此声称 95% 全量 gate 已通过。完整流程必须用 `wb-chat.routing_evaluation` 的生产一致 profile capture predictions 后 `--require-semantic`。

### low — schema 与手写 validator 是双重事实面

JSON schema 和 `validate_model_result()` 同时维护，实际运行主要靠手写精确字段校验。任何字段演进都必须同时更新 prompt/schema/validator/tests，否则模型输出会安全 fallback，但容易表现为无声 coverage 下降。

### 环境限制

运行时声明的 `web_search` child tool 未加载（配置错误）；本任务主要由仓库事实足够支撑，未声称做过外部 API/`sn-cli` 最新文档研究。外部 `~/.sn` profile 的真实 model/reasoning 值也未读取，以避免接触本机私有配置；仓库只能确认这些值归 profile owner。

## 8. 测试与验证入口

### 当前核心单测

- `apps/agent/tests/test_skill_router.py`：显式/完整短语、scope/load/internal、配置拒绝。
- `apps/agent/tests/test_chat_router.py`：local answer、interaction、continuity/handoff、score/margin/high-risk、模型 fallback、TaskRecord context 脱敏。
- `apps/api/tests/test_session_context.py`：API route injection、history normalization、语义 router 接线；但连续性测试使用人工 envelope。
- `apps/agent/tests/test_skill_execution.py`：Stage/profile/runtime/local override/fail closed。
- `apps/agent/tests/test_wb_model_dispatch.py`：`sn-cli profile exec`、fallback、进度脱敏、未知 Stage。
- `apps/agent/tests/test_finalize.py`：envelope -> TaskRecord routing trace。
- `apps/agent/tests/test_skill_routing_evaluate.py`：评测计算、prediction shape、quality gate。
- `apps/agent/tests/test_skill_routing_regression.py`：rules/skill/map 静态一致性。

### 建议后续新增的关键测试

1. 真实/高保真 `SnCLIClient.show()` fixture：上一 turn 只通过 memory 提交后，下一 turn 能否恢复 routing；不能则先修持久化桥。
2. deterministic exact/explicit route 的 `interaction_mode` envelope 测试，尤其 review/execution 冲突。
3. API -> provider -> finalize 的 route envelope 贯通测试，禁止从 final skill 反推。
4. local profile override 后 `router_source` 不虚报 `cx-adv`。
5. 非空 `reasoning_effort/model/provider_overrides` 的 API 兼容性测试，明确预期错误层。
6. 若引入动态 tier：simple/complex、risk/context-truncated 的 selector table tests；未登记 tier fail closed；完整 semantic evaluation 分档对比成本/准确率。

## 9. 已运行命令

- `scripts/agent-run python -m unittest apps.agent.tests.test_skill_router apps.agent.tests.test_chat_router apps.agent.tests.test_skill_routing_evaluate apps.agent.tests.test_skill_routing_regression`：40 tests，PASS。
- `scripts/agent-run python -m wb.agent.skill_router --check`：PASS，126 exact phrases / 40 routes。
- `scripts/agent-run python -m wb.agent.skill_index --check`：PASS，index up to date。
- `scripts/agent-run python -m wb.agent.skill_execution validate`：PASS，17 stages。
- `scripts/agent-run python -m wb.agent.skill_routing_evaluate`：命令成功但结果 PARTIAL；17/59 measured，top1 100%，accepted precision 100%，coverage 25.42%，route contract 100%。
- `scripts/agent-run python -m unittest apps.agent.tests.test_skill_execution apps.agent.tests.test_wb_model_dispatch apps.agent.tests.test_finalize apps.api.tests.test_session_context`：62 tests 中 1 failure；`test_get_session_and_add_message_use_sn_cli_client` 期望 `api-cx`，实际 `cx`。仓库本来很脏，本轮未修改，无法把该失败归因于本任务；它显示当前 API profile 选择测试基线有不一致。

## 10. 给后续规划/实现 Agent 的 compact contract

**Goal**：在不混淆业务路由 owner 与模型执行 owner 的前提下，决定是否及如何为 `wb-chat` 增加动态 effort/profile；同时先封住连续性、interaction envelope 和 TaskRecord bridge 的真实缺口。

**Evidence**：生产接线点 `wb/api/workbench.py:450-470`；动态 complexity `wb/agent/chat_router.py:991-1012`；固定 Stage 调用 `wb/agent/chat_router.py:1370-1382`；固定 profile `configs/skills-execution.yaml:19-27`；reasoning/profile owner 边界 `wb/agent/skill_execution.py:18-26` 与 `wb/task/sn_cli.py:216-230`；envelope/finalize 链 `wb/agent/route_envelope.py:9-26`、`wb/agent/finalize.py:795-815`。

**Success criteria**：
- 先用 integration evidence 证实/否定 API continuity 与 finalize bridge 风险；
- 动态机制不得接受 raw provider/model/reasoning override，必须映射到预登记配置；
- deterministic fast path 仍不调用模型；
- 所有 lanes 都携带可信 interaction mode；
- actual profile/source 可审计但不泄漏本机私有配置；
- 全量 routing evaluation（含 semantic predictions）满足 95%/coverage/zero-FP gate；
- TaskRecord V2 兼容策略明确。

**Hard constraints**：`wb-model` 不成为业务 owner；高风险当前轮证据；scope/load surface；bounded/redacted context；未知 Stage fail closed；reasoning 仍由 profile 管理，除非先批准并修改整体 execution contract。

**Suggested approach**：先做 runtime/history integration discovery；若连续性桥不成立，优先修 bridge。动态 effort 优先比较“多个稳定 Stage/profile”与“execution policy 受控 tier”两案，不直接扩展 route envelope 为 provider 参数。

**Validation**：上述核心单测 + API history integration + finalize bridge + `skill_router --check` + `skill_index --check` + `skill_execution validate` + production-profile semantic prediction capture 后 `skills evaluate --require-semantic`。

**Stop/escalation**：Stage 是否可以表达算力档位、是否升级 TaskRecord schema、是否允许改变 `sn-cli profile` reasoning owner 都是架构/产品决定；未批准前只做 discovery/proposal，不修改长期契约。

## Acceptance report

```acceptance-report
{
  "criteriaSatisfied": [
    {
      "id": "criterion-1",
      "status": "satisfied",
      "evidence": "已给出带 severity 的具体发现，并引用 wb/agent/chat_router.py、skill_router.py、skill_execution.py、wb/api/workbench.py、TaskRecord/envelope、配置/schema/tests 的路径与行号。"
    }
  ],
  "changedFiles": [],
  "testsAddedOrUpdated": [],
  "commandsRun": [
    {
      "command": "scripts/agent-run python -m unittest apps.agent.tests.test_skill_router apps.agent.tests.test_chat_router apps.agent.tests.test_skill_routing_evaluate apps.agent.tests.test_skill_routing_regression",
      "result": "passed",
      "summary": "40 tests passed"
    },
    {
      "command": "scripts/agent-run python -m wb.agent.skill_router --check && scripts/agent-run python -m wb.agent.skill_index --check && scripts/agent-run python -m wb.agent.skill_execution validate",
      "result": "passed",
      "summary": "126 phrases/40 routes valid; skill index current; 17 execution stages valid"
    },
    {
      "command": "scripts/agent-run python -m wb.agent.skill_routing_evaluate",
      "result": "passed",
      "summary": "Command completed with PARTIAL evaluation: 17/59 measured; semantic predictions not supplied"
    },
    {
      "command": "scripts/agent-run python -m unittest apps.agent.tests.test_skill_execution apps.agent.tests.test_wb_model_dispatch apps.agent.tests.test_finalize apps.api.tests.test_session_context",
      "result": "failed",
      "summary": "61 passed, 1 failed: session context expected api-cx but got cx"
    }
  ],
  "validationOutput": [
    "Core routing/router/evaluation/regression unit tests: 40/40 passed",
    "Static router/index/execution checks passed",
    "Routing evaluation is PARTIAL, not a full 95% semantic gate",
    "Extended execution/finalize/API suite has one pre-existing-profile-selection failure in the dirty worktree"
  ],
  "residualRisks": [
    "high: API route memory may not persist as message routing, so active_owner/pending_handoff continuity is not proven",
    "high: deterministic direct lanes do not attach interaction_mode",
    "high: API route memory is not visibly bridged to wb-finalize WORKBENCH_ROUTE_V1 envelope",
    "medium: router_source hardcodes cx-adv despite allowed local profile override",
    "medium: API reasoning_effort/provider_overrides surface conflicts with SnCLIClient rejection",
    "medium: full semantic quality gate was not run without captured predictions",
    "External web_search capability was unavailable due runtime configuration"
  ],
  "noStagedFiles": true,
  "diffSummary": "只读分析；未修改、未格式化、未暂存仓库文件。仅写入运行时要求的仓库外分析 artifact。",
  "reviewFindings": [
    "high: wb/api/workbench.py:217-226,522-530 + wb/agent/chat_router.py:218-250 - route memory 到下一轮 message.routing 的持久化链未被真实实现证明",
    "high: wb/api/workbench.py:456-469 + wb/agent/skill_router.py:399-442 - explicit/exact direct 绕过 interaction_mode 推断",
    "high: wb/api/workbench.py:103-113 + wb/agent/finalize.py:804-811 - API memory 与 finalize route envelope 之间缺少可见自动桥",
    "medium: wb/agent/chat_router.py:1267,1397 - 实际 profile 可本机覆盖但审计 source 固定 cx-adv",
    "medium: wb/api/workbench.py:365-390 + wb/task/sn_cli.py:225-230 - reasoning/model override API 面不可用"
  ],
  "manualNotes": "仓库初始即有大量未提交改动且 staged 为空；本轮严格未触碰。完整 semantic evaluation 需生产一致 profile 的 captured predictions。"
}
```
