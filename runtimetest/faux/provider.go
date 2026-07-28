// Package faux provides deterministic, test-only model and HTTP providers for
// Runtime vNext contract tests. It never imports or replaces a production
// Runtime provider.
package faux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yy003x/runtime/runtimetest/scenario"
)

const SchemaVersion = 1

type Set struct {
	SchemaVersion int            `json:"schema_version"`
	Scripts       []Script       `json:"scripts"`
	HTTP          []HTTPScenario `json:"http"`
}

type Script struct {
	Name    string                 `json:"name"`
	Steps   []Step                 `json:"steps"`
	Result  *scenario.ModelResult  `json:"result,omitempty"`
	Error   *scenario.RuntimeError `json:"error,omitempty"`
	Secrets []string               `json:"secrets,omitempty"`
}

type Step struct {
	DelayMS int64           `json:"delay_ms,omitempty"`
	Event   *scenario.Event `json:"event,omitempty"`
}

type HTTPScenario struct {
	Name         string            `json:"name"`
	Status       int               `json:"status"`
	Headers      map[string]string `json:"headers,omitempty"`
	Chunks       []string          `json:"chunks,omitempty"`
	ChunkDelayMS int64             `json:"chunk_delay_ms,omitempty"`
	CloseEarly   bool              `json:"close_early,omitempty"`
}

type Provider struct {
	scripts  map[string]Script
	mu       sync.Mutex
	attempts map[string]int
}

type Sink func(scenario.Event) error

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
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Set{}, fmt.Errorf("faux input contains multiple JSON values")
		}
		return Set{}, err
	}
	if err := set.Validate(); err != nil {
		return Set{}, err
	}
	return set, nil
}

func (s Set) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("faux schema_version=%d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if len(s.Scripts) == 0 || len(s.HTTP) == 0 {
		return fmt.Errorf("faux set requires scripts and HTTP scenarios")
	}
	names := map[string]struct{}{}
	for _, script := range s.Scripts {
		if script.Name == "" {
			return fmt.Errorf("script name is required")
		}
		if _, exists := names[script.Name]; exists {
			return fmt.Errorf("duplicate faux scenario %q", script.Name)
		}
		names[script.Name] = struct{}{}
		if (script.Result == nil) == (script.Error == nil) {
			return fmt.Errorf("script %q must define exactly one result or error", script.Name)
		}
		var events []scenario.Event
		for index, step := range script.Steps {
			if step.DelayMS < 0 {
				return fmt.Errorf("script %q step[%d] has negative delay", script.Name, index)
			}
			if step.DelayMS == 0 && step.Event == nil {
				return fmt.Errorf("script %q step[%d] is empty", script.Name, index)
			}
			if step.Event != nil {
				events = append(events, *step.Event)
			}
		}
		if len(events) > 0 {
			if err := scenario.ValidateEvents(events); err != nil {
				return fmt.Errorf("script %q: %w", script.Name, err)
			}
		}
	}
	for _, current := range s.HTTP {
		if current.Name == "" {
			return fmt.Errorf("HTTP scenario name is required")
		}
		if _, exists := names[current.Name]; exists {
			return fmt.Errorf("duplicate faux scenario %q", current.Name)
		}
		names[current.Name] = struct{}{}
		if current.Status < 100 || current.Status > 599 {
			return fmt.Errorf("HTTP scenario %q has invalid status %d", current.Name, current.Status)
		}
		if current.ChunkDelayMS < 0 {
			return fmt.Errorf("HTTP scenario %q has negative chunk delay", current.Name)
		}
	}
	return nil
}

func NewProvider(scripts []Script) (*Provider, error) {
	values := make(map[string]Script, len(scripts))
	for _, script := range scripts {
		if script.Name == "" {
			return nil, fmt.Errorf("script name is required")
		}
		if _, exists := values[script.Name]; exists {
			return nil, fmt.Errorf("duplicate script %q", script.Name)
		}
		values[script.Name] = script
	}
	return &Provider{scripts: values, attempts: map[string]int{}}, nil
}

// Stream executes exactly one scripted attempt. It deliberately contains no
// retry policy.
func (p *Provider) Stream(
	ctx context.Context,
	name string,
	sink Sink,
) (scenario.ModelResult, *scenario.RuntimeError) {
	p.mu.Lock()
	p.attempts[name]++
	p.mu.Unlock()
	script, exists := p.scripts[name]
	if !exists {
		return scenario.ModelResult{}, &scenario.RuntimeError{
			Code: "invalid_request", Phase: "provider",
			Message: "unknown faux scenario", Retryable: false,
		}
	}
	for _, step := range script.Steps {
		if step.DelayMS > 0 {
			timer := time.NewTimer(time.Duration(step.DelayMS) * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return scenario.ModelResult{}, contextError(ctx.Err())
			}
		}
		if step.Event == nil {
			continue
		}
		event, err := redactedClone(*step.Event, script.Secrets)
		if err != nil {
			return scenario.ModelResult{}, &scenario.RuntimeError{
				Code: "internal", Phase: "provider",
				Message: "cannot clone faux event", Retryable: false,
			}
		}
		if sink != nil {
			if err := sink(event); err != nil {
				return scenario.ModelResult{}, &scenario.RuntimeError{
					Code: "cancelled", Phase: "consumer",
					Message: "event sink stopped", Retryable: false,
				}
			}
		}
	}
	if script.Error != nil {
		value, err := redactedClone(*script.Error, script.Secrets)
		if err != nil {
			return scenario.ModelResult{}, &scenario.RuntimeError{
				Code: "internal", Phase: "provider",
				Message: "cannot clone faux error", Retryable: false,
			}
		}
		return scenario.ModelResult{}, &value
	}
	value, err := redactedClone(*script.Result, script.Secrets)
	if err != nil {
		return scenario.ModelResult{}, &scenario.RuntimeError{
			Code: "internal", Phase: "provider",
			Message: "cannot clone faux result", Retryable: false,
		}
	}
	return value, nil
}

func (p *Provider) Attempts(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts[name]
}

func contextError(err error) *scenario.RuntimeError {
	code := "cancelled"
	if err == context.DeadlineExceeded {
		code = "timeout"
	}
	return &scenario.RuntimeError{
		Code: code, Phase: "provider", Message: err.Error(), Retryable: false,
	}
}

func redactedClone[T any](value T, secrets []string) (T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero, err
	}
	text := string(data)
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	var result T
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}
