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
		Name:    "tui",
		Summary: "Run a CLI Profile interactively without creating a Runtime Session or Run.",
		Usage: []string{
			"sn-cli <cli-profile-id> [options...] [input]",
			"sn-cli <cli-profile-id> resume [session-id]",
			"sn-cli <cli-profile-id> --resume [session-id]",
		},
		Notes: []string{
			"The target CLI owns the terminal and keeps its native stdout, stderr, and exit status.",
			"resume and --resume are equivalent; they continue a Codex/Claude/Grok native session and do not create a Runtime Session.",
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
			"exec accepts CLI Profiles only and keeps the target process native stdout, stderr, and exit status.",
			"Use session exec when the call must create canonical Session facts.",
		},
	},
	{
		Name:    "call",
		Summary: "Send one request through an API Profile without creating a Runtime Session or Run.",
		Usage: []string{
			"sn-cli call <api-profile-id> [options...] [input]",
			"sn-cli --json call <api-profile-id> [options...] [input]",
		},
		Notes: []string{
			"call accepts API Profiles only, performs one Provider call, and never executes a returned tool call.",
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
			"profile check is symbolic schema only; doctor checks live local dependencies.",
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
			"sn-cli session call <api-profile-id> [options...] [input]",
			"sn-cli session open <cli-profile-id> [--session-id <id>] [--retention ephemeral|standard|pinned] [--model M] [--effort E] [--prompt FILE_OR_TEXT] [--cwd DIR] [--attach|--detach] [input]",
			"sn-cli session send|attach|interrupt|close --session-id <id>",
			"sn-cli session close-all",
			"sn-cli session list [--state <state>] [--interface managed|native_tui]",
			"sn-cli session show|messages|events|logs|executions|execution [options...]",
			"sn-cli session reconcile|configure|export|delete|gc [options...]",
		},
		Notes: []string{
			"Managed: session exec/call use interface=managed and create canonical Turn/Execution facts; --queue creates a durable Run.",
			"Native TUI: session open/send/attach/interrupt/close/close-all use interface=native_tui in a private tmux PTY; open supports --prompt and is detached unless --attach is supplied.",
			"session send injects raw input into that TUI; accepted=true only means tmux accepted the transport operation.",
			"Raw TUI input follows terminal and provider line-editor semantics; use session exec for structured non-interactive tasks.",
			"session open creates one running kind=native_tui durable Run with opaque lifecycle Execution evidence; TUI input/output still create no canonical Turn, Message, Event, or transcript.",
			"session show projects that lifecycle Run/Execution; terminal Run events remain queryable through job events without creating Session Event duplicates.",
			"Provider exit settles the lifecycle Run and closes the window; session close settles it as cancelled, requests Provider termination, forces exit after a bounded grace period, and waits for the supervisor to exit while retaining the native_tui Session fact.",
			"A native_tui Session ID cannot be used by session exec/call; provider-native resume identity is not inferred from a Runtime Session ID.",
			"The tmux carrier is private; all public discovery and control use Session IDs.",
		},
	},
	{
		Name:    "agent",
		Summary: "Run the durable API-only Agent Kernel and its model/tool loop.",
		Usage: []string{
			"sn-cli agent <api-profile-id> [options...] [input]",
		},
		Notes: []string{
			"agent is the API-only Kernel; it does not wrap CLI Profiles such as gk/cx/cc.",
			"agent accepts API Profiles only; --queue submits work without starting sn-server.",
			"Tool execution follows the active Tool Catalog effect and risk contract.",
		},
	},
	{
		Name:    "job",
		Summary: "Query and control existing durable Runs without submitting fresh work.",
		Usage: []string{
			"sn-cli job get|result|trace|events|watch --run-id <id>",
			"sn-cli job list [options...]",
			"sn-cli job cancel|continue|retry|reconcile --run-id <id> [options...]",
			"sn-cli job gc [options...]",
		},
		Notes: []string{
			"job is a control plane over Run records (run_id); new work enters through session or agent.",
			"job continue resumes a paused Run; it is not the same as <cli-profile> resume.",
			"kind=native_tui Runs are query-only here; use session close or close-all to stop their windows.",
			"Terminal Run state, result/error, terminal event, and run.settled share one SQLite transaction.",
		},
	},
	{
		Name:    "server",
		Summary: "Manage the HTTP server and durable Run scheduler/worker lifecycle.",
		Usage: []string{
			"sn-cli server info|start|status|stop",
		},
		Notes: []string{
			"server process output is written to logs/sn-server.log.",
			"Release install uses sn-cli update; server upgrade-activate remains an internal activation action.",
		},
	},
	{
		Name:    "update",
		Summary: "Check, download, or activate a Runtime release without starting a Profile.",
		Usage: []string{
			"sn-cli update [options...]",
			"sn-cli update upgrade-check [options...]",
		},
		Notes: []string{
			"update follows the installed release contract.",
			"upgrade-check is the pre-activation preflight; upgrade-activate stays under server as an internal action.",
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
  sn-cli <cli-profile-id> --resume [session-id]
  sn-cli exec <cli-profile-id> [options...] [input]
  sn-cli call <api-profile-id> [options...] [input]
  sn-cli --json call <api-profile-id> [options...] [input]
  sn-cli --json <management-command> [args...]
  sn-cli doctor
  sn-cli profile list|show|check
  sn-cli session exec <cli-profile-id> [options...] [input]
  sn-cli session call <api-profile-id> [options...] [input]
  sn-cli session open <cli-profile-id> [--attach|--detach] [options...] [input]
  sn-cli session send|attach|interrupt|close --session-id <id>
  sn-cli session close-all
  sn-cli session list [--state <state>] [--interface managed|native_tui]
  sn-cli session show|messages|events|logs|executions|execution
  sn-cli session reconcile|configure|export|delete|gc
  sn-cli agent <api-profile-id> [options...] [input]
  sn-cli job get|list|result|trace|events|watch|cancel|continue|retry|reconcile|gc
  sn-cli server info|start|status|stop
  sn-cli update [options...]
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
  session open       create a native_tui Session and opaque lifecycle Run
  session close-all  close Session-bound native TUI windows; retain Session facts

Global:
  -h, --help         show root help
  --version          show build version
  --json             stable req/management output; must be first
                     interactive/exec CLI output remains target-native

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
