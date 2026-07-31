package toolbuiltin

import (
	"context"
	"encoding/json"
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

func TestBuildSnapshotsCanonicalExecutionConfiguration(t *testing.T) {
	const secret = "must-not-appear-in-tool-execution-snapshot"
	t.Setenv("SN_TOOL_TEST_SECRET", secret)
	realRoot := t.TempDir()
	canonicalRealRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	linkParent := t.TempDir()
	linkedRoot := filepath.Join(linkParent, "workspace")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	secondRoot := t.TempDir()
	canonicalSecondRoot, err := filepath.EvalSymlinks(secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	realDevice, realInode, err := workspaceRootIdentity(canonicalRealRoot)
	if err != nil {
		t.Fatal(err)
	}
	secondDevice, secondInode, err := workspaceRootIdentity(
		canonicalSecondRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := Build(Options{
		Names: []string{"read_file"},
		Roots: []string{linkedRoot, secondRoot},
		CWD:   linkedRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := registry.ToolExecutionSnapshot()
	if snapshot.SchemaVersion != agent.ToolExecutionSnapshotSchemaVersion ||
		snapshot.Implementation != toolExecutionImplementation ||
		snapshot.ImplementationVersion != toolExecutionImplementationVersion {
		t.Fatalf("unexpected snapshot identity: %#v", snapshot)
	}
	var configuration toolExecutionConfiguration
	if err := json.Unmarshal(snapshot.Configuration, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.SchemaVersion != toolExecutionConfigSchemaVersion {
		t.Fatalf("configuration schema_version=%d", configuration.SchemaVersion)
	}
	if configuration.CWD != filepath.Clean(canonicalRealRoot) {
		t.Fatalf("canonical cwd=%q want=%q", configuration.CWD, canonicalRealRoot)
	}
	if len(configuration.WorkspaceRoots) != 2 ||
		configuration.WorkspaceRoots[0].Lexical != filepath.Clean(linkedRoot) ||
		configuration.WorkspaceRoots[0].Canonical != filepath.Clean(canonicalRealRoot) ||
		configuration.WorkspaceRoots[0].Device != realDevice ||
		configuration.WorkspaceRoots[0].Inode != realInode ||
		configuration.WorkspaceRoots[1].Lexical != filepath.Clean(secondRoot) ||
		configuration.WorkspaceRoots[1].Canonical != filepath.Clean(canonicalSecondRoot) ||
		configuration.WorkspaceRoots[1].Device != secondDevice ||
		configuration.WorkspaceRoots[1].Inode != secondInode {
		t.Fatalf("workspace roots lost configured order: %#v", configuration.WorkspaceRoots)
	}
	canonical, err := snapshot.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), secret) {
		t.Fatalf("snapshot captured an environment secret: %s", canonical)
	}
}

func TestReadAndListReturnStableErrorsWithoutLeakingPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "binary.txt"),
		[]byte{0xff, 0xfe},
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "large.txt"),
		make([]byte, maxToolOutputBytes+1),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	outsideArguments, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: outside})
	if err != nil {
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
	testCases := []struct {
		name      string
		tool      string
		arguments string
		code      string
	}{
		{
			name:      "missing",
			tool:      "read_file",
			arguments: `{"path":"missing.txt"}`,
			code:      "not_found",
		},
		{
			name:      "list missing",
			tool:      "list_directory",
			arguments: `{"path":"missing"}`,
			code:      "not_found",
		},
		{
			name:      "outside workspace",
			tool:      "read_file",
			arguments: string(outsideArguments),
			code:      "outside_workspace",
		},
		{
			name:      "symlink",
			tool:      "read_file",
			arguments: `{"path":"link.txt"}`,
			code:      "symlink_not_allowed",
		},
		{
			name:      "list symlink",
			tool:      "list_directory",
			arguments: `{"path":"link.txt"}`,
			code:      "symlink_not_allowed",
		},
		{
			name:      "invalid utf8",
			tool:      "read_file",
			arguments: `{"path":"binary.txt"}`,
			code:      "invalid_utf8",
		},
		{
			name:      "not regular",
			tool:      "read_file",
			arguments: `{"path":"."}`,
			code:      "not_regular_file",
		},
		{
			name:      "read limit",
			tool:      "read_file",
			arguments: `{"path":"large.txt"}`,
			code:      "file_too_large",
		},
		{
			name:      "not directory",
			tool:      "list_directory",
			arguments: `{"path":"value.txt"}`,
			code:      "not_directory",
		},
		{
			name: "filesystem io",
			tool: "read_file",
			arguments: `{"path":"` +
				strings.Repeat("x", 512) + `"}`,
			code: "io_error",
		},
	}
	messages := map[string]string{
		"not_found":           "path does not exist",
		"outside_workspace":   "path is outside configured workspace roots",
		"symlink_not_allowed": "symlink paths are not allowed",
		"invalid_utf8":        "file is not valid UTF-8 text",
		"not_regular_file":    "path is not a regular file",
		"file_too_large":      "file exceeds the read limit",
		"directory_too_large": "directory exceeds the listing limit",
		"not_directory":       "path is not a directory",
		"io_error":            "filesystem operation failed",
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var first string
			for attempt := 0; attempt < 2; attempt++ {
				result, err := registry.Execute(
					context.Background(),
					agent.ToolRequest{
						Name:      testCase.tool,
						Arguments: []byte(testCase.arguments),
					},
				)
				if err != nil {
					t.Fatalf("error=%v", err)
				}
				if !result.IsError || result.Content == "" ||
					len(result.Content) > maxReadOnlyToolErrorBytes {
					t.Fatalf("result=%#v", result)
				}
				var envelope readOnlyToolErrorEnvelope
				if err := json.Unmarshal(
					[]byte(result.Content),
					&envelope,
				); err != nil {
					t.Fatalf("content=%q error=%v", result.Content, err)
				}
				if envelope.Error.Code != testCase.code ||
					envelope.Error.Message != messages[testCase.code] {
					t.Fatalf("content=%s", result.Content)
				}
				expected, err := json.Marshal(readOnlyToolErrorEnvelope{
					Error: readOnlyToolError{
						Code:    testCase.code,
						Message: messages[testCase.code],
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if result.Content != string(expected) {
					t.Fatalf(
						"content=%q want=%q",
						result.Content,
						expected,
					)
				}
				if strings.Contains(result.Content, outside) ||
					strings.Contains(result.Content, root) {
					t.Fatalf(
						"error leaked an absolute path: %s",
						result.Content,
					)
				}
				if attempt == 0 {
					first = result.Content
				} else if result.Content != first {
					t.Fatalf(
						"unstable error content: %q != %q",
						first,
						result.Content,
					)
				}
			}
		})
	}
	if _, err := registry.Execute(
		context.Background(),
		agent.ToolRequest{
			Name: "read_file", Arguments: []byte(`{}`),
		},
	); err == nil {
		t.Fatal("schema-invalid arguments reached the handler")
	}
}

func TestWriteRequiresExplicitOptInAndExecCommandIsRemoved(t *testing.T) {
	root := t.TempDir()
	registry, err := Build(Options{
		Names: []string{"write_file"},
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
	if _, err := Build(Options{
		Names: []string{"exec_command"}, Roots: []string{root}, CWD: root,
	}); err == nil || !strings.Contains(err.Error(), "unknown built-in tool") {
		t.Fatalf("removed exec_command was accepted: %v", err)
	}
}
