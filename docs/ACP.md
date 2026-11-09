# Agent Client Protocol

sn-code 支持 **Agent Client Protocol v1 的 stdio JSON-RPC 接入**：

```sh
sn-code acp
```

编辑器启动配置的核心参数为：

```json
{"command":"sn-code","args":["acp"],"env":{}}
```

具体配置位置与外层字段遵循客户端的文档。stdout 只输出协议 JSON，诊断写入
stderr。交互 TUI 继续使用原入口；`--profile` 可在启动配置中指定初始 API
profile。会话 ID、恢复和输入由 ACP 请求管理，不使用 CLI 的 `--resume`。

## 协议与能力

依据 [ACP v1 规范](https://agentclientprotocol.com/protocol/v1/overview)实现，
协议类型为仓内手写投影（`internal/interfaces/codingacp/`），不引入外部 SDK：

- `initialize`：协商版本 1，报告实际能力与 sn-code 版本；按一期实际支持如实
  声明（不接收图像与嵌入上下文），`authMethods` 为空——认证即本机 active
  home 下的 API Profile 配置，无需独立认证方法。
- `session/new`：按客户端的绝对 `cwd` 创建新会话；会话落在该 cwd 对应的
  `sncode/sessions/<cwd-slug>/` 目录。
- `session/load`：按完整会话 ID 在请求 cwd 下精确匹配恢复（不使用前缀），
  先回放历史再返回；回放后可继续 prompt。
- `session/prompt`：一期只接收文本内容块（1–256 块、共 16 MiB 上限；文本块
  的平铺与 SDK 嵌套两种 wire 形态都接受）；输出正文与工具状态。
- `session/cancel`：取消对应会话的运行及待处理审批，不取消同连接其它会话。
- `session/update`：发送流式文本（每帧 ≤16 KiB）与工具状态；工具状态为
  pending、in_progress、completed、failed。
- `session/request_permission`：把内核确认门（写副作用工具）映射为客户端
  授权，提供 allow_once / allow_always / reject_once 三选项；`allow_always`
  即内核的会话级 always 记忆。超时（120 秒）、断连、取消或未识别的响应一律
  不视为批准。

prompt 响应表示本轮处理已结束，所有本轮更新先于响应发送。模型失败通过
JSON-RPC internal error 返回（`data.code=model_request_failed`），不伪装为
正常 `end_turn`；取消以原 prompt 的 `stopReason:"cancelled"` 收尾，cancel
通知本身不产生独立响应；输出长度截断返回 `max_tokens`。同会话正在运行时，
新的 prompt 返回 busy 错误，不隐式中断或加入旧队列。

加载同一活跃会话不能更换 cwd；要换目录应重连后 load。关闭连接时会取消本
连接的全部运行并关闭会话文件；stdin EOF 等价于关闭连接。

## 内容与范围

一期不支持的输入在 decode 阶段明确拒绝，不静默忽略：

- 图像、resource_link、嵌入 resource 内容块；
- 客户端 MCP 服务（`mcpServers` 非空）与 `additionalDirectories` 非空；
- 客户端 `fs/read_text_file` 委托与多 workspace。

不支持的输入或未获授权的审批明确失败/取消，不会自行视为用户批准。运行
文件与终端工具继续使用 sn-code 自身的执行环境与权限链。

注意区分：仓内 `executor=acp`（外部 agent 作为 CLI Profile 的 ACP client
adapter）与 AEP（durable Run 的 HTTP 入站协议）是另外两个互不相干的协议。

## 有界传输与验证

单行 JSON 输入最多 32 MiB（不含换行和 CRLF 的 CR）。错误不回显原始输入
正文；超限行丢弃到下一行后仍可继续握手或调用。连接的会话（≤32）、并发
请求（≤32）、审批请求（≤64）和写队列都有上限，客户端需要持续读取 stdout，
并保持输入连接直到收到对应结果。工具结果的文本展示最多 64 KiB，并标明
截断；wire 工具 ID 形如 `t{turn}-{seq}`，与内部原始调用 ID 一一对应。

实现直接复用 sn-code 自己的 AgentSession 内核（模型、工具、确认门、上下文
压缩与 JSONL 会话），不启动第二个 sn-code 进程，也不引入第二套状态机；与
TUI 共享同一会话文件格式，`sn-code --resume` 可以恢复 ACP 会话创建的文件
（反之亦然）。
