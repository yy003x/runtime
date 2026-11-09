package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/sn-code/pkg/agent"
)

func runTool(t *testing.T, name string, arguments map[string]any, environment Environment) agent.ToolResult {
	t.Helper()
	registry, err := Registry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	ctx := WithEnvironment(context.Background(), environment)
	result, err := registry.Execute(ctx, agent.ToolRequest{
		CallID:    "call-test",
		Name:      name,
		Arguments: json.RawMessage(encoded),
	})
	if err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return result
}

func writeProject(t *testing.T) Environment {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"main.go":               "package main\n\nfunc main() {}\n",
		"internal/util/util.go": "package util\n\nfunc Util() string { return \"util\" }\n",
		"notes/data.txt":        "alpha\nbeta\ngamma\n",
		".hidden/secret.txt":    "hidden\n",
		"sub/main.go":           "package sub\n",
	}
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "skip.txt"), []byte("skip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "debug.log"), []byte("log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Environment{CWD: root, SessionID: "s-test", Profile: "cc", Model: "test-model"}
}

func TestRegistryRejectsUnknownToolAndBadArguments(t *testing.T) {
	environment := writeProject(t)
	registry, err := Registry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if err := registry.Validate(agent.ToolRequest{Name: "nope", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("unknown tool must be rejected")
	}
	if err := registry.Validate(agent.ToolRequest{Name: "read", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("missing required path must be rejected")
	}
	if err := registry.Validate(agent.ToolRequest{Name: "edit", Arguments: json.RawMessage(`{"path":"a","edits":[]}`)}); err == nil {
		t.Fatal("empty edits must be rejected")
	}
	_ = environment
}

func TestReadPaginationAndTruncationNote(t *testing.T) {
	environment := writeProject(t)
	result := runTool(t, "read", map[string]any{"path": "notes/data.txt"}, environment)
	if result.IsError || result.Content != "alpha\nbeta\ngamma" {
		t.Fatalf("unexpected read result %+v", result)
	}
	result = runTool(t, "read", map[string]any{"path": "notes/data.txt", "offset": 2}, environment)
	if result.IsError || result.Content != "beta\ngamma" {
		t.Fatalf("offset read = %q", result.Content)
	}
	result = runTool(t, "read", map[string]any{"path": "notes/data.txt", "offset": 2, "limit": 1}, environment)
	if result.IsError || !strings.Contains(result.Content, "[Showing lines 2-2 of 3") {
		t.Fatalf("limit read missing note: %q", result.Content)
	}
	result = runTool(t, "read", map[string]any{"path": "notes/data.txt", "offset": 99}, environment)
	if !result.IsError {
		t.Fatalf("out-of-range offset must be an error: %+v", result)
	}
}

func TestWriteCreatesDirectoriesAndEditSemantics(t *testing.T) {
	environment := writeProject(t)
	result := runTool(t, "write", map[string]any{
		"path": "nested/dir/new.txt", "content": "line1\nline2\nline3\n",
	}, environment)
	if result.IsError || result.Content != "Successfully wrote to nested/dir/new.txt" {
		t.Fatalf("write result %+v", result)
	}
	result = runTool(t, "edit", map[string]any{
		"path": "nested/dir/new.txt",
		"edits": []map[string]string{
			{"oldText": "line1", "newText": "first"},
			{"oldText": "line3", "newText": "third"},
		},
	}, environment)
	if result.IsError || !strings.Contains(result.Content, "Successfully replaced 2 block(s)") {
		t.Fatalf("edit result %+v", result)
	}
	raw, err := os.ReadFile(filepath.Join(environment.CWD, "nested/dir/new.txt"))
	if err != nil || string(raw) != "first\nline2\nthird\n" {
		t.Fatalf("edited content = %q err=%v", string(raw), err)
	}
	result = runTool(t, "edit", map[string]any{
		"path":  "nested/dir/new.txt",
		"edits": []map[string]string{{"oldText": "line2", "newText": "x"}},
	}, environment)
	if result.IsError {
		t.Fatalf("single match edit failed: %+v", result)
	}
	result = runTool(t, "edit", map[string]any{
		"path":  "notes/data.txt",
		"edits": []map[string]string{{"oldText": "a", "newText": "x"}},
	}, environment)
	if !result.IsError || !strings.Contains(result.Content, "more than once") {
		t.Fatalf("ambiguous oldText must fail: %+v", result)
	}
	result = runTool(t, "edit", map[string]any{
		"path":  "missing.txt",
		"edits": []map[string]string{{"oldText": "a", "newText": "b"}},
	}, environment)
	if !result.IsError {
		t.Fatal("editing a missing file must fail")
	}
}

func TestWritePreservesExistingModeAndUsesDefaultForNewFiles(t *testing.T) {
	environment := writeProject(t)
	path := filepath.Join(environment.CWD, "script.sh")
	if err := os.WriteFile(path, []byte("old\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o751); err != nil {
		t.Fatal(err)
	}
	result := runTool(t, "write", map[string]any{"path": "script.sh", "content": "new\n"}, environment)
	if result.IsError {
		t.Fatalf("write existing file: %+v", result)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("existing mode = %v, err=%v", info.Mode(), err)
	}
	result = runTool(t, "write", map[string]any{"path": "new.txt", "content": "new\n"}, environment)
	info, err = os.Stat(filepath.Join(environment.CWD, "new.txt"))
	if result.IsError || err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("new file mode = %v, result=%+v, err=%v", info.Mode(), result, err)
	}
}

func TestEditPreservesCRLFFile(t *testing.T) {
	environment := writeProject(t)
	path := filepath.Join(environment.CWD, "crlf.txt")
	if err := os.WriteFile(path, []byte("one\r\ntwo\r\nthree\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runTool(t, "edit", map[string]any{
		"path":  "crlf.txt",
		"edits": []map[string]string{{"oldText": "two", "newText": "2"}},
	}, environment)
	if result.IsError {
		t.Fatalf("crlf edit failed: %+v", result)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "\n") && !strings.Contains(string(raw), "\r\n") {
		t.Fatalf("crlf endings lost: %q", string(raw))
	}
}

func TestBashExitCodeAndEcho(t *testing.T) {
	environment := writeProject(t)
	result := runTool(t, "bash", map[string]any{"command": "echo hello"}, environment)
	if result.IsError || !strings.Contains(result.Content, "hello") {
		t.Fatalf("bash echo result %+v", result)
	}
	result = runTool(t, "bash", map[string]any{"command": "exit 3"}, environment)
	if !result.IsError || !strings.Contains(result.Content, "Command exited with code 3") {
		t.Fatalf("bash exit code result %+v", result)
	}
	result = runTool(t, "bash", map[string]any{"command": "echo $SN_CODE_SESSION_ID"}, environment)
	if result.IsError || !strings.Contains(result.Content, "s-test") {
		t.Fatalf("bash environment injection result %+v", result)
	}
}

func TestBashTimeout(t *testing.T) {
	environment := writeProject(t)
	started := time.Now()
	result := runTool(t, "bash", map[string]any{"command": "sleep 5", "timeout": 1}, environment)
	elapsed := time.Since(started)
	if !result.IsError || !strings.Contains(result.Content, "timed out") {
		t.Fatalf("timeout result %+v", result)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestBashStreamsLargeOutputToFileWithBoundedTail(t *testing.T) {
	environment := writeProject(t)
	result := runTool(t, "bash", map[string]any{
		"command": "head -c 200000 /dev/zero | tr '\\0' x",
	}, environment)
	if result.IsError {
		t.Fatalf("large output command: %+v", result)
	}
	marker := "[Full output: "
	start := strings.Index(result.Content, marker)
	if start < 0 || len(result.Content) > maxOutputBytes+maxOutputLines {
		t.Fatalf("missing full output path or unbounded result: length=%d", len(result.Content))
	}
	end := strings.Index(result.Content[start:], "]")
	if end < 0 {
		t.Fatalf("malformed output note: %q", result.Content[start:])
	}
	path := result.Content[start+len(marker) : start+end]
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) != 200000 {
		t.Fatalf("full output length=%d, err=%v", len(raw), err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func TestGrepHonorsGitignoreAndGlob(t *testing.T) {
	environment := writeProject(t)
	result := runTool(t, "grep", map[string]any{"pattern": "package"}, environment)
	if result.IsError {
		t.Fatalf("grep failed: %+v", result)
	}
	if !strings.Contains(result.Content, "main.go:1:package main") ||
		!strings.Contains(result.Content, "sub/main.go:1:package sub") {
		t.Fatalf("grep missing go packages: %q", result.Content)
	}
	if strings.Contains(result.Content, "skip.txt") || strings.Contains(result.Content, "debug.log") ||
		strings.Contains(result.Content, "secret.txt") {
		t.Fatalf("grep leaked ignored/hidden files: %q", result.Content)
	}
	result = runTool(t, "grep", map[string]any{"pattern": "package", "glob": "*.go", "path": "internal"}, environment)
	if result.IsError || !strings.Contains(result.Content, "util.go:1:package util") {
		t.Fatalf("glob+path grep = %q", result.Content)
	}
	result = runTool(t, "grep", map[string]any{"pattern": "ALPHA", "ignoreCase": true, "path": "notes/data.txt"}, environment)
	if result.IsError || !strings.Contains(result.Content, "alpha") {
		t.Fatalf("ignoreCase grep = %q", result.Content)
	}
	result = runTool(t, "grep", map[string]any{"pattern": "a(l", "literal": true, "path": "notes/data.txt"}, environment)
	if result.IsError || result.Content != "No matches found" {
		t.Fatalf("literal grep of regex metacharacters must not error: %+v", result)
	}
	result = runTool(t, "grep", map[string]any{"pattern": "alpha", "literal": true, "path": "notes/data.txt"}, environment)
	if result.IsError || !strings.Contains(result.Content, "alpha") {
		t.Fatalf("literal grep result %+v", result)
	}
}

func TestGrepDirectFileHonorsGlob(t *testing.T) {
	environment := writeProject(t)
	matched := runTool(t, "grep", map[string]any{
		"pattern": "package", "path": "main.go", "glob": "*.go",
	}, environment)
	if matched.IsError || !strings.Contains(matched.Content, "main.go:1:package main") {
		t.Fatalf("matching direct file glob: %+v", matched)
	}
	excluded := runTool(t, "grep", map[string]any{
		"pattern": "package", "path": "main.go", "glob": "*.txt",
	}, environment)
	if excluded.IsError || excluded.Content != "No matches found" {
		t.Fatalf("excluded direct file glob: %+v", excluded)
	}
}

func TestFindAndLs(t *testing.T) {
	environment := writeProject(t)
	result := runTool(t, "find", map[string]any{"pattern": "*.go"}, environment)
	if result.IsError || !strings.Contains(result.Content, "main.go") ||
		!strings.Contains(result.Content, "sub/main.go") ||
		strings.Contains(result.Content, "skip.txt") {
		t.Fatalf("find result %q", result.Content)
	}
	result = runTool(t, "find", map[string]any{"pattern": "**/*.txt"}, environment)
	if result.IsError || !strings.Contains(result.Content, "notes/data.txt") ||
		strings.Contains(result.Content, "ignored/skip.txt") {
		t.Fatalf("find txt result %q", result.Content)
	}
	result = runTool(t, "ls", map[string]any{"path": "notes"}, environment)
	if result.IsError || result.Content != "data.txt" {
		t.Fatalf("ls result %q", result.Content)
	}
	if err := os.MkdirAll(filepath.Join(environment.CWD, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	result = runTool(t, "ls", map[string]any{"path": "emptydir"}, environment)
	if result.IsError || result.Content != "(empty directory)" {
		t.Fatalf("empty ls result %q", result.Content)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", true}, // 无斜杠 → 匹配任意深度的文件名
		{"*.go", "main.txt", false},
		{"src/**/*.ts", "src/a/b/c.ts", true},
		{"src/**/*.ts", "src/c.ts", true},
		{"src/**/*.ts", "other/src/c.ts", false},
		{"**/*.json", "a/b/c.json", true},
		{"a/*.md", "a/x.md", true},
		{"a/*.md", "a/b/x.md", false},
	}
	for _, item := range cases {
		if got := matchGlob(item.pattern, item.rel); got != item.want {
			t.Fatalf("matchGlob(%q, %q) = %v, want %v", item.pattern, item.rel, got, item.want)
		}
	}
}
