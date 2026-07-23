# Agent Runtime 生产可用度 85% 执行计划

目标：将当前项目提升到可量化的“可靠单机 Agent Runtime”生产可用度 85% 以上，清零已知 P0，并补齐目标范围内的 P1 功能与工程闭环。

边界：不实现分布式调度、多租户权限系统、Windows 支持和向量化长期记忆；不调用付费或生产 API；不执行 `git push`、tag、发布或部署；不删除或覆盖既有运行数据；历史产物只做兼容读取和增量演进，不自动改写。

任务类型：`code`、`doc`、`workflow`

风险等级：`medium`。本地源码、配置、测试和文档修改已获用户整体确认；任何外部发布、付费 API 调用或不可逆动作仍为 `high`，必须再次确认。

输入来源：当前仓库源码、测试、`README.md`、`docs/cli-routing-contract.md`、`docs/integration-arch.md`、用户确认的推荐方案、现有未提交修改。

产物路径：

- 执行计划：`docs/plans/2026-07-16-runtime-readiness-85.md`
- 实现：`cmd/`、`internal/`、`configs/`
- 工程门禁：`.github/workflows/`
- 契约与说明：`README.md`、`docs/cli-routing-contract.md`、`docs/integration-arch.md`
- 验证证据：测试命令输出、覆盖率报告和最终工作区 diff

验收标准：

1. 综合评分达到 85/100 以上：核心功能闭环 25、配置与安全 20、生命周期与稳定性 20、Provider/Adapter 15、测试与 CI 15、文档与可维护性 5。
2. 已知 P0 清零；计划范围内的 P1 清零，未完成项必须明确降级为实验能力且不计入完成分。
3. `go vet ./...`、`go test -p 1 ./... -count=1` 通过；普通并行 `go test ./... -count=1` 连续 5 次通过。
4. 核心非 tmux 包 race 测试通过；tmux 生命周期测试不再因共享会话名或残留资源产生并行冲突。
5. 全仓语句覆盖率不低于 65%；`internal/agentrun`、`internal/provider`、`internal/transport` 等核心包目标不低于 70%，确因外部进程边界无法覆盖的分支需记录理由。
6. CLI 帮助、路由契约、README、架构文档和实际行为一致；历史 profile 和运行产物保持兼容。
7. CI 覆盖 pull request/push 的构建、测试、vet 和关键 race 门禁。

建议执行方式：在 `feat/runtime-readiness-85` 分支按阶段连续执行；每阶段最多 3 个任务，测试失败时先定位并修复，不带失败进入下一阶段。

## 场景路由

- 应调用：代码库本地工具；全部完成后调用 `mozi-task-finalizer` 收尾。
- 不应调用：生产数据、外部消息、付费模型 API、发布部署类 Skill。
- 与 Record 边界：本计划是仓库执行协议，不写入个人 Record。
- 是否需要升级 Agent Loop：否；本次是有界的本地开发任务，不创建长期调度。

## 阶段 0：基线与变更隔离

### 任务 0.1：保护现有改动并建立可复现基线

- 动作：保留 `internal/agentrun/command.go`、`service.go`、`service_test.go`、`tmux.go` 的既有修改；在新分支记录状态；运行串行测试、并行测试、vet 和覆盖率基线。
- 输出：基线失败清单和覆盖率基线，不修改用户已有意图。
- 验证：`git status --short --branch`、`go vet ./...`、`go test -p 1 ./... -count=1`、`go test ./... -count=1`、`go test ./... -coverprofile=/tmp/runtime-baseline.cover`。
- 阻塞条件：现有修改无法编译，或发现其意图与本计划冲突。
- 是否需要用户确认：否；用户已确认保留并纳入基线。
- 建议调用：代码库本地工具。

## 阶段 1：P0 安全、路径与契约

### 任务 1.1：收紧 HTTP Server 默认安全边界

- 动作：先补失败测试，再将默认监听地址改为 loopback；增加请求体限制、完整 HTTP 超时、Bearer Token 策略、方法/Content-Type 校验，以及 `cwd`、profile、prompt file 输入边界校验。
- 输出：安全默认值、显式的非 loopback 鉴权门禁、稳定错误响应和对应文档。
- 验证：`go test ./internal/transport ./cmd/sn-server -count=1`；定向测试覆盖超大 body、无 token 的非 loopback 配置、非法路径和超时配置。
- 阻塞条件：安全行为必须破坏已公开且无法兼容的 HTTP 契约。
- 是否需要用户确认：否；推荐安全策略已确认。
- 建议调用：代码库本地工具。

### 任务 1.2：阻断 loop_id 路径穿越和覆盖

- 动作：为所有运行 ID 建立统一校验；在任何 `Join`、创建、覆盖、删除前校验；补 `../`、绝对路径、分隔符、空值、超长值和合法值测试。
- 输出：安全 ID 类型或校验函数，loop/task/turn/session/command 路径行为一致。
- 验证：`go test ./internal/agentrun -run 'Loop|ID|Path' -count=1`，并确认非法 ID 不在 runtime root 外创建或删除文件。
- 阻塞条件：历史合法 ID 不满足新规则且无法兼容读取。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具。

### 任务 1.3：修复 profile 配置并对齐 CLI 路由契约

- 动作：修改前完整读取 `docs/cli-routing-contract.md`；修复 `cc` API Key 字面量；让 `cx`/`cc` 默认继承账号目录；固定账号改由专用 preset/profile 承担；保留 `providers`/`upgrade` 为 legacy alias 并纳入保留字、帮助、测试和文档；严格校验未知配置键。
- 输出：配置、CLI、帮助、测试、README 和契约同步变更。
- 验证：`go test ./internal/cli/... ./internal/provider/... -count=1`；以隔离 `SN_CLI_HOME` 运行 `profiles`、`doctor`、`config validate`；检查帮助与契约一致。
- 阻塞条件：现有 profile 的必要账号语义无法由 preset 兼容承接。
- 是否需要用户确认：否；兼容策略已确认。
- 建议调用：代码库本地工具。

## 阶段 2：生命周期、并发与产物一致性

### 任务 2.1：落实并发限制、跨进程锁和幂等语义

- 动作：补并发失败测试；让 `max_concurrency` 在创建运行前生效；为 status/events/registry 和 memory 的读改写建立跨进程安全机制；同一幂等键必须比较规范化请求，不同请求返回冲突。
- 输出：确定性的并发上限、无丢失事件的持久化和可解释的幂等响应。
- 验证：`go test -race ./internal/agentrun ./internal/capability -count=1`，增加多 goroutine/多进程竞争测试和冲突请求测试。
- 阻塞条件：需要引入外部数据库或破坏历史文件格式。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具。

### 任务 2.2：执行 deadline 并收敛 tmux 生命周期

- 动作：命令超时后按 TERM/KILL 阶段退出；日志持久化完成后清理 tmux 会话；为失败现场提供有界保留配置；生成唯一测试会话名并可靠清理；修复会话消失但状态仍 running 的分支。
- 输出：无永久 sleep pane、无共享测试会话冲突、超时与退出状态准确。
- 验证：`go test ./internal/agentrun ./internal/provider/tmux ./internal/daemon -count=1`；普通并行回归至少连续 5 次。
- 阻塞条件：当前平台 tmux 本身不可用，且无法以 fake backend 验证核心状态机。
- 是否需要用户确认：否；生命周期策略已确认。
- 建议调用：代码库本地工具。

### 任务 2.3：统一 loop 产物与 action 门禁

- 动作：loop 写入标准 `result.json`，统一 outcome 枚举并兼容读取旧产物；将 `allowed_actions`/`forbidden_actions` 接入内置 tool/native 执行边界；外部 CLI 无法硬拦截的能力明确标记为 advisory。
- 输出：统一的 status/events/output/result 契约和实际生效的本地 action 策略。
- 验证：`go test ./internal/agentrun ./internal/capability ./internal/provider/native -count=1`；覆盖允许、拒绝、旧产物读取和取消/失败/完成结果。
- 阻塞条件：公开产物契约与历史数据无法做兼容映射。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具。

## 阶段 3：Native Adapter 与 Provider 一致性

### 任务 3.1：统一 OpenAI/Anthropic 客户端协议语义

- 动作：补齐 endpoint 拼接、认证 header、额外 header、超时、错误体限制和响应解析测试；复用统一客户端能力，避免 direct API 与 native API 形成两套漂移实现。
- 输出：OpenAI-compatible 和 Anthropic-compatible 的一致请求模型及测试 server 契约。
- 验证：`go test ./internal/llm/... ./internal/provider/native... -count=1`；`httptest.Server` 验证 URL/header/body/error。
- 阻塞条件：需要真实供应商凭据才能验证的特性不得假定通过。
- 是否需要用户确认：否；不调用真实付费 API。
- 建议调用：代码库本地工具。

### 任务 3.2：补齐 Done 判定和 tool-call 回路

- 动作：定义模型完成、继续、工具调用、最大轮次和取消条件；真实 adapter 正确设置 `Done`；工具结果回填上下文；拒绝未授权 action；持久化每轮可诊断信息并脱敏。
- 输出：可终止、可取消、可受控调用工具的 native loop。
- 验证：`go test -race ./internal/provider/native ./internal/agentrun -run 'Native|Tool|Loop' -count=1`；mock 覆盖立即完成、多轮工具、拒绝、超限、取消和上游错误。
- 阻塞条件：工具协议需要大幅改变公开请求 schema，且无法做增量字段兼容。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具。

### 任务 3.3：补齐 Native profile 与端到端契约测试

- 动作：提供不含密钥的示例 native profile/preset；增加 mock provider 端到端运行；doctor 能明确区分缺凭据、配置错误和网络错误。
- 输出：开箱可理解的 native 配置样例和无付费依赖的 e2e 测试。
- 验证：隔离 `SN_CLI_HOME` 运行 profile validate/doctor/native mock e2e；`go test ./internal/provider/... ./internal/cli/... -count=1`。
- 阻塞条件：配置字段不足以表达统一客户端语义。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具。

## 阶段 4：测试、CI、可维护性与文档

### 任务 4.1：建立持续集成和覆盖率门禁

- 动作：增加 pull request/push CI，执行构建、vet、串行与并行测试、关键 race 和覆盖率检查；为外部二进制/tmux 测试提供明确安装或跳过契约。
- 输出：`.github/workflows/ci.yml`、覆盖率检查脚本或 Make target。
- 验证：本地逐条执行 CI 命令；校验 workflow YAML；不 push、不触发远端工作流。
- 阻塞条件：现有测试只能依赖本机私有资源且无法隔离。
- 是否需要用户确认：否；仅修改仓库文件。
- 建议调用：代码库本地工具。

### 任务 4.2：补齐薄弱测试并拆分高复杂文件

- 动作：优先提高 openai/persona/transport/CLI 覆盖；在行为测试保护下拆分 `runtime_commands.go`、`provider/config.go`、`agentrun/service.go`，保持包 API 和 CLI 行为不变。
- 输出：职责更清晰的文件布局、回归测试和达到门槛的覆盖率。
- 验证：按包覆盖率报告、`go test ./... -count=1`、`go vet ./...`、`git diff --check`。
- 阻塞条件：拆分会掩盖未解决的行为缺陷；此时先补测试，不强行重构。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具。

### 任务 4.3：同步文档并完成最终评分

- 动作：同步 README、CLI 契约、架构、配置样例、HTTP 安全和产物说明；按评分矩阵逐项提供证据；列出未计分的实验能力和剩余 P2。
- 输出：一致的用户文档、最终评分表和交付报告。
- 验证：全量命令通过；帮助、profile 数量、配置字段和文档示例可执行；`git diff --check` 且工作区仅含本计划相关变更与已知基线修改。
- 阻塞条件：最终评分不足 85 或仍有 P0/目标 P1；不得宣称完成。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具；完成后 `mozi-task-finalizer`。

## 阶段 5：API Provider Agent Runtime 增补目标

### 任务 5.1：统一 API Agent loop 与本地 context

- 动作：保持 `type=api` direct request 兼容；以 `api.runtime.enabled` 显式进入进程内 Agent loop；OpenAI/Anthropic 复用统一消息、tool call、finish reason、usage 和 context snapshot；接入 block/continue/patch-resume/stop/cancel。
- 输出：`context-snapshot.json`、标准 status/events/result 与 OpenAI/Anthropic API Agent profile 模板。
- 验证：本地 `httptest` 分别覆盖 OpenAI `tool_calls` 和 Anthropic `tool_use/tool_result` 两轮回路；API mock 覆盖 context patch-resume。
- 阻塞条件：需要真实供应商凭据才能验证的特性不得假定通过。
- 是否需要用户确认：否；用户已明确追加该目标。
- 建议调用：代码库本地工具。

### 任务 5.2：接入 MCP、skill 与 memory

- 动作：实现 MCP stdio JSON-RPC 生命周期、工具发现分页和工具调用；skill 显式加载/自动路由；memory 首轮召回及受权读写删除工具；所有工具执行接入 `allowed_actions`/`forbidden_actions`。
- 输出：MCP client、API Agent capability 装配器、HTTP 权限字段和安全说明。
- 验证：本地 MCP 子进程 fixture 完成 initialize/list/call；API e2e 验证 MCP 结果回填、skill/memory 注入与 memory 持久化。
- 阻塞条件：HTTP MCP、OAuth、MCP resources/prompts/sampling 不纳入本阶段，明确列为后续能力。
- 是否需要用户确认：否；stdio 是本阶段推荐最小完整实现。
- 建议调用：代码库本地工具。

### 任务 5.3：API Runtime 全量验收

- 动作：同步 README/架构/配置/doctor；重新执行格式、构建、vet、串并行、race 和覆盖率门禁；按新增范围复评完成度。
- 输出：可执行文档、验证证据、评分与剩余缺口。
- 验证：与阶段 4 的全量门禁相同，且 API Runtime 新增测试全部通过。
- 阻塞条件：覆盖率跌破门禁、协议 fixture 失败或 context 无法恢复时不得宣称完成。
- 是否需要用户确认：否。
- 建议调用：代码库本地工具；完成后统一 finalizer。

## 完成检查

- [x] 所有任务均有具体测试或文件回读证据。
- [x] P0 清零，目标范围内 P1 清零。
- [x] 高风险动作未执行；未调用真实/付费模型 API，未 push、tag、发布或部署。
- [x] 外部副作用为零，无需外部 readback。
- [x] 产物路径与历史兼容策略明确。
- [x] 全量测试、race、vet、覆盖率和文档一致性达标。
- [x] 已通过 `mozi-task-finalizer` 完成 session/history 和默认完成通知。

最终工程评分：91/100。核心功能 23/25、配置与安全 18/20、生命周期与稳定性 18/20、Provider/Adapter 14/15、测试与 CI 13/15、文档与可维护性 5/5。

未纳入本阶段的 P2：MCP Streamable HTTP/OAuth/resources/prompts/sampling、API Agent 流式 tool-call delta、向量化/语义 memory、自动摘要式 context compaction、真实供应商凭据 smoke、分布式调度和多租户权限系统。
