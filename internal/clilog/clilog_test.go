package clilog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendWritesCompactRecord(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2026, 8, 4, 10, 40, 23, 0, time.UTC)
	rec := Record{
		Time: when, Namespace: NamespaceProfile, Profile: "cc-glm",
		Source:  "sn-cli cc-glm --exec=true",
		Command: "ANTHROPIC_API_KEY=${Z_AI_API_KEY} && cd /tmp && /bin/claude --model x",
	}
	if err := Append(dir, rec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-08-04.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("expected one newline-terminated line, got %q", data)
	}
	var decoded recordJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Time != "2026-08-04 10:40:23" {
		t.Fatalf("time=%q want 2026-08-04 10:40:23", decoded.Time)
	}
	if decoded.Namespace != "profile" || decoded.Profile != "cc-glm" ||
		decoded.Source != "sn-cli cc-glm --exec=true" {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestFormatCommandEnvReferencesCWDAndArgs(t *testing.T) {
	apiKey := "${Z_AI_API_KEY}"
	home := "${HOME}/.claude-aip"
	got := FormatCommand(
		map[string]*string{
			"ANTHROPIC_API_KEY": &apiKey,
			"CLAUDE_CONFIG_DIR": &home,
			"UNSET":             nil,
		},
		"/Users/yang/p", "/opt/homebrew/bin/claude",
		[]string{"--model", "glm-5.2", "-p", "hello world"},
	)
	want := "ANTHROPIC_API_KEY=${Z_AI_API_KEY} CLAUDE_CONFIG_DIR=${HOME}/.claude-aip UNSET= " +
		"&& cd /Users/yang/p && /opt/homebrew/bin/claude --model glm-5.2 -p \"hello world\""
	if got != want {
		t.Fatalf("got=%q\nwant=%q", got, want)
	}
}

func TestFormatCommandOmitsEnvAndCWDWhenEmpty(t *testing.T) {
	got := FormatCommand(nil, "", "/bin/sh", []string{"-c", "echo hi"})
	want := "/bin/sh -c \"echo hi\""
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestSourceFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"/bin/sn-cli"}, "sn-cli"},
		{[]string{"/bin/sn-cli", "cc-glm", "--exec=true"}, "sn-cli cc-glm --exec=true"},
	}
	for _, c := range cases {
		if got := SourceFromArgs(c.args); got != c.want {
			t.Fatalf("args=%v got=%q want=%q", c.args, got, c.want)
		}
	}
}

func TestAppendSeparatesByDay(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := Append(dir, Record{
			Time: day, Namespace: NamespaceSession, Profile: "cx",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Append(dir, Record{
		Time: time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC),
		Namespace: NamespaceTmux, Profile: "cx",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-08-04.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1; lines != 3 {
		t.Fatalf("expected 3 records on 08-04, got %d", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08-05.jsonl")); err != nil {
		t.Fatalf("next-day file: %v", err)
	}
}
