package openairesponses

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	httpx "github.com/yy003x/runtime/internal/infrastructure/providerhttp"
	"github.com/yy003x/runtime/pkg/contract"
)

// decodeStream requires a terminal Response, not EOF or [DONE]. Intermediate
// events are wire evidence; only the terminal response.output is projected.
func decodeStream(reader io.Reader) (responseEnvelope, *contract.RuntimeError) {
	fail := func(message string) (responseEnvelope, *contract.RuntimeError) {
		return responseEnvelope{}, httpx.ProtocolError(providerName, message)
	}
	limited := &io.LimitedReader{R: reader, N: httpx.MaxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), int(httpx.MaxResponseBytes))
	var data []string
	eventName := ""
	for scanner.Scan() {
		if limited.N == 0 {
			return fail("Responses SSE exceeds response byte limit")
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line != "" {
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				eventName = value
			case "data":
				data = append(data, value)
			}
			continue
		}
		if len(data) == 0 {
			eventName = ""
			continue
		}
		payload := strings.Join(data, "\n")
		data = nil
		if strings.TrimSpace(payload) == "[DONE]" {
			return fail("Responses SSE ended without a terminal response")
		}
		var event struct {
			Type     string            `json:"type"`
			Response *responseEnvelope `json:"response"`
			Code     string            `json:"code"`
			Message  string            `json:"message"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fail("decode Responses SSE: " + err.Error())
		}
		if event.Type == "" || (eventName != "" && eventName != event.Type) {
			return fail("Responses SSE event type is missing or inconsistent")
		}
		eventName = ""
		if event.Type == "error" {
			return responseEnvelope{}, (responseError{Code: event.Code, Message: event.Message}).runtimeError()
		}
		switch event.Type {
		case "response.completed", "response.incomplete", "response.failed":
			if event.Response == nil || event.Response.Status != strings.TrimPrefix(event.Type, "response.") {
				return fail("Responses SSE terminal event and response status disagree")
			}
			return *event.Response, nil
		default:
			if !strings.HasPrefix(event.Type, "response.") {
				return fail("unsupported Responses SSE event: " + event.Type)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fail("read Responses SSE: " + err.Error())
	}
	return fail("Responses SSE ended without a terminal response")
}
