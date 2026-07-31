package toolbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/agent"
)

func newSafeTestResolver(t *testing.T, root string) *resolver {
	t.Helper()
	value, err := newResolver([]string{root}, root)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func pathToolRequest(t *testing.T, name, path string) agent.ToolRequest {
	t.Helper()
	arguments, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return agent.ToolRequest{Name: name, Arguments: arguments}
}

func writeToolRequest(
	t *testing.T,
	path string,
	content string,
) agent.ToolRequest {
	t.Helper()
	arguments, err := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: path, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return agent.ToolRequest{Name: "write_file", Arguments: arguments}
}

func requireReadOnlyErrorCode(
	t *testing.T,
	result agent.ToolResult,
	err error,
	code string,
) {
	t.Helper()
	if err != nil {
		t.Fatalf("error=%v", err)
	}
	if !result.IsError || result.Content == "" ||
		len(result.Content) > maxReadOnlyToolErrorBytes {
		t.Fatalf("result=%#v", result)
	}
	var envelope readOnlyToolErrorEnvelope
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatalf("content=%q error=%v", result.Content, err)
	}
	if envelope.Error.Code != code || envelope.Error.Message == "" {
		t.Fatalf("content=%s", result.Content)
	}
}

func requireNoWriteTemps(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".runtime-tool-") {
			t.Fatalf("write temp leaked: %s", entry.Name())
		}
	}
}

func TestPinnedReadAndListResistDeterministicPathSwaps(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "workspace")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(root, "value.txt"),
			[]byte("pinned-root"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		moved := filepath.Join(parent, "pinned-workspace")
		resolver.testHooks = &resolverTestHooks{
			afterRootOpened: func() {
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(root, "value.txt"),
					[]byte("replacement-root"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		}
		result, err := resolver.readFile(
			context.Background(),
			pathToolRequest(t, "read_file", "value.txt"),
		)
		if err != nil || result.Content != "pinned-root" {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("parent", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(parent, "value.txt"),
			[]byte("pinned-parent"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		moved := filepath.Join(root, "pinned-parent")
		resolver.testHooks = &resolverTestHooks{
			afterDirectoryOpened: func(component string) {
				if component != "parent" {
					return
				}
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(parent, "value.txt"),
					[]byte("replacement-parent"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		}
		result, err := resolver.readFile(
			context.Background(),
			pathToolRequest(t, "read_file", "parent/value.txt"),
		)
		if err != nil || result.Content != "pinned-parent" {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("leaf", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "value.txt")
		if err := os.WriteFile(path, []byte("pinned-leaf"), 0o600); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		resolver.testHooks = &resolverTestHooks{
			afterReadLeafOpened: func() {
				if err := os.Rename(
					path,
					filepath.Join(root, "pinned-value.txt"),
				); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					path,
					[]byte("replacement-leaf"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		}
		result, err := resolver.readFile(
			context.Background(),
			pathToolRequest(t, "read_file", "value.txt"),
		)
		if err != nil || result.Content != "pinned-leaf" {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("list directory", func(t *testing.T) {
		root := t.TempDir()
		directory := filepath.Join(root, "items")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(directory, "old.txt"),
			[]byte("old"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		moved := filepath.Join(root, "pinned-items")
		resolver.testHooks = &resolverTestHooks{
			afterDirectoryOpened: func(component string) {
				if component != "items" {
					return
				}
				if err := os.Rename(directory, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(directory, "new.txt"),
					[]byte("new"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		}
		result, err := resolver.listDirectory(
			context.Background(),
			pathToolRequest(t, "list_directory", "items"),
		)
		if err != nil ||
			!strings.Contains(result.Content, `"old.txt"`) ||
			strings.Contains(result.Content, `"new.txt"`) {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})
}

func TestPinnedRootIdentityRejectsPreOperationReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "value.txt"),
		[]byte("original"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	resolver := newSafeTestResolver(t, root)
	if err := os.Rename(
		root,
		filepath.Join(parent, "original-workspace"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "value.txt"),
		[]byte("replacement"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	result, err := resolver.readFile(
		context.Background(),
		pathToolRequest(t, "read_file", "value.txt"),
	)
	requireReadOnlyErrorCode(t, result, err, "workspace_changed")
	if strings.Contains(result.Content, root) ||
		strings.Contains(result.Content, "replacement") {
		t.Fatalf("replacement root leaked through result: %s", result.Content)
	}
}

func TestReadRejectsFIFOHardlinkAndOversize(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "large.txt"),
		make([]byte, maxToolOutputBytes+1),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	resolver := newSafeTestResolver(t, root)
	type readResult struct {
		result agent.ToolResult
		err    error
	}
	fifoResult := make(chan readResult, 1)
	fifoRequest := pathToolRequest(t, "read_file", "pipe")
	go func() {
		result, err := resolver.readFile(
			context.Background(),
			fifoRequest,
		)
		fifoResult <- readResult{result: result, err: err}
	}()
	select {
	case value := <-fifoResult:
		requireReadOnlyErrorCode(
			t,
			value.result,
			value.err,
			"not_regular_file",
		)
	case <-time.After(2 * time.Second):
		t.Fatal("read_file blocked while opening a FIFO")
	}
	for _, testCase := range []struct {
		path string
		code string
	}{
		{path: "linked.txt", code: "hardlink_not_allowed"},
		{path: "large.txt", code: "file_too_large"},
	} {
		result, err := resolver.readFile(
			context.Background(),
			pathToolRequest(t, "read_file", testCase.path),
		)
		requireReadOnlyErrorCode(t, result, err, testCase.code)
	}
}

func TestListDirectoryBoundsEnumerationBeforeMarshal(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 900; index++ {
		name := fmt.Sprintf(
			"%04d-%s",
			index,
			strings.Repeat("x", 220),
		)
		if err := os.WriteFile(
			filepath.Join(root, name),
			nil,
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	resolver := newSafeTestResolver(t, root)
	result, err := resolver.listDirectory(
		context.Background(),
		pathToolRequest(t, "list_directory", "."),
	)
	requireReadOnlyErrorCode(
		t,
		result,
		err,
		"directory_too_large",
	)
	if strings.Contains(result.Content, root) {
		t.Fatalf("directory error leaked a path: %s", result.Content)
	}
}

func TestWriteUsesPinnedParentAndDurableAtomicPublish(t *testing.T) {
	t.Run("pinned parent", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(parent, "value.txt"),
			[]byte("old"),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		moved := filepath.Join(root, "pinned-parent")
		fileSynced := false
		directorySynced := false
		resolver.testHooks = &resolverTestHooks{
			afterDirectoryOpened: func(component string) {
				if component != "parent" {
					return
				}
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			afterWriteFileSynced: func() {
				fileSynced = true
			},
			afterWriteDirSynced: func() {
				directorySynced = true
			},
		}
		result, err := resolver.writeFile(
			context.Background(),
			writeToolRequest(t, "parent/value.txt", "durable"),
		)
		if err != nil || result.Content != `{"written":true}` {
			t.Fatalf("result=%#v error=%v", result, err)
		}
		if !fileSynced || !directorySynced {
			t.Fatalf(
				"durability barriers file=%t directory=%t",
				fileSynced,
				directorySynced,
			)
		}
		data, err := os.ReadFile(filepath.Join(moved, "value.txt"))
		if err != nil || string(data) != "durable" {
			t.Fatalf("data=%q error=%v", data, err)
		}
		if _, err := os.Stat(
			filepath.Join(parent, "value.txt"),
		); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("replacement parent was modified: %v", err)
		}
		info, err := os.Stat(filepath.Join(moved, "value.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("published mode=%#o", info.Mode().Perm())
		}
		requireNoWriteTemps(t, moved)
		requireNoWriteTemps(t, parent)
	})

	t.Run("leaf swap", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "workspace")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "value.txt")
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(parent, "outside.txt")
		if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		resolver.testHooks = &resolverTestHooks{
			beforeWritePublish: func() {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			},
		}
		if _, err := resolver.writeFile(
			context.Background(),
			writeToolRequest(t, "value.txt", "new"),
		); !errors.Is(err, errWriteTargetInvalid) {
			t.Fatalf("error=%v", err)
		}
		data, err := os.ReadFile(outside)
		if err != nil || string(data) != "secret" {
			t.Fatalf("outside=%q error=%v", data, err)
		}
		requireNoWriteTemps(t, root)
	})

	t.Run("failure cleanup", func(t *testing.T) {
		root := t.TempDir()
		resolver := newSafeTestResolver(t, root)
		injected := errors.New("injected temp failure")
		resolver.testHooks = &resolverTestHooks{
			afterWriteTempCreated: func() error {
				return injected
			},
		}
		if _, err := resolver.writeFile(
			context.Background(),
			writeToolRequest(t, "value.txt", "new"),
		); !errors.Is(err, injected) {
			t.Fatalf("error=%v", err)
		}
		if _, err := os.Stat(
			filepath.Join(root, "value.txt"),
		); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target unexpectedly published: %v", err)
		}
		requireNoWriteTemps(t, root)
	})

	t.Run("directory fsync acknowledgement lost", func(t *testing.T) {
		root := t.TempDir()
		resolver := newSafeTestResolver(t, root)
		injected := errors.New("injected directory fsync failure")
		resolver.testHooks = &resolverTestHooks{
			syncWriteDirectory: func(int) error {
				return injected
			},
		}
		if _, err := resolver.writeFile(
			context.Background(),
			writeToolRequest(t, "value.txt", "published"),
		); !errors.Is(err, injected) {
			t.Fatalf("error=%v", err)
		}
		data, err := os.ReadFile(filepath.Join(root, "value.txt"))
		if err != nil || string(data) != "published" {
			t.Fatalf("published data=%q error=%v", data, err)
		}
		requireNoWriteTemps(t, root)
	})

	t.Run("pre-existing symlink", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "workspace")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(parent, "outside.txt")
		if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(
			outside,
			filepath.Join(root, "value.txt"),
		); err != nil {
			t.Fatal(err)
		}
		resolver := newSafeTestResolver(t, root)
		if _, err := resolver.writeFile(
			context.Background(),
			writeToolRequest(t, "value.txt", "new"),
		); !errors.Is(err, errWriteTargetInvalid) {
			t.Fatalf("error=%v", err)
		}
		data, err := os.ReadFile(outside)
		if err != nil || string(data) != "secret" {
			t.Fatalf("outside=%q error=%v", data, err)
		}
		requireNoWriteTemps(t, root)
	})
}
