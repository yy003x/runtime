package cli

import (
	"fmt"
	"os"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/interfaces/cli/version"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

var fixedNamespaces = profileid.ReservedNamespaces()

func Main(args []string) int {
	if len(args) > 0 && args[0] == runtimetmux.HelperCommandName {
		if err := runTmuxHelperVNext(args[1:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(args) > 0 && args[0] == sessionTerminalHelperCommandName {
		if err := runSessionTerminalHelperVNext(args[1:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
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
		if len(args) != 1 {
			return output.fail(cliValidationf(
				"%s does not accept arguments", args[0],
			))
		}
		if err := printHelp(output); err != nil {
			return output.fail(err)
		}
		return 0
	case "--version", "version":
		if len(args) != 1 {
			return output.fail(cliValidationf(
				"%s does not accept arguments", args[0],
			))
		}
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
	activationCommand := len(args) > 1 &&
		args[0] == "server" && args[1] == "upgrade-activate"
	if !activationCommand {
		err = activationgate.RequireOpen(paths.StateDir)
	}
	if err == nil {
		switch args[0] {
		case "doctor":
			if len(args) != 1 {
				err = cliValidationf("doctor does not accept arguments")
			} else {
				err = runtimeDoctor(paths, output)
			}
		case "exec":
			err = runProfileExecutionNamespace(
				paths, args[1:], runtimeprofile.KindCommand,
				runtimecommand.ModeExec, "exec", output,
			)
		case "req":
			err = runProfileExecutionNamespace(
				paths, args[1:], runtimeprofile.KindModel,
				"", "req", output,
			)
		case "profile":
			err = runVNextProfileNamespace(paths, args[1:], output)
		case "session":
			err = runSessionNamespaceVNext(paths, args[1:], output)
		case "tmux":
			err = runTmuxNamespaceVNext(paths, args[1:], output)
		case "agent":
			err = runAgentNamespace(paths, args[1:], output)
		case "run":
			err = runRunNamespaceVNext(paths, args[1:], output)
		case "server":
			err = runServerNamespaceVNext(paths, args[1:], output)
		default:
			runtime, loadErr := runtimebootstrap.LoadProfileServices(
				paths, fixedNamespaces...,
			)
			if loadErr != nil {
				err = loadErr
				break
			}
			err = runLoadedProfileID(
				runtime, paths.LogsDir, args[0], args[1:], runtimeprofile.KindCommand,
				runtimecommand.ModeInteractive, "direct", output,
			)
		}
	}
	appendControlAudit(paths.LogsDir, args, err)
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
	return output.text(`sn-cli - SN Runtime

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
  sn-cli session open <cli-profile-id> [options...] [input]
  sn-cli session send|attach|interrupt|close --session-id <id>
  sn-cli session close-all
  sn-cli session list|show|messages|events|logs|executions|execution
  sn-cli session reconcile|configure|export|delete|gc
  sn-cli tmux open <cli-profile-id> [options...] [input]
  sn-cli tmux list|show|send|attach|interrupt|stop|stop-all
  sn-cli agent <api-profile-id> [options...] [input]
  sn-cli run get|list|result|trace|events|watch|cancel|resume|retry|reconcile|gc
  sn-cli server info|start|status|stop|update|upgrade-check

Execution semantics:
  <cli-profile-id>   direct CLI; no Session/Run; best-effort local CLI log
  exec               noninteractive CLI; no Session/Run; best-effort CLI log
  req                one API request; no Session/Run; best-effort API log
  session exec|req   one recorded Session turn; --queue submits a durable Run
  session open       tmux console; every input creates a durable Session Run
  tmux open          one managed window in configured sn-session; no Runtime Session
  agent              durable API-only model/tool loop; --queue submits only
  run ...            durable Run query and control plane; never submits new work

Global:
  -h, --help         show this help
  --version          show build version
  --json             stable req/management output; must be first
                     direct/exec CLI output remains target-native

Runtime home:        ${SN_CLI_HOME:-~/.sn}
Profiles:            <runtime-home>/configs
Tools:               <runtime-home>/tools
Sessions:            <runtime-home>/sessions
Logs:                <runtime-home>/logs
Run database:        <runtime-home>/state/runtime.db`)
}
