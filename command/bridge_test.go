package command

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareInvocationPreservesFixedAndUserArgumentBoundaries(t *testing.T) {
	binary := "/bin/echo"
	if runtime.GOOS == "windows" {
		binary = filepath.Join(t.TempDir(), "echo.exe")
		t.Skip("command bridge is only supported on Darwin and Linux")
	}
	profile := Profile{
		Binary: binary, Args: []string{"fixed", "${PROFILE_VALUE}", "--"},
		Transport: TransportTTY, PromptDelivery: PromptArgv,
	}
	resolved, err := prepareInvocation(
		profile,
		[]string{"${PROFILE_VALUE}", "user value"},
		[]string{"PROFILE_VALUE=expanded", "KEEP=value"},
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{binary, "fixed", "expanded", "--", "${PROFILE_VALUE}", "user value"}
	if !reflect.DeepEqual(resolved.Argv, expected) {
		t.Fatalf("argv=%q want=%q", resolved.Argv, expected)
	}
}

func TestPrepareInvocationAppliesEnvironmentSetAndUnset(t *testing.T) {
	profile := Profile{
		Binary: "/bin/echo", Transport: TransportTTY, PromptDelivery: PromptManual,
		Env: map[string]*string{
			"SET":    stringPointer("prefix-${SOURCE}"),
			"REMOVE": nil,
		},
	}
	resolved, err := prepareInvocation(
		profile,
		nil,
		[]string{"SOURCE=value", "REMOVE=inherited", "KEEP=yes"},
	)
	if err != nil {
		t.Fatal(err)
	}
	environment := environmentMap(resolved.Environment)
	if environment["SET"] != "prefix-value" || environment["KEEP"] != "yes" {
		t.Fatalf("environment=%#v", environment)
	}
	if _, exists := environment["REMOVE"]; exists {
		t.Fatalf("REMOVE reached target: %#v", environment)
	}
}

func TestPrepareInvocationFailsBeforeExecWhenReferenceIsMissing(t *testing.T) {
	profile := Profile{
		Binary: "/bin/echo", Args: []string{"${MISSING}"},
		Transport: TransportTTY, PromptDelivery: PromptArgv,
	}
	_, err := prepareInvocation(profile, nil, []string{"HOME=/tmp"})
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("error=%v", err)
	}
}

func stringPointer(value string) *string {
	return &value
}
