// Package scenario defines the test-side canonical fixtures used to compare
// future SN Runtime model, CLI, and HTTP implementations. These types are
// deliberately not a production contract.
package scenario

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const SchemaVersion = 1

type Set struct {
	SchemaVersion int       `json:"schema_version"`
	Scenarios     []Fixture `json:"scenarios"`
}

type Fixture struct {
	Name    string        `json:"name"`
	Request ModelRequest  `json:"request"`
	Events  []Event       `json:"events"`
	Result  *ModelResult  `json:"result,omitempty"`
	Error   *RuntimeError `json:"error,omitempty"`
}

type ModelRequest struct {
	System   string          `json:"system,omitempty"`
	Messages []Message       `json:"messages"`
	Tools    []ToolSpec      `json:"tools,omitempty"`
	Options  GenerateOptions `json:"options,omitempty"`
	Trace    TraceContext    `json:"trace,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type GenerateOptions struct {
	MaxOutputTokens int64   `json:"max_output_tokens,omitempty"`
	Temperature     float64 `json:"temperature,omitempty"`
}

type TraceContext struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type Event struct {
	Sequence   uint64        `json:"sequence"`
	Time       string        `json:"time,omitempty"`
	Type       string        `json:"type"`
	RunID      string        `json:"run_id,omitempty"`
	RequestID  string        `json:"request_id,omitempty"`
	Text       string        `json:"text,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCall   *ToolCall     `json:"tool_call,omitempty"`
	Result     *ModelResult  `json:"result,omitempty"`
	Error      *RuntimeError `json:"error,omitempty"`
}

type ModelResult struct {
	Message      Message      `json:"message"`
	FinishReason string       `json:"finish_reason"`
	Usage        Usage        `json:"usage,omitempty"`
	Provider     ProviderInfo `json:"provider,omitempty"`
}

type Usage struct {
	InputTokens     *int64 `json:"input_tokens,omitempty"`
	OutputTokens    *int64 `json:"output_tokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
	CacheReadTokens *int64 `json:"cache_read_tokens,omitempty"`
	TotalTokens     *int64 `json:"total_tokens,omitempty"`
	Source          string `json:"source,omitempty"`
	Completeness    string `json:"completeness,omitempty"`
}

type ProviderInfo struct {
	Name      string `json:"name,omitempty"`
	Model     string `json:"model,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type RuntimeError struct {
	Code         string `json:"code"`
	Phase        string `json:"phase"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Provider     string `json:"provider,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

func LoadFile(path string) (Set, error) {
	file, err := os.Open(path)
	if err != nil {
		return Set{}, err
	}
	defer file.Close()
	return Load(file)
}

func Load(reader io.Reader) (Set, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 4<<20))
	decoder.DisallowUnknownFields()
	var set Set
	if err := decoder.Decode(&set); err != nil {
		return Set{}, err
	}
	if err := ensureEOF(decoder); err != nil {
		return Set{}, err
	}
	if err := set.Validate(); err != nil {
		return Set{}, err
	}
	return set, nil
}

func (s Set) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("scenario schema_version=%d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if len(s.Scenarios) == 0 {
		return fmt.Errorf("scenario set is empty")
	}
	names := map[string]struct{}{}
	for _, fixture := range s.Scenarios {
		if fixture.Name == "" {
			return fmt.Errorf("scenario name is required")
		}
		if _, exists := names[fixture.Name]; exists {
			return fmt.Errorf("duplicate scenario %q", fixture.Name)
		}
		names[fixture.Name] = struct{}{}
		if len(fixture.Request.Messages) == 0 {
			return fmt.Errorf("scenario %q has no messages", fixture.Name)
		}
		if (fixture.Result == nil) == (fixture.Error == nil) {
			return fmt.Errorf("scenario %q must define exactly one result or error", fixture.Name)
		}
		if err := ValidateEvents(fixture.Events); err != nil {
			return fmt.Errorf("scenario %q: %w", fixture.Name, err)
		}
	}
	return nil
}

func ValidateEvents(events []Event) error {
	if len(events) == 0 {
		return fmt.Errorf("event sequence is empty")
	}
	settled := false
	for index, event := range events {
		expected := uint64(index + 1)
		if event.Sequence != expected {
			return fmt.Errorf("event[%d] sequence=%d, want %d", index, event.Sequence, expected)
		}
		if event.Type == "" {
			return fmt.Errorf("event[%d] type is empty", index)
		}
		if settled {
			return fmt.Errorf("event[%d] follows run.settled", index)
		}
		if event.Type == "run.settled" {
			settled = true
		}
	}
	return nil
}

// Normalize removes values that legitimately vary between transports or runs
// while preserving semantic payloads and sequence numbers.
func Normalize(events []Event) ([]Event, error) {
	data, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	var normalized []Event
	if err := json.Unmarshal(data, &normalized); err != nil {
		return nil, err
	}
	for index := range normalized {
		normalized[index].Time = ""
		normalized[index].RunID = ""
		normalized[index].RequestID = ""
		if normalized[index].Result != nil {
			normalized[index].Result.Provider.RequestID = ""
		}
		if normalized[index].Error != nil {
			normalized[index].Error.RequestID = ""
		}
	}
	return normalized, nil
}

func (s Set) ByName(name string) (Fixture, bool) {
	for _, fixture := range s.Scenarios {
		if fixture.Name == name {
			return fixture, true
		}
	}
	return Fixture{}, false
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("scenario input contains multiple JSON values")
		}
		return err
	}
	return nil
}
