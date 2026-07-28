package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeIsBoundedAndStrict(t *testing.T) {
	var target struct {
		Name string `json:"name"`
	}
	if err := Decode(strings.NewReader(`{"name":"ok"}`), 64, &target); err != nil {
		t.Fatal(err)
	}
	if target.Name != "ok" {
		t.Fatalf("name=%q", target.Name)
	}
	for _, input := range []string{
		`{"name":"ok","extra":true}`,
		`{"name":"one"} {"name":"two"}`,
	} {
		if err := Decode(strings.NewReader(input), 64, &target); err == nil {
			t.Fatalf("Decode(%q) returned nil", input)
		}
	}
	if err := Decode(strings.NewReader(`{"name":"too large"}`), 4, &target); err == nil {
		t.Fatal("oversized JSON was accepted")
	}
}
