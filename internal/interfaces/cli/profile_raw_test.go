package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

const rawFixtureSecret = "fixture-raw-secret"
const rawResponsesJSON = `{"id":"raw-fixture","model":"fixture","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"raw answer"}]}],"provider_extra":{"debug":"fixture-raw-secret"}}`
const rawResponsesToolJSON = `{"id":"fixture-id","model":"fixture","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]},{"type":"function_call","id":"fc-1","call_id":"call-1","name":"weather","arguments":"{}"}],"usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13}}`

func TestCallRawWireOutput(t *testing.T) {
	openAIJSON := `{"id":"raw-fixture","model":"fixture","choices":[{"message":{"content":"raw answer"},"finish_reason":"stop"}],"provider_extra":{"debug":"fixture-raw-secret"}}`
	for _, test := range []struct {
		name        string
		driver      string
		contentType string
		body        string
		status      int
		retryOnce   bool
		frames      int
		attempts    int
		failed      bool
	}{
		{"openai_json", "openai", "application/json", openAIJSON, 200, false, 1, 1, false},
		{"responses_json", "openai-responses", "application/json", rawResponsesJSON, 200, false, 1, 1, false},
		{"responses_sse", "openai-responses", "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"raw answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":" + rawResponsesJSON + "}\n\n", 200, false, 2, 1, false},
		{"responses_retry", "openai-responses", "application/json", rawResponsesJSON, 200, true, 1, 2, false},
		{"responses_error", "openai-responses", "application/json", `{"error":{"code":"invalid_api_key","message":"fixture-raw-secret rejected"}}`, 401, false, 1, 1, true},
		{"responses_sse_error", "openai-responses", "text/event-stream", "data: {\"type\":\"error\",\"code\":\"invalid_request\",\"message\":\"fixture-raw-secret rejected\"}\n\n", 200, false, 1, 1, true},
		{"responses_sse_truncated", "openai-responses", "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", 200, false, 1, 1, true},
		{"openai_sse", "openai", "text/event-stream", "data: {\"id\":\"raw-fixture\",\"model\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"raw answer\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", 200, false, 2, 1, false},
		{"anthropic_json", "anthropic", "application/json", `{"id":"raw-fixture","type":"message","role":"assistant","model":"fixture","content":[{"type":"text","text":"raw answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, 200, false, 1, 1, false},
		{"anthropic_sse", "anthropic", "text/event-stream", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"raw-fixture\",\"model\":\"fixture\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"raw answer\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n", 200, false, 3, 1, false},
		{"tool_call", "openai", "application/json", `{"id":"raw-fixture","model":"fixture","choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, 200, false, 1, 1, false},
		{"retry_success", "openai", "application/json", openAIJSON, 200, true, 1, 2, false},
		{"retry_exhausted", "openai", "application/json", `{"error":{"message":"fixture-raw-secret unavailable"}}`, 503, false, 1, 3, true},
		{"auth_error", "anthropic", "application/json", `{"error":{"type":"authentication_error","message":"fixture-raw-secret rejected"}}`, 401, false, 1, 1, true},
		{"flat_error", "openai", "application/json", `{"code":"invalid_key","message":"fixture-raw-secret rejected"}`, 401, false, 1, 1, true},
		{"non_json_error", "openai", "text/html", "<html>fixture-raw-secret rejected</html>", 400, false, 0, 1, true},
		{"invalid_provider_json", "openai", "application/json", `{"unexpected":"fixture-raw-secret"}`, 200, false, 1, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			wireBodies := make(chan []byte, 4)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				wireBodies <- body
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Set-Cookie", "session=fixture-cookie")
				w.Header().Set("X-Debug", rawFixtureSecret)
				if test.retryOnce && len(wireBodies) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(503)
					fmt.Fprint(w, `{"error":{"message":"fixture-raw-secret transient"}}`)
					return
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			paths := prepareRawModel(t, test.driver, server.URL+"/raw", server.Client().Transport)
			var stdout, stderr bytes.Buffer
			output := newCLIOutput(false, &stdout, &stderr)
			err := runTestReq(paths, []string{
				"cc-api", "--raw", "--system", "fixture system", "--max-tokens", "17", "hello",
			}, output)
			if (err != nil) != test.failed || output.fail(err) != boolExitCode(test.failed) {
				t.Fatalf("error=%v stderr=%s", err, &stderr)
			}
			rawRequest, rawResponse := decodeRawCall(t, stdout.Bytes())
			logged := readRawCallLog(t, paths)
			if len(logged.Attempts) != test.attempts || len(wireBodies) != test.attempts {
				t.Fatalf("attempts=%d wire=%d", len(logged.Attempts), len(wireBodies))
			}
			if (logged.Error != nil) != test.failed || (stderr.Len() > 0) != test.failed {
				t.Fatalf("final error=%v stderr=%s", logged.Error, &stderr)
			}
			for i, attempt := range logged.Attempts {
				actual := <-wireBodies
				assertRawJSONEqual(t, attempt.Request.Body, actual)
				var captured map[string]any
				if err := json.Unmarshal(attempt.Request.Body, &captured); err != nil {
					t.Fatal(err)
				}
				tokenField := "max_completion_tokens"
				if test.driver == "anthropic" {
					tokenField = "max_tokens"
				}
				if test.driver == "openai-responses" {
					tokenField = "max_output_tokens"
				}
				if captured["model"] != "fixture" || captured["stream"] != true ||
					captured[tokenField] != float64(17) || attempt.Request.Method != "POST" ||
					!strings.HasPrefix(attempt.Request.URL, server.URL) {
					t.Fatalf("request=%#v", attempt.Request)
				}
				if attempt.Attempt != i+1 || attempt.Response == nil {
					t.Fatalf("attempt=%#v", attempt)
				}
				wantStatus, wantFrames, wantError := test.status, test.frames, test.failed
				if test.retryOnce && i == 0 {
					wantStatus, wantFrames, wantError = 503, 1, true
				}
				if attempt.Response.Status != wantStatus || len(attempt.Response.Data) != wantFrames || (attempt.Error != nil) != wantError {
					t.Fatalf("attempt=%#v response=%#v", attempt, attempt.Response)
				}
				if attempt.Response.Headers["Set-Cookie"] != "[REDACTED]" || attempt.Response.Headers["X-Debug"] != "[REDACTED]" {
					t.Fatalf("headers=%#v", attempt.Response.Headers)
				}
				if i == len(logged.Attempts)-1 {
					assertRawJSONEqual(t, rawRequest, actual)
				}
			}
			// Compare the bare response with Provider JSON, not a canonical result
			// or a Runtime error envelope. Unknown fields and native error keys survive.
			var wantResponse []byte
			if test.frames == 0 {
				wantResponse = []byte("null")
			} else if test.contentType == "text/event-stream" {
				wantResponse, err = json.Marshal(provider.ResponseData([]byte(test.body), test.contentType))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				wantResponse = []byte(test.body)
			}
			wantResponse = bytes.ReplaceAll(wantResponse, []byte(rawFixtureSecret), []byte("[REDACTED]"))
			assertRawJSONEqual(t, rawResponse, wantResponse)
			if strings.Contains(stdout.String()+stderr.String(), rawFixtureSecret) || strings.Contains(stdout.String(), "fixture-cookie") {
				t.Fatal("credentials leaked into raw output")
			}
			for _, path := range []string{paths.SessionsDir, filepath.Join(paths.StateDir, "runtime.db")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("call created canonical state at %s: %v", path, err)
				}
			}
		})
	}
}

func prepareRawModel(t *testing.T, driver, endpoint string, transport http.RoundTripper, stream ...bool) layout.Paths {
	t.Helper()
	paths := prepareRuntimeHome(t)
	parameters := map[string]any{"max_tokens": 1024}
	if len(stream) > 0 {
		parameters["stream"] = stream[0]
	}
	value := map[string]any{
		"type": "api", "driver": driver, "endpoint": endpoint, "model": "fixture",
		"headers":    map[string]string{"Authorization": "${MODEL_API_KEY}"},
		"parameters": parameters, "timeout": "1m",
	}
	if driver == "anthropic" {
		value["headers"] = map[string]string{"x-api-key": "${MODEL_API_KEY}"}
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ConfigDir, "cc-api.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_API_KEY", rawFixtureSecret)
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	return paths
}

func decodeRawCall(t *testing.T, data []byte) (json.RawMessage, json.RawMessage) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != 2 || !json.Valid(lines[0]) || !json.Valid(lines[1]) {
		t.Fatalf("want exactly two bare JSON values: %s", data)
	}
	return lines[0], lines[1]
}

func assertRawJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("JSON differs: got=%s want=%s", got, want)
	}
}

type rawCallLog struct {
	Attempts []executionlog.APIAttempt `json:"attempts"`
	Error    *contract.RuntimeError    `json:"error"`
}

func readRawCallLog(t *testing.T, paths layout.Paths) rawCallLog {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(paths.LogsDir, "*", "api.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("logs=%v error=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	// Unmarshal rejects multiple log lines: one logical call has one record.
	var record rawCallLog
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 7 || string(fields["profile"]) != `"cc-api"` ||
		fields["call_id"] == nil || fields["attempts"] == nil || fields["error"] == nil {
		t.Fatalf("invalid API log fields: %s", data)
	}
	if bytes.Contains(data, []byte(rawFixtureSecret)) {
		t.Fatal("API log leaked credentials")
	}
	return record
}

func boolExitCode(failed bool) int {
	if failed {
		return 1
	}
	return 0
}

type rawRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip rawRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestCallRawNetworkFailure(t *testing.T) {
	paths := prepareRawModel(t, "openai", "https://example.invalid/raw", rawRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("fixture-raw-secret connection failed")
	}))
	var stdout, stderr bytes.Buffer
	output := newCLIOutput(false, &stdout, &stderr)
	err := runTestReq(paths, []string{"cc-api", "--raw", "hello"}, output)
	if err == nil || output.fail(err) != 1 {
		t.Fatalf("error=%v", err)
	}
	request, responseJSON := decodeRawCall(t, stdout.Bytes())
	logged := readRawCallLog(t, paths)
	if len(logged.Attempts) != 3 || logged.Error == nil || string(responseJSON) != "null" {
		t.Fatalf("attempts=%d error=%v response=%s", len(logged.Attempts), logged.Error, responseJSON)
	}
	assertRawJSONEqual(t, request, logged.Attempts[2].Request.Body)
	for _, response := range logged.Attempts {
		if response.Response != nil || response.Error == nil {
			t.Fatalf("response=%#v", response)
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), rawFixtureSecret) {
		t.Fatal("network error leaked credentials")
	}
}

func TestCallRawRejectsInvalidOptionsBeforeNetwork(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		json        bool
		missingAuth bool
	}{
		{"duplicate", []string{"cc-api", "--raw", "--raw", "hello"}, false, false},
		{"stream", []string{"cc-api", "--raw", "--stream", "hello"}, false, false},
		{"stream_first", []string{"cc-api", "--stream", "--raw", "hello"}, false, false},
		{"json", []string{"cc-api", "--raw", "hello"}, true, false},
		{"after_input", []string{"cc-api", "hello", "--raw"}, false, false},
		{"option_value", []string{"cc-api", "--system", "--raw", "hello"}, false, false},
		{"attached_value", []string{"cc-api", "--raw=true", "hello"}, false, false},
		{"bad_file", []string{"cc-api", "--raw", "--request-file", "/does-not-exist/raw-fixture.json"}, false, false},
		{"missing_auth", []string{"cc-api", "--raw", "hello"}, false, true},
		{"cli_profile", []string{"cx", "--raw", "hello"}, false, false},
		{"unknown_profile", []string{"missing-api", "--raw", "hello"}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := prepareRawModel(t, "openai", "https://example.invalid/raw", rawRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Error("invalid raw input performed an HTTP attempt")
				return nil, errors.New("unexpected network call")
			}))
			writeRuntimeCommand(t, paths.ConfigDir, "cx")
			if test.missingAuth {
				t.Setenv("MODEL_API_KEY", "")
			}
			var stdout, stderr bytes.Buffer
			output := newCLIOutput(test.json, &stdout, &stderr)
			err := runTestReq(paths, test.args, output)
			if err == nil || output.fail(err) != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("error=%v stdout=%s stderr=%s", err, &stdout, &stderr)
			}
			logs, err := filepath.Glob(filepath.Join(paths.LogsDir, "*", "api.jsonl"))
			if err != nil || len(logs) != 0 {
				t.Fatalf("preflight created attempt logs: %v error=%v", logs, err)
			}
		})
	}
}

func TestParseCallRawInputSources(t *testing.T) {
	requestJSON := `{"system":"fixture system","messages":[{"role":"user","content":"fixture input"}],"options":{"max_output_tokens":19}}`
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(requestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"file", []string{"--raw", "--request-file", path}, "", "fixture input"},
		{"stdin_json", []string{"--raw", "--request-file", "-"}, requestJSON, "fixture input"},
		{"stdin_text", []string{"--raw"}, "fixture stdin", "fixture stdin"},
		{"terminator", []string{"--raw", "--", "--leading-input"}, "", "--leading-input"},
		{"raw_as_input", []string{"--", "--raw"}, "", "--raw"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputPath := filepath.Join(t.TempDir(), "stdin")
			if err := os.WriteFile(inputPath, []byte(test.stdin), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stdin
			os.Stdin = file
			t.Cleanup(func() { os.Stdin = previous; file.Close() })
			request, options, err := parseDirectModelInput("cc-api", testModelProfile("openai"), test.args)
			if err != nil {
				t.Fatal(err)
			}
			if options.Raw != (test.name != "raw_as_input") || options.Stream || request.Input.Messages[0].Content != test.want {
				t.Fatalf("request=%#v options=%#v", request, options)
			}
		})
	}
}

type rawFailWriter struct {
	writes int
	failAt int
	err    error
}

func (writer *rawFailWriter) Write(data []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, writer.err
	}
	return len(data), nil
}

func TestCallRawOutputFailureDoesNotRetryProvider(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			calls := 0
			paths := prepareRawModel(t, "openai", "https://example.invalid/raw", rawRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"model":"fixture","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)),
				}, nil
			}))
			writeError := errors.New("fixture stdout failed")
			writer := &rawFailWriter{failAt: failAt, err: writeError}
			err := runTestReq(paths, []string{"cc-api", "--raw", "hello"}, newCLIOutput(false, writer, io.Discard))
			if !errors.Is(err, writeError) || calls != 1 || writer.writes != failAt {
				t.Fatalf("error=%v calls=%d writes=%d", err, calls, writer.writes)
			}
		})
	}
}

func TestCallRawIgnoresLogWriteFailure(t *testing.T) {
	paths := prepareRawModel(t, "openai", "https://example.invalid/raw", rawRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"model":"fixture","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)),
		}, nil
	}))
	if err := os.WriteFile(paths.LogsDir, []byte("fixture blocks logs directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := runTestReq(paths, []string{"cc-api", "--raw", "hello"}, newCLIOutput(false, &stdout, io.Discard)); err != nil {
		t.Fatal(err)
	}
	_, response := decodeRawCall(t, stdout.Bytes())
	assertRawJSONEqual(t, response, []byte(`{"model":"fixture","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`))
}

func TestCallRawHelp(t *testing.T) {
	for _, topic := range []string{"", "call"} {
		for _, machine := range []bool{false, true} {
			var stdout bytes.Buffer
			if err := printHelp(newCLIOutput(machine, &stdout, io.Discard), topic); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), "--raw") {
				t.Fatalf("topic=%q machine=%t: %s", topic, machine, &stdout)
			}
		}
	}
}

func TestResponsesCallPreflightHasNoRawOrAttemptLog(t *testing.T) {
	for name, request := range map[string]string{
		"history": `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"history"}]}`,
		"stop":    `{"messages":[{"role":"user","content":"hello"}],"options":{"stop_sequences":["END"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			paths := prepareRawModel(t, "openai-responses", "https://example.invalid/v1/responses", rawRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected HTTP") }))
			requestFile := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(requestFile, []byte(request), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := runTestReq(paths, []string{"cc-api", "--raw", "--request-file", requestFile}, newCLIOutput(false, &stdout, io.Discard))
			if err == nil || calls != 0 || stdout.Len() != 0 {
				t.Fatalf("error=%v calls=%d stdout=%s", err, calls, &stdout)
			}
			if _, err := os.Stat(paths.LogsDir); !os.IsNotExist(err) {
				t.Fatalf("preflight wrote logs: %v", err)
			}
		})
	}
}

func TestCallAttemptsAreLoggedWithoutRaw(t *testing.T) {
	for _, mode := range []string{"text", "json", "stream"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			paths := prepareRawModel(t, "openai", "https://example.invalid/raw", rawRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				status, body := 200, `{"model":"fixture","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`
				if calls == 1 {
					status, body = 503, `{"message":"fixture-raw-secret unavailable"}`
				}
				return &http.Response{
					StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(body)),
				}, nil
			}), false)
			args := []string{"cc-api"}
			if mode == "stream" {
				args = append(args, "--stream")
			}
			args = append(args, "hello")
			var stdout bytes.Buffer
			if err := runTestReq(paths, args, newCLIOutput(mode == "json", &stdout, io.Discard)); err != nil {
				t.Fatal(err)
			}
			logged := readRawCallLog(t, paths)
			if calls != 2 || len(logged.Attempts) != 2 || logged.Error != nil ||
				logged.Attempts[0].Error == nil || logged.Attempts[1].Error != nil {
				t.Fatalf("calls=%d log=%#v", calls, logged)
			}
			for _, attempt := range logged.Attempts {
				var request map[string]any
				if err := json.Unmarshal(attempt.Request.Body, &request); err != nil {
					t.Fatal(err)
				}
				if request["stream"] != false {
					t.Fatalf("retry changed stream: %s", attempt.Request.Body)
				}
			}
			if mode == "text" && stdout.String() != "OK\n" {
				t.Fatalf("text output=%s", &stdout)
			}
			if mode != "text" && (!strings.Contains(stdout.String(), `"schema_version":1`) || !strings.Contains(stdout.String(), `"state":"completed"`)) {
				t.Fatalf("%s output=%s", mode, &stdout)
			}
		})
	}
}

func TestCallProfileStreamAndResponseModes(t *testing.T) {
	fixtures := map[string]struct{ json, sse string }{
		"openai-responses": {
			json: rawResponsesToolJSON,
			sse:  "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":" + rawResponsesToolJSON + "}\n\n",
		},
		"openai": {
			json: `{"id":"fixture-id","model":"fixture","choices":[{"message":{"content":"OK","tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}}`,
			sse:  "data: {\"id\":\"fixture-id\",\"model\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"OK\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":5,\"total_tokens\":13}}\n\ndata: [DONE]\n\n",
		},
		"anthropic": {
			json: `{"id":"fixture-id","model":"fixture","content":[{"type":"text","text":"OK"},{"type":"tool_use","id":"call-1","name":"weather","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":8,"output_tokens":5}}`,
			sse:  "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture-id\",\"model\":\"fixture\",\"usage\":{\"input_tokens\":8}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"OK\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"weather\",\"input\":{}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n",
		},
	}
	for driver, fixture := range fixtures {
		for _, upstreamStream := range []bool{true, false} {
			for _, responseStream := range []bool{true, false} {
				for _, mode := range []string{"text", "json", "stream", "raw"} {
					name := fmt.Sprintf("%s/request_%t/response_%t/%s", driver, upstreamStream, responseStream, mode)
					t.Run(name, func(t *testing.T) {
						contentType, responseBody := "application/json", fixture.json
						if responseStream {
							contentType, responseBody = "text/event-stream", fixture.sse
						}
						wireBodies := make(chan []byte, 4)
						server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}
							wireBodies <- body
							var fields map[string]json.RawMessage
							if err := json.Unmarshal(body, &fields); err != nil {
								t.Error(err)
							}
							if string(fields["stream"]) != fmt.Sprint(upstreamStream) {
								t.Errorf("stream=%s", fields["stream"])
							}
							wantAccept := "application/json"
							if upstreamStream {
								wantAccept = "text/event-stream, application/json"
							}
							if r.Method != http.MethodPost || r.Header.Get("Accept") != wantAccept {
								t.Errorf("method=%s Accept=%s", r.Method, r.Header.Get("Accept"))
							}
							_, hasStreamOptions := fields["stream_options"]
							if hasStreamOptions != (driver == "openai" && upstreamStream) {
								t.Errorf("stream_options=%s", fields["stream_options"])
							}
							w.Header().Set("Content-Type", contentType)
							fmt.Fprint(w, responseBody)
						}))
						defer server.Close()
						paths := prepareRawModel(t, driver, server.URL+"/fixture", server.Client().Transport, upstreamStream)
						args := []string{"cc-api"}
						if mode == "raw" || mode == "stream" {
							args = append(args, "--"+mode)
						}
						args = append(args, "hello")
						var stdout, stderr bytes.Buffer
						if err := runTestReq(paths, args, newCLIOutput(mode == "json", &stdout, &stderr)); err != nil {
							t.Fatal(err)
						}
						logged := readRawCallLog(t, paths)
						if len(wireBodies) != 1 || len(logged.Attempts) != 1 || logged.Error != nil || stderr.Len() != 0 {
							t.Fatalf("calls=%d log=%#v stderr=%s", len(wireBodies), logged, &stderr)
						}
						actualRequest := <-wireBodies
						attempt := logged.Attempts[0]
						assertRawJSONEqual(t, attempt.Request.Body, actualRequest)
						wantData := provider.ResponseData([]byte(responseBody), contentType)
						if attempt.Response == nil || !reflect.DeepEqual(attempt.Response.Data, wantData) {
							t.Fatalf("response diagnostics=%#v", attempt.Response)
						}
						if mode == "raw" {
							requestJSON, responseJSON := decodeRawCall(t, stdout.Bytes())
							assertRawJSONEqual(t, requestJSON, actualRequest)
							want := []byte(responseBody)
							if responseStream {
								want, _ = json.Marshal(wantData)
							}
							assertRawJSONEqual(t, responseJSON, want)
						} else if mode == "text" {
							if stdout.String() != "OK\n" {
								t.Fatalf("text output=%s", &stdout)
							}
						} else {
							for _, value := range []string{"requires_action", "weather", "OK"} {
								if !strings.Contains(stdout.String(), value) {
									t.Fatalf("%s missing %s: %s", mode, value, &stdout)
								}
							}
							for _, value := range []string{`"input_tokens":8`, `"output_tokens":5`} {
								if !strings.Contains(stdout.String(), value) {
									t.Fatalf("missing usage %s: %s", value, &stdout)
								}
							}
						}
					})
				}
			}
		}
	}
}

func TestCallNonStreamingRawProviderError(t *testing.T) {
	for _, driver := range []string{"openai", "anthropic", "openai-responses"} {
		t.Run(driver, func(t *testing.T) {
			body := `{"code":"invalid_key","message":"fixture-raw-secret rejected"}`
			paths := prepareRawModel(t, driver, "https://example.invalid/fixture", rawRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}), false)
			var stdout, stderr bytes.Buffer
			output := newCLIOutput(false, &stdout, &stderr)
			err := runTestReq(paths, []string{"cc-api", "--raw", "hello"}, output)
			if err == nil || output.fail(err) != 1 {
				t.Fatalf("error=%v", err)
			}
			request, response := decodeRawCall(t, stdout.Bytes())
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(request, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["stream"]) != "false" {
				t.Fatalf("request=%s", request)
			}
			assertRawJSONEqual(t, response, []byte(strings.ReplaceAll(body, rawFixtureSecret, "[REDACTED]")))
			log := readRawCallLog(t, paths)
			if len(log.Attempts) != 1 || log.Error == nil || log.Attempts[0].Response == nil || log.Attempts[0].Response.Status != 401 {
				t.Fatalf("log=%#v", log)
			}
		})
	}
}
