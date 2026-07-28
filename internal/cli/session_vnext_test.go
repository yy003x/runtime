package cli

import (
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/runtime/command"
	runtimemodel "github.com/yy003x/runtime/model"
	runtimeprofile "github.com/yy003x/runtime/profile"
)

func TestCarrierSendInputDoesNotTreatOptionValueAsInput(t *testing.T) {
	if _, err := carrierSendInput([]string{"--session-id", "session_deadbeef"}); err == nil {
		t.Fatal("accepted missing send input")
	}
	value, err := carrierSendInput([]string{
		"--session-id", "session_deadbeef", "继续",
	})
	if err != nil || value != "继续" {
		t.Fatalf("value=%q error=%v", value, err)
	}
	if _, err := carrierSendInput([]string{
		"--session-id", "session_deadbeef", "one", "two",
	}); err == nil {
		t.Fatal("accepted more than one positional input")
	}
}

func TestSessionTokenLimitFlagMatchesModelDriver(t *testing.T) {
	openAIInvocation, err := parseSessionInvocation([]string{
		"--max-completion-tokens", "128", "api-cx", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	openAIProfiles := sessionTestProfiles(t, runtimemodel.Profile{
		Driver:   runtimemodel.DriverOpenAICompatible,
		Endpoint: "https://example.invalid/v1/chat/completions", Model: "fixture",
		Auth: runtimemodel.Auth{
			Header: "Authorization", Scheme: "Bearer", FromEnv: "MODEL_API_KEY",
		},
		Timeout: "1m",
	})
	if err := validateSessionProfileOptions(
		openAIInvocation, openAIProfiles,
	); err != nil {
		t.Fatal(err)
	}

	invalidInvocation, err := parseSessionInvocation([]string{
		"--max-tokens", "128", "api-cx", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSessionProfileOptions(
		invalidInvocation, openAIProfiles,
	); err == nil || !strings.Contains(err.Error(), "--max-completion-tokens") {
		t.Fatalf("error=%v", err)
	}
}

func sessionTestProfiles(
	t *testing.T,
	apiProfile runtimemodel.Profile,
) *runtimeprofile.Catalog {
	t.Helper()
	commands, err := runtimecommand.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := runtimemodel.NewCatalog(
		map[string]runtimemodel.Profile{"api-cx": apiProfile},
	)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := runtimeprofile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}
