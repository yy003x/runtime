package command

import (
	"reflect"
	"strings"
	"testing"
)

func TestPromptByteLimitIs128000(t *testing.T) {
	if MaxTokenBytes != 128_000 {
		t.Fatalf("MaxTokenBytes=%d want=128000", MaxTokenBytes)
	}
	if _, err := ReadPrompt(strings.NewReader(strings.Repeat("x", MaxTokenBytes))); err != nil {
		t.Fatalf("exact prompt limit rejected: %v", err)
	}
	if _, err := ReadPrompt(strings.NewReader(strings.Repeat("x", MaxTokenBytes+1))); err == nil {
		t.Fatal("oversized prompt was accepted")
	}
}

func TestProfileUsesTypedCommandProtocol(t *testing.T) {
	profile := Profile{
		Command: "codex",
		Args:    []string{"--image", "${IMAGE}"},
		Env: map[string]*string{
			"CODEX_HOME": stringPointer("${HOME}/.codex-aip"),
			"REMOVE_ME":  nil,
		},
		Model: "gpt-5.6-sol", Effort: EffortHigh,
		Prompt: "base", Exec: true, CWD: "${HOME}/work",
	}
	err := CheckProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Command != "codex" || profile.Model != "gpt-5.6-sol" ||
		profile.Effort != EffortHigh || !profile.Exec ||
		profile.Env["REMOVE_ME"] != nil {
		t.Fatalf("profile=%#v", profile)
	}
	for _, invalid := range []Profile{
		{},
		{Command: "unknown"},
		{Command: "codex/"},
		{Command: "codex/."},
		{Command: "codex", Args: []string{"${BAD-NAME}"}},
		{Command: "codex", Env: map[string]*string{"BAD=NAME": stringPointer("value")}},
		{Command: "codex", Effort: Effort("extreme")},
		{Command: "codex", Effort: Effort(" high ")},
	} {
		if err := CheckProfile(invalid); err == nil {
			t.Fatalf("CheckProfile(%#v) returned nil", invalid)
		}
	}
}

func TestCheckProfileIsSymbolic(t *testing.T) {
	err := CheckProfile(Profile{
		Command: "/not-installed/codex",
		Args:    []string{"--image", "${RUNTIME_IMAGE}"},
		Env:     map[string]*string{"SECRET": stringPointer("${MISSING_SECRET}")},
		CWD:     "${MISSING_CWD}/workspace",
		Prompt:  "missing-prompt-file.md",
	})
	if err != nil {
		t.Fatalf("symbolic check resolved runtime dependencies: %v", err)
	}
}

func TestCatalogReturnsDefensiveTypedProfileCopies(t *testing.T) {
	catalog, err := NewCatalog(map[string]Profile{
		"cx": {
			Command: "codex",
			Args:    []string{"--sandbox", "read-only"},
			Model:   "gpt-5.6-sol",
			Effort:  EffortHigh,
			Exec:    true,
			Env:     map[string]*string{"SET": stringPointer("value")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, exists := catalog.Get("cx")
	if !exists {
		t.Fatal("profile missing")
	}
	first.Args[0] = "changed"
	*first.Env["SET"] = "changed"
	second, _ := catalog.Get("cx")
	if !reflect.DeepEqual(second.Args, []string{"--sandbox", "read-only"}) ||
		*second.Env["SET"] != "value" || !second.Exec {
		t.Fatalf("profile=%#v", second)
	}
}

func stringPointer(value string) *string { return &value }
