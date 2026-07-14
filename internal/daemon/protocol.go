package daemon

import "time"

type MessageType string

const (
	MessageStatus        MessageType = "status"
	MessageShutdown      MessageType = "shutdown"
	MessageAcquire       MessageType = "acquire"
	MessageRelease       MessageType = "release"
	MessageTmuxStart     MessageType = "tmux_start"
	MessageTmuxHas       MessageType = "tmux_has"
	MessageTmuxCapture   MessageType = "tmux_capture"
	MessageTmuxSend      MessageType = "tmux_send"
	MessageTmuxInterrupt MessageType = "tmux_interrupt"
	MessageTmuxKill      MessageType = "tmux_kill"
)

type Dependency struct {
	Command  string `json:"command"`
	WaitTCP  string `json:"wait_tcp,omitempty"`
	WaitHTTP string `json:"wait_http,omitempty"`
	Restart  bool   `json:"restart,omitempty"`
	Silent   bool   `json:"silent,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

type ExecutionEnvironment struct {
	AuditProxy bool     `json:"audit_proxy,omitempty"`
	Upstreams  []string `json:"upstreams,omitempty"`
	Bypass     []string `json:"bypass,omitempty"`
	Shim       bool     `json:"shim,omitempty"`
	Dylib      string   `json:"dylib,omitempty"`
}

type TmuxStartRequest struct {
	ProcessID string               `json:"process_id"`
	Session   string               `json:"session"`
	CWD       string               `json:"cwd"`
	Command   string               `json:"command"`
	Depends   []Dependency         `json:"depends,omitempty"`
	Execution ExecutionEnvironment `json:"execution,omitempty"`
}

type Request struct {
	Type      MessageType          `json:"type"`
	Token     string               `json:"token,omitempty"`
	ProcessID string               `json:"process_id,omitempty"`
	Session   string               `json:"session,omitempty"`
	Tail      int                  `json:"tail,omitempty"`
	Text      string               `json:"text,omitempty"`
	Submit    bool                 `json:"submit,omitempty"`
	Bracketed bool                 `json:"bracketed,omitempty"`
	Cleanup   bool                 `json:"cleanup,omitempty"`
	TmuxStart *TmuxStartRequest    `json:"tmux_start,omitempty"`
	Depends   []Dependency         `json:"depends,omitempty"`
	Execution ExecutionEnvironment `json:"execution,omitempty"`
}

type ProcessStatus struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Session    string    `json:"session,omitempty"`
	Alive      bool      `json:"alive"`
	AuditProxy bool      `json:"audit_proxy,omitempty"`
	Shim       bool      `json:"shim,omitempty"`
	Dylib      bool      `json:"dylib,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

type DependencyStatus struct {
	Command  string `json:"command"`
	PID      int    `json:"pid,omitempty"`
	Healthy  bool   `json:"healthy"`
	Restart  bool   `json:"restart"`
	Optional bool   `json:"optional"`
	Owners   int    `json:"owners"`
	Error    string `json:"error,omitempty"`
}

type ProxyStatus struct {
	Enabled       bool   `json:"enabled"`
	Listen        string `json:"listen,omitempty"`
	UpstreamCount int    `json:"upstream_count"`
	RequestCount  int64  `json:"request_count"`
}

type Status struct {
	Version          string             `json:"version"`
	BinaryPath       string             `json:"binary_path,omitempty"`
	BinaryMtimeNanos int64              `json:"binary_mtime_nanos,omitempty"`
	PID              int                `json:"pid"`
	Socket           string             `json:"socket"`
	UptimeSeconds    int64              `json:"uptime_seconds"`
	Clients          int                `json:"clients"`
	Processes        []ProcessStatus    `json:"processes"`
	Dependencies     []DependencyStatus `json:"dependencies"`
	Proxy            ProxyStatus        `json:"proxy"`
}

type Response struct {
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Status      *Status           `json:"status,omitempty"`
	Session     string            `json:"session,omitempty"`
	Alive       bool              `json:"alive,omitempty"`
	Output      string            `json:"output,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}
