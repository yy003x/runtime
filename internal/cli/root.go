package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/yy003x/runtime/internal/cli/version"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
)

var fixedNamespaces = []string{
	"profile", "session", "agent", "run", "system", "help", "version",
}

func Main(args []string) int {
	if len(args) == 0 {
		printHelp()
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		printHelp()
		return 0
	case "--version", "version":
		fmt.Println(version.String())
		return 0
	}
	paths, err := layout.Resolve()
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "profile":
		return exit(runVNextProfileNamespace(paths, args[1:]))
	case "session":
		return exit(runSessionNamespaceVNext(paths, args[1:]))
	case "agent":
		return exit(runAgentNamespace(paths, args[1:]))
	case "run":
		return exit(runRunNamespaceVNext(paths, args[1:]))
	case "system":
		return exit(runSystemNamespaceVNext(paths, args[1:]))
	}
	runtime, err := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
	if err != nil {
		return fail(err)
	}
	subcommand, exists := runtime.Subcommands.Get(args[0])
	if !exists {
		return fail(fmt.Errorf("unknown command %q", args[0]))
	}
	profileArgs := append([]string{subcommand.Profile}, args[1:]...)
	return exit(runLoadedVNextProfile(runtime, profileArgs))
}

func printHelp() {
	fmt.Println(`sn-cli - Runtime vNext

Usage:
  sn-cli <command-id> [native-cli-args...]
  sn-cli profile <profile-id> [input]
  sn-cli profile list|show|check
  sn-cli session run|submit [runtime-options] <profile-id> <input>
  sn-cli agent run --profile <model-profile-id> [options] <input>
  sn-cli run submit|get|list|result|events|watch|cancel|resume|retry|reconcile|gc
  sn-cli system info|doctor|start|status|stop|update

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

Runtime home:        ${SN_CLI_HOME:-~/.sn}
Profiles:            <runtime-home>/configs
Subcommands:         <runtime-home>/commands
Sessions:            <runtime-home>/sessions
Run database:        <runtime-home>/state/runtime.db`)
}

func exit(err error) int {
	if err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
