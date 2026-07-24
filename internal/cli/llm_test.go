package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/runtimeapi"
)

func TestReadLLMRequestUsesStrictBoundedRegularFile(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "request.json")
	if err := os.WriteFile(valid, []byte(`{"profile":"test","prompt":"hello","max_tokens":512}`), 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := readLLMRequest(valid)
	if err != nil {
		t.Fatal(err)
	}
	if request.Profile != "test" || request.Prompt != "hello" || request.MaxTokens != 512 {
		t.Fatalf("request=%#v", request)
	}

	unknown := filepath.Join(root, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"profile":"test","prompt":"hello","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLLMRequest(unknown); err == nil {
		t.Fatal("unknown request field was accepted")
	}

	multiple := filepath.Join(root, "multiple.json")
	if err := os.WriteFile(multiple, []byte(`{"profile":"test","prompt":"one"} {"profile":"test","prompt":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLLMRequest(multiple); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}

	link := filepath.Join(root, "request-link.json")
	if err := os.Symlink(valid, link); err == nil {
		if _, err := readLLMRequest(link); err == nil {
			t.Fatal("symlink request file was accepted")
		}
	}
}

func TestLLMNamespaceGeneratesNormalAndStreamOutput(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] == true {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"stream\"},\"finish_reason\":\"stop\"}]}\n\n"))
			_, _ = writer.Write([]byte("data: [DONE]\n\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"normal"},"finish_reason":"stop"}]}`))
	}))
	defer providerServer.Close()

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_CLI_LLM_KEY", "secret")
	profile := fmt.Sprintf(
		`{"protocol":"openai","base_url":%q,"model":"test","api_key":"${TEST_CLI_LLM_KEY}"}`,
		providerServer.URL,
	)
	if err := os.WriteFile(filepath.Join(home, "configs", "test.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(home, "request.json")
	requestData, _ := json.Marshal(runtimeapi.Request{Profile: "test", Prompt: "hello"})
	if err := os.WriteFile(requestPath, requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Home: home}

	normal := captureStdout(t, func() {
		if err := runLLMNamespace(cfg, []string{"generate", "--request-file", requestPath}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(normal, `"content": "normal"`) || !strings.Contains(normal, `"done": true`) {
		t.Fatalf("normal output=%s", normal)
	}

	stream := captureStdout(t, func() {
		if err := runLLMNamespace(cfg, []string{"generate", "--request-file", requestPath, "--stream"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, expected := range []string{
		`"type":"request.started"`,
		`"type":"output.delta"`,
		`"delta":"stream"`,
		`"type":"response.completed"`,
	} {
		if !strings.Contains(stream, expected) {
			t.Fatalf("stream output missing %q:\n%s", expected, stream)
		}
	}
}
