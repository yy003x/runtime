package aep

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/yy003x/sn-code/internal/infrastructure/strictjson"
)

// rpcRequestEnvelope 是入站 JSON-RPC 消息的外层。id 保留原始字节原样回显，
// 既支持字符串也支持数字；显式 null 被严格解码层直接拒绝。
type rpcRequestEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErrorValue struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcSuccessResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type rpcFailureResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcErrorValue   `json:"error"`
}

type rpcNotificationMessage struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// decodeRequestLine 按本仓库 JSON 纪律解码一行 NDJSON：拒绝未知字段、重复
// key、显式 null 与多值文档。返回的 envelope 未做语义校验。
// 参数 line 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 rpcRequestEnvelope、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func decodeRequestLine(line []byte) (rpcRequestEnvelope, error) {
	var envelope rpcRequestEnvelope
	err := strictjson.DecodeObjectNoNulls(
		bytes.NewReader(line), MaxMessageBytes, &envelope,
	)
	return envelope, err
}

// validateEnvelope 校验 jsonrpc 版本与 method；id 形状由 validateRequestID
// 单独校验（非法 id 需回 null id）。
// 参数 envelope 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func validateEnvelope(envelope rpcRequestEnvelope) error {
	if envelope.JSONRPC != "2.0" {
		return fmt.Errorf("jsonrpc must be \"2.0\"")
	}
	if envelope.Method == "" {
		return fmt.Errorf("method is required")
	}
	return nil
}

// decodeParams 执行 decodeParams 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params、target 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func decodeParams(
	params json.RawMessage,
	target any,
) error {
	if len(bytes.TrimSpace(params)) == 0 {
		params = []byte("{}")
	}
	return strictjson.DecodeObjectNoNulls(
		bytes.NewReader(params), MaxMessageBytes, target,
	)
}

// lineWriter 串行化协议写出；等待锁与写出都可取消。生命周期取消时关闭
// 可关闭的输出，使真实 stdio pipe 上阻塞的 Write 也能退出。
type lineWriter struct {
	ctx       context.Context
	stop      context.CancelCauseFunc
	lock      chan struct{}
	writer    *bufio.Writer
	onFailure func(error)
}

// newLineWriter 执行 newLineWriter 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 out 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *lineWriter 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func newLineWriter(out io.Writer) *lineWriter {
	return newContextLineWriter(context.Background(), out, func(error) {})
}

// newContextLineWriter 执行 newContextLineWriter 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 out、onFailure 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *lineWriter 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func newContextLineWriter(
	ctx context.Context,
	out io.Writer,
	onFailure func(error),
) *lineWriter {
	writeCtx, stop := context.WithCancelCause(ctx)
	writer := &lineWriter{
		ctx: writeCtx, stop: stop, lock: make(chan struct{}, 1),
		writer: bufio.NewWriter(out), onFailure: onFailure,
	}
	if closer, ok := out.(io.Closer); ok {
		context.AfterFunc(writeCtx, func() { _ = closer.Close() })
	}
	return writer
}

// writeMessage 执行 writeMessage 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (writer *lineWriter) writeMessage(value any) error {
	select {
	case <-writer.ctx.Done():
		return context.Cause(writer.ctx)
	case writer.lock <- struct{}{}:
	}
	if writer.ctx.Err() != nil {
		<-writer.lock
		return context.Cause(writer.ctx)
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-writer.lock }()
		encoder := json.NewEncoder(writer.writer)
		encoder.SetEscapeHTML(false)
		err := encoder.Encode(value)
		if err == nil {
			err = writer.writer.Flush()
		} else {
			_ = writer.writer.Flush()
		}
		if err != nil && writer.ctx.Err() == nil {
			writer.stop(err)
			writer.onFailure(err)
		}
		done <- err
	}()
	select {
	case <-writer.ctx.Done():
		return context.Cause(writer.ctx)
	case err := <-done:
		return err
	}
}

// sendResult 执行 sendResult 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 id、result 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (writer *lineWriter) sendResult(id json.RawMessage, result any) error {
	return writer.writeMessage(rpcSuccessResponse{
		JSONRPC: "2.0", ID: id, Result: result,
	})
}

// sendError 执行 sendError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 id、err 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (writer *lineWriter) sendError(id json.RawMessage, err *rpcError) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return writer.writeMessage(rpcFailureResponse{
		JSONRPC: "2.0", ID: id,
		Error: rpcErrorValue{
			Code: err.code, Message: err.message, Data: err.data,
		},
	})
}

// sendNotification 执行 sendNotification 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 method、params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (writer *lineWriter) sendNotification(method string, params any) error {
	return writer.writeMessage(rpcNotificationMessage{
		JSONRPC: "2.0", Method: method, Params: params,
	})
}

// eventID 执行 eventID 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 runID、sequence 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func eventID(runID string, sequence uint64) string {
	return runID + ":" + strconv.FormatUint(sequence, 10)
}

// scanProtocolLines 在 ctx 取消或 stdin EOF 时结束；读端在全部已接受帧
// 处理完后通知输出生命周期。超长行无法安全重新同步，返回 framing 错误。
// 参数 in、handle、onInputEnd 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func scanProtocolLines(
	ctx context.Context,
	in io.Reader,
	handle func(line []byte) error,
	onInputEnd func(error),
) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes+1)
	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		err := scanner.Err()
		if err == bufio.ErrTooLong {
			err = fmt.Errorf("stdin line exceeds %d bytes", int64(MaxMessageBytes))
		} else if err != nil {
			err = fmt.Errorf("read stdin: %w", err)
		}
		scanErr <- err
	}()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case line, ok := <-lines:
			if ok {
				if err := handle(line); err != nil {
					return err
				}
				continue
			}
			var err error
			select {
			case err = <-scanErr:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			// 关闭 lines 之前，scanner 已交付全部完整帧；只有消费并处理完
			// 最后一帧后才通知 EOF，避免 settle 漏掉刚接受的 Run。
			if err == nil {
				err = io.EOF
				onInputEnd(io.EOF)
			} else {
				onInputEnd(err)
			}
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return err
		}
	}
}
