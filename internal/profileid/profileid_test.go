package profileid

import "testing"

func TestValidate(t *testing.T) {
	for _, value := range []string{"cx", "cc-batch", "model_1", "vendor.model"} {
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
