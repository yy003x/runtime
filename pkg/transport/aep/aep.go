// Package aep implements the AEP (Agent Execution Protocol) inbound adapter:
// stdio + JSON-RPC 2.0 + NDJSON。协议规范见 docs/aep/spec.md，Runtime 语义以
// docs/runtime-contract.md 为准。本包只做协议投影，不建立第二套 Run/Session
// 状态机：所有执行、取消、终态与事件都复用 pkg/run 的 durable Run。
package aep

// ProtocolName 与 EventEnvelope.protocol 一致。
const ProtocolName = "aep"

// ProtocolVersion 是本服务端实现并协商的唯一协议版本；EventEnvelope.version
// 与 initialize 返回的 version 使用同一值。
const ProtocolVersion = "1.0"

// MaxMessageBytes 限制单条 NDJSON 消息大小，与既有 invocation/manifest 硬上限
// （1 MiB）对齐。
const MaxMessageBytes = 1 << 20
