package token

import "strings"

type Counter interface {
	CountText(text string) int
}

type ApproxCounter struct{}

func NewApproxCounter() ApproxCounter {
	return ApproxCounter{}
}

func (ApproxCounter) CountText(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}

	runes := len([]rune(text))
	tokens := runes / 4
	if runes%4 != 0 {
		tokens++
	}
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}
