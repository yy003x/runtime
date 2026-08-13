package cli

import (
	"fmt"
	"strings"

	"github.com/yy003x/runtime/internal/interfaces/cli/version"
)

type cliHelpTopic struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Usage   []string `json:"usage"`
	Notes   []string `json:"notes,omitempty"`
}

var cliHelpTopics = []cliHelpTopic{
	{
		Name:    "direct",
		Summary: "Run a CLI Profile interactively without creating a Runtime Session or Run.",
		Usage: []string{
			"sn-cli <cli-profile-id> [options...] [input]",
			"sn-cli <cli-profile-id> resume [session-id]",
		},
		Notes: []string{
			"The target CLI owns the terminal and keeps its native stdout, stderr, and exit status.",
			"resume continues a Codex/Claude native session; it does not create a Runtime Session.",
			"A best-effort execution record is written to logs/YYMMDD/cli.jsonl.",
		},
	},
	{
		Name:    "exec",
		Summary: "Run a CLI Profile non-interactively without creating a Runtime Session or Run.",
		Usage: []string{
			"sn-cli exec <cli-profile-id> [options...] [input]",
		},
		Notes: []string{
			"The target process receives null stdin and keeps its native stdout, stderr, and exit status.",
			"Use session exec when the call must create canonical Session facts.",
		},
	},
	{
		Name:    "req",
		Summary: "Send one request through an API Profile without creating a Runtime Session or Run.",
		Usage: []string{
			"sn-cli req <api-profile-id> [options...] [input]",
			"sn-cli --json req <api-profile-id> [options...] [input]",
		},
		Notes: []string{
			"req performs one Provider call and never executes a returned tool call.",
			"A best-effort execution record is written to logs/YYMMDD/api.jsonl.",
		},
	},
	{
		Name:    "doctor",
		Summary: "Check local Runtime readiness without calling a Provider or MCP endpoint.",
		Usage: []string{
			"sn-cli doctor",
			"sn-cli --json doctor",
		},
		Notes: []string{
			"Checks Profile structure, CLI executables, API/Tool auth environment, SQLite, audit logs, and tmux.",
			"CLI Profile args environment references are invocation inputs and are not required by doctor.",
			"Returns non-zero when a configured local dependency is unavailable.",
		},
	},
	{
		Name:    "profile",
		Summary: "Inspect and statically validate active CLI and API Profiles.",
		Usage: []string{
			"sn-cli profile list",
			"sn-cli profile show <profile-id>",
			"sn-cli profile check [profile-id]",
		},
		Notes: []string{
			"Profile actions are read-only and never execute a Profile.",
			"profile check is symbolic: it does not resolve live environment, PATH, cwd, or remote access.",
		},
	},
	{
		Name:    "session",
		Summary: "Run canonical managed Sessions or provider-native TUI Sessions backed by tmux.",
		Usage: []string{
			"sn-cli session exec <cli-profile-id> [options...] [input]",
			"sn-cli session req <api-profile-id> [options...] [input]",
			"sn-cli session open <cli-profile-id> [--attach|--detach] [options...] [input]",
			"sn-cli session send|attach|interrupt|close --session-id <id>",
			"sn-cli session close-all",
			"sn-cli session list|show|messages|events|logs|executions|execution [options...]",
			"sn-cli session reconcile|configure|export|delete|gc [options...]",
		},
		Notes: []string{
			"session exec/req use interface=managed and create canonical Turn/Execution facts; --queue creates a durable Run.",
			"session open uses interface=native_tui and launches the CLI Profile's native interactive mode directly in a tmux PTY; it is detached unless --attach is supplied.",
			"session send injects raw input into that TUI; accepted=true only means tmux accepted the transport operation.",
			"Raw TUI input follows terminal and provider line-editor semantics; use session exec for structured non-interactive tasks.",
			"session open creates one running kind=native_tui durable Run with opaque lifecycle Execution evidence; TUI input/output still create no canonical Turn, Message, Event, or transcript.",
			"Provider exit settles the lifecycle Run and closes the window; session close settles it as cancelled before stopping the window while retaining the native_tui Session fact.",
			"A native_tui Session ID cannot be used by session exec/req; provider-native resume identity is not inferred from a Runtime Session ID.",
			"Use tmux for raw windows that must not create Runtime Session state.",
		},
	},
	{
		Name:    "tmux",
		Summary: "Manage raw interactive windows in the dedicated sn-session tmux session.",
		Usage: []string{
			"sn-cli tmux open <cli-profile-id> [options...] [input]",
			"sn-cli tmux list",
			"sn-cli tmux show|attach|interrupt|stop --tmux-id <id>",
			"sn-cli tmux send --tmux-id <id> [input]",
			"sn-cli tmux stop-all",
		},
		Notes: []string{
			"Raw tmux windows do not create a Runtime Session, Turn, or Run.",
			"tmux stop-all is all-or-nothing before mutation when a Session-bound window exists; run session close-all first.",
			"tmux open and session open share the open action and interactive adapter; only session open publishes an opaque native_tui Session binding.",
		},
	},
	{
		Name:    "agent",
		Summary: "Run the durable API-only Agent Kernel and its model/tool loop.",
		Usage: []string{
			"sn-cli agent <api-profile-id> [options...] [input]",
		},
		Notes: []string{
			"agent accepts API Profiles only; --queue submits work without starting sn-server.",
			"Tool execution follows the active Tool Catalog effect and risk contract.",
		},
	},
	{
		Name:    "run",
		Summary: "Query and control existing durable Runs without submitting fresh work.",
		Usage: []string{
			"sn-cli run get|result|trace|events|watch --run-id <id>",
			"sn-cli run list [options...]",
			"sn-cli run cancel|resume|retry|reconcile --run-id <id> [options...]",
			"sn-cli run gc [options...]",
		},
		Notes: []string{
			"run is a control plane; new work enters through session or agent.",
			"kind=native_tui Runs are query-only here; use session close or close-all to stop their windows.",
			"Terminal Run state, result/error, terminal event, and run.settled share one SQLite transaction.",
		},
	},
	{
		Name:    "server",
		Summary: "Manage the HTTP server and durable Run scheduler/worker lifecycle.",
		Usage: []string{
			"sn-cli server info|start|status|stop",
			"sn-cli server update [options...]",
			"sn-cli server upgrade-check [options...]",
		},
		Notes: []string{
			"server process output is written to logs/sn-server.log.",
			"server update and activation follow the installed release contract.",
		},
	},
}

func printHelp(output *cliOutput, topic string) error {
	if topic != "" {
		value, exists := findHelpTopic(topic)
		if !exists {
			return cliValidationf(
				"unknown help topic %q; available topics: %s",
				topic, strings.Join(helpTopicNames(), ", "),
			)
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"name": "sn-cli", "version": version.String(),
				"topic": value,
			})
		}
		return renderHelpTopic(output, value)
	}
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"name":         "sn-cli",
			"version":      version.String(),
			"namespaces":   fixedNamespaces,
			"topics":       helpTopicNames(),
			"commands":     cliHelpTopics,
			"runtime_home": "${SN_CLI_HOME:-~/.sn}",
			"log_root":     "<runtime-home>/logs",
		})
	}
	return renderRootHelp(output)
}

func findHelpTopic(name string) (cliHelpTopic, bool) {
	for _, topic := range cliHelpTopics {
		if topic.Name == name {
			return topic, true
		}
	}
	return cliHelpTopic{}, false
}

func helpTopicNames() []string {
	values := make([]string, 0, len(cliHelpTopics))
	for _, topic := range cliHelpTopics {
		values = append(values, topic.Name)
	}
	return values
}

func renderHelpTopic(output *cliOutput, topic cliHelpTopic) error {
	if err := output.line("sn-cli help %s", topic.Name); err != nil {
		return err
	}
	if err := output.line(""); err != nil {
		return err
	}
	if err := output.line("%s", topic.Summary); err != nil {
		return err
	}
	if err := output.line(""); err != nil {
		return err
	}
	if err := output.line("Usage:"); err != nil {
		return err
	}
	for _, usage := range topic.Usage {
		if err := output.line("  %s", usage); err != nil {
			return err
		}
	}
	if len(topic.Notes) == 0 {
		return nil
	}
	if err := output.line(""); err != nil {
		return err
	}
	if err := output.line("Notes:"); err != nil {
		return err
	}
	for _, note := range topic.Notes {
		if err := output.line("  - %s", note); err != nil {
			return err
		}
	}
	return nil
}

func renderRootHelp(output *cliOutput) error {
	if err := output.text(`sn-cli - SN Runtime

Usage:
  sn-cli <cli-profile-id> [options...] [input]
  sn-cli <cli-profile-id> resume [session-id]
  sn-cli exec <cli-profile-id> [options...] [input]
  sn-cli req <api-profile-id> [options...] [input]
  sn-cli --json req <api-profile-id> [options...] [input]
  sn-cli --json <management-command> [args...]
  sn-cli doctor
  sn-cli profile list|show|check
  sn-cli session exec <cli-profile-id> [options...] [input]
  sn-cli session req <api-profile-id> [options...] [input]
  sn-cli session open <cli-profile-id> [--attach|--detach] [options...] [input]
  sn-cli session send|attach|interrupt|close --session-id <id>
  sn-cli session close-all
  sn-cli session list|show|messages|events|logs|executions|execution
  sn-cli session reconcile|configure|export|delete|gc
  sn-cli tmux open <cli-profile-id> [options...] [input]
  sn-cli tmux list|show|send|attach|interrupt|stop|stop-all
  sn-cli agent <api-profile-id> [options...] [input]
  sn-cli run get|list|result|trace|events|watch|cancel|resume|retry|reconcile|gc
  sn-cli server info|start|status|stop|update|upgrade-check
  sn-cli help <topic>`); err != nil {
		return err
	}
	if err := output.line(""); err != nil {
		return err
	}
	if err := output.line("Commands:"); err != nil {
		return err
	}
	for _, topic := range cliHelpTopics {
		if err := output.line("  %-10s %s", topic.Name, topic.Summary); err != nil {
			return err
		}
	}
	return output.text(fmt.Sprintf(`
Detailed help:
  sn-cli help <topic>
  topics: %s

Lifecycle safety:
  open               shared creation action for session and tmux; it cannot be omitted
  session close-all  close Session-bound native TUI windows; retain Session facts
  tmux stop-all      close raw windows only; reject before mutation if a Session binding exists

Global:
  -h, --help         show root help
  --version          show build version
  --json             stable req/management output; must be first
                     direct/exec CLI output remains target-native

Runtime home:        ${SN_CLI_HOME:-~/.sn}
Profiles:            <runtime-home>/configs
Tools:               <runtime-home>/tools
Sessions:            <runtime-home>/sessions
Run database:        <runtime-home>/state/runtime.db

Logs:
  CLI/API execution  <runtime-home>/logs/YYMMDD/{cli,api}.jsonl
  Control audit      <runtime-home>/logs/YYMMDD/audit.jsonl
  Server process     <runtime-home>/logs/sn-server.log`, strings.Join(helpTopicNames(), ", ")))
}
