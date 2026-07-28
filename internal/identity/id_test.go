package identity

import "testing"

func TestNewAndValidate(t *testing.T) {
	value, err := New("session")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(value, "session"); err != nil {
		t.Fatal(err)
	}
	if err := Validate(value, "turn"); err == nil {
		t.Fatal("session ID was accepted as a turn ID")
	}
}
