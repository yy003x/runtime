package aep

import (
	"fmt"

	"github.com/yy003x/sn-code/pkg/contract"
)

// JSON-RPC 2.0 标准错误码。
const (
	// codeParseError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeParseError int32 = -32700
	// codeInvalidRequest 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeInvalidRequest int32 = -32600
	// codeMethodNotFound 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeMethodNotFound int32 = -32601
	// codeInvalidParams 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeInvalidParams int32 = -32602
	// codeInternalError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeInternalError int32 = -32603
)

// AEP server 错误码（-32000 段）。
const (
	// codeServerError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeServerError int32 = -32000
	// codeExecutorUnavailable 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeExecutorUnavailable int32 = -32001
	// codeServerNotInitialized 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeServerNotInitialized int32 = -32002
	// codeNotFound 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeNotFound int32 = -32004
	// codeConflict 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeConflict int32 = -32005
)

// rpcError 是内部错误值；写出时映射为 JSON-RPC error 对象。
type rpcError struct {
	code    int32
	message string
	data    any
}

// invalidParamsError 执行 invalidParamsError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 format、args 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func invalidParamsError(format string, args ...any) *rpcError {
	return &rpcError{code: codeInvalidParams, message: fmt.Sprintf(format, args...)}
}

// notFoundError 执行 notFoundError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 format、args 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func notFoundError(format string, args ...any) *rpcError {
	return &rpcError{code: codeNotFound, message: fmt.Sprintf(format, args...)}
}

// conflictError 执行 conflictError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 format、args 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func conflictError(format string, args ...any) *rpcError {
	return &rpcError{code: codeConflict, message: fmt.Sprintf(format, args...)}
}

// internalError 执行 internalError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 err 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func internalError(err error) *rpcError {
	return &rpcError{code: codeInternalError, message: err.Error()}
}

// executorUnavailableError 按 AEP 规范 §3.2 示例 shape 携带 reason/executor/
// retryable。
// 参数 executor、reason 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func executorUnavailableError(executor, reason string) *rpcError {
	return &rpcError{
		code:    codeExecutorUnavailable,
		message: "Executor unavailable",
		data: map[string]any{
			"reason": reason, "executor": executor, "retryable": false,
		},
	}
}

// notInitializedError 执行 notInitializedError 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func notInitializedError() *rpcError {
	return &rpcError{
		code:    codeServerNotInitialized,
		message: "Server is not initialized; call initialize first",
	}
}

// methodNotFoundError 执行 methodNotFoundError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 method 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func methodNotFoundError(method string) *rpcError {
	return &rpcError{
		code:    codeMethodNotFound,
		message: fmt.Sprintf("Method %q not found", method),
	}
}

// errorFromRuntime 把 contract.RuntimeError 映射到 JSON-RPC 错误段；data 携带
// 脱敏后的 code/phase，不透传 secret 或传输层细节。
// 参数 err 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func errorFromRuntime(err *contract.RuntimeError) *rpcError {
	if err == nil {
		return internalError(fmt.Errorf("runtime error is nil"))
	}
	switch err.Code {
	case contract.ErrorNotFound:
		return &rpcError{code: codeNotFound, message: err.Message}
	case contract.ErrorConflict, contract.ErrorCancelled:
		return &rpcError{code: codeConflict, message: err.Message}
	case contract.ErrorInvalidRequest, contract.ErrorValidationFailed:
		return &rpcError{code: codeInvalidParams, message: err.Message}
	default:
		return &rpcError{
			code:    codeServerError,
			message: err.Message,
			data: map[string]any{
				"runtime_error": map[string]string{
					"code": string(err.Code), "phase": string(err.Phase),
				},
			},
		}
	}
}
