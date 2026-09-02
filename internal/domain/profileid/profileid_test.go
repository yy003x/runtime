package profileid

import (
	"reflect"
	"testing"
)

func TestReservedNamespacesAreCanonicalAndDefensivelyCopied(t *testing.T) {
	want := []string{
		"exec", "call", "profile", "session", "agent", "job", "server", "doctor", "help", "version", "update",
	}
	first := ReservedNamespaces()
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("namespaces=%v want=%v", first, want)
	}
	first[0] = "mutated"
	if got := ReservedNamespaces(); !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical namespaces were mutated: %v", got)
	}
}

func TestValidate(t *testing.T) {
	for _, value := range []string{
		"cx", "cc-batch", "model_1", "vendor.model", "tmux",
	} {
		if err := Validate(value); err != nil {
			t.Fatalf("Validate(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", ".", "..", "-bad", "bad/name", "bad name"} {
		if err := Validate(value); err == nil {
			t.Fatalf("Validate(%q) returned nil", value)
		}
	}
}
