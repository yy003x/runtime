package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestControlAuditIntentNeverRetainsInput(t *testing.T) {
	intent, ok := controlAuditIntentFromArgs([]string{
		"session", "send", "--session-id",
		"session_11111111111111111111111111111111",
		"private prompt",
	})
	if !ok || intent.namespace != "session" || intent.action != "send" ||
		intent.targets["session_id"] == "" {
		t.Fatalf("intent=%#v ok=%t", intent, ok)
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private prompt") {
		t.Fatalf("audit intent retained input: %s", data)
	}
	if _, ok := controlAuditIntentFromArgs([]string{"session", "list"}); ok {
		t.Fatal("read-only list was selected for control audit")
	}
	agent, ok := controlAuditIntentFromArgs([]string{
		"agent", "api-cx", "--queue", "private agent prompt",
	})
	if !ok || agent.namespace != "agent" || agent.action != "run" ||
		agent.targets["profile_id"] != "api-cx" {
		t.Fatalf("agent intent=%#v ok=%t", agent, ok)
	}
	agentData, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(agentData), "private agent prompt") {
		t.Fatalf("agent audit retained input: %s", agentData)
	}
	invalid, ok := controlAuditIntentFromArgs([]string{
		"session", "close", "--session-id", "private-secret",
	})
	if !ok || len(invalid.targets) != 0 {
		t.Fatalf("invalid target was retained: %#v ok=%t", invalid, ok)
	}
}

func TestAppendControlAuditWritesCanonicalFailureIdentity(t *testing.T) {
	root := t.TempDir()
	appendControlAudit(
		root,
		[]string{"tmux", "stop", "--tmux-id", "fixture"},
		&contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseTransport,
			Message: "sensitive detail is not persisted",
		},
	)
	day := time.Now().Format("060102")
	data, err := os.ReadFile(filepath.Join(root, day, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sensitive detail") ||
		!strings.Contains(string(data), `"error_code":"conflict"`) ||
		!strings.Contains(string(data), `"error_phase":"transport"`) {
		t.Fatalf("audit=%s", data)
	}
}
