package aep

import (
	"encoding/json"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
	"github.com/yy003x/sn-code/pkg/run"
)

// AEP RunStatus 常量（D4 映射表）。
const (
	// statusPending 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusPending = "pending"
	// statusRunning 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusRunning = "running"
	// statusWaiting 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusWaiting = "waiting"
	// statusCancelling 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusCancelling = "cancelling"
	// statusSucceeded 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusSucceeded = "succeeded"
	// statusFailed 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusFailed = "failed"
	// statusCancelled 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	statusCancelled = "cancelled"
)

// projectRunStatus 把 durable Run state 投影为 AEP RunStatus：
// queued→pending、running→running、paused→waiting、completed→succeeded、
// failed/cancelled→failed/cancelled；cancel_requested 且非终态投影为
// cancelling；needs_reconciliation 投影为 failed（不新建 AEP 状态）。
// 参数 record 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func projectRunStatus(record run.Record) string {
	if !record.State.Terminal() && record.CancelRequested {
		return statusCancelling
	}
	return projectState(record.State)
}

// projectState 执行 projectState 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 state 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func projectState(state run.State) string {
	switch state {
	case run.StateQueued:
		return statusPending
	case run.StateRunning:
		return statusRunning
	case run.StatePaused:
		return statusWaiting
	case run.StateCompleted:
		return statusSucceeded
	case run.StateCancelled:
		return statusCancelled
	case run.StateFailed, run.StateNeedsReconciliation:
		return statusFailed
	default:
		return statusFailed
	}
}

// projectRunError 优先按 needs_reconciliation 投影协议错误码（D4），
// 同时保留 RuntimeError 的 message 和 phase。
// 参数 record 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RunError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func projectRunError(record run.Record) *RunError {
	var projected *RunError
	if record.Error != nil {
		projected = &RunError{
			Code: string(record.Error.Code), Phase: string(record.Error.Phase),
			Message: record.Error.Message,
		}
	}
	if record.State == run.StateNeedsReconciliation {
		if projected == nil {
			projected = &RunError{
				Message: "Run outcome is unknown and requires explicit " +
					"reconciliation after an interrupted execution",
			}
		}
		projected.Code = "reconciliation_required"
	}
	return projected
}

// projectEvent 把 durable journal event 投影为 AEP EventEnvelope：
// run.completed/run.failed/run.cancelled 保留类型并携带 data.status，
// run.settled 投影为 run.status_changed；executor 流式归一化事件映射为
// message.*/tool.*（携带原生记录类型）；未识别类型兜底 executor.event 且
// 不映射为成功或失败（D5）。
// 参数 event、runID、executor、now 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 EventEnvelope、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func projectEvent(
	event contract.Event,
	runID string,
	executor string,
	now func() time.Time,
) (EventEnvelope, bool) {
	var eventType string
	var data any
	nativeType := string(event.Type)
	switch event.Type {
	case contract.EventRunCompleted:
		eventType = "run.completed"
		data = map[string]string{"status": statusSucceeded}
	case contract.EventRunFailed:
		eventType = "run.failed"
		failure := map[string]string{"status": statusFailed}
		if event.Error != nil {
			failure["error"] = string(event.Error.Code)
		}
		data = failure
	case contract.EventRunCancelled:
		eventType = "run.cancelled"
		data = map[string]string{"status": statusCancelled}
	case contract.EventRunSettled:
		eventType = "run.status_changed"
		data = map[string]any{"settled": true}
		if event.Run != nil {
			data.(map[string]any)["status"] = projectState(run.State(event.Run.State))
		}
	case contract.EventExecutorMessageDelta:
		executorEvent := event.Executor
		eventType = "message.delta"
		nativeType = executorNativeType(executorEvent, nativeType)
		data = executorMessageData(executorEvent)
	case contract.EventExecutorMessage:
		executorEvent := event.Executor
		eventType = "message.completed"
		nativeType = executorNativeType(executorEvent, nativeType)
		data = executorMessageData(executorEvent)
	case contract.EventExecutorToolStarted:
		executorEvent := event.Executor
		eventType = "tool.started"
		nativeType = executorNativeType(executorEvent, nativeType)
		tool := map[string]any{"toolCallId": executorEvent.ID}
		if executorEvent.Name != "" {
			tool["name"] = executorEvent.Name
		}
		if len(executorEvent.Data) > 0 {
			tool["input"] = json.RawMessage(executorEvent.Data)
		}
		data = tool
	case contract.EventExecutorToolCompleted:
		executorEvent := event.Executor
		eventType = "tool.completed"
		nativeType = executorNativeType(executorEvent, nativeType)
		tool := map[string]any{
			"toolCallId": executorEvent.ID, "isError": executorEvent.IsError,
		}
		if executorEvent.Name != "" {
			tool["name"] = executorEvent.Name
		}
		if executorEvent.Text != "" {
			tool["output"] = executorEvent.Text
		}
		if len(executorEvent.Data) > 0 {
			tool["detail"] = json.RawMessage(executorEvent.Data)
		}
		data = tool
	case contract.EventExecutorNative:
		executorEvent := event.Executor
		eventType = "executor.event"
		nativeType = executorNativeType(executorEvent, nativeType)
		fallback := map[string]any{"nativeType": nativeType}
		if len(executorEvent.Data) > 0 {
			fallback["data"] = json.RawMessage(executorEvent.Data)
		}
		data = fallback
	default:
		eventType = "executor.event"
		data = map[string]string{"nativeType": nativeType}
	}
	timestamp := ""
	if event.Time != nil {
		timestamp = event.Time.UTC().Format(time.RFC3339Nano)
	} else if now != nil {
		timestamp = now().UTC().Format(time.RFC3339Nano)
	}
	return EventEnvelope{
		Protocol: ProtocolName, Version: ProtocolVersion,
		EventID: eventID(runID, event.Sequence), RunID: runID,
		Seq: event.Sequence, Timestamp: timestamp,
		Type: eventType, Data: data,
		Source: &EventSource{
			Executor: executor, NativeEventType: nativeType,
		},
	}, true
}

// executorNativeType 执行 executorNativeType 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 executorEvent、fallback 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func executorNativeType(
	executorEvent *contract.ExecutorEvent,
	fallback string,
) string {
	if executorEvent == nil || executorEvent.NativeType == "" {
		return fallback
	}
	return executorEvent.NativeType
}

// executorMessageData 执行 executorMessageData 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 executorEvent 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 map[string]any 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func executorMessageData(executorEvent *contract.ExecutorEvent) map[string]any {
	data := map[string]any{"text": executorEvent.Text}
	if executorEvent.ID != "" {
		data["messageId"] = executorEvent.ID
	}
	return data
}

// formatTimestamp 执行 formatTimestamp 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func formatTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// sessionResultShape 是 session.RunResult 终态 JSON 中 AEP 需要的字段。
type sessionResultShape struct {
	ExitCode *int `json:"exit_code"`
	Message  *struct {
		Content string `json:"content"`
	} `json:"message"`
}

// parseSessionResult 执行 parseSessionResult 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 record 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 sessionResultShape 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func parseSessionResult(record run.Record) sessionResultShape {
	var shape sessionResultShape
	if len(record.Result) == 0 {
		return shape
	}
	// 终态 result 已过 Store barrier 提交；此处仅读取已知字段，畸形结果不
	// 影响协议投影（字段缺省即省略）。
	_ = json.Unmarshal(record.Result, &shape)
	return shape
}
