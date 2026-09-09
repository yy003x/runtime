package model

import (
	"strings"
	"testing"
)

func TestServiceExecutionSnapshotIsCanonicalNonSecretAndCloned(
	t *testing.T,
) {
	maxTokens := int64(2048)
	temperature := 0.25
	topP := 0.9
	stream := false
	summaryEnabled := true
	profile := Profile{
		Driver:   DriverOpenAIChatCompletions,
		Endpoint: "https://example.invalid/v1/chat/completions",
		Model:    "snapshot-model",
		Headers: map[string]string{
			"Authorization": "${SNAPSHOT_MODEL_SECRET}",
			"X-Zeta":        "z", "X-Alpha": "a",
		},
		Parameters: Parameters{
			MaxTokens:     &maxTokens,
			Temperature:   &temperature,
			TopP:          &topP,
			StopSequences: []string{"END"},
			Stream:        &stream,
		},
		Timeout: "90s",
		Context: ContextPolicy{
			WindowTokens: 128000, ReservedOutputTokens: 8192,
			KeepRecentTurns: 4, SummaryEnabled: &summaryEnabled,
		},
	}
	catalog, err := NewCatalog(map[string]Profile{"snapshot": profile})
	if err != nil {
		t.Fatal(err)
	}
	getenvCalls := 0
	const secretValue = "must-not-enter-model-execution-snapshot"
	service, err := NewService(
		catalog,
		map[DriverName]Driver{
			DriverOpenAIChatCompletions: &testDriver{},
		},
		ServiceOptions{Getenv: func(string) (string, bool) {
			getenvCalls++
			return secretValue, true
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.ExecutionSnapshot("snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if getenvCalls != 0 {
		t.Fatalf("ExecutionSnapshot resolved header environment %d times", getenvCalls)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	firstJSON, err := first.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(firstJSON), secretValue) {
		t.Fatalf("snapshot exposed secret value: %s", firstJSON)
	}
	if !strings.Contains(string(firstJSON), "SNAPSHOT_MODEL_SECRET") {
		t.Fatalf("snapshot omitted header environment reference: %s", firstJSON)
	}

	first.Profile.Headers["X-Alpha"] = "mutated"
	*first.Profile.Parameters.MaxTokens = 1
	*first.Profile.Parameters.TopP = 0.1
	*first.Profile.Parameters.Stream = true
	first.Profile.Parameters.StopSequences[0] = "mutated"
	*first.Profile.Context.SummaryEnabled = false
	second, err := service.ExecutionSnapshot("snapshot")
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if second.Profile.Headers["X-Alpha"] != "a" ||
		*second.Profile.Parameters.MaxTokens != 2048 ||
		*second.Profile.Parameters.TopP != 0.9 ||
		*second.Profile.Parameters.Stream ||
		second.Profile.Parameters.StopSequences[0] != "END" ||
		!*second.Profile.Context.SummaryEnabled {
		t.Fatalf("snapshot shares mutable Profile storage: %#v", second.Profile)
	}
	if secondDigest != firstDigest ||
		string(secondJSON) != string(firstJSON) {
		t.Fatalf(
			"canonical snapshot changed after caller mutation:\nfirst=%s\nsecond=%s",
			firstJSON,
			secondJSON,
		)
	}
	if getenvCalls != 0 {
		t.Fatalf("ExecutionSnapshot resolved header environment %d times", getenvCalls)
	}
}

func TestExecutionSnapshotStrictlyRejectsTampering(t *testing.T) {
	service := newTestService(t, &testDriver{}, "secret")
	for name, mutate := range map[string]func(*ExecutionSnapshot){
		"schema": func(snapshot *ExecutionSnapshot) {
			snapshot.SchemaVersion++
		},
		"profile_id": func(snapshot *ExecutionSnapshot) {
			snapshot.ProfileID = "invalid/id"
		},
		"profile": func(snapshot *ExecutionSnapshot) {
			snapshot.Profile.Model = ""
		},
		"profile_digest": func(snapshot *ExecutionSnapshot) {
			snapshot.ProfileDigest = "sha256:forged"
		},
		"stream": func(snapshot *ExecutionSnapshot) {
			snapshot.Profile.Parameters.Stream = boolPointer(false)
		},
		"driver": func(snapshot *ExecutionSnapshot) {
			snapshot.DriverIdentity.Driver = DriverAnthropicMessages
		},
		"implementation": func(snapshot *ExecutionSnapshot) {
			snapshot.DriverIdentity.Implementation = " invalid "
		},
		"implementation_version": func(snapshot *ExecutionSnapshot) {
			snapshot.DriverIdentity.ImplementationVersion = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := service.ExecutionSnapshot("fixture")
			if err != nil {
				t.Fatal(err)
			}
			mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("tampered model execution snapshot was accepted")
			}
			if _, err := snapshot.CanonicalJSON(); err == nil {
				t.Fatal("tampered snapshot produced canonical JSON")
			}
		})
	}
}

func TestExecutionSnapshotStreamModeChangesDigest(t *testing.T) {
	profile := Profile{
		Driver: DriverOpenAIChatCompletions, BaseURL: "https://example.invalid", Model: "fixture", Timeout: "1m",
	}
	identity := (&testDriver{}).ExecutionIdentity()
	seenProfiles, seenSnapshots := make(map[string]bool), make(map[string]bool)
	for _, stream := range []*bool{nil, boolPointer(true), boolPointer(false)} {
		profile.Parameters.Stream = stream
		snapshot, err := newExecutionSnapshot("fixture", profile, identity)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := snapshot.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if seenProfiles[snapshot.ProfileDigest] || seenSnapshots[digest] {
			t.Fatal("stream change was not captured in execution identity")
		}
		seenProfiles[snapshot.ProfileDigest], seenSnapshots[digest] = true, true
	}
}

func TestNewServiceRequiresMatchingDriverExecutionIdentity(t *testing.T) {
	profile := Profile{
		Driver:   DriverOpenAIChatCompletions,
		Endpoint: "https://example.invalid/v1/chat/completions",
		Model:    "fixture",
		Headers:  map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Timeout:  "1m",
	}
	catalog, err := NewCatalog(map[string]Profile{"fixture": profile})
	if err != nil {
		t.Fatal(err)
	}
	for name, identity := range map[string]DriverExecutionIdentity{
		"wrong_driver": {
			Driver:                DriverAnthropicMessages,
			Implementation:        "runtime.model.test-driver",
			ImplementationVersion: 1,
		},
		"missing_implementation": {
			Driver:                DriverOpenAIChatCompletions,
			ImplementationVersion: 1,
		},
		"invalid_version": {
			Driver:         DriverOpenAIChatCompletions,
			Implementation: "runtime.model.test-driver",
		},
	} {
		t.Run(name, func(t *testing.T) {
			driver := &testDriver{identity: &identity}
			if _, err := NewService(
				catalog,
				map[DriverName]Driver{
					DriverOpenAIChatCompletions: driver,
				},
				ServiceOptions{},
			); err == nil {
				t.Fatal("invalid driver execution identity was accepted")
			}
		})
	}
}

func TestServiceExecutionSnapshotRejectsUnknownProfile(t *testing.T) {
	service := newTestService(t, &testDriver{}, "secret")
	if _, err := service.ExecutionSnapshot("missing"); err == nil {
		t.Fatal("unknown Profile produced an execution snapshot")
	}
}
