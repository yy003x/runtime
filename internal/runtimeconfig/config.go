package runtimeconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/internal/strictjson"
	"github.com/yy003x/runtime/internal/toolconfig"
)

const maxConfigBytes int64 = 1 << 20

const maxAgentTools = 256

type Config struct {
	Agent     Agent     `json:"agent"`
	MCP       MCP       `json:"mcp"`
	Scheduler Scheduler `json:"scheduler"`
	Run       Run       `json:"run"`
}

// MCP 配置外部 MCP tool 的 Streamable HTTP 协议协商。零值表示采用 toolmcp 的
// 内置默认（requested=2025-06-18，allowed={2025-06-18, 2024-11-05}），保持
// 既有 runtime.json 不显式配置时的行为不变。
type MCP struct {
	RequestedProtocolVersion string   `json:"requested_protocol_version"`
	AllowedProtocolVersions  []string `json:"allowed_protocol_versions"`
}

type Agent struct {
	Tools          []string `json:"tools"`
	WorkspaceRoots []string `json:"workspace_roots,omitempty"`
	MaxRounds      int      `json:"max_rounds"`
	MaxToolCalls   int      `json:"max_tool_calls"`
	MaxTotalTokens int64    `json:"max_total_tokens,omitempty"`
	MaxWallTime    string   `json:"max_wall_time"`
}

type Scheduler struct {
	Workers      int    `json:"workers"`
	PollInterval string `json:"poll_interval"`
}

type Run struct {
	SettledRetention string `json:"settled_retention"`
	Reaper           Reaper `json:"reaper"`
}

// Reaper configures the background sweep that settles paused and
// needs_reconciliation Runs whose updated_at is older than the configured TTL.
// A zero duration disables that field's sweep.
type Reaper struct {
	Interval               string `json:"interval"`
	PausedTTL              string `json:"paused_ttl"`
	NeedsReconciliationTTL string `json:"needs_reconciliation_ttl"`
}

func Default() Config {
	return Config{
		Agent: Agent{
			Tools: []string{
				"read_file", "list_directory", "web_search", "web_fetch",
			},
			WorkspaceRoots: []string{},
			MaxRounds:      16, MaxToolCalls: 64, MaxWallTime: "15m",
		},
		Scheduler: Scheduler{Workers: 1, PollInterval: "250ms"},
		Run: Run{
			SettledRetention: "168h",
			Reaper: Reaper{
				Interval: "5m", PausedTTL: "30m",
				NeedsReconciliationTTL: "24h",
			},
		},
	}
}

func Load(path string) (Config, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	return loadPresent(path, info)
}

// LoadRequired decodes a runtime config that must already exist. Activation
// uses this variant so a disappearing payload or staged runtime.json cannot be
// mistaken for the normal runtime default.
func LoadRequired(path string) (Config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, err
	}
	return loadPresent(path, info)
}

func loadPresent(path string, info os.FileInfo) (Config, error) {
	value := Default()
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("runtime config must be a regular file, not a symlink")
	}
	var raw map[string]json.RawMessage
	if err := strictjson.ReadRegularFile(path, maxConfigBytes, &raw); err != nil {
		return Config{}, err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Config{}, fmt.Errorf("%s: normalize runtime config: %w", path, err)
	}
	if err := strictjson.RejectNulls(data, nil); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := strictjson.Decode(
		bytes.NewReader(data), int64(len(data)), &value,
	); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := value.Validate(); err != nil {
		return Config{}, err
	}
	return value, nil
}

func (config Config) Validate() error {
	if config.Agent.Tools == nil {
		return fmt.Errorf("agent.tools must be an array when provided")
	}
	if config.Agent.WorkspaceRoots == nil {
		return fmt.Errorf("agent.workspace_roots must be an array when provided")
	}
	if config.Agent.MaxRounds <= 0 || config.Agent.MaxRounds > 128 {
		return fmt.Errorf("agent.max_rounds must be between 1 and 128")
	}
	if config.Agent.MaxToolCalls <= 0 || config.Agent.MaxToolCalls > 1024 {
		return fmt.Errorf("agent.max_tool_calls must be between 1 and 1024")
	}
	if config.Agent.MaxTotalTokens < 0 {
		return fmt.Errorf("agent.max_total_tokens must not be negative")
	}
	if _, err := parseBoundedDuration(
		config.Agent.MaxWallTime, time.Second, 24*time.Hour,
	); err != nil {
		return fmt.Errorf("agent.max_wall_time: %w", err)
	}
	seenTools := make(map[string]struct{}, len(config.Agent.Tools))
	if len(config.Agent.Tools) > maxAgentTools {
		return fmt.Errorf("agent.tools must not exceed %d items", maxAgentTools)
	}
	for _, name := range config.Agent.Tools {
		if err := toolconfig.ValidateName(name); err != nil {
			return fmt.Errorf(
				"agent.tools contains invalid tool name %q: %w", name, err,
			)
		}
		if _, exists := seenTools[name]; exists {
			return fmt.Errorf("agent.tools contains duplicate %q", name)
		}
		seenTools[name] = struct{}{}
	}
	seenRoots := make(map[string]struct{}, len(config.Agent.WorkspaceRoots))
	for _, root := range config.Agent.WorkspaceRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("agent.workspace_roots must contain absolute paths")
		}
		clean := filepath.Clean(root)
		if _, exists := seenRoots[clean]; exists {
			return fmt.Errorf(
				"agent.workspace_roots contains duplicate %q", clean,
			)
		}
		seenRoots[clean] = struct{}{}
	}
	if err := config.validateMCP(); err != nil {
		return err
	}
	if config.Scheduler.Workers <= 0 || config.Scheduler.Workers > 32 {
		return fmt.Errorf("scheduler.workers must be between 1 and 32")
	}
	if _, err := parseBoundedDuration(
		config.Scheduler.PollInterval, 10*time.Millisecond, time.Minute,
	); err != nil {
		return fmt.Errorf("scheduler.poll_interval: %w", err)
	}
	if _, err := parseBoundedDuration(
		config.Run.SettledRetention, time.Hour, 365*24*time.Hour,
	); err != nil {
		return fmt.Errorf("run.settled_retention: %w", err)
	}
	if _, err := parseBoundedDuration(
		config.Run.Reaper.Interval, 0, time.Hour,
	); err != nil {
		return fmt.Errorf("run.reaper.interval: %w", err)
	}
	if _, err := parseBoundedDuration(
		config.Run.Reaper.PausedTTL, 0, 720*time.Hour,
	); err != nil {
		return fmt.Errorf("run.reaper.paused_ttl: %w", err)
	}
	if _, err := parseBoundedDuration(
		config.Run.Reaper.NeedsReconciliationTTL, 0, 720*time.Hour,
	); err != nil {
		return fmt.Errorf("run.reaper.needs_reconciliation_ttl: %w", err)
	}
	return nil
}

func (config Config) AgentBudget() agent.Budget {
	wallTime, _ := time.ParseDuration(config.Agent.MaxWallTime)
	return agent.Budget{
		MaxRounds:      config.Agent.MaxRounds,
		MaxToolCalls:   config.Agent.MaxToolCalls,
		MaxTotalTokens: config.Agent.MaxTotalTokens,
		MaxWallTime:    wallTime,
	}
}

func (config Config) PollInterval() time.Duration {
	value, _ := time.ParseDuration(config.Scheduler.PollInterval)
	return value
}

func (config Config) SettledRetention() time.Duration {
	value, _ := time.ParseDuration(config.Run.SettledRetention)
	return value
}

func (config Config) ReaperInterval() time.Duration {
	value, _ := time.ParseDuration(config.Run.Reaper.Interval)
	return value
}

func (config Config) ReaperPausedTTL() time.Duration {
	value, _ := time.ParseDuration(config.Run.Reaper.PausedTTL)
	return value
}

func (config Config) ReaperNeedsReconciliationTTL() time.Duration {
	value, _ := time.ParseDuration(config.Run.Reaper.NeedsReconciliationTTL)
	return value
}

// validateMCP 仅校验配置形态（非空、无重复、requested ∈ allowed）；零值配置
// 放行，由 toolmcp 在 Build 时填充默认值。
func (config Config) validateMCP() error {
	if config.MCP.RequestedProtocolVersion == "" &&
		len(config.MCP.AllowedProtocolVersions) == 0 {
		return nil
	}
	if strings.TrimSpace(config.MCP.RequestedProtocolVersion) == "" {
		return fmt.Errorf(
			"mcp.requested_protocol_version must not be blank when mcp is configured",
		)
	}
	seen := make(map[string]struct{}, len(config.MCP.AllowedProtocolVersions))
	for _, version := range config.MCP.AllowedProtocolVersions {
		if strings.TrimSpace(version) == "" {
			return fmt.Errorf(
				"mcp.allowed_protocol_versions must not contain blank strings",
			)
		}
		if _, exists := seen[version]; exists {
			return fmt.Errorf(
				"mcp.allowed_protocol_versions contain duplicate %q", version,
			)
		}
		seen[version] = struct{}{}
	}
	if _, accepts := seen[config.MCP.RequestedProtocolVersion]; !accepts {
		return fmt.Errorf(
			"mcp.requested_protocol_version %q must be in allowed_protocol_versions",
			config.MCP.RequestedProtocolVersion,
		)
	}
	return nil
}

func parseBoundedDuration(
	value string,
	minimum, maximum time.Duration,
) (time.Duration, error) {
	current, err := time.ParseDuration(value)
	if err != nil || current < minimum || current > maximum {
		return 0, fmt.Errorf(
			"must be a duration between %s and %s", minimum, maximum,
		)
	}
	return current, nil
}
