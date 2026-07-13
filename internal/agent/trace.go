package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-arch/internal/llm"
)

type TraceLogger interface {
	Log(ctx context.Context, entry LLMTrace) error
}

type LLMTrace struct {
	TraceID        string            `json:"trace_id"`
	SessionID      string            `json:"session_id"`
	Turn           int               `json:"turn"`
	Provider       string            `json:"provider"`
	Model          string            `json:"model"`
	StartedAt      time.Time         `json:"started_at"`
	CompletedAt    time.Time         `json:"completed_at"`
	DurationMillis int64             `json:"duration_millis"`
	Request        LLMTraceRequest   `json:"request"`
	Response       *LLMTraceResponse `json:"response,omitempty"`
	Error          string            `json:"error,omitempty"`
}

type LLMTraceRequest struct {
	System          string        `json:"system"`
	Messages        []llm.Message `json:"messages"`
	Temperature     float64       `json:"temperature"`
	MaxOutputTokens int           `json:"max_output_tokens"`
}

type LLMTraceResponse struct {
	OutputText   string `json:"output_text"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type FileTraceLogger struct {
	baseDir string
}

func NewFileTraceLogger(baseDir string) *FileTraceLogger {
	return &FileTraceLogger{baseDir: baseDir}
}

func (l *FileTraceLogger) Log(_ context.Context, entry LLMTrace) error {
	if l == nil || l.baseDir == "" {
		return nil
	}

	sessionDir := filepath.Join(l.baseDir, entry.SessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return fmt.Errorf("create llm trace dir: %w", err)
	}

	path := filepath.Join(sessionDir, fmt.Sprintf("turn_%04d.json", entry.Turn))
	payload, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal llm trace: %w", err)
	}
	payload = append(payload, '\n')

	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write llm trace: %w", err)
	}
	return nil
}
