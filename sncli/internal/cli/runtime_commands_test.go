package cli

import (
	"reflect"
	"testing"

	"agent-arch/internal/agentrun"
)

func TestParseRunOptionsMergesTypedOverrides(t *testing.T) {
	options, err := parseRunOptions(agentrun.RunTask, []string{
		"--model", "first", "--image", "one.png", "--provider-overrides", `{"model":"final","verbosity":"high"}`, "prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Prompt != "prompt" || options.ProviderOverrides["model"] != "final" || options.ProviderOverrides["verbosity"] != "high" {
		t.Fatalf("options=%#v", options)
	}
	if !reflect.DeepEqual(options.ProviderOverrides["images"], []string{"one.png"}) {
		t.Fatalf("images=%#v", options.ProviderOverrides["images"])
	}
}

func TestParseCommandOptionsPreservesRemainder(t *testing.T) {
	options, err := parseCommandOptions([]string{"--profile", "tcx", "--label", "smoke", "--", "printf", "%s", "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "tcx" || options.Label != "smoke" || !reflect.DeepEqual(options.Argv, []string{"printf", "%s", "hello world"}) {
		t.Fatalf("options=%#v", options)
	}
}

func TestParseLoopOptionsSupportsContractFields(t *testing.T) {
	options, err := parseLoopOptions([]string{"--input", "work", "--actions-json", `[{"type":"respond","content":"ok"}]`, "--result-schema", "custom.yaml", "--deadline-seconds", "9"})
	if err != nil {
		t.Fatal(err)
	}
	if options.ResultSchema != "custom.yaml" || options.DeadlineSeconds != 9 || len(options.Actions) != 1 {
		t.Fatalf("options=%#v", options)
	}
}
