package executionlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/provider"
)

func TestAppendSeparatesDailyCLIAndAPIRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	when := time.Date(2026, 8, 4, 10, 40, 23, 0, time.Local)
	if err := AppendCLI(root, CLIRecord{
		Time: when, Namespace: NamespaceExec, Profile: "cx",
		Source:  "sn-cli exec cx task",
		Command: "MODEL_API_KEY=${MODEL_API_KEY} && /bin/cx task",
	}); err != nil {
		t.Fatal(err)
	}
	if err := AppendAPI(root, APIRecord{
		Time: when, Namespace: NamespaceRequest, Profile: "api-cc",
		Source: "sn-cli req api-cc 简单介绍下自己", CallID: "call_fixture",
		Request: provider.Request{
			Method: "POST", URL: "https://example.invalid/v1/messages",
			Headers: map[string]string{
				"Content-Type": "application/json",
				"X-Api-Key":    "${MODEL_API_KEY}",
			},
			Body: json.RawMessage(`{"model":"fixture","messages":[]}`),
		},
		Response: &provider.Response{
			Status: 200, Headers: map[string]string{"Content-Type": "application/json"},
			Data: []json.RawMessage{json.RawMessage(`{"id":"msg-1"}`)},
		},
		Error: &contract.RuntimeError{
			Code:  contract.ErrorProviderUnavailable,
			Phase: contract.PhaseProvider, Message: "fixture", Retryable: true,
		},
	}); err != nil {
		t.Fatal(err)
	}

	dayDir := filepath.Join(root, "260804")
	cli := decodeLine(t, filepath.Join(dayDir, "cli.jsonl"))
	api := decodeLine(t, filepath.Join(dayDir, "api.jsonl"))
	if got := sortedKeys(cli); !reflect.DeepEqual(got, []string{
		"command", "namespace", "profile", "source", "time",
	}) {
		t.Fatalf("CLI keys=%v", got)
	}
	if got := sortedKeys(api); !reflect.DeepEqual(got, []string{
		"call_id", "error", "namespace", "profile", "request", "response", "source", "time",
	}) {
		t.Fatalf("API keys=%v", got)
	}
	if api["time"] != "2026-08-04 10:40:23" || api["namespace"] != "req" ||
		api["profile"] != "api-cc" || api["call_id"] != "call_fixture" {
		t.Fatalf("API record=%#v", api)
	}
	if _, exists := api["command"]; exists {
		t.Fatalf("API record unexpectedly contains command: %#v", api)
	}
	request := api["request"].(map[string]any)
	headers := request["headers"].(map[string]any)
	if headers["X-Api-Key"] != "${MODEL_API_KEY}" {
		t.Fatalf("request headers=%#v", headers)
	}
	assertMode(t, root, 0o700)
	assertMode(t, dayDir, 0o700)
	assertMode(t, filepath.Join(dayDir, "cli.jsonl"), 0o600)
	assertMode(t, filepath.Join(dayDir, "api.jsonl"), 0o600)
}

func TestAppendConcurrentRecordsRemainValidJSONLines(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	when := time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC)
	const count = 64
	var group sync.WaitGroup
	for index := 0; index < count; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := AppendAPI(root, APIRecord{
				Time: when, Namespace: NamespaceAgent, Profile: "api",
				Source: "agent run_fixture", CallID: "call_fixture",
				Request: provider.Request{Method: "POST", Body: json.RawMessage(`{}`)},
			}); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	group.Wait()
	file, err := os.Open(filepath.Join(root, "260804", "api.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lines := 0
	for scanner.Scan() {
		lines++
		if !json.Valid(scanner.Bytes()) {
			t.Fatalf("invalid JSON line %d: %q", lines, scanner.Bytes())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != count {
		t.Fatalf("lines=%d want=%d", lines, count)
	}
}

func TestAppendLeavesLegacyFlatLogUntouchedAndEncodesNetworkFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "2026-08-04.jsonl")
	if err := os.WriteFile(legacy, []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)
	if err := AppendAPI(root, APIRecord{
		Time: when, Namespace: NamespaceRequest, Profile: "api",
		Source: "sn-cli req api hello", CallID: "call_fixture",
		Request: provider.Request{Method: "POST", Body: json.RawMessage(`{}`)},
		Error: &contract.RuntimeError{
			Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseProvider,
			Message: "connection failed", Retryable: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(legacy); err != nil || string(data) != "legacy\n" {
		t.Fatalf("legacy=%q error=%v", data, err)
	}
	record := decodeLine(t, filepath.Join(root, "260804", "api.jsonl"))
	if response, exists := record["response"]; !exists || response != nil {
		t.Fatalf("response=%#v exists=%t", response, exists)
	}
	if record["error"] == nil {
		t.Fatalf("error=%#v", record["error"])
	}
}

func TestAppendRejectsSymlinkLogRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "logs")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	err := AppendCLI(link, CLIRecord{Time: time.Now(), Profile: "cx"})
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("error=%v", err)
	}
	entries, err := os.ReadDir(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v", entries)
	}
}

func TestAppendRejectsRedirectedDayAndFileTargets(t *testing.T) {
	when := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string, string)
	}{
		{
			name: "day_symlink",
			setup: func(t *testing.T, root, outside string) {
				if err := os.Symlink(outside, filepath.Join(root, "260804")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "file_symlink",
			setup: func(t *testing.T, root, outside string) {
				day := filepath.Join(root, "260804")
				if err := os.Mkdir(day, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(
					filepath.Join(outside, "target"), filepath.Join(day, "api.jsonl"),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "file_hardlink",
			setup: func(t *testing.T, root, outside string) {
				day := filepath.Join(root, "260804")
				if err := os.Mkdir(day, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(
					filepath.Join(outside, "target"), filepath.Join(day, "api.jsonl"),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "logs")
			outside := filepath.Join(parent, "outside")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(outside, "target")
			if err := os.WriteFile(target, []byte("outside\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			test.setup(t, root, outside)
			err := AppendAPI(root, APIRecord{
				Time: when, Profile: "api",
				Request: provider.Request{Method: "POST"},
			})
			if err == nil {
				t.Fatal("redirected target was accepted")
			}
			data, readErr := os.ReadFile(target)
			if readErr != nil || string(data) != "outside\n" {
				t.Fatalf("outside target changed: data=%q error=%v", data, readErr)
			}
		})
	}
}

func TestAppendDoesNotBlockOnFIFOLogTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	day := filepath.Join(root, "260804")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(day, "api.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- AppendAPI(root, APIRecord{
			Time:    time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
			Profile: "api", Request: provider.Request{Method: "POST"},
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO target was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO target blocked diagnostic logging")
	}
}

func TestFormatCommandAndSourcePreserveReferences(t *testing.T) {
	key := "${MODEL_API_KEY}"
	got := FormatCommand(
		map[string]*string{"MODEL_API_KEY": &key, "UNSET": nil},
		"/tmp/work tree", "/bin/model-cli", []string{"--prompt", "hello world"},
	)
	want := `MODEL_API_KEY=${MODEL_API_KEY} UNSET= && cd "/tmp/work tree" && /bin/model-cli --prompt "hello world"`
	if got != want {
		t.Fatalf("command=%q want=%q", got, want)
	}
	if got := SourceFromArgs([]string{"/tmp/sn-cli", "req", "api", "hello"}); got != "sn-cli req api hello" {
		t.Fatalf("source=%q", got)
	}
}

func decodeLine(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("record is not one newline-terminated JSON line: %q", data)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%#o want=%#o", path, got, want)
	}
}
