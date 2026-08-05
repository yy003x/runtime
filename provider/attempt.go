// Package provider defines the HTTP wire evidence produced by one Provider
// call. Drivers return this evidence to the model service; they do not persist
// it themselves.
package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Attempt is the HTTP evidence for one Driver call. Started is true only once
// http.Client.Do has been invoked; validation and encoding failures before that
// point are not Provider interactions.
type Attempt struct {
	Started  bool      `json:"-"`
	Request  Request   `json:"request"`
	Response *Response `json:"response"`
}

type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Data    []json.RawMessage `json:"data"`
}

// SingleAttemptClient clones client and disables automatic redirects so one
// driver Stream call cannot silently produce multiple HTTP requests.
func SingleAttemptClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

// SafeHeaders returns a readable HTTP header map while redacting literal
// credential-bearing values. Callers may overwrite entries with Profile
// ${VAR} references after this function returns.
func SafeHeaders(header http.Header) map[string]string {
	values := make(map[string]string, len(header))
	for name, items := range header {
		value := strings.Join(items, ", ")
		if SensitiveHeader(name) {
			value = "[REDACTED]"
		} else if strings.EqualFold(name, "Location") ||
			strings.EqualFold(name, "Content-Location") {
			value = SafeURL(value)
		} else if strings.EqualFold(name, "Link") {
			value = "[REDACTED]"
		}
		values[http.CanonicalHeaderKey(name)] = value
	}
	return values
}

// SafeNetworkError preserves transport semantics while removing query values
// from the URL that net/http includes in *url.Error messages.
func SafeNetworkError(err error) error {
	if err == nil {
		return nil
	}
	var urlError *url.Error
	if !errors.As(err, &urlError) {
		return err
	}
	safe := *urlError
	safe.URL = SafeURL(urlError.URL)
	return &safe
}

func RequestHeaders(
	header http.Header,
	profileReferences map[string]string,
) map[string]string {
	values := SafeHeaders(header)
	for name, value := range profileReferences {
		values[http.CanonicalHeaderKey(name)] = value
	}
	return values
}

// SafeURL preserves non-sensitive request parameters while redacting values
// whose query names conventionally carry credentials.
func SafeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.RawQuery == "" {
		return raw
	}
	parts := strings.Split(parsed.RawQuery, "&")
	for index, part := range parts {
		rawName, _, _ := strings.Cut(part, "=")
		name, decodeErr := url.QueryUnescape(rawName)
		if decodeErr != nil || !SensitiveQueryParameter(name) {
			continue
		}
		parts[index] = rawName + "=" + url.QueryEscape("[REDACTED]")
	}
	parsed.RawQuery = strings.Join(parts, "&")
	return parsed.String()
}

func SensitiveQueryParameter(name string) bool {
	value := strings.ToLower(strings.TrimSpace(name))
	compact := strings.NewReplacer("-", "", "_", "", ".", "").Replace(value)
	return compact == "key" || compact == "sig" ||
		strings.Contains(compact, "apikey") ||
		strings.Contains(compact, "accesskey") ||
		strings.Contains(compact, "auth") ||
		strings.Contains(compact, "token") ||
		strings.Contains(compact, "secret") ||
		strings.Contains(compact, "signature") ||
		strings.Contains(compact, "credential") ||
		strings.Contains(compact, "password")
}

// ResponseData converts the consumed HTTP body into the diagnostic JSON
// collection. SSE [DONE] is a control marker rather than JSON and is omitted.
func ResponseData(body []byte, contentType string) []json.RawMessage {
	values := make([]json.RawMessage, 0)
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) || !json.Valid(data) {
				continue
			}
			values = append(values, append(json.RawMessage(nil), data...))
		}
		return values
	}
	data := bytes.TrimSpace(body)
	if json.Valid(data) {
		values = append(values, append(json.RawMessage(nil), data...))
	}
	return values
}

func SensitiveHeader(name string) bool {
	value := strings.ToLower(strings.TrimSpace(name))
	compact := strings.NewReplacer("-", "", "_", "").Replace(value)
	return value == "authorization" || value == "proxy-authorization" ||
		value == "cookie" || value == "set-cookie" ||
		value == "authentication-info" || value == "www-authenticate" ||
		strings.Contains(compact, "apikey") ||
		strings.Contains(value, "token") || strings.Contains(value, "secret")
}
