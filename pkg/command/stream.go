package command

import (
	"encoding/json"

	"github.com/yy003x/sn-code/pkg/contract"
)

// StreamDecoder 由支持流式 stdout 协议的 adapter 实现。Build 接受
// OutputStreaming 的 adapter 必须实现该接口；managed exec 在执行前创建
// StreamSession 做增量解码。
type StreamDecoder interface {
	// NewStreamSession 执行 NewStreamSession 对应的包内处理，并遵循调用方提供的输入契约。
	// 返回 StreamSession 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	NewStreamSession() StreamSession
}

// StreamSession 是一次流式执行的解码会话：由单个 goroutine 按 stdout
// 顺序调用 DecodeLine，进程退出且 stdout 读尽后调用 Finish 校验终态。
// DecodeLine 返回错误表示流已不可信，调用方停止后续解码（继续排空
// stdout），Finish 一并返回该错误。
type StreamSession interface {
	// DecodeLine 执行 DecodeLine 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 line、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	DecodeLine(line []byte, emit func(contract.Event) error) error
	// Finish 执行 Finish 对应的包内处理，并遵循调用方提供的输入契约。
	// 返回 CanonicalOutput、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	Finish() (CanonicalOutput, error)
}

// structuredProtocols 报告 protocol 是否要求结构化 stdout（canonical 与
// streaming 共享 selector 纪律：forbidInCanon/kindCanonicalForbidden 选项
// 与 selector 归属规则同等适用）。
func structuredProtocols(protocol OutputProtocol) bool {
	return protocol == OutputCanonical || protocol == OutputStreaming
}

// boundedExecutorData 把原生记录摘录裁剪到 contract 上限内：超限时保留
// 独立字段的 shape 并截断长字符串。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 json.RawMessage 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func boundedExecutorData(value json.RawMessage) json.RawMessage {
	if len(value) <= contract.MaxExecutorEventDataBytes {
		return value
	}
	var decoded map[string]any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	trimmed := make(map[string]any, len(decoded))
	for key, item := range decoded {
		if text, ok := item.(string); ok && len(text) > 256 {
			trimmed[key] = text[:256] + "…"
			continue
		}
		trimmed[key] = item
	}
	bounded, err := json.Marshal(trimmed)
	if err != nil || len(bounded) > contract.MaxExecutorEventDataBytes {
		return json.RawMessage(`{"truncated":true}`)
	}
	return bounded
}

// newExecutorEvent 组装一条 executor 流式事件。data 接受原生记录摘录
// （json.RawMessage）或可序列化的键值摘录，统一裁剪到契约上限。
// 参数 kind、nativeType、id、name、text、isError、data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 contract.Event 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func newExecutorEvent(
	kind contract.EventType,
	nativeType, id, name, text string,
	isError bool,
	data any,
) contract.Event {
	payload := &contract.ExecutorEvent{
		NativeType: nativeType, ID: id, Name: name,
		Text: text, IsError: isError,
	}
	switch value := data.(type) {
	case nil:
	case json.RawMessage:
		if len(value) > 0 {
			payload.Data = boundedExecutorData(value)
		}
	default:
		if encoded, err := json.Marshal(value); err == nil && len(encoded) > 0 {
			payload.Data = boundedExecutorData(encoded)
		}
	}
	return contract.Event{Type: kind, Executor: payload}
}

// emitEvent 推送事件；未提供 sink 时静默跳过。
// 参数 event、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func emitEvent(
	event contract.Event,
	emit func(contract.Event) error,
) error {
	if emit == nil {
		return nil
	}
	return emit(event)
}
