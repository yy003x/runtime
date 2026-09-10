package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestCallExtraBodyDefaultsOverridesAndRawWire(t *testing.T) {
	for _, test := range []struct {
		driver   string
		extra    string
		response string
	}{
		{"openai-chat-completions", `{"reasoning_effort":"high","response_format":{"type":"json_object"},"tool_choice":"auto","parallel_tool_calls":true,"frequency_penalty":1,"presence_penalty":0,"logprobs":false,"top_logprobs":0,"logit_bias":{},"seed":9007199254740993,"metadata":{"default":true},"prompt_cache_key":"fixture","prompt_cache_retention":null,"prompt_cache_options":{"ttl":"30m"},"service_tier":"auto","stream_options":null,"verbosity":"low"}`, `{"id":"fixture","model":"fixture","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`},
		{"openai-responses", `{"reasoning":{"effort":"high","summary":"auto"},"text":{"format":{"type":"text"}},"include":[],"tool_choice":"auto","parallel_tool_calls":true,"max_tool_calls":0,"metadata":{"default":true},"service_tier":"auto","prompt_cache_key":"fixture","prompt_cache_retention":null,"store":null}`, rawResponsesJSON},
		{"anthropic-messages", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tool_choice":{"type":"auto"},"top_k":0,"metadata":{"default":true},"cache_control":{"type":"ephemeral","ttl":"5m"},"service_tier":null}`, `{"id":"fixture","type":"message","model":"fixture","role":"assistant","content":[{"type":"thinking","thinking":"upstream only","signature":"opaque-signature"},{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`},
	} {
		t.Run(test.driver, func(t *testing.T) {
			captured := make(chan []byte, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- body
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, test.response)
			}))
			defer server.Close()
			paths := prepareRawModel(t, test.driver, server.URL+"/fixture", server.Client().Transport, false)
			profilePath := filepath.Join(paths.ConfigDir, "cc-api.json")
			profileBytes, err := os.ReadFile(profilePath)
			if err != nil {
				t.Fatal(err)
			}
			var profile map[string]json.RawMessage
			if err := json.Unmarshal(profileBytes, &profile); err != nil {
				t.Fatal(err)
			}
			var parameters map[string]json.RawMessage
			if err := json.Unmarshal(profile["parameters"], &parameters); err != nil {
				t.Fatal(err)
			}
			parameters["extra_body"] = json.RawMessage(test.extra)
			profile["parameters"], err = json.Marshal(parameters)
			if err != nil {
				t.Fatal(err)
			}
			profileBytes, err = json.Marshal(profile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(profilePath, profileBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			var defaults contract.ExtraBody
			if err := json.Unmarshal([]byte(test.extra), &defaults); err != nil {
				t.Fatal(err)
			}
			overrides := contract.ExtraBody{"metadata": json.RawMessage(`null`), "tool_choice": json.RawMessage(`null`), "provider_extension": json.RawMessage(`{"zero":0,"false":false,"integer":9007199254740993}`)}
			if test.driver == "openai-chat-completions" {
				overrides["frequency_penalty"] = json.RawMessage(`0`)
				overrides["parallel_tool_calls"] = json.RawMessage(`false`)
			}
			if test.driver == "openai-responses" {
				overrides["reasoning"] = json.RawMessage(`{"effort":"low"}`)
			}
			request := contract.ModelRequest{Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}}, Options: contract.GenerateOptions{ExtraBody: overrides}}
			requestBytes, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(requestPath, requestBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if err := runTestReq(paths, []string{"cc-api", "--raw", "--request-file", requestPath}, newCLIOutput(false, &stdout, &stderr)); err != nil {
				t.Fatalf("%v stderr=%s", err, &stderr)
			}
			rawRequest, _ := decodeRawCall(t, stdout.Bytes())
			var body []byte
			select {
			case body = <-captured:
			default:
				t.Fatal("no actual HTTP request")
			}
			assertRawJSONEqual(t, rawRequest, body)
			for _, value := range [][]byte{rawRequest, body} {
				if !bytes.Contains(value, []byte(`9007199254740993`)) {
					t.Fatalf("native integer lost precision: %s", value)
				}
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			for name, expected := range contract.MergeExtraBody(defaults, overrides) {
				assertRawJSONEqual(t, fields[name], expected)
			}
			if _, wrapped := fields["extra_body"]; wrapped || string(fields["model"]) != `"fixture"` || string(fields["stream"]) != "false" {
				t.Fatalf("unexpected wire body: %s", body)
			}
			logged := readRawCallLog(t, paths)
			if len(logged.Attempts) != 1 {
				t.Fatal("expected one HTTP attempt")
			}
			assertRawJSONEqual(t, logged.Attempts[0].Request.Body, body)
			for _, path := range []string{paths.SessionsDir, filepath.Join(paths.StateDir, "runtime.db")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("call created canonical state: %s", path)
				}
			}
		})
	}
}
