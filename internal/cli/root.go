package cli

import (
	"fmt"
	"os"

	"github.com/yy003x/runtime/internal/cli/version"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
)

var fixedNamespaces = []string{
	"profile", "session", "agent", "run", "server", "help", "version",
}

func Main(args []string) int {
	jsonOutput := len(args) > 0 && args[0] == "--json"
	if jsonOutput {
		args = args[1:]
	}
	output := newCLIOutput(jsonOutput, os.Stdout, os.Stderr)
	if len(args) == 0 {
		if err := printHelp(output); err != nil {
			return output.fail(err)
		}
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := printHelp(output); err != nil {
			return output.fail(err)
		}
		return 0
	case "--version", "version":
		if output.JSON() {
			if err := output.writeJSON(map[string]any{
				"schema_version":   cliOutputSchemaVersion,
				"contract_version": cliOutputContractVersion,
				"version":          version.String(),
			}); err != nil {
				return output.fail(err)
			}
		} else if err := output.line("%s", version.String()); err != nil {
			return output.fail(err)
		}
		return 0
	}
	paths, err := layout.Resolve()
	if err != nil {
		return output.fail(err)
	}
	switch args[0] {
	case "profile":
		err = runVNextProfileNamespace(paths, args[1:], output)
	case "session":
		err = runSessionNamespaceVNext(paths, args[1:], output)
	case "agent":
		err = runAgentNamespace(paths, args[1:], output)
	case "run":
		err = runRunNamespaceVNext(paths, args[1:], output)
	case "server":
		err = runServerNamespaceVNext(paths, args[1:], output)
	default:
		runtime, loadErr := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
		if loadErr != nil {
			return output.fail(loadErr)
		}
		subcommand, exists := runtime.Subcommands.Get(args[0])
		if !exists {
			return output.fail(fmt.Errorf("unknown command %q", args[0]))
		}
		err = runLoadedVNextShortcut(
			runtime, subcommand.Profile, args[1:],
		)
	}
	return output.fail(err)
}

func printHelp(output *cliOutput) error {
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"schema_version":   cliOutputSchemaVersion,
			"contract_version": cliOutputContractVersion,
			"name":             "sn-cli",
			"version":          version.String(),
			"namespaces":       fixedNamespaces,
		})
	}
	return output.text(`sn-cli - Runtime vNext

Usage:
  sn-cli <command-id> [native-cli-args...]
  sn-cli --json <management-command> [args...]
  sn-cli profile <profile-id> [--effort <level>] [input]
  sn-cli profile list|show|check
  sn-cli session run|submit [runtime-options] <profile-id> <input>
  sn-cli agent run --profile <model-profile-id> [options] <input>
  sn-cli run submit|get|list|result|events|watch|cancel|resume|retry|reconcile|gc
  sn-cli server info|doctor|start|status|stop|update

Execution semantics:
  <command-id>       transparent native CLI process replacement; no Runtime record
  profile <id>       exactly one command or API model call; no Runtime record
  session run        one recorded Session turn; Session never executes tools
  session submit     durable queued Session turn
  agent run          durable API-only model/tool loop; Session is opt-in
  run ...            durable Run query and control plane

Global:
  -h, --help         show this help
  --version          show build version
  --json             stable machine output; must be the first argument

Profile override:
  --effort LEVEL     low|medium|high|xhigh|max; requires profile adapter

Runtime home:        ${SN_CLI_HOME:-~/.sn}
Profiles:            <runtime-home>/configs
Subcommands:         <runtime-home>/commands
Sessions:            <runtime-home>/sessions
Run database:        <runtime-home>/state/runtime.db`)
}
