package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/layout"
	runtime "github.com/yy003x/runtime/run"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = original }()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	value, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return string(value)
}

type agentStreamFixture struct {
	Paths  layout.Paths
	Output *cliOutput
	Stdout *bytes.Buffer
	Stderr *bytes.Buffer
	Err    error
}

func executeAgentStreamFixture(
	t *testing.T,
	statusCode int,
	responseBody string,
) agentStreamFixture {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(statusCode)
		_, _ = writer.Write([]byte(responseBody))
	}))
	defer server.Close()
	paths := prepareVNextHome(t)
	writeVNextModel(
		t, paths.ConfigDir, "api-agent",
		server.URL+"/v1/chat/completions",
	)
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = originalTransport }()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	output := newCLIOutput(false, stdout, stderr)
	err := runAgentNamespace(
		paths,
		[]string{
			"run", "--profile", "api-agent", "--stream", "hello",
		},
		output,
	)
	return agentStreamFixture{
		Paths: paths, Output: output, Stdout: stdout, Stderr: stderr, Err: err,
	}
}

type runStreamInspection struct {
	LineCount  int
	EventCount int
	FinalCount int
	FinalIndex int
	Run        runtime.Record
}

func inspectRunStream(t *testing.T, value string) runStreamInspection {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(value), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	result := runStreamInspection{
		LineCount: len(lines), FinalIndex: -1,
	}
	for index, line := range lines {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("line %d is not NDJSON: %v: %q", index, err, line)
		}
		if _, exists := raw["sequence"]; exists {
			result.EventCount++
			continue
		}
		runValue, exists := raw["run"]
		if !exists {
			continue
		}
		result.FinalCount++
		result.FinalIndex = index
		if err := json.Unmarshal(runValue, &result.Run); err != nil {
			t.Fatalf("decode final run: %v", err)
		}
	}
	return result
}

func assertSingleV2StreamError(
	t *testing.T,
	stdout string,
	stderr string,
) {
	t.Helper()
	if inspection := inspectRunStream(t, stdout); inspection.FinalCount != 0 {
		t.Fatalf("failed stream contains final: %q", stdout)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr is not one NDJSON error: %q", stderr)
	}
	var payload struct {
		ContractVersion int `json:"contract_version"`
		Error           struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ContractVersion != 2 || payload.Error.Message == "" {
		t.Fatalf("payload=%#v", payload)
	}
}
