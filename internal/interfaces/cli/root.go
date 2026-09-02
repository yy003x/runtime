package cli

import (
	"fmt"
	"os"

	"github.com/yy003x/runtime/internal/application/nativeconsole"
	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimetmux "github.com/yy003x/runtime/internal/infrastructure/tmux"
	"github.com/yy003x/runtime/internal/interfaces/cli/version"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
)

var fixedNamespaces = profileid.ReservedNamespaces()

func Main(args []string) int {
	if len(args) > 0 && args[0] == runtimetmux.HelperCommandName {
		if err := runTmuxHelper(args[1:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(args) > 0 && args[0] == nativeconsole.SupervisorCommand {
		if err := runNativeTUISupervisor(args[1:]); err != nil {
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
		if err := printHelp(output, ""); err != nil {
			return output.fail(err)
		}
		return 0
	}
	switch args[0] {
	case "-h", "--help":
		if len(args) != 1 {
			return output.fail(cliValidationf(
				"%s does not accept arguments", args[0],
			))
		}
		if err := printHelp(output, ""); err != nil {
			return output.fail(err)
		}
		return 0
	case "help":
		if len(args) > 2 {
			return output.fail(cliValidationf(
				"usage: sn-cli help [topic]",
			))
		}
		topic := ""
		if len(args) == 2 {
			topic = args[1]
		}
		if err := printHelp(output, topic); err != nil {
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
			err = cliValidationf("unknown namespace %q; use call", args[0])
		case "call":
			err = runProfileExecutionNamespace(
				paths, args[1:], runtimeprofile.KindModel,
				"", "call", output,
			)
		case "profile":
			err = runProfileNamespace(paths, args[1:], output)
		case "session":
			err = runSessionNamespace(paths, args[1:], output)
		case "agent":
			err = runAgentNamespace(paths, args[1:], output)
		case "run":
			err = cliValidationf("unknown namespace %q; use job", args[0])
		case "job":
			err = runRunNamespace(paths, args[1:], output)
		case "update":
			err = runUpdateNamespace(paths, args[1:], output)
		case "server":
			err = runServerNamespace(paths, args[1:], output)
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
