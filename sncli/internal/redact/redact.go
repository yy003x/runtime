package redact

import "regexp"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(token|secret|password|cookie|private_key)=([^\s]+)`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`),
}

func Text(input string) string {
	out := input
	for _, pattern := range patterns {
		out = pattern.ReplaceAllString(out, "${1}****")
	}
	return out
}
