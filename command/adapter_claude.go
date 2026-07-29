package command

import (
	"fmt"
)

type claudeAdapter struct{}

func (claudeAdapter) Name() string { return "claude" }

func (claudeAdapter) Build(request BuildRequest) (Invocation, error) {
	if request.Mode != ModeInteractive && request.Mode != ModeExec {
		return Invocation{}, fmt.Errorf("unsupported command mode %q", request.Mode)
	}
	if request.OutputProtocol != OutputNative &&
		request.OutputProtocol != OutputCanonical {
		return Invocation{}, fmt.Errorf("unsupported output protocol %q", request.OutputProtocol)
	}
	if request.Mode == ModeInteractive &&
		request.OutputProtocol == OutputCanonical {
		return Invocation{}, fmt.Errorf("interactive mode does not support canonical output")
	}
	prepared, err := prepareEffectiveConfig(request)
	if err != nil {
		return Invocation{}, err
	}
	parsed, err := claudeOptions.parse(prepared.args)
	if err != nil {
		return Invocation{}, err
	}
	plan, err := classifyOptions(parsed, request.Mode, request.OutputProtocol)
	if err != nil {
		return Invocation{}, err
	}
	model, effort, _, err := effectiveTyped(request)
	if err != nil {
		return Invocation{}, err
	}
	modelOption, err := oneSelector("model", plan.model, model != "")
	if err != nil {
		return Invocation{}, err
	}
	effortOption, err := oneSelector("effort", plan.effort, effort != "")
	if err != nil {
		return Invocation{}, err
	}
	argv := []string{request.Profile.Command}
	argv = append(argv, plan.common...)
	if model != "" {
		argv = append(argv, "--model", model)
	} else if modelOption != nil {
		argv = append(argv, modelOption.tokens...)
	}
	if effort != "" {
		argv = append(argv, "--effort", string(effort))
	} else if effortOption != nil {
		argv = append(argv, effortOption.tokens...)
	}
	if request.Mode == ModeExec {
		argv = append(argv, plan.exec...)
		if request.OutputProtocol == OutputCanonical {
			argv = append(
				argv,
				"--no-session-persistence",
				"--output-format", "json",
			)
		} else {
			for _, item := range plan.stateless {
				argv = append(argv, item.tokens...)
			}
			if len(plan.output) > 1 {
				return Invocation{}, fmt.Errorf("--output-format is configured multiple times")
			}
			if len(plan.output) == 1 {
				argv = append(argv, plan.output[0].tokens...)
			}
			for _, item := range plan.input {
				argv = append(argv, item.tokens...)
			}
		}
		argv = append(argv, "-p")
	}
	argv, err = appendPrompt(argv, request)
	if err != nil {
		return Invocation{}, err
	}
	return finishInvocation(request, prepared, argv)
}

func (claudeAdapter) Decode(stdout []byte) (CanonicalOutput, error) {
	if len(stdout) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Claude canonical result exceeds 1048576 bytes")
	}
	var result struct {
		Type    string  `json:"type"`
		Subtype string  `json:"subtype"`
		IsError *bool   `json:"is_error"`
		Result  *string `json:"result"`
	}
	if err := decodeSingleJSON(stdout, &result); err != nil {
		return CanonicalOutput{}, err
	}
	if result.Type != "result" || result.Subtype != "success" ||
		result.IsError == nil || *result.IsError ||
		result.Result == nil {
		return CanonicalOutput{}, fmt.Errorf("Claude canonical output is not a successful result")
	}
	if len(*result.Result) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Claude assistant result exceeds 1048576 bytes")
	}
	return CanonicalOutput{Assistant: *result.Result}, nil
}

var claudeOptions = newOptionParser([]optionSpec{
	{name: "--add-dir", aliases: []string{"--add-dir"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--agent", aliases: []string{"--agent"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--agents", aliases: []string{"--agents"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--allow-dangerously-skip-permissions", aliases: []string{"--allow-dangerously-skip-permissions"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--allowed-tools", aliases: []string{"--allowedTools", "--allowed-tools"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--append-system-prompt", aliases: []string{"--append-system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--ax-screen-reader", aliases: []string{"--ax-screen-reader"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--bare", aliases: []string{"--bare"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--betas", aliases: []string{"--betas"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--brief", aliases: []string{"--brief"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--chrome", aliases: []string{"--chrome"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--dangerously-skip-permissions", aliases: []string{"--dangerously-skip-permissions"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--disable-slash-commands", aliases: []string{"--disable-slash-commands"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--disallowed-tools", aliases: []string{"--disallowedTools", "--disallowed-tools"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--effort", aliases: []string{"--effort"}, arity: arityValue, scope: scopeCommon, kind: kindEffort},
	{name: "--exclude-dynamic-system-prompt-sections", aliases: []string{"--exclude-dynamic-system-prompt-sections"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--file", aliases: []string{"--file"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--mcp-config", aliases: []string{"--mcp-config"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--model", aliases: []string{"--model"}, arity: arityValue, scope: scopeCommon, kind: kindModel},
	{name: "--name", aliases: []string{"-n", "--name"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-chrome", aliases: []string{"--no-chrome"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--permission-mode", aliases: []string{"--permission-mode"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--plugin-dir", aliases: []string{"--plugin-dir"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--plugin-url", aliases: []string{"--plugin-url"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--safe-mode", aliases: []string{"--safe-mode"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--setting-sources", aliases: []string{"--setting-sources"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--settings", aliases: []string{"--settings"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--strict-mcp-config", aliases: []string{"--strict-mcp-config"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--system-prompt", aliases: []string{"--system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--tools", aliases: []string{"--tools"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--verbose", aliases: []string{"--verbose"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--print", aliases: []string{"-p", "--print"}, arity: arityFlag, scope: scopeExec, kind: kindMode},
	{name: "--fallback-model", aliases: []string{"--fallback-model"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--max-budget-usd", aliases: []string{"--max-budget-usd"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--no-session-persistence", aliases: []string{"--no-session-persistence"}, arity: arityFlag, scope: scopeExec, kind: kindStateless},
	{name: "--output-format", aliases: []string{"--output-format"}, arity: arityValue, scope: scopeExec, kind: kindOutput},
	{name: "--input-format", aliases: []string{"--input-format"}, arity: arityValue, scope: scopeExec, kind: kindInput, forbidInCanon: true},
	{name: "--continue", aliases: []string{"-c", "--continue"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--resume", aliases: []string{"-r", "--resume"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--session-id", aliases: []string{"--session-id"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--fork-session", aliases: []string{"--fork-session"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--background", aliases: []string{"--bg", "--background"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--worktree", aliases: []string{"-w", "--worktree"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--tmux", aliases: []string{"--tmux"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--debug-file", aliases: []string{"--debug-file"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--json-schema", aliases: []string{"--json-schema"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--forward-subagent-text", aliases: []string{"--forward-subagent-text"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--include-hook-events", aliases: []string{"--include-hook-events"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--include-partial-messages", aliases: []string{"--include-partial-messages"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--replay-user-messages", aliases: []string{"--replay-user-messages"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--prompt-suggestions", aliases: []string{"--prompt-suggestions"}, arity: arityOptional, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
})
