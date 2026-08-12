package scenario

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestScenarioFixturesRoundTripAndNormalizeIdempotently(t *testing.T) {
	set, err := LoadFile(filepath.Join(testdataDir(t), "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Scenarios) != 3 {
		t.Fatalf("scenarios=%d", len(set.Scenarios))
	}
	for _, fixture := range set.Scenarios {
		normalized, err := Normalize(fixture.Events)
		if err != nil {
			t.Fatalf("%s normalize: %v", fixture.Name, err)
		}
		again, err := Normalize(normalized)
		if err != nil {
			t.Fatalf("%s renormalize: %v", fixture.Name, err)
		}
		if !reflect.DeepEqual(normalized, again) {
			t.Fatalf("%s normalization is not idempotent", fixture.Name)
		}
		for _, event := range normalized {
			if event.Time != "" || event.RunID != "" || event.RequestID != "" {
				t.Fatalf("%s retained nondeterministic event fields: %#v", fixture.Name, event)
			}
			if event.Result != nil && event.Result.Provider.RequestID != "" {
				t.Fatalf("%s retained provider request id", fixture.Name)
			}
			if event.Error != nil && event.Error.RequestID != "" {
				t.Fatalf("%s retained error request id", fixture.Name)
			}
		}
	}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Load(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	decodedData, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, decodedData) {
		t.Fatal("scenario JSON round trip changed the fixture")
	}
}

func TestScenarioLoaderRejectsOutOfOrderSequence(t *testing.T) {
	_, err := LoadFile(filepath.Join(testdataDir(t), "invalid-sequence.json"))
	if err == nil || !strings.Contains(err.Error(), "sequence=3, want 2") {
		t.Fatalf("err=%v", err)
	}
}

func TestScenarioLoaderIsStrictAndRequiresOneOutcome(t *testing.T) {
	for _, input := range []string{
		`{"schema_version":1,"scenarios":[{"name":"x","request":{"messages":[{"role":"user"}]},"events":[{"sequence":1,"type":"model.started"}],"result":{"message":{"role":"assistant"},"finish_reason":"stop"},"extra":true}]}`,
		`{"schema_version":1,"scenarios":[{"name":"x","request":{"messages":[{"role":"user"}]},"events":[{"sequence":1,"type":"model.started"}]}]}`,
	} {
		if _, err := Load(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid fixture accepted: %s", input)
		}
	}
}

func testdataDir(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(currentFile), "testdata")
}
