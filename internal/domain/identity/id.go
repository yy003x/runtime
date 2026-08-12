package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

var valuePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*_[0-9a-f]{32}$`)

func New(prefix string) (string, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(prefix) {
		return "", fmt.Errorf("invalid ID prefix %q", prefix)
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(entropy[:]), nil
}

func Validate(value, prefix string) error {
	if !valuePattern.MatchString(value) {
		return fmt.Errorf("invalid %s ID %q", prefix, value)
	}
	want := prefix + "_"
	if len(value) <= len(want) || value[:len(want)] != want {
		return fmt.Errorf("invalid %s ID %q", prefix, value)
	}
	return nil
}
