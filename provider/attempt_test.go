package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestResponseDataCollectsJSONFramesInOrder(t *testing.T) {
	body := []byte("event: ping\n" +
		"data: {\"type\":\"one\"}\n\n" +
		"data: not-json\n" +
		"data: {\"type\":\"two\"}\n" +
		"data: [DONE]\n")
	got := ResponseData(body, "text/event-stream; charset=utf-8")
	if len(got) != 2 || string(got[0]) != `{"type":"one"}` ||
		string(got[1]) != `{"type":"two"}` {
		t.Fatalf("data=%q", got)
	}
	got = ResponseData([]byte(` {"ok":true} `), "application/json")
	if len(got) != 1 || string(got[0]) != `{"ok":true}` {
		t.Fatalf("non-stream data=%q", got)
	}
	if got := ResponseData([]byte("not-json"), "text/plain"); len(got) != 0 {
		t.Fatalf("plain data=%q", got)
	}
}

func TestRequestHeadersPreserveReferencesAndRedactLiterals(t *testing.T) {
	header := http.Header{
		"Authorization": []string{"Bearer plaintext"},
		"X-Api-Key":     []string{"plaintext-key"},
		"Content-Type":  []string{"application/json"},
		"X-Trace":       []string{"one", "two"},
	}
	got := RequestHeaders(header, map[string]string{
		"Authorization": "Bearer ${MODEL_API_KEY}",
	})
	want := map[string]string{
		"Authorization": "Bearer ${MODEL_API_KEY}",
		"X-Api-Key":     "[REDACTED]",
		"Content-Type":  "application/json",
		"X-Trace":       "one, two",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("headers=%#v want=%#v", got, want)
	}
}

func TestSafeURLRedactsSensitiveQueryValues(t *testing.T) {
	got := SafeURL("https://example.invalid/v1?api_key=secret&region=cn")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/v1" || parsed.Query().Get("api_key") != "[REDACTED]" ||
		parsed.Query().Get("region") != "cn" {
		t.Fatalf("url=%q", got)
	}
}

func TestSafeNetworkErrorRedactsURLAndPreservesCause(t *testing.T) {
	cause := errors.New("connection failed")
	err := &url.Error{
		Op: "Post", URL: "https://example.invalid/v1?api_key=secret", Err: cause,
	}
	safe := SafeNetworkError(err)
	if !errors.Is(safe, cause) || strings.Contains(safe.Error(), "secret") ||
		!strings.Contains(safe.Error(), "%5BREDACTED%5D") {
		t.Fatalf("safe error=%v", safe)
	}
}

func TestAttemptJSONContainsHTTPShapeOnly(t *testing.T) {
	value := Attempt{
		Started: true,
		Request: Request{
			Method: "POST", URL: "https://example.invalid",
			Headers: map[string]string{}, Body: json.RawMessage(`{}`),
		},
		Response: &Response{Status: 200, Data: []json.RawMessage{}},
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"request":{"method":"POST","url":"https://example.invalid","headers":{},"body":{}},"response":{"status":200,"headers":null,"data":[]}}` {
		t.Fatalf("attempt=%s", data)
	}
}

func TestSingleAttemptClientDoesNotMutateInput(t *testing.T) {
	originalRedirect := func(*http.Request, []*http.Request) error { return nil }
	original := &http.Client{CheckRedirect: originalRedirect}
	got := SingleAttemptClient(original)
	if got == original || got.CheckRedirect == nil || original.CheckRedirect == nil {
		t.Fatalf("original=%p clone=%p", original, got)
	}
	request := &http.Request{}
	if err := got.CheckRedirect(request, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error=%v", err)
	}
	if err := original.CheckRedirect(request, nil); err != nil {
		t.Fatalf("original redirect policy changed: %v", err)
	}
}
