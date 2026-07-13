package persona

import (
	"fmt"
	"strings"
)

func RenderSystem(p Persona) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(p.SystemPrompt))

	if len(p.StyleRules) > 0 {
		b.WriteString("\n\nStyle Rules:\n")
		for _, rule := range p.StyleRules {
			b.WriteString("- ")
			b.WriteString(rule)
			b.WriteString("\n")
		}
	}

	b.WriteString("\nResponse Policy:\n")
	b.WriteString(fmt.Sprintf("- format: %s\n", p.ResponsePolicy.Format))
	b.WriteString(fmt.Sprintf("- verbosity: %s\n", p.ResponsePolicy.Verbosity))

	return strings.TrimSpace(b.String())
}
