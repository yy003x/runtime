package command

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestResolvePromptTextAndFileLimits(t *testing.T) {
	for _, test := range []struct {
		name     string
		text     string
		file     bool
		overflow bool
	}{
		{name: "short_text", text: "请分析当前项目，并给出实施方案"},
		{name: "long_filename_text", text: strings.Repeat("x", 300)},
		{name: "long_unicode_text", text: strings.Repeat("分析", 300)},
		{name: "text_at_limit", text: strings.Repeat("x", MaxTokenBytes)},
		{name: "text_over_limit", text: strings.Repeat("x", MaxTokenBytes+1), overflow: true},
		{name: "unicode_within_limit", text: strings.Repeat("界", MaxTokenBytes/3)},
		{name: "unicode_over_limit", text: strings.Repeat("界", MaxTokenBytes/3+1), overflow: true},
		{name: "file_at_limit", text: strings.Repeat("x", MaxTokenBytes), file: true},
		{name: "file_over_limit", text: strings.Repeat("x", MaxTokenBytes+1), file: true, overflow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			input := test.text
			if test.file {
				input = "prompt.txt"
				if err := os.WriteFile(filepath.Join(root, input), []byte(test.text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ResolvePrompt(input, root)
			if test.overflow {
				assertPromptOverflow(t, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.text {
				t.Fatalf("resolved prompt length=%d want=%d", len(got), len(test.text))
			}
		})
	}
}

func TestPromptSourcesUseSameOverflowError(t *testing.T) {
	oversized := strings.Repeat("x", MaxTokenBytes+1)
	_, err := ReadPrompt(strings.NewReader(oversized))
	assertPromptOverflow(t, err)
	_, err = MergePrompt(oversized)
	assertPromptOverflow(t, err)
	_, err = MergePrompt(strings.Repeat("x", MaxTokenBytes), "next")
	assertPromptOverflow(t, err)
}

func TestResolvePromptPreservesReadFailures(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolvePrompt(root, root); err == nil {
		t.Fatal("directory was accepted as prompt text")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read files without read permission")
	}
	path := filepath.Join(root, "unreadable.txt")
	if err := os.WriteFile(path, []byte("private prompt"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePrompt(path, root)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable file error=%v want permission error", err)
	}
}

func assertPromptOverflow(t *testing.T, err error) {
	t.Helper()
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != contract.ErrorContextOverflow ||
		runtimeErr.Phase != contract.PhaseRequest ||
		runtimeErr.Message != fmt.Sprintf("prompt exceeds %d bytes", MaxTokenBytes) {
		t.Fatalf("prompt overflow error=%v", err)
	}
}
