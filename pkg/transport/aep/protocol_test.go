package aep

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestScanProtocolLinesDefersEOFUntilFinalFrameHandled(t *testing.T) {
	handling := make(chan struct{})
	release := make(chan struct{})
	inputEnded := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- scanProtocolLines(context.Background(), strings.NewReader("frame\n"), func(line []byte) error {
			close(handling)
			<-release
			return nil
		}, func(err error) {
			inputEnded <- err
		})
	}()
	<-handling
	select {
	case err := <-inputEnded:
		t.Fatalf("EOF reported while final frame was blocked: %v", err)
	default:
	}
	close(release)
	if err := <-inputEnded; err != io.EOF {
		t.Fatalf("input end error = %v", err)
	}
	if err := <-done; err != io.EOF {
		t.Fatalf("scan error = %v", err)
	}
}

func TestEventSourceOmitsEmptyExecutor(t *testing.T) {
	data, err := json.Marshal(EventSource{NativeEventType: "turn.completed"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "executor") {
		t.Fatalf("source=%s", data)
	}
}

func TestDecodeRequestLineRejectsInvalidFrames(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
	}{
		{"not_json", `not-json`},
		{"array_root", `[]`},
		{"null_root", `null`},
		{
			"duplicate_key",
			`{"jsonrpc":"2.0","id":"1","id":"1","method":"run.get"}`,
		},
		{
			"unknown_field",
			`{"jsonrpc":"2.0","id":"1","method":"run.get","extra":true}`,
		},
		{
			"explicit_null",
			`{"jsonrpc":"2.0","id":"1","method":"run.get","params":null}`,
		},
		{
			"trailing_data",
			`{"jsonrpc":"2.0","id":"1","method":"run.get"} {}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeRequestLine([]byte(test.line)); err == nil {
				t.Fatalf("decode accepted %q", test.line)
			}
		})
	}
}

func TestDecodeRequestLineAcceptsStringAndNumberID(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
		id   string
	}{
		{
			"string_id",
			`{"jsonrpc":"2.0","id":"req_1","method":"run.get","params":{"runId":"run_1"}}`,
			`"req_1"`,
		},
		{
			"number_id",
			`{"jsonrpc":"2.0","id":7,"method":"run.get"}`,
			`7`,
		},
		{
			"notification",
			`{"jsonrpc":"2.0","method":"run.cancel","params":{"runId":"run_1"}}`,
			``,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := decodeRequestLine([]byte(test.line))
			if err != nil {
				t.Fatal(err)
			}
			if envelope.Method != "run.get" && envelope.Method != "run.cancel" {
				t.Fatalf("method=%q", envelope.Method)
			}
			if got := string(bytes.TrimSpace(envelope.ID)); got != test.id {
				t.Fatalf("id=%q want=%q", got, test.id)
			}
		})
	}
}

func TestValidateRequestIDShape(t *testing.T) {
	for _, test := range []struct {
		id    string
		valid bool
	}{
		{``, true},
		{`"req_1"`, true},
		{`42`, true},
		{`true`, false},
		{`[1]`, false},
		{`{"x":1}`, false},
	} {
		err := validateRequestID(json.RawMessage(test.id))
		if test.valid && err != nil {
			t.Fatalf("validateRequestID(%s): %v", test.id, err)
		}
		if !test.valid && err == nil {
			t.Fatalf("validateRequestID(%s) accepted", test.id)
		}
	}
}

func TestValidateEnvelopeRequiresJSONRPC20AndMethod(t *testing.T) {
	if err := validateEnvelope(rpcRequestEnvelope{
		JSONRPC: "2.0", Method: "run.get",
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateEnvelope(rpcRequestEnvelope{
		JSONRPC: "1.0", Method: "run.get",
	}); err == nil {
		t.Fatal("jsonrpc 1.0 accepted")
	}
	if err := validateEnvelope(rpcRequestEnvelope{JSONRPC: "2.0"}); err == nil {
		t.Fatal("missing method accepted")
	}
}

func TestLineWriterWritesSingleNDJSONLinePerMessage(t *testing.T) {
	var out bytes.Buffer
	writer := newLineWriter(&out)
	if err := writer.sendResult(json.RawMessage(`"id-1"`), map[string]string{"ok": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.sendNotification("run.event", map[string]int{"seq": 1}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d output=%q", len(lines), out.String())
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid JSON line %q", line)
		}
	}
}
