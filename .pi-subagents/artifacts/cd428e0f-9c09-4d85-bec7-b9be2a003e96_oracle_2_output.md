# `wb-chat` 自适应决策架构建议

## Inherited decisions

- `wb-chat` 只负责确定性路由后的语义分流、连续性判断、必要追问和轻量问答，不执行领域任务。依据：`skills/wb-chat/SKILL.md:85-104`。
- 每轮只有一个业务 owner；`interaction_mode` 是先于 owner 的授权边界，不是 skill mode。依据：`rules/routing-policy.md:3-18`、`skills/wb-chat/references/interaction-mode-policy.md:3-24`。
- `wb-model` 是内部 transport；owner 定义业务 Stage、prompt 和输出复核，Stage 再映射到固定 profile。依据：`documents/designs/contracts/workbench-model-stage-contract.md:116-129`。
- model、reasoning、deadline、预算和 Provider 属于 `sn-cli` profile/Runtime，不进入 Workbench Stage 配置。依据：`documents/designs/contracts/workbench-model-stage-contract.md:107-114`。
- 连续任务只延续 `active_owner`、`pending_handoff` 和只读授权；高风险动作不能靠旧状态或泛化“继续”授权。依据：`skills/wb-chat/references/semantic-routing-contract.md:9-14`。
- 长期行为变化只能经 `wb-learning` 的重复证据、candidate、用户确认和目标 owner 晋升，不能由单轮 Agent 自动写入。依据：`documents/designs/contracts/workbench-learning-profile-loop-design.md:85-114`。
- Runtime 与 Workbench 分层：Runtime 拥有 Agent loop、预算、模型调用和 Run；Workbench 拥有业务 owner、Stage、skill 和 TaskRecord。依据：`documents/designs/contracts/runtime-vnext.md:19-31,46-54`。

## Diagnosis

### 从 `pi-adaptive-thinking` 真正值得借鉴的机制

只读检查了 `pi-adaptive-thinking@0.1.1` 及其 `src/index.ts`。其价值不只是 `set_thinking_level`，而是：

1. turn 开始重新评估；
2. 临时变更在 `agent_end` 恢复；
3. persistent 与 temporary baseline 分离；
4. 根据当前模型枚举合法 level；
5. 相同 level no-op，连续两次调节被抑制；
6. 新证据或新用户输入后才能再次调整；
7. 配置分层和并发锁避免状态漂移。

Workbench 应把这些转译为“每轮重算、短期 hint、稳定偏好、能力协商、抗抖动和可审计反馈”，而不是给 `wb-chat` 一个能直接修改 Runtime reasoning 的工具。

### 当前几个轴仍被混在一起

| 维度 | 应回答的问题 | 合理 owner |
|---|---|---|
| 路由复杂度 | owner 是否清晰、是否应追问 | `wb-chat` |
| 推理 effort | 已选 owner 应做多深的证据与综合 | 领域 owner |
| model/profile | 哪个已登记 Stage 使用哪个执行能力 | `wb-model` + `sn-cli` |
| 风险授权 | 本轮允许哪些副作用 | `interaction_mode`/安全规则 |
| 预算/延迟 | 最多等多久、花多少尝试 | Runtime；用户可给 SLO |
| 连续任务 | 是否仍是同一任务、owner、phase | route envelope + owner handoff |
| 反馈学习 | 哪种策略反复成功或失败 | `wb-learning`，受控晋升 |

当前 `complexity=simple|complex` 主要由关键词组和多步骤词计算，见 `wb/agent/chat_router.py:991-1012`；它既不足以表示 owner 歧义，也不应被解释为 reasoning level。

### Concrete findings

1. **blocker — 模型可以升级执行授权**  
   `wb/agent/chat_router.py:1246-1259` 的注释称模型不能升级授权，但实现使用：

   ```python
   context.get("interaction_mode") or result.get("interaction_mode")
   ```

   当本地没有识别出 mode 时，模型可自报 `execution`，随后 `wb-work` 通过写入门禁。只读探针证明，无历史上下文的“可以”或“好的”，只要模型返回 `wb-work/execution`，结果就是 `direct`。这违反 `interaction-mode-policy.md:22-24` 中“泛化确认不得授权写入”的既有决策。

2. **medium — 路由复杂度与任务深度不可区分**  
   `wb/agent/chat_router.py:991-1012` 将 repository、execution、proposal、Git、session 信号组计数直接视为复杂度。一个单 owner 的深度诊断和多 owner 的浅层复合请求无法区分。

3. **medium — 现有评测只能证明 owner/decision，不能验证自适应策略**  
   `skills/wb-chat/references/routing-evaluation-cases.json:1-8` 只设 owner accuracy、precision、coverage、route contract gate；没有授权不可升级、effort 上下界、延迟或预算指标。

4. **medium — 当前 TaskRecord 无法归因 reasoning/预算策略**  
   `apps/agent/conf/task-record/v2.schema.json:220-274` 保存 route complexity、interaction mode 和用户 routing feedback，但没有 turn-local deliberation hint、策略版本或粗粒度服务目标。直接添加字段会因 `additionalProperties: false` 失败，必须显式做 schema 演进。

5. **info — 固定 Stage/profile 边界应保留**  
   `configs/skills-execution.yaml:19-27` 已将 `wb-chat.semantic_routing` 固定到 `cx-adv`；`documents/designs/contracts/workbench-model-stage-contract.md:107-129` 明确禁止把 reasoning、预算、授权塞进 Stage 配置。让 `wb-chat` 按输入直接挑 `cx-spark/cx-deep/cc` 会破坏现有 owner/profile 契约。

## Drift / contradiction check

- 最大漂移是“模型不得升级授权”的注释、策略与实际代码不一致。
- 如果把 `complexity` 直接映射到 thinking/profile，会悄然把“owner 难选”改成“任务需要深思”，两者并不等价。
- 如果把 adaptive baseline 持久化到 session 或 memory，会把一次运行偏好升级为长期 Agent 行为，绕过 `wb-learning`。
- 如果由 `wb-chat` 决定 owner 后续调用哪些 Stage 或 critic，它就会从路由器漂移成通用 Workflow 编排器。
- 如果为了低延迟在歧义情况下猜 owner，会违背当前“降低 coverage、增加澄清，不降低安全门槛”的质量策略。

## Recommendation

### 1. P0：建立不可被模型升级的 Authorization Arbiter

将 `interaction_mode` 的事实来源限制为：

1. 当前输入的确定性授权规则；
2. 最近 route envelope 中更严格的既有 mode；
3. 明确 execution preview/confirmation gate 的机器状态。

语义模型只允许：

- 保持当前 mode；
- 建议更保守的 mode；
- 输出 `clarify`；

不得从 `None/review/discovery/proposal` 升级到 `execution`。

**收益**

- 消除模型误判直接获得写权限的路径。
- 让风险授权与模型质量彻底解耦。
- 为后续 adaptive effort 建立安全底座。

**代价**

- 需要补充中英文执行授权短语或结构化 API mode。
- 本地未识别的真实执行请求可能多一次追问。

**失败模式**

- 为提高 coverage 又把模糊“可以/继续”加入 execution 词表。
- 只修 prompt，不修接受门禁。
- 将 Git/session 的显式证据门禁误认为覆盖了普通 `wb-work`。

**最小验证**

- 表驱动测试：`可以/好的/继续/please proceed` 在无 pending gate 时，即使模型返回 `execution`，也不能 direct 到写 owner。
- property：`effective_mode <= deterministic_authorization`。
- 高风险及写 owner false positive 必须为零。

---

### 2. P1：用多轴 `Decision Vector` 替代单个 adaptive level

建议将路由结果内部拆成：

```text
owner_target
authorization
routing_shape:
  owner_ambiguity
  scope_completeness
  task_breadth
  evidence_uncertainty
service_objective:
  latency_class
  budget_class
  source
deliberation_hint:
  routine | standard | deep
  reasons
  ttl=turn
```

关键约束：

- `deliberation_hint` 是给 owner 的一轮 advisory，不是 profile 名，也不授权工具。
- `service_objective` 来自用户/API/session policy，不从“复杂”关键词擅自推断。
- owner 收到真实证据后有权重新评估 deliberation。
- 第一阶段只 shadow 计算，不改变执行。

**收益**

- 明确区分“难路由”和“难完成”。
- 可解释为什么追问、为什么建议深度审查。
- 允许逐轴评测，避免一个 `complex` 字段承担所有语义。

**代价**

- route envelope、schema、prompt、TaskRecord 和评测集需要协调演进。
- 轴过多会造成配置和调试复杂度。

**失败模式**

- 将 vector 变成新的通用 Workflow manifest。
- 允许 `wb-chat` 根据 hint 选择任意 model/profile。
- 把风险分数当作授权。

**最小验证**

- 同 owner 对照集：浅层代码解释与深度根因诊断应 owner 相同、deliberation 不同。
- 同 effort 对照集：多 owner 浅请求应高 routing ambiguity，但不必 deep。
- schema 拒绝 profile、model、permission、allowed path 等越权字段。

---

### 3. P1：自适应的是 Owner Workflow 拓扑，而非单次 thinking level

优先复用现有 Stage 结构：

- routine：确定性读取或单 Stage；
- standard：证据收集 + 生产 Stage；
- deep：证据分片 + 综合 + 独立 critic/review；
- blocked：缺证据或授权时停止。

例如：

- `wb-repo.inspect` 与 `wb-repo.diagnosis` 已自然表达不同深度；
- `wb-design.architecture → critic → implementation_plan` 已比“调高 thinking”更可靠；
- 高风险决策应增加独立 reviewer，而不是只增加同一模型的 token。

`wb-chat` 只能给 hint；是否进入 critic/review 由领域 owner 根据自身契约决定。

**收益**

- 提升来自证据和独立复核，而非不可解释的 token 增长。
- 保持 prompt、证据回读和质量责任在 owner。
- 可对每个 Workflow 做真实验证。

**代价**

- 深度路径增加模型调用、延迟和实现复杂度。
- 各 owner 需要定义何时升级/降级。

**失败模式**

- 所有 `complex` 请求无条件进入 critic，成本失控。
- critic 直接修改产物，破坏只读 review 边界。
- owner 盲信 `wb-chat` hint，不根据新证据重评。

**最小验证**

- 在诊断/设计小型语料上比较 single-pass 与 evidence+critic：
  - validator/reviewer finding 命中率；
  - 不实结论率；
  - 调用次数与耗时。
- 仅当质量增益超过预设阈值时启用 deep path。

---

### 4. P1：引入 turn-local deliberation lease 与抗抖动

借鉴 Pi 的 temporary reset 和 back-to-back no-op：

- 每轮开始重新计算 deliberation hint；
- hint 在 owner 切换、project/scope 变化、用户新输入或 turn settled 后失效；
- 新证据、测试失败、critic revise 才允许升级；
- 无新证据时不连续升降；
- persistent baseline 只能来自用户明确 session 偏好；
- repo/project 长期偏好必须走 `wb-learning`。

不要把 effort 写入 `active_owner`；连续性只保持任务身份、owner、授权和 handoff gate。

**收益**

- 防止长会话因一次复杂任务永久停留在高成本模式。
- 防止 profile/effort 来回振荡。
- 保留当前连续 owner fast lane。

**代价**

- 需要定义 turn settled、evidence delta 和 task shift。
- Runtime 与 Workbench envelope 需要稳定 correlation。

**失败模式**

- 把旧 TaskRecord 中的“deep”当当前授权或默认。
- scope 已变化仍复用旧 lease。
- 用 Agent 自己的 persist 请求建立长期偏好。

**最小验证**

- 生命周期测试：turn start、new evidence、owner handoff、scope change、settled。
- 无新证据的连续两次 effort 调整必须 no-op。
- settled 后新轻量问题必须回到默认，不继承 deep。

---

### 5. P1：把预算/延迟设计成独立 Service Objective

建议三层预算分别负责：

1. **路由预算**：exact/local/model/clarify 的最大花费；
2. **owner workflow 预算**：证据范围、Stage 次数、是否 critic；
3. **Runtime attempt 预算**：model/tool call、deadline、pause/cancel。

原则：

- 低预算且 owner 歧义时选择 `clarify`，不能低质量猜测。
- 预算耗尽返回 `partial|blocked`，不能扩大权限或切换到更危险 owner。
- Runtime hard cap 仍由 `sn-cli` Agent Kernel 持有，见 `runtime-vnext.md:46-54`。
- Workbench 只传显式 service objective，不复制 Runtime 预算实现。

**收益**

- 用户可选择“快答”或“深度核验”，而不影响授权。
- 延迟优化可以单独度量。
- 避免用 model/profile 名表达产品 SLO。

**代价**

- 当前 profile exec 无持久调用 telemetry，需要 Runtime 公共 usage/result 摘要或独立评测 harness。
- 预算跨 route、owner、Runtime 的归属需要明确。

**失败模式**

- 将 budget class 映射为绕过必要 review。
- timeout 后自动改用不适合的 owner。
- 把 Runtime 原始 transcript 或私有状态写入 TaskRecord。

**最小验证**

- `fast + ambiguous => clarify`；
- `deep + clear owner => owner 可升级 workflow`；
- Runtime budget exhausted => partial/blocked；
- 所有预算组合下授权 invariant 不变。

---

### 6. P2：使用 Capability Negotiation，而不是由 `wb-chat` 直接选 profile

短期维持：

```text
wb-chat -> owner -> stable Stage -> wb-model -> fixed profile -> sn-cli
```

增加只读 effective-policy 展示即可回答：

- owner 收到什么 deliberation hint；
- owner 选择了哪个 Stage；
- Stage 当前解析到哪个 profile；
- Runtime 支持什么能力和 deadline。

只有当至少两个真实 owner 证明“同一稳定 Stage 必须按 service objective 选择不同执行策略”时，才考虑修订 Model Stage 契约。届时应修订为**预注册策略类**，而不是允许任意 profile：

```text
stage + policy_class -> runtime-owned registered profile
```

这是明确的契约 pivot：修订当前“一 Stage 一个 profile”的假设；在证据出现前不应提前实施。

**收益**

- 保持配置和运行事实单一来源。
- 防止业务路由被 model/provider 名污染。
- 支持可解释 readback。

**代价**

- 无法立即做到同 Stage 动态换模型。
- 真正 capability API 需要 Runtime 配合。

**失败模式**

- route envelope 携带任意 profile。
- `WB_MODEL_PROFILE` 被当成生产 adaptive API。
- 根据模型自报能力选择安全策略。

**最小验证**

- 任意路由输出均不能包含 profile/model/provider。
- `skill_execution resolve --stage ...` 仍是唯一 profile 解析路径。
- effective-policy readback 与实际调用 profile 一致。

---

### 7. P1：把连续任务建模为有限 Routing Lease，不做中央任务编排

在现有 `active_owner/pending_handoff` 上增加明确失效条件：

- project 或 scope 变化；
- interaction mode 升级；
- 用户交付物变化；
- owner 的 handoff gate 已满足；
- 高风险动作首次出现；
- active owner 不再在加载面。

可记录 `reassessment_reason`，但不要让 `wb-chat` 保存 owner 内部 phase/checkpoint。

**收益**

- 减少“继续”误接旧任务。
- 延续 owner 时仍可重新评估 effort 和预算。
- 保持 `wb-chat` 不是 Workflow executor。

**代价**

- 需要更丰富的连续任务测试。
- 对模糊延续可能增加追问。

**失败模式**

- routing lease 逐渐演变成通用任务状态机。
- pending handoff 被自动执行。
- effort lease 与授权 lease 混为一体。

**最小验证**

- 同任务“继续分析”保持 owner；
- “按方案落地”必须重新过 execution gate；
- scope/project 切换必须失效；
- generic confirmation 不能满足 write/high-risk gate。

---

### 8. P1/P2：Shadow Evaluation + 受控反馈学习

第一阶段只 shadow 记录策略版本和建议，不改变执行。评测扩展为：

- owner/decision 正确率；
- authorization false positive；
- deliberation 上下界；
- unnecessary deep rate；
- deep-required miss rate；
- 路由延迟；
- owner Stage 数量；
- validator/critic 质量结果。

实际 token/cost/latency 应通过 Runtime 公共 typed usage/result 或离线 harness 获取，不直接读取 `~/.sn`，不复制 transcript。

学习规则：

- 单次慢或失败不调 profile；
- 至少两个独立证据；
- routing mismatch、effort mismatch、稳定 latency preference 分开；
- 只生成 candidate；
- 阈值、skill、config 的最终写入仍交对应 owner 并需用户确认。

**收益**

- 可以证明 adaptive 策略是否真的提升质量，而不是只增加成本。
- 避免在线自调造成不可复现漂移。
- 复用现有 learning gate。

**代价**

- 需要新增标注集和 aggregate telemetry。
- 用户满意度不足以单独归因，仍需 validator/外部证据。

**失败模式**

- 以 Agent 自评分替代真实质量。
- 根据一次成功自动沉淀长期偏好。
- 在线 bandit 自动改授权阈值或 profile。

**最小验证**

- 先跑 shadow corpus，不改变任何 route/profile；
- owner accuracy 不低于现有 95%；
- write/Git/session 授权误报为零；
- deep path 必须有可测质量增益，且延迟/成本在约束内；
- 无显著增益则停止，不进入执行面。

## 推荐实施顺序

1. 先修复模型升级授权的 blocker。
2. 新增 shadow-only Decision Vector，不接 profile。
3. 在 `wb-repo`、`wb-design` 两个已有多 Stage owner 上试验自适应 Workflow。
4. 加入 turn-local lease、抗抖动和 budget invariant。
5. 完成端到端评测后，再决定是否需要修订“一 Stage 一 profile”契约。

停止条件：

- 任一写入/高风险授权误报；
- owner accuracy 或 accepted precision 低于现有门槛；
- deep 路径没有稳定质量增益；
- 必须让 `wb-chat` 编排领域步骤才能工作；
- 需要读取 Runtime 私有状态或复制 transcript 才能评估。

## Risks

- 当前工作区存在大量非本任务未提交改动；所有结论基于当前机器状态，不能假设它们已进入基线。
- 未执行真实 `cx-adv`/Provider 调用，因此没有当前 profile 的实际 latency、token 或 cost 数据。
- `pi-adaptive-thinking` 是外部小型扩展，其 session/runtime 模型与 Workbench 不同，只能借鉴生命周期和抗抖动原则。
- Decision Vector 若一次引入过多字段，可能变成新的重复事实源；建议 shadow、最小字段、逐个消费者验证。
- Runtime usage/cost 若尚未通过公共 CLI/HTTP 暴露，应先扩展 Runtime 合约，不能由 Workbench 读取私有存储。

## Need from main agent

本轮无需额外决策。若后续进入实施，应先单独确认：

- 是否先只修授权 blocker；
- Decision Vector 是否只做 shadow；
- 是否接受在证据充分前维持固定 Stage/profile 契约。

## Suggested execution prompt

本轮是只读架构评估，不建议直接交给 executor。应先由主 Agent确认上述三个范围；尤其不能把完整方案一次性交给 `wb-work` 落地。