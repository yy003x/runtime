package codingacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadFrameAcceptsLFAndCRLF(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("first\nsecond\r\n\nthird\n"))
	var frames []string
	for range 4 {
		frame := readFrame(reader)
		if frame.err != nil {
			t.Fatalf("unexpected frame error: %v", frame.err)
		}
		frames = append(frames, string(frame.data))
	}
	want := []string{"first", "second", "", "third"}
	for index, value := range want {
		if frames[index] != value {
			t.Fatalf("frame %d = %q, want %q", index, frames[index], value)
		}
	}
	if frame := readFrame(reader); frame.err != io.EOF {
		t.Fatalf("tail frame err = %v, want EOF", frame.err)
	}
}

func TestReadFrameRejectsOversizedLineAndRecovers(t *testing.T) {
	oversized := strings.Repeat("x", maxInputBytes+16)
	input := oversized + "\n{\"ok\":true}\n"
	reader := bufio.NewReader(strings.NewReader(input))
	first := readFrame(reader)
	if !first.tooLarge {
		t.Fatalf("oversized frame not flagged: tooLarge=%v data=%d bytes", first.tooLarge, len(first.data))
	}
	second := readFrame(reader)
	if second.err != nil || string(second.data) != `{"ok":true}` {
		t.Fatalf("recovery frame = (%v, %q), want (nil, {\"ok\":true})", second.err, second.data)
	}
}

func TestIDKeyNormalizesEquivalentNumbers(t *testing.T) {
	// "01" 不是合法 JSON number（禁止前导零），不在等价集合内。
	equivalent := []string{"1", "1.0", "1e0", "1.00"}
	var first string
	for index, raw := range equivalent {
		key, ok := idKey(json.RawMessage(raw))
		if !ok {
			t.Fatalf("idKey(%q) rejected", raw)
		}
		if index == 0 {
			first = key
		} else if key != first {
			t.Fatalf("idKey(%q) = %q, want %q", raw, key, first)
		}
	}
	if key, _ := idKey(json.RawMessage(`"abc"`)); key != "s:abc" {
		t.Fatalf("string key = %q", key)
	}
	if key, _ := idKey(json.RawMessage("null")); key != "z:" {
		t.Fatalf("null key = %q", key)
	}
	if key, ok := idKey(json.RawMessage("")); !ok || key != "" {
		t.Fatalf("absent key = (%q, %v)", key, ok)
	}
	if _, ok := idKey(json.RawMessage(`{}`)); ok {
		t.Fatalf("object id must be rejected")
	}
	if _, ok := idKey(json.RawMessage(`"` + strings.Repeat("a", maxRequestID+1) + `"`)); ok {
		t.Fatalf("oversized id must be rejected")
	}
}

// echoAdmit 记录收到的调用并原样返回 params；未知方法按协议拒绝。
func echoAdmit(calls *[]string) admitFunc {
	return func(_ context.Context, method string, params json.RawMessage) (admittedCall, *RequestError) {
		if method != "echo" {
			return nil, NewMethodNotFound(method)
		}
		*calls = append(*calls, method)
		return func(context.Context) (any, *RequestError) {
			return map[string]any{"method": method, "params": json.RawMessage(params)}, nil
		}, nil
	}
}

func serveTestTransport(t *testing.T, admit admitFunc) (input io.Writer, output chan string) {
	t.Helper()
	reader, writer := io.Pipe()
	lines := make(chan string, 64)
	server := newTransport(&chanWriter{lines: lines}, admit)
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), reader) }()
	t.Cleanup(func() {
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("transport did not stop on EOF")
		}
		server.Close()
	})
	return writer, lines
}

type chanWriter struct {
	lines  chan string
	buffer []byte
}

func (writer *chanWriter) Write(chunk []byte) (int, error) {
	writer.buffer = append(writer.buffer, chunk...)
	for {
		index := bytes.IndexByte(writer.buffer, '\n')
		if index < 0 {
			break
		}
		line := string(writer.buffer[:index])
		writer.buffer = writer.buffer[index+1:]
		writer.lines <- line
	}
	return len(chunk), nil
}

func readLine(t *testing.T, lines chan string) map[string]json.RawMessage {
	t.Helper()
	select {
	case line := <-lines:
		decoded := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("output line is not JSON: %q", line)
		}
		return decoded
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for output line")
		return nil
	}
}

// awaitResponse 跳过通知，直到读到与 id 匹配的响应；乱序到达的响应缓存在
// stash 中不被丢弃。
func awaitResponse(t *testing.T, lines chan string, stash map[string]map[string]json.RawMessage, id string) map[string]json.RawMessage {
	t.Helper()
	if cached, ok := stash[id]; ok {
		delete(stash, id)
		return cached
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			decoded := map[string]json.RawMessage{}
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatalf("output line is not JSON: %q", line)
			}
			var gotID string
			if raw, ok := decoded["id"]; ok {
				_ = json.Unmarshal(raw, &gotID)
			}
			if gotID == id {
				return decoded
			}
			if gotID != "" {
				stash[gotID] = decoded
			}
		case <-deadline:
			t.Fatalf("timed out waiting for response %q", id)
		}
	}
}

func TestTransportServesRequestsAndNotifications(t *testing.T) {
	calls := []string{}
	input, lines := serveTestTransport(t, echoAdmit(&calls))
	stash := map[string]map[string]json.RawMessage{}
	if err := writeJSONLine(input, map[string]any{
		"jsonrpc": "2.0", "id": "r-1", "method": "echo", "params": map[string]any{"hello": true},
	}); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response := awaitResponse(t, lines, stash, "r-1")
	if string(response["error"]) != "" || response["result"] == nil {
		t.Fatalf("echo response missing result: %v", response)
	}
	// 通知没有响应；后续请求仍可继续。
	if err := writeJSONLine(input, map[string]any{
		"jsonrpc": "2.0", "method": "notify/only",
	}); err != nil {
		t.Fatalf("write notification: %v", err)
	}
	if err := writeJSONLine(input, map[string]any{
		"jsonrpc": "2.0", "id": "r-2", "method": "echo", "params": map[string]any{},
	}); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	awaitResponse(t, lines, stash, "r-2")
	if len(calls) != 2 {
		t.Fatalf("admitted calls = %v, want two echo calls", calls)
	}
}

func TestTransportRejectsMalformedEnvelopes(t *testing.T) {
	calls := []string{}
	input, lines := serveTestTransport(t, echoAdmit(&calls))
	cases := []struct {
		name string
		line string
		want int64
	}{
		{"not json", "{broken", codeParseError},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"echo"}`, codeInvalidRequest},
		{"missing method", `{"jsonrpc":"2.0","id":2}`, codeInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":3,"method":"missing","params":{}}`, codeMethodNotFound},
	}
	for _, testCase := range cases {
		if _, err := io.WriteString(input, testCase.line+"\n"); err != nil {
			t.Fatalf("%s: write: %v", testCase.name, err)
		}
		decoded := readLine(t, lines)
		var id string
		_ = json.Unmarshal(decoded["id"], &id)
		var errValue struct {
			Code int64 `json:"code"`
		}
		if raw, ok := decoded["error"]; ok {
			if err := json.Unmarshal(raw, &errValue); err != nil {
				t.Fatalf("%s: error field invalid: %v", testCase.name, err)
			}
		}
		if errValue.Code != testCase.want {
			t.Fatalf("%s: error code = %d, want %d", testCase.name, errValue.Code, testCase.want)
		}
	}
}

func writeJSONLine(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}
