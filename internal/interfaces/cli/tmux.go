package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	runtimetmux "github.com/yy003x/runtime/internal/infrastructure/tmux"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
)

const nativeTUIInputLimit = 1 << 20

// tmuxManager is the private PTY carrier surface composed by native_tui
// Sessions. tmux is not a public CLI namespace.
type tmuxManager interface {
	Start(context.Context, runtimetmux.StartRequest) (runtimetmux.StartResult, error)
	List(context.Context) ([]runtimetmux.Window, error)
	Send(context.Context, string, string) (runtimetmux.ActionResult, error)
	Attach(context.Context, string, runtimetmux.TTYFiles) error
	Interrupt(context.Context, string) (runtimetmux.ActionResult, error)
	Stop(context.Context, string) (runtimetmux.ActionResult, error)
}

type nativeTUIInvocationOptions struct {
	profileID string
	model     *string
	effort    *runtimecommand.Effort
	prompt    *string
	cwd       *string
	input     *string
}

// runTmuxHelper is a private bootstrap protocol used by the tmux carrier.
// Root calls it before layout or Profile loading; it is not part of the public
// command tree.
func runTmuxHelper(args []string) error {
	return runtimetmux.RunHelper(args)
}

func resolveNativeTUIInvocation(
	catalog *runtimeprofile.Catalog,
	options nativeTUIInvocationOptions,
	pipedInput string,
	invocationBase string,
	inheritedEnvironment []string,
	logsDir string,
) (runtimetmux.Invocation, string, error) {
	if catalog == nil {
		return runtimetmux.Invocation{}, "", fmt.Errorf("Profile catalog is required")
	}
	entry, exists := catalog.Resolve(options.profileID)
	if !exists {
		return runtimetmux.Invocation{}, "", cliValidationf(
			"unknown profile %q", options.profileID,
		)
	}
	if entry.Kind != runtimeprofile.KindCommand || entry.Command == nil {
		return runtimetmux.Invocation{}, "", cliValidationf(
			"session open requires a CLI profile; %q is an API profile",
			options.profileID,
		)
	}
	basePrompt, err := runtimecommand.ResolvePrompt(
		entry.Command.Prompt, invocationBase,
	)
	if err != nil {
		return runtimetmux.Invocation{}, "", err
	}
	typedPrompt := ""
	if options.prompt != nil {
		typedPrompt, err = runtimecommand.ResolvePrompt(
			*options.prompt, invocationBase,
		)
		if err != nil {
			return runtimetmux.Invocation{}, "", err
		}
	}
	positional := ""
	if options.input != nil {
		positional = *options.input
	}
	prompt, err := runtimecommand.MergePrompt(
		basePrompt, typedPrompt, pipedInput, positional,
	)
	if err != nil {
		return runtimetmux.Invocation{}, "", err
	}
	requestInput, err := runtimecommand.MergePrompt(
		typedPrompt, pipedInput, positional,
	)
	if err != nil {
		return runtimetmux.Invocation{}, "", err
	}
	var argvPrompt *string
	if prompt != "" {
		argvPrompt = &prompt
	}
	invocation, err := runtimecommand.Build(runtimecommand.BuildRequest{
		Mode:           runtimecommand.ModeInteractive,
		OutputProtocol: runtimecommand.OutputNative,
		Profile:        *entry.Command,
		Overrides: runtimecommand.Overrides{
			Model: options.model, Effort: options.effort, CWD: options.cwd,
		},
		ArgvPrompt:           argvPrompt,
		InheritedEnvironment: inheritedEnvironment,
		InvocationBase:       invocationBase,
	})
	if err != nil {
		return runtimetmux.Invocation{}, "", err
	}
	_ = executionlog.AppendCLI(logsDir, executionlog.CLIRecord{
		Time: time.Now(), Namespace: executionlog.NamespaceSession,
		Profile: options.profileID,
		Source:  executionlog.SourceFromArgs(os.Args),
		Command: executionlog.FormatCommand(
			entry.Command.Env, invocation.CWD,
			invocation.Path, invocation.Argv,
		),
	})
	digest, err := nativeTUIConfigDigest(
		options.profileID, *entry.Command, options,
	)
	if err != nil {
		return runtimetmux.Invocation{}, "", err
	}
	return runtimetmux.Invocation{
		ProfileID: options.profileID, Path: invocation.Path,
		Argv: invocation.Argv, Environment: invocation.Environment,
		CWD: invocation.CWD, ConfigDigest: digest,
	}, requestInput, nil
}

func nativeTUIConfigDigest(
	profileID string,
	profile runtimecommand.Profile,
	options nativeTUIInvocationOptions,
) (string, error) {
	value := struct {
		ProfileID string                   `json:"profile_id"`
		Mode      runtimecommand.Mode      `json:"mode"`
		Profile   runtimecommand.Profile   `json:"profile"`
		Overrides runtimecommand.Overrides `json:"overrides"`
	}{
		ProfileID: profileID, Mode: runtimecommand.ModeInteractive,
		Profile: profile,
		Overrides: runtimecommand.Overrides{
			Model: options.model, Effort: options.effort, CWD: options.cwd,
		},
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode native TUI effective config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readOptionalNativeTUIInput(file *os.File) (string, error) {
	if file == nil || runtimecommand.IsTerminal(file) {
		return "", nil
	}
	value, err := io.ReadAll(io.LimitReader(file, nativeTUIInputLimit+1))
	if err != nil {
		return "", fmt.Errorf("read native TUI input: %w", err)
	}
	if len(value) > nativeTUIInputLimit {
		return "", cliValidationf(
			"native TUI input exceeds %d bytes", nativeTUIInputLimit,
		)
	}
	return string(value), nil
}
