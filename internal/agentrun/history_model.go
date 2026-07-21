package agentrun

import "time"

const SessionSchemaVersion = 1

const (
	RecordFull     = "full"
	RecordMetadata = "metadata"
	RecordOff      = "off"
)

const (
	RetentionEphemeral = "ephemeral"
	RetentionStandard  = "standard"
	RetentionPinned    = "pinned"
)

const (
	CaptureStructured     = "structured"
	CaptureParsed         = "parsed"
	CaptureTranscriptOnly = "transcript_only"
	CaptureMetadataOnly   = "metadata_only"
)

const (
	ExecutionAPI        = "api"
	ExecutionCLIManaged = "cli_managed"
	ExecutionTmux       = "tmux"
	ExecutionTerminal   = "terminal"
)

const TurnStateSubmitted = "submitted"

const (
	SessionStateIdle     = "idle"
	SessionStateActive   = "active"
	SessionStateBlocked  = "blocked"
	SessionStateArchived = "archived"
)

type SessionRecord struct {
	SchemaVersion  int       `json:"schema_version"`
	SessionID      string    `json:"session_id"`
	ProjectID      string    `json:"project_id"`
	State          string    `json:"state"`
	Title          string    `json:"title,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	CWD            string    `json:"cwd,omitempty"`
	Runtime        string    `json:"runtime,omitempty"`
	Profile        string    `json:"profile,omitempty"`
	RecordMode     string    `json:"record_mode"`
	Retention      string    `json:"retention"`
	CaptureQuality string    `json:"capture_quality"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastSequence   int64     `json:"last_sequence"`
	TurnCount      int       `json:"turn_count"`
	RunCount       int       `json:"run_count"`
	LastTurnID     string    `json:"last_turn_id,omitempty"`
	Providers      []string  `json:"providers,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
}

type TurnRecord struct {
	SchemaVersion   int        `json:"schema_version"`
	TurnID          string     `json:"turn_id"`
	SessionID       string     `json:"session_id"`
	Sequence        int        `json:"sequence"`
	State           string     `json:"state"`
	Runtime         string     `json:"runtime,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	Profile         string     `json:"profile,omitempty"`
	Model           string     `json:"model,omitempty"`
	RecordMode      string     `json:"record_mode"`
	InputMessageID  string     `json:"input_message_id,omitempty"`
	OutputMessageID string     `json:"output_message_id,omitempty"`
	ContextManifest string     `json:"context_manifest,omitempty"`
	WinningRunID    string     `json:"winning_run_id,omitempty"`
	ResultRef       *ResultRef `json:"result_ref,omitempty"`
	CaptureQuality  string     `json:"capture_quality"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type RunAttemptRecord struct {
	SchemaVersion int        `json:"schema_version"`
	RunID         string     `json:"run_id"`
	RunType       string     `json:"run_type"`
	SessionID     string     `json:"session_id"`
	TurnID        string     `json:"turn_id"`
	ExecutionID   string     `json:"execution_id"`
	Attempt       int        `json:"attempt"`
	Provider      string     `json:"provider"`
	Profile       string     `json:"profile"`
	State         string     `json:"state"`
	FailureReason string     `json:"failure_reason,omitempty"`
	ResultRef     *ResultRef `json:"result_ref,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   time.Time  `json:"completed_at,omitempty"`
}

type ExecutionRecord struct {
	SchemaVersion  int        `json:"schema_version"`
	ExecutionID    string     `json:"execution_id"`
	SessionID      string     `json:"session_id"`
	Kind           string     `json:"kind"`
	Carrier        string     `json:"carrier,omitempty"`
	CarrierID      string     `json:"carrier_id,omitempty"`
	Profile        string     `json:"profile,omitempty"`
	Provider       string     `json:"provider,omitempty"`
	State          string     `json:"state"`
	CaptureQuality string     `json:"capture_quality"`
	CWD            string     `json:"cwd,omitempty"`
	ProcessID      int        `json:"process_id,omitempty"`
	RawArgCount    int        `json:"raw_arg_count,omitempty"`
	TranscriptRef  string     `json:"transcript_ref,omitempty"`
	RunIDs         []string   `json:"run_ids,omitempty"`
	TurnIDs        []string   `json:"turn_ids,omitempty"`
	ResultRef      *ResultRef `json:"result_ref,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    time.Time  `json:"completed_at,omitempty"`
}

type SessionMessage struct {
	SchemaVersion int            `json:"schema_version"`
	MessageID     string         `json:"message_id"`
	SessionID     string         `json:"session_id"`
	TurnID        string         `json:"turn_id,omitempty"`
	Sequence      int64          `json:"sequence"`
	Timestamp     time.Time      `json:"timestamp"`
	Role          string         `json:"role"`
	Kind          string         `json:"kind,omitempty"`
	Content       string         `json:"content,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type SessionEvent struct {
	SchemaVersion int            `json:"schema_version"`
	EventID       string         `json:"event_id"`
	SessionID     string         `json:"session_id"`
	TurnID        string         `json:"turn_id,omitempty"`
	ExecutionID   string         `json:"execution_id,omitempty"`
	RunID         string         `json:"run_id,omitempty"`
	Sequence      int64          `json:"sequence"`
	Timestamp     time.Time      `json:"timestamp"`
	Type          string         `json:"type"`
	Data          map[string]any `json:"data,omitempty"`
}

type ResultRef struct {
	RunID        string `json:"run_id"`
	RunType      string `json:"run_type"`
	ResultFile   string `json:"result_file"`
	ResultDigest string `json:"result_digest,omitempty"`
}

type ContextManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	SessionID     string              `json:"session_id"`
	TurnID        string              `json:"turn_id"`
	CreatedAt     time.Time           `json:"created_at"`
	CWD           string              `json:"cwd,omitempty"`
	Profile       string              `json:"profile,omitempty"`
	ConfigDigest  string              `json:"config_digest,omitempty"`
	PolicyDigest  string              `json:"policy_digest,omitempty"`
	MessageRange  SequenceRange       `json:"message_range"`
	MessageDigest string              `json:"message_digest,omitempty"`
	Skills        []ContextAssetRef   `json:"skills,omitempty"`
	Tools         []ContextAssetRef   `json:"tools,omitempty"`
	MemoryReads   []ContextMemoryRead `json:"memory_reads,omitempty"`
}

type SequenceRange struct {
	After int64 `json:"after"`
	To    int64 `json:"to"`
}

type ContextAssetRef struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest"`
	Source  string `json:"source,omitempty"`
}

type ContextMemoryRead struct {
	ID     string `json:"id"`
	Type   string `json:"type,omitempty"`
	Digest string `json:"digest"`
	Source string `json:"source,omitempty"`
}

type SessionIndex struct {
	SchemaVersion int                          `json:"schema_version"`
	UpdatedAt     time.Time                    `json:"updated_at"`
	Sessions      map[string]SessionIndexEntry `json:"sessions"`
}

type SessionIndexEntry struct {
	SessionID      string    `json:"session_id"`
	ProjectID      string    `json:"project_id"`
	State          string    `json:"state"`
	Title          string    `json:"title,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	SessionDir     string    `json:"session_dir"`
	RecordMode     string    `json:"record_mode"`
	Retention      string    `json:"retention"`
	CaptureQuality string    `json:"capture_quality"`
	Runtime        string    `json:"runtime,omitempty"`
	Profile        string    `json:"profile,omitempty"`
	TurnCount      int       `json:"turn_count"`
	RunCount       int       `json:"run_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Tags           []string  `json:"tags,omitempty"`
}

type SessionFilter struct {
	ProjectID string
	State     string
	Retention string
	Tags      []string
	FromTime  time.Time
	ToTime    time.Time
	Limit     int
}

type SessionView struct {
	Session    SessionRecord      `json:"session"`
	Turns      []TurnRecord       `json:"turns"`
	Attempts   []RunAttemptRecord `json:"attempts"`
	Executions []ExecutionRecord  `json:"executions"`
	Messages   []SessionMessage   `json:"messages"`
	Events     []SessionEvent     `json:"events"`
}

type RecordDecision struct {
	RecordMode     string `json:"record_mode"`
	Retention      string `json:"retention"`
	CaptureQuality string `json:"capture_quality"`
	Reason         string `json:"reason"`
}

type SessionImport struct {
	SchemaVersion    int                        `json:"schema_version,omitempty"`
	ExportedAt       time.Time                  `json:"exported_at,omitempty"`
	Session          SessionRecord              `json:"session"`
	Turns            []TurnRecord               `json:"turns,omitempty"`
	Attempts         []RunAttemptRecord         `json:"attempts,omitempty"`
	Executions       []ExecutionRecord          `json:"executions,omitempty"`
	ContextManifests map[string]ContextManifest `json:"context_manifests,omitempty"`
	Messages         []SessionMessage           `json:"messages,omitempty"`
	Events           []SessionEvent             `json:"events,omitempty"`
}
