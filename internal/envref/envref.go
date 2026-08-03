// Package envref expands ${VAR} environment references in profile strings.
// Both the command and model domains resolve the same ${VAR_NAME} grammar, so
// the expansion logic lives here once.
package envref

import (
	"fmt"
	"strings"
)

// Expand replaces every ${VAR} reference in value using lookup. References
// must use the form ${VAR_NAME}; malformed references and unset variables
// produce an error.
func Expand(value string, lookup func(string) (string, bool)) (string, error) {
	var resolved strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			resolved.WriteString(value)
			return resolved.String(), nil
		}
		resolved.WriteString(value[:start])
		remainder := value[start+2:]
		end := strings.IndexByte(remainder, '}')
		if end < 0 {
			return "", fmt.Errorf("environment reference is missing }")
		}
		name := remainder[:end]
		if !ValidName(name) {
			return "", fmt.Errorf("invalid environment reference; only ${VAR_NAME} is supported")
		}
		replacement, exists := lookup(name)
		if !exists {
			return "", fmt.Errorf("environment variable is not set: %s", name)
		}
		resolved.WriteString(replacement)
		value = remainder[end+1:]
	}
}

// ValidName reports whether name is a legal ${VAR_NAME} reference identifier:
// an ASCII letter or underscore followed by letters, digits, or underscores.
func ValidName(name string) bool {
	if name == "" || !isAlpha(name[0]) && name[0] != '_' {
		return false
	}
	for index := 1; index < len(name); index++ {
		current := name[index]
		if !isAlpha(current) && (current < '0' || current > '9') && current != '_' {
			return false
		}
	}
	return true
}

// References returns the names of every ${VAR} reference in value, in order of
// appearance. Malformed references are skipped.
func References(value string) []string {
	var names []string
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			return names
		}
		remainder := value[start+2:]
		end := strings.IndexByte(remainder, '}')
		if end < 0 {
			return names
		}
		name := remainder[:end]
		if ValidName(name) {
			names = append(names, name)
		}
		value = remainder[end+1:]
	}
}

func isAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
