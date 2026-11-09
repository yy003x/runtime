package codingacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"unicode/utf8"
)

// 界限常量：单行输入、请求 ID、方法名、并发任务、server→client 请求与
// 写队列都有上限，客户端必须持续读取 stdout 并在界限内调用。
const (
	// maxInputBytes 限定单个 JSONL 帧（不含 LF 与可选 CR）。
	maxInputBytes = 32 << 20
	// maxRequestID 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxRequestID = 1024
	// maxMethod 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxMethod = 256
	// maxActiveJobs 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxActiveJobs = 32
	// maxClientCall 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxClientCall = 64
	// maxQueuedWrite 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxQueuedWrite = 8
)

var (
	errTransportClosed = errors.New("ACP transport closed")
	errOutputEncoding  = errors.New("ACP output cannot be encoded within the frame limit")
)

// admittedCall 是通过准入检查后的实际工作；transport 只负责关联与帧化，
// 不解释 ACP 语义。准入按输入顺序串行执行，长任务随后异步执行。
type admittedCall func(context.Context) (any, *RequestError)
type admitFunc func(context.Context, string, json.RawMessage) (admittedCall, *RequestError)

type incomingCall struct{ cancel context.CancelFunc }
type acpReply struct {
	result json.RawMessage
	err    error
}
type acpWrite struct {
	data []byte
	done chan error
	ctx  context.Context
}
type acpFrame struct {
	data     []byte
	tooLarge bool
	err      error
}
type acpEnvelope struct {
	version json.RawMessage
	id      json.RawMessage
	method  json.RawMessage
	params  json.RawMessage
	result  json.RawMessage
	err     json.RawMessage
}

type transport struct {
	output     io.Writer
	admit      admitFunc
	ctx        context.Context
	stop       context.CancelFunc
	writes     chan acpWrite
	writeSlots chan struct{}

	mu       sync.Mutex
	serving  bool
	closed   bool
	err      error
	reader   io.Closer
	active   int
	nextID   uint64
	incoming map[string]*incomingCall
	pending  map[string]chan acpReply
}

// newTransport 执行 newTransport 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 output、admit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *transport 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func newTransport(output io.Writer, admit admitFunc) *transport {
	ctx, stop := context.WithCancel(context.Background())
	instance := &transport{
		output: output, admit: admit, ctx: ctx, stop: stop,
		writes: make(chan acpWrite, maxQueuedWrite), writeSlots: make(chan struct{}, maxQueuedWrite),
		incoming: make(map[string]*incomingCall), pending: make(map[string]chan acpReply),
	}
	go instance.writeLoop()
	return instance
}

// Close 取消已准入任务与待处理的 client 请求；可关闭的输入被关闭以打断
// 阻塞读，output 的所有权仍在调用方。
func (t *transport) Close() { t.shutdown(nil) }

// shutdown 执行 shutdown 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 err 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (t *transport) shutdown(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed, t.err = true, err
	reader := t.reader
	for _, call := range t.incoming {
		call.cancel()
	}
	t.stop()
	t.mu.Unlock()
	if reader != nil {
		_ = reader.Close()
	}
}

// Serve 串行准入调用，但不让运行中的 prompt 阻塞回复与取消通知。
// stdin EOF 结束连接并取消其全部任务。
// 参数 input 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) Serve(ctx context.Context, input io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	if t.serving || t.closed {
		t.mu.Unlock()
		return errTransportClosed
	}
	t.serving = true
	t.reader, _ = input.(io.Closer)
	t.mu.Unlock()
	frames := make(chan acpFrame, 1)
	go func() {
		reader := bufio.NewReaderSize(input, 32<<10)
		for {
			frame := readFrame(reader)
			select {
			case frames <- frame:
			case <-t.ctx.Done():
				return
			}
			if frame.err != nil {
				return
			}
		}
	}()
	defer t.Close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.ctx.Done():
			t.mu.Lock()
			err := t.err
			t.mu.Unlock()
			return err
		case frame := <-frames:
			if frame.err != nil {
				t.mu.Lock()
				terminalErr := t.err
				t.mu.Unlock()
				if terminalErr != nil {
					return terminalErr
				}
				if errors.Is(frame.err, io.EOF) {
					return nil
				}
				return errors.New("ACP input read failed")
			}
			if frame.tooLarge {
				if err := t.respond(t.ctx, nil, nil, NewInvalidRequest(map[string]any{
					"code": "input_too_large", "maxBytes": maxInputBytes,
				})); err != nil {
					return err
				}
				continue
			}
			if len(bytes.TrimSpace(frame.data)) == 0 {
				continue
			}
			if err := t.receive(frame.data); err != nil {
				return err
			}
		}
	}
}

// readFrame 读取一行并丢弃超限行的剩余部分，不为其保留内存。
// 参数 reader 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 acpFrame 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func readFrame(reader *bufio.Reader) acpFrame {
	var data []byte
	var count int64
	var last byte
	for {
		part, err := reader.ReadSlice('\n')
		terminated := len(part) > 0 && part[len(part)-1] == '\n'
		if terminated {
			part = part[:len(part)-1]
		}
		count += int64(len(part))
		if len(part) > 0 {
			last = part[len(part)-1]
			if remaining := maxInputBytes + 1 - len(data); remaining > 0 {
				keep := min(remaining, len(part))
				if needed := len(data) + keep; needed > cap(data) {
					grown := make([]byte, len(data), min(maxInputBytes+1, max(needed, cap(data)*2)))
					copy(grown, data)
					data = grown
				}
				data = append(data, part[:keep]...)
			}
		}
		if terminated || errors.Is(err, io.EOF) {
			if count == 0 && errors.Is(err, io.EOF) {
				return acpFrame{err: io.EOF}
			}
			if last == '\r' {
				count--
			}
			if count > maxInputBytes {
				return acpFrame{tooLarge: true}
			}
			return acpFrame{data: data[:int(count)]}
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return acpFrame{err: err}
		}
	}
}

// parseEnvelope 执行 parseEnvelope 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 acpEnvelope、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func parseEnvelope(data []byte) (acpEnvelope, bool) {
	var fields map[string]json.RawMessage
	if !utf8.Valid(data) || json.Unmarshal(data, &fields) != nil || fields == nil {
		return acpEnvelope{}, false
	}
	return acpEnvelope{
		fields["jsonrpc"], fields["id"], fields["method"],
		fields["params"], fields["result"], fields["error"],
	}, true
}

// receive 执行 receive 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) receive(data []byte) error {
	envelope, parsed := parseEnvelope(data)
	if !parsed {
		requestErr := NewInvalidRequest(nil)
		if !json.Valid(data) || !utf8.Valid(data) {
			requestErr = NewParseError(nil)
		}
		return t.respond(t.ctx, nil, nil, requestErr)
	}
	key, validID := idKey(envelope.id)
	var version string
	versionOK := json.Unmarshal(envelope.version, &version) == nil && version == "2.0"
	t.mu.Lock()
	knownReply := validID && t.pending[key] != nil
	t.mu.Unlock()
	if len(envelope.method) == 0 && len(envelope.id) != 0 &&
		(len(envelope.result) != 0 || len(envelope.err) != 0 || knownReply) {
		// 回复永远不会被当成新请求，包括迟到或未知的回复。
		if validID {
			t.receiveReply(key, envelope, versionOK)
		}
		return nil
	}
	var method string
	methodOK := json.Unmarshal(envelope.method, &method) == nil && method != "" && len(method) <= maxMethod
	if !validID || !versionOK || !methodOK || len(envelope.result) != 0 || len(envelope.err) != 0 {
		id := envelope.id
		if !validID {
			id = nil
		}
		return t.respond(t.ctx, id, nil, NewInvalidRequest(nil))
	}
	if method == "$/cancel_request" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(envelope.params, &params) == nil {
			if cancelKey, ok := idKey(params.RequestID); ok && len(params.RequestID) != 0 {
				t.mu.Lock()
				if call := t.incoming[cancelKey]; call != nil {
					call.cancel()
				}
				t.mu.Unlock()
			}
		}
		if len(envelope.id) != 0 {
			return t.respond(t.ctx, envelope.id, nil, nil)
		}
		return nil
	}
	return t.dispatch(envelope, key, method)
}

// dispatch 执行 dispatch 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 envelope、key、method 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) dispatch(envelope acpEnvelope, key, method string) error {
	hasID := len(envelope.id) != 0
	ctx, cancel := context.WithCancel(t.ctx)
	call := &incomingCall{cancel: cancel}
	t.mu.Lock()
	duplicate := hasID && t.incoming[key] != nil
	full := t.active >= maxActiveJobs
	if !duplicate && !full {
		t.active++
		if hasID {
			t.incoming[key] = call
		}
	}
	t.mu.Unlock()
	if duplicate || (full && method != "session/cancel") {
		cancel()
		if !hasID {
			return nil
		}
		return t.respond(t.ctx, envelope.id, nil, NewInvalidRequest(map[string]any{
			"code": "request_limit_or_duplicate_id",
		}))
	}
	// session/cancel 同步准入，因此容量满时仍然可用。
	var released sync.Once
	release := func() {
		released.Do(func() {
			cancel()
			if !full {
				t.mu.Lock()
				t.active--
				if hasID && t.incoming[key] == call {
					delete(t.incoming, key)
				}
				t.mu.Unlock()
			}
		})
	}
	job, requestErr := t.safeAdmit(ctx, method, envelope.params)
	if requestErr != nil || job == nil || full {
		release()
		if !hasID {
			return nil
		}
		if full && job != nil {
			requestErr = NewInternalError(nil)
		}
		return t.respond(t.ctx, envelope.id, nil, requestErr)
	}
	go func() {
		defer release()
		result, requestErr := safeJob(ctx, job)
		if ctx.Err() != nil {
			requestErr = NewRequestCancelled(nil)
		}
		// 观察到响应后复用同一 ID，不得与旧清理竞争。
		release()
		if hasID {
			_ = t.respond(t.ctx, envelope.id, result, requestErr)
		}
	}()
	return nil
}

// safeAdmit 执行 safeAdmit 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 method、params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 admittedCall、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) safeAdmit(ctx context.Context, method string, params json.RawMessage) (job admittedCall, requestErr *RequestError) {
	defer func() {
		if recover() != nil {
			job, requestErr = nil, NewInternalError(nil)
		}
	}()
	if t.admit == nil {
		return nil, NewMethodNotFound(method)
	}
	return t.admit(ctx, method, params)
}

// safeJob 执行 safeJob 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 job 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func safeJob(ctx context.Context, job admittedCall) (result any, requestErr *RequestError) {
	defer func() {
		if recover() != nil {
			result, requestErr = nil, NewInternalError(nil)
		}
	}()
	return job(ctx)
}

// receiveReply 执行 receiveReply 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 key、envelope、versionOK 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (t *transport) receiveReply(key string, envelope acpEnvelope, versionOK bool) {
	t.mu.Lock()
	reply := t.pending[key]
	delete(t.pending, key)
	t.mu.Unlock()
	if reply == nil {
		return
	}
	response := acpReply{result: envelope.result}
	if !versionOK || (len(envelope.result) == 0) == (len(envelope.err) == 0) {
		response.err = errors.New("invalid ACP client response")
	} else if len(envelope.err) != 0 {
		var requestErr RequestError
		if json.Unmarshal(envelope.err, &requestErr) != nil || requestErr.Message == "" {
			response.err = errors.New("invalid ACP client error response")
		} else {
			response.err = &requestErr
		}
	}
	reply <- response
}

// respond 执行 respond 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 id、result、requestErr 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) respond(ctx context.Context, id json.RawMessage, result any, requestErr *RequestError) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	value := map[string]any{"jsonrpc": "2.0", "id": id}
	if requestErr != nil {
		value["error"] = requestErr
	} else {
		value["result"] = result
	}
	err := t.send(ctx, value)
	if errors.Is(err, errOutputEncoding) {
		// 无法编码的处理器结果也必须安全地终结其请求。
		return t.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "error": NewInternalError(nil)})
	}
	return err
}

// Notify 执行 Notify 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 method、params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) Notify(ctx context.Context, method string, params any) error {
	return t.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Call 发起 server→client 请求，不阻塞输入读取循环。
// 参数 method、params、result 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) Call(ctx context.Context, method string, params, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	if t.closed || len(t.pending) >= maxClientCall {
		t.mu.Unlock()
		return errors.New("ACP client request unavailable")
	}
	t.nextID++
	id := fmt.Sprintf("sn-acp-%d", t.nextID)
	key := "s:" + id
	reply := make(chan acpReply, 1)
	t.pending[key] = reply
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}()
	if err := t.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		if ctx.Err() != nil {
			t.queueCancellation(id)
		}
		return err
	}
	select {
	case <-ctx.Done():
		t.queueCancellation(id)
		return ctx.Err()
	case <-t.ctx.Done():
		return errTransportClosed
	case response := <-reply:
		if err := ctx.Err(); err != nil {
			t.queueCancellation(id)
			return err
		}
		if t.ctx.Err() != nil {
			return errTransportClosed
		}
		if response.err != nil {
			return response.err
		}
		if result == nil {
			return nil
		}
		if json.Unmarshal(response.result, result) != nil {
			return errors.New("invalid ACP client result")
		}
		return nil
	}
}

// queueCancellation 尽力而为且有界：既不被阻塞的客户端写延迟本地取消，
// 也不为每个取消的调用建 goroutine。
// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (t *transport) queueCancellation(id string) {
	if t.ctx.Err() != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "$/cancel_request",
		"params": map[string]any{"requestId": id},
	})
	write := acpWrite{data: append(data, '\n'), done: make(chan error, 1), ctx: t.ctx}
	select {
	case t.writes <- write:
	default:
	}
}

// send 执行 send 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (t *transport) send(ctx context.Context, value any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case t.writeSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return errTransportClosed
	}
	defer func() { <-t.writeSlots }()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxInputBytes {
		return errOutputEncoding
	}
	write := acpWrite{data: append(data, '\n'), done: make(chan error, 1), ctx: ctx}
	select {
	case t.writes <- write:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return errTransportClosed
	}
	select {
	case err := <-write.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return errTransportClosed
	}
}

// writeLoop 串行化全部输出写入；一次失败即终结连接。
func (t *transport) writeLoop() {
	for {
		select {
		case <-t.ctx.Done():
			return
		case write := <-t.writes:
			if t.ctx.Err() != nil {
				write.done <- errTransportClosed
				return
			}
			if err := write.ctx.Err(); err != nil {
				write.done <- err
				continue
			}
			data := write.data
			var err error
			for len(data) > 0 {
				var count int
				if t.output == nil {
					err = io.ErrClosedPipe
				} else {
					count, err = t.output.Write(data)
				}
				if count < 0 || count > len(data) || (count == 0 && err == nil) {
					err = io.ErrShortWrite
				}
				if err != nil {
					break
				}
				data = data[count:]
			}
			if err != nil {
				err = errors.New("ACP output write failed")
				t.shutdown(err)
			}
			write.done <- err
			if err != nil {
				return
			}
		}
	}
}

// idKey 回复关联保留原始 ID，等价的数字形态共享同一关联键；指数归一化
// 不分配巨大的幂。
// 参数 raw 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func idKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true // 缺失：通知
	}
	if len(raw) > maxRequestID {
		return "", false
	}
	if bytes.Equal(raw, []byte("null")) {
		return "z:", true
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return "", false
		}
		return "s:" + value, true
	}
	if !json.Valid(raw) || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return "", false
	}
	number := string(raw)
	negative := strings.HasPrefix(number, "-")
	number = strings.TrimPrefix(number, "-")
	exponent := new(big.Int)
	if index := strings.IndexAny(number, "eE"); index >= 0 {
		if _, ok := exponent.SetString(number[index+1:], 10); !ok {
			return "", false
		}
		number = number[:index]
	}
	if index := strings.IndexByte(number, '.'); index >= 0 {
		exponent.Sub(exponent, big.NewInt(int64(len(number)-index-1)))
		number = number[:index] + number[index+1:]
	}
	number = strings.TrimLeft(number, "0")
	if number == "" {
		return "n:0", true
	}
	coefficient := strings.TrimRight(number, "0")
	exponent.Add(exponent, big.NewInt(int64(len(number)-len(coefficient))))
	if negative {
		coefficient = "-" + coefficient
	}
	return "n:" + coefficient + "e" + exponent.String(), true
}
