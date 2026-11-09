package modelruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/sn-code/internal/domain/identity"
	"github.com/yy003x/sn-code/internal/infrastructure/executionlog"
	"github.com/yy003x/sn-code/pkg/model"
	"github.com/yy003x/sn-code/pkg/provider"
)

func TestExecutionCallObserverWritesAPIRecordBestEffort(t *testing.T) {
	logsDir := filepath.Join(t.TempDir(), "logs")
	observer := executionCallObserver(logsDir)
	observer(model.CallObservation{
		Origin: model.AttemptOrigin{
			Namespace: model.AttemptNamespaceAgent, Source: "agent run_fixture",
		},
		ProfileID: "api-cc",
		Attempts: []model.Attempt{{Wire: provider.Attempt{
			Started: true,
			Request: provider.Request{
				Method: "POST", URL: "https://example.invalid/v1/messages",
				Headers: map[string]string{"X-Api-Key": "${MODEL_API_KEY}"},
				Body:    json.RawMessage(`{"model":"fixture"}`),
			},
			Response: &provider.Response{Status: 200, Data: []json.RawMessage{}},
		}}},
	})
	paths, err := filepath.Glob(filepath.Join(logsDir, "*", "api.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("paths=%v error=%v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Namespace string                    `json:"namespace"`
		Profile   string                    `json:"profile"`
		Source    string                    `json:"source"`
		CallID    string                    `json:"call_id"`
		Attempts  []executionlog.APIAttempt `json:"attempts"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Namespace != "agent" || record.Profile != "api-cc" ||
		record.Source != "agent run_fixture" || identity.Validate(record.CallID, "call") != nil ||
		len(record.Attempts) != 1 || record.Attempts[0].Attempt != 1 {
		t.Fatalf("record=%#v", record)
	}

	broken := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(broken, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The observer is diagnostic-only: an unwritable target must be swallowed.
	executionCallObserver(broken)(model.CallObservation{
		Origin:    model.AttemptOrigin{Namespace: model.AttemptNamespaceRequest},
		ProfileID: "api", Attempts: []model.Attempt{{Wire: provider.Attempt{Started: true}}},
	})
	content, err := os.ReadFile(broken)
	if err != nil || strings.TrimSpace(string(content)) != "not a directory" {
		t.Fatalf("broken target changed: content=%q error=%v", content, err)
	}
}
