# AEP：统一 AI Agent 执行协议规范

> **入库说明（sn-runtime 维护注释）**
>
> - 本文是 AEP 规范 `0.1.0-draft` 原文在 `docs/aep/spec.md` 的落库副本（P0-1 交付物），
>   来源为规范作者提供的文本；除本说明块外未改写正文。
> - **规范缺口：** 原文自 §6.1 表格中部截断（`executor.get` 行），§6.1 之后（方法清单
>   其余条目、错误码表、`permission.requested` 回应方法、session 方法、生命周期等）尚未
>   提供。在补全前，sn-runtime 按 `docs/aep-landing-plan.md` D7 的 fallback 以 MVP 最小
>   方法集先行：`initialize`、`executor.list`、`executor.get`、`run.start`、`run.cancel`、
>   `run.get`、`run.result`、`run.events`、`shutdown`，事件通知方法 `run.event`。规范补全
>   后须回修并核对 D4/D5/D6/D9 是否冲突。
> - **D1 已确认（2026-10-28）：** 正文「实现语言」条目已按用户确认修订为「服务端
>   参考实现：Go（sn-runtime）；Client SDK：TypeScript（独立仓库）」。协议线上格式
>   为 JSON，语言无关。
> - 与 `docs/runtime-contract.md` 冲突时，以契约为准；本文只是协议规范，不是 Runtime 契约。

**英文名称：** Agent Execution Protocol
**协议标识：** `aep`
**文档版本：** `0.1.0-draft`
**传输协议：** stdio + JSON-RPC 2.0 + NDJSON
**实现语言：** 服务端参考实现 Go（sn-runtime）；Client SDK TypeScript（独立仓库）
**目标执行器：** Codex、Claude Code、PI
**兼容策略：** 内部协议为主，提供 Agent Client Protocol（ACP）兼容适配器

---

## 1. 协议目标

### 1.1 解决的问题

不同编码 Agent 在启动方式、参数配置、输出格式、执行状态、取消方式和结果结构方面存在差异。

AEP 为这些执行器提供统一的执行接口，使上层系统不再直接依赖某个 Agent 的命令行参数或输出格式。

AEP 必须提供：

1. 统一启动 Codex、Claude Code、PI。
2. 统一任务输入、执行参数和工作目录。
3. 统一查询执行状态。
4. 统一接收实时事件和增量输出。
5. 统一取消执行，并尽可能终止底层进程及其子进程。
6. 统一获取最终结果、错误和交付物。
7. 统一记录执行历史，并为后续恢复、重试提供基础。
8. 通过 ACP Adapter 接入符合现有 Agent Client Protocol 的 Agent。

### 1.2 非目标

AEP 不负责：

- 定义 PRD 拆分、方案设计、代码开发等业务工作流。
- 决定任务由哪个专家处理。
- 定义 Skill、MCP Tool 或 Subagent 的业务语义。
- 统一不同 Agent 的模型推理能力。
- 保证不同 Agent 产生完全相同的执行行为。
- 将所有 Agent 内部工具调用强行转换为完全等价的结构。

AEP 负责执行层契约，不负责业务编排。

### 1.3 核心设计原则

**原则一：统一契约，不统一实现。**

所有执行器实现同一套 AEP 接口，底层保留各自原生能力。

**原则二：协议与执行器解耦。**

增加新执行器时，原则上只需增加 Adapter，不修改协议核心。

**原则三：执行事实优先于文本推断。**

进程退出码、原生结构化事件和明确的完成信号优先于从普通文本中推断成功或失败。

**原则四：执行状态与 Agent 对话状态分离。**

Agent 输出结束不必然意味着任务成功；进程退出也不必然意味着产物符合预期。

**原则五：事件和结果可追溯。**

每次执行具有独立的 `runId`，事件具有稳定的 `eventId` 和单调递增的 `seq`。

**原则六：取消必须具有明确语义。**

取消请求、取消中、已取消是不同概念。发送取消请求不代表底层任务已经停止。

---

## 2. 整体架构

```text
                 Client / Workflow / CLI
                           |
                           v
                  AEP Client SDK
                           |
                   JSON-RPC over stdio
                           |
                           v
                   AEP Runtime Server
                           |
          +----------------+----------------+
          |                |                |
          v                v                v
     CodexAdapter    ClaudeCodeAdapter    PiAdapter
          |                |                |
          v                v                v
       Codex CLI       Claude Code CLI    PI Runtime
          |                |                |
          +----------------+----------------+
                           |
                           v
                Normalized Event Stream
                           |
                           v
                  Run Result / Artifacts
```

另设独立兼容入口：

```text
ACP Client
    |
    v
ACP Adapter
    |
    v
AEP Runtime
    |
    v
Codex / Claude Code / PI
```

ACP Adapter 的作用是把现有 ACP 的请求、会话、通知和取消语义映射到 AEP；它不是另一个执行引擎。

### 2.1 模块职责

| 模块 | 职责 |
|---|---|
| Protocol | 定义消息、类型、状态和错误码 |
| RPC Server | 解析请求、校验参数、分发方法 |
| Runtime | 管理执行生命周期和运行时资源 |
| Adapter | 将统一请求转换为执行器原生操作 |
| Event Normalizer | 将原生事件转换为 AEP 事件 |
| Result Collector | 收集最终状态、摘要、产物引用 |
| Run Store | 持久化执行元数据和事件索引 |
| ACP Adapter | 兼容现有 ACP 客户端 |
| Client SDK | 为调用方提供类型安全的调用接口 |

### 2.2 信任边界

Runtime 应当是执行器进程的生命周期管理者，但不应假设 Agent 是完全可信的。

执行器的命令、参数、环境变量、工作目录和权限策略必须经过校验。默认不允许通过协议请求任意执行未经授权的程序。

协议输出中的密钥、令牌和敏感环境变量不得进入事件日志或最终结果。

---

## 3. 传输与消息格式

### 3.1 传输约定

AEP v1 使用：

- JSON-RPC 2.0。
- 标准输入接收消息。
- 标准输出发送消息。
- NDJSON：每行一个完整 JSON-RPC 消息。
- UTF-8 编码。
- 每条消息写入后及时 flush。

标准输出只允许出现协议消息。日志、调试信息和诊断信息必须写入标准错误。

禁止把普通文本日志写入 stdout，否则会破坏协议流。

### 3.2 JSON-RPC 请求

```json
{
  "jsonrpc": "2.0",
  "id": "req_001",
  "method": "run.start",
  "params": {
    "executor": "pi",
    "input": {
      "prompt": "实现订单查询接口",
      "cwd": "/workspace/order-service"
    }
  }
}
```

成功响应：

```json
{
  "jsonrpc": "2.0",
  "id": "req_001",
  "result": {
    "runId": "run_001",
    "status": "pending"
  }
}
```

失败响应：

```json
{
  "jsonrpc": "2.0",
  "id": "req_001",
  "error": {
    "code": -32001,
    "message": "Executor unavailable",
    "data": {
      "reason": "EXECUTOR_NOT_INSTALLED",
      "executor": "pi",
      "retryable": false
    }
  }
}
```

### 3.3 通知

通知没有 `id`，不返回 JSON-RPC 响应。

例如：

```json
{
  "jsonrpc": "2.0",
  "method": "run.cancel",
  "params": {
    "runId": "run_001",
    "reason": "user_requested"
  }
}
```

**规范要求：** MVP 中取消采用有响应的 `run.cancel` 请求，以便明确返回是否接受取消以及当前状态。请求参数固定为 `{ "runId": "..." }`；上例中的 `reason` 不属于 MVP 字段，服务端 strict decode 按未知字段拒绝。上述无 `id` 的通知形式仅用于需要明确声明为通知的扩展场景，不作为首期取消接口。

### 3.4 请求约束

- 请求 `id` 在同一连接内必须唯一，且不能使用 `null`。
- 同一连接允许多个请求在途。
- 响应必须携带对应请求的 `id`。
- 事件通知不得伪装成请求响应。
- 未知方法返回 `METHOD_NOT_FOUND` 对应的 JSON-RPC 错误。
- 无法解析的 JSON 返回标准 JSON-RPC 解析错误。
- 不能将请求响应与事件输出混为同一种消息。

---

## 4. 核心对象模型

### 4.1 ExecutorDescriptor

描述一个执行器及其能力。

```ts
type ExecutorId = "codex" | "claude-code" | "pi" | "acp";

interface ExecutorDescriptor {
  id: ExecutorId;
  name: string;
  version?: string;
  available: boolean;

  capabilities: {
    streaming: boolean;
    cancellation: boolean;
    structuredEvents: boolean;
    sessionResume: boolean;
    nativeResult: boolean;
  };

  metadata?: Record<string, unknown>;
}
```

能力必须通过 Adapter 实际检测或配置确定，不允许仅因为协议支持某项功能，就宣称底层执行器也支持。

### 4.2 RunInput

```ts
interface RunInput {
  prompt: string;
  cwd: string;

  executor: ExecutorId;

  sessionId?: string;
  resumeFromRunId?: string;

  options?: {
    model?: string;
    reasoningEffort?: string;
    timeoutMs?: number;
    maxOutputBytes?: number;
  };

  permissions?: {
    mode?: "default" | "read_only" | "workspace_write";
    approvalPolicy?: "never" | "on_request" | "always";
  };

  metadata?: Record<string, unknown>;
}
```

约束：

- `cwd` 必须是规范化后的绝对路径。
- `prompt` 不能为空。
- `timeoutMs` 和输出上限必须经过服务端校验。
- 模型、推理强度和权限参数由 Adapter 映射到具体执行器。
- 不支持的参数必须明确拒绝或返回降级信息，不得静默声称生效。
- `resumeFromRunId` 仅在执行器确实支持会话恢复且相关上下文可用时有效。

### 4.3 Run

Run 表示一次实际执行实例。

```ts
type RunStatus =
  | "pending"
  | "running"
  | "waiting"
  | "cancelling"
  | "succeeded"
  | "failed"
  | "cancelled";

interface Run {
  runId: string;
  executor: ExecutorId;
  status: RunStatus;

  createdAt: string;
  startedAt?: string;
  finishedAt?: string;

  exitCode?: number | null;

  sessionId?: string;
  parentRunId?: string;

  resultRef?: string;
  error?: RunError;

  capabilities?: Record<string, boolean>;
}
```

`Run` 是运行时对象。`RunInput` 是启动参数，二者不能混为一体。

### 4.4 Session

Session 表示与 Agent 持续交互的上下文，不等于 Run。

```ts
interface Session {
  sessionId: string;
  executor: ExecutorId;

  createdAt: string;
  updatedAt: string;

  nativeSessionId?: string;
  resumable: boolean;

  metadata?: Record<string, unknown>;
}
```

不同执行器的 Session 能力可能不同：

- 有的支持显式会话 ID。
- 有的只能通过原生会话文件恢复。
- 有的需要特定的恢复参数。
- 有的仅能运行一次性任务。

Runtime 不得伪造原生会话能力。无法恢复时，`resumable` 必须为 `false`。

### 4.5 RunResult

```ts
interface RunResult {
  runId: string;

  status: "succeeded" | "failed" | "cancelled";

  summary?: string;
  exitCode?: number | null;

  usage?: {
    inputTokens?: number;
    outputTokens?: number;
    totalTokens?: number;
  };

  artifacts: ArtifactRef[];

  error?: RunError;

  startedAt?: string;
  finishedAt: string;

  completeness: "complete" | "partial";
}
```

`usage` 中的值只有在执行器提供可信数据时才能填写。未知值必须省略，不得估算后冒充真实用量。

`completeness` 表示结果信息是否完整，不等同于任务是否成功。

### 4.6 ArtifactRef

```ts
interface ArtifactRef {
  artifactId: string;

  type:
    | "file"
    | "directory"
    | "patch"
    | "report"
    | "log"
    | "other";

  uri: string;
  mediaType?: string;

  sizeBytes?: number;
  checksum?: string;

  metadata?: Record<string, unknown>;
}
```

产物通过引用传递，避免把大型文件内容塞进 JSON-RPC 消息。

---

## 5. 统一事件体系

### 5.1 EventEnvelope

所有事件必须使用统一外层结构。

```ts
interface EventEnvelope<T = unknown> {
  protocol: "aep";
  version: "1.0";

  eventId: string;
  runId: string;
  seq: number;

  timestamp: string;
  type: string;

  data: T;

  source?: {
    executor: ExecutorId;
    nativeEventType?: string;
  };
}
```

规范：

1. `eventId` 全局唯一。
2. `seq` 对每个 Run 单调递增，从 `1` 开始。
3. 同一 Run 的事件按 `seq` 顺序发出。
4. `timestamp` 使用 UTC RFC 3339 格式。
5. `type` 使用小写点分隔命名。
6. 原生事件无法识别时，允许使用 `executor.event` 携带原生事件元数据，但不得将未知事件强行映射为确定的成功或失败事件。

### 5.2 事件类型

| 事件 | 说明 |
|---|---|
| `run.created` | Run 已创建 |
| `run.started` | 底层执行已启动 |
| `run.status_changed` | 状态变化 |
| `run.progress` | 可选的进度信息 |
| `message.started` | 一条消息开始 |
| `message.delta` | 增量文本 |
| `message.completed` | 一条消息完成 |
| `tool.started` | 工具调用开始 |
| `tool.updated` | 工具调用进展 |
| `tool.completed` | 工具调用结束 |
| `permission.requested` | 需要用户或策略确认 |
| `artifact.created` | 产物已生成 |
| `run.completed` | Run 进入终态 |
| `run.failed` | Run 失败 |
| `run.cancelled` | Run 已取消 |
| `executor.event` | 无法标准化的原生事件 |

`run.completed` 只表示生命周期结束。其 `data.status` 必须进一步区分 `succeeded`、`failed` 和 `cancelled`，不能把三者都当作成功。

### 5.3 事件消息

事件使用 JSON-RPC 通知：

```json
{
  "jsonrpc": "2.0",
  "method": "run.event",
  "params": {
    "protocol": "aep",
    "version": "1.0",
    "eventId": "evt_001",
    "runId": "run_001",
    "seq": 3,
    "timestamp": "2026-10-28T07:10:00Z",
    "type": "message.delta",
    "data": {
      "messageId": "msg_001",
      "text": "正在实现订单查询接口"
    },
    "source": {
      "executor": "pi"
    }
  }
}
```

### 5.4 事件可靠性

stdio 本身不提供断线重放保证。因此：

- 事件写出后应在 Run Store 中记录。
- 客户端通过 `run.events` 请求查询已有事件。
- 客户端通过 `afterSeq` 查询后续事件。
- 客户端收到重复 `eventId` 时应去重。
- 客户端发现 `seq` 缺口时应重新查询。
- 事件落盘失败时必须记录可观测错误，不得假装事件已可靠保存。

对于 MVP，允许查询最近或全部已保留事件；不要求构建独立消息队列。

---

## 6. 方法清单

所有方法均采用 JSON-RPC 2.0。

### 6.1 初始化与能力发现

| 方法 | 作用 |
|---|---|
| `initialize` | 协议握手、版本和能力协商 |
| `executor.list` | 列出已配置执行器 |
| `executor.get` | 获取单个执行器描述符 |

<!-- AEP-SPEC-GAP：原文自此截断。§6.1 之后内容（其余方法清单、错误码表、
     permission.requested 回应方法、session 方法、生命周期/_shutdown 语义等）
     由规范作者补全后接续；sn-runtime 先按入库说明中的 D7 fallback 方法集执行。 -->
