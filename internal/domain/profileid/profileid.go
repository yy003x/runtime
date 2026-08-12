// Package profileid validates command and model profile identifiers without
// coupling their independent document schemas.
package profileid

import "fmt"

const maxLength = 128

var reservedNamespaces = [...]string{
	"exec", "req", "profile", "session", "tmux", "agent", "run", "server", "help", "version",
}

// ReservedNamespaces returns the canonical public namespaces that every
// Runtime entrypoint must reject as Profile IDs.
func ReservedNamespaces() []string {
	return append([]string(nil), reservedNamespaces[:]...)
}

func Validate(value string) error {
	if value == "" {
		return fmt.Errorf("profile id is required")
	}
	if len(value) > maxLength {
		return fmt.Errorf("profile id exceeds %d bytes", maxLength)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("profile id %q is reserved", value)
	}
	for index := 0; index < len(value); index++ {
		current := value[index]
		if asciiAlphaNumeric(current) || current == '-' || current == '_' || (current == '.' && index > 0) {
			continue
		}
		return fmt.Errorf("profile id %q contains an invalid character", value)
	}
	if !asciiAlphaNumeric(value[0]) {
		return fmt.Errorf("profile id %q must start with a letter or digit", value)
	}
	return nil
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}
