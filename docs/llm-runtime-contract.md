# LLM Runtime 契约

## 1. 定位

Runtime 是模型执行与上下文装配层，不是业务 Agent 框架。

- 调用方拥有业务 Session、Agent loop、重试、人工干预和业务编排。
- Runtime 负责读取受控上下文资产、生成统一模型请求、调用 API/CLI provider，并按显式模式返回或执行 tool call。
- 具体业务能力不进入 Runtime；它们由调用方实现，或以 tool/MCP 服务接入。
- Runtime 以本地调用为主，公开 `sn-cli` command、Go SDK 和本地 HTTP API（RPC 风格）三类入口。

## 2. 统一调用链

command、Go SDK 和本地 HTTP API 使用同一 Provider 与执行核心：

```text
local caller
  ├─ sn-cli llm generate --request-file ...
  ├─ llmruntime.Runtime.Generate/GenerateStream(...)
  └─ runtimeclient.Client.Generate/GenerateStream(...) -> local HTTP API
                  │
                  ▼
        AssetResolver / Registries
                  │
                  ▼
        canonical LLM request
                  │
          ┌───────┴────────┐
          ▼                ▼
      API adapter       CLI adapter
```

Provider 只接收已编译的 `system/messages/tools`，不读取 skill、prompt 或 memory 文件。

## 3. 请求模型

`runtimeapi.Request` 是 SDK 与 `POST /v1/llm/generate` 的共同 JSON 契约，包含：

- `profile`：现有 `configs/<profile_id>.json`；
- `system`、`prompt`、`messages`：调用方直接提供的上下文；
- `context.prompts`、`context.skills`、`context.memory`：需要 Runtime 加载的资产引用；
- `context.recall`：选择进程内预注册 `MemoryProvider` 并执行受限 recall；
- `tools.inline`：调用方提供 schema 并由调用方执行；
- `tools.registered`：选择进程内预注册 tool；
- `tools.mcp`：选择进程内预注册 MCP server；
- `tool_mode`：`schema_only` 或 `runtime_execute`；
- `max_rounds`、`temperature`、`max_tokens`。

`schema_only` 是默认值。此模式只向模型暴露 tool schema，Runtime 返回 tool calls，由调用方决定后续循环。`runtime_execute` 会在当前请求内执行已注册 tool/MCP，并继续模型循环；inline tool 没有 Runtime handler，因此不能用于该模式。

API profile 可配置 `max_tokens` 作为默认最大输出 token 数。SDK/HTTP 请求的 `max_tokens` 非零时覆盖 profile；两者都未提供时保留现有兼容默认值。Runtime 当前没有独立的输入 token 上限字段。

## 4. 资产加载

资产使用以下二选一输入：

- `inline`：内容随请求传入；
- `uri`：`asset://<root>/<relative-path>`。

`root` 必须由进程启动时配置。Resolver 会拒绝未知 root、绝对路径、目录穿越、NUL、越过 root 的 symlink、非常规文件和超限文件。HTTP 请求不能临时注册宿主机路径或执行命令。

当前 loader 约定：

- prompt：UTF-8 文本；
- memory：UTF-8 文本或 JSON，作为低优先级上下文注入；
- skill：Markdown 文件，或现有 `skill.yaml` / `*.skill.yaml`；YAML skill 的 `entry` 仍必须位于 skill 目录内；
- `sha256` 可用于锁定资产内容，校验失败即拒绝执行。

同一本地运行环境内，SDK 与 HTTP 都能读取已配置目录中的 `asset://` 资产。嵌入式 Go 应用也可以直接构造 Runtime，并提供自己的 root 配置。

文件资产使用有界进程内缓存，默认最多 128 项；命中条件是解析后的文件路径、
size 与 mtime 均未变化，超限时淘汰最久未访问项。`sha256` 即使在缓存命中时也会
继续校验。SDK 可通过 `MaxAssetSize` 和 `AssetCacheEntries` 调整限制。

## 5. Memory

`RegisterMemoryProvider(name, provider)` 注册动态 memory recall 实现。请求只能通过
`context.recall[].provider` 选择已注册名称，不能上传实现或查询任意本地索引。

- `query` 为空时使用本次 `prompt`，否则使用最后一条 user message；
- `top_k` 默认 5，最大 100；
- provider 返回项按顺序截断到 `top_k`，单项内容最大 64 KiB；
- ContextCompiler 的固定顺序是 system、prompt assets、skills、静态 memory assets、
  recall 结果；
- 静态文件和动态 recall 都只负责读取与上下文装配，Runtime 不定义长期 memory
  写入策略。

stock `sn-server` 默认没有通用 memory backend。嵌入式服务可以注册
`MemoryProvider`；只需要文件输入时使用 `context.memory` 与 `asset://` 即可。

## 6. Tool 与 MCP

Tool schema 和 executor 分离：

- `RegisterTool` 注册稳定名称、JSON Schema 和 Go handler；
- 请求只引用注册名称，不能通过 HTTP 上传 handler；
- 同名 inline、registered、MCP tool 冲突时请求失败；
- handler 返回值会序列化为 tool result message。

MCP 是一种 ToolProvider，但保留独立的 `RegisterMCP`，因为它还包含进程生命周期、环境、超时、发现和关闭语义。当前支持预注册的 stdio MCP；HTTP 请求只能引用已注册名称，不能提交任意 command。

stock `sn-server` 从 `configs/runtime.yaml` 的 `llm.mcp_servers` 静态注册 MCP。
server 只在请求通过 `tools.mcp` 选择名称后启动，并在该请求结束时关闭。MCP
子进程只获得基础运行环境、`env_passthrough` 白名单和显式 `env`，不继承全部
server 环境。

Runtime 不提供另一套私有 HTTP tool executor：进程内实现使用 `RegisterTool`，
进程外实现使用 MCP。

## 7. 流式事件

`GenerateStream` 与非流式 `Generate` 使用相同请求和最终响应。事件按单次请求内
单调递增的 `sequence` 输出：

```text
request.started
context.compiled
provider.started
output.delta
tool.call
tool.started
tool.completed
response.completed
error
```

API adapter 的 `output.delta` 是 Provider stream delta；CLI adapter 是可观察的
stdout chunk。`tool.started|tool.completed` 只在 `runtime_execute` 中出现。
EventSink 返回错误会终止当前请求。

三类入口的流式传输：

- Go SDK：`Runtime.GenerateStream(ctx, request, sink)`；
- HTTP：`POST /v1/llm/generate` 携带 `Accept: text/event-stream`；
- command：`sn-cli llm generate --request-file request.json --stream`，每行一个
  `runtimeapi.Event` JSON。

非流式 HTTP 和 command 分别返回最终 `runtimeapi.Response` JSON。

## 8. Provider 边界

- API profile 支持结构化 messages、tools 和 tool calls。
- CLI profile 接收 ContextCompiler 生成的文本 prompt；当前只支持无 tools 的单轮请求。
- 业务 Session ID、持久化、分布式 lease/queue 不属于本接口。调用方若要关联链路，应使用自己的 metadata 和日志/trace 体系。
- Runtime 不自动扫描调用方工作目录；所有本地能力必须显式 inline、`asset://` 引用或进程内注册。

## 9. 安全与部署

- `sn-server` 非 loopback 监听时仍必须配置 `SN_SERVER_TOKEN`。
- HTTP 资产 root 由 `configs/runtime.yaml` 的 `assets.roots` 配置，修改后重启生效。
- Tool/MCP 注册属于部署配置或 SDK 启动代码，不属于每次请求。
- API key 继续由 provider profile 的环境变量占位符解析，不进入请求体和运行产物。
- 当前部署模型是 local-first：profile、asset root、MCP 和 Session 都由单机
  Runtime 配置与 Runtime home 管理，不承诺共享 Store、共享 Queue、分布式
  Lease 或多副本一致性。
