package toolbuiltin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/agent"
)

func TestBuildRegistersOnlyExplicitTools(t *testing.T) {
	root := t.TempDir()
	registry, err := Build(Options{
		Names: []string{"read_file", "list_directory"},
		Roots: []string{root},
		CWD:   root,
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 2 ||
		definitions[0].Name != "list_directory" ||
		definitions[1].Name != "read_file" {
		t.Fatalf("definitions=%#v", definitions)
	}
	if _, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name: "write_file", Arguments: []byte(`{"path":"new.txt","content":"x"}`),
	}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("write_file should be unavailable: %v", err)
	}
}

func TestReadAndListStayWithinRootAndRejectSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	registry, err := Build(Options{
		Names: []string{"read_file", "list_directory"},
		Roots: []string{root},
		CWD:   root,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name: "read_file", Arguments: []byte(`{"path":"value.txt"}`),
	})
	if err != nil || result.Content != "hello" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	for _, input := range []string{
		`{"path":"../secret.txt"}`,
		`{"path":"link.txt"}`,
	} {
		if _, err := registry.Execute(context.Background(), agent.ToolRequest{
			Name: "read_file", Arguments: []byte(input),
		}); err == nil {
			t.Fatalf("accepted unsafe input %s", input)
		}
	}
}

func TestWriteAndExecRequireExplicitOptIn(t *testing.T) {
	root := t.TempDir()
	registry, err := Build(Options{
		Names: []string{"write_file", "exec_command"},
		Roots: []string{root},
		CWD:   root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "write_file",
		Arguments: []byte(`{"path":"created.txt","content":"value"}`),
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "created.txt"))
	if err != nil || string(data) != "value" {
		t.Fatalf("data=%q error=%v", data, err)
	}
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name: "exec_command",
		Arguments: []byte(
			`{"argv":["/bin/echo","$HOME"],"timeout_seconds":5}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `$HOME`) {
		t.Fatalf("exec_command unexpectedly used a shell: %s", result.Content)
	}
}
