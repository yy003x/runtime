package command

import (
	"fmt"
)

type grokAdapter struct{}

func (grokAdapter) Name() string { return "grok" }

func (grokAdapter) Build(request BuildRequest) (Invocation, error) {
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
	parsed, err := grokOptions.parse(prepared.args)
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
	if request.Resume != nil {
		if request.Mode != ModeInteractive {
			return Invocation{}, fmt.Errorf("resume is only supported in interactive mode")
		}
		argv = append(argv, "--resume")
		if *request.Resume != "" {
			argv = append(argv, *request.Resume)
		}
	}
	if request.Mode == ModeExec {
		argv = append(argv, plan.exec...)
		if request.OutputProtocol == OutputCanonical {
			argv = append(argv, "--output-format", "json")
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
		argv, err = appendGrokSinglePrompt(argv, request)
		if err != nil {
			return Invocation{}, err
		}
	} else {
		argv, err = appendPrompt(argv, request)
		if err != nil {
			return Invocation{}, err
		}
	}
	return finishInvocation(request, prepared, argv)
}

func (grokAdapter) Decode(stdout []byte) (CanonicalOutput, error) {
	if len(stdout) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Grok canonical result exceeds 1048576 bytes")
	}
	var result struct {
		Type    string  `json:"type"`
		Message string  `json:"message"`
		Text    *string `json:"text"`
	}
	if err := decodeSingleJSON(stdout, &result); err != nil {
		return CanonicalOutput{}, err
	}
	if result.Type == "error" {
		if result.Message != "" {
			return CanonicalOutput{}, fmt.Errorf(
				"Grok canonical output reported error: %s", result.Message,
			)
		}
		return CanonicalOutput{}, fmt.Errorf("Grok canonical output reported error")
	}
	if result.Text == nil {
		return CanonicalOutput{}, fmt.Errorf("Grok canonical output is not a successful result")
	}
	if len(*result.Text) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Grok assistant result exceeds 1048576 bytes")
	}
	return CanonicalOutput{Assistant: *result.Text}, nil
}

func appendGrokSinglePrompt(argv []string, request BuildRequest) ([]string, error) {
	prompt := ""
	if request.ArgvPrompt != nil {
		prompt = *request.ArgvPrompt
	}
	if len(prompt) > MaxTokenBytes {
		return nil, &invocationLimitError{message: fmt.Sprintf(
			"prompt exceeds %d bytes", MaxTokenBytes,
		)}
	}
	if err := validateTextToken(
		"prompt", prompt, MaxTokenBytes, true,
	); err != nil {
		return nil, err
	}
	// Grok's --single consumes the next token as PROMPT. A bare "--" is the
	// prompt value, so leading-dash prompts must use the attached form.
	return append(argv, "--single="+prompt), nil
}

var grokOptions = newOptionParser([]optionSpec{
	{name: "--agent", aliases: []string{"--agent"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--agents", aliases: []string{"--agents"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--allow", aliases: []string{"--allow"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--always-approve", aliases: []string{"--always-approve"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--deny", aliases: []string{"--deny"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--disable-web-search", aliases: []string{"--disable-web-search"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--disallowed-tools", aliases: []string{"--disallowed-tools"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--effort", aliases: []string{"--effort", "--reasoning-effort"}, arity: arityValue, scope: scopeCommon, kind: kindEffort},
	{name: "--fullscreen", aliases: []string{"--fullscreen"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--leader-socket", aliases: []string{"--leader-socket"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--minimal", aliases: []string{"--minimal"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--model", aliases: []string{"-m", "--model"}, arity: arityValue, scope: scopeCommon, kind: kindModel},
	{name: "--no-alt-screen", aliases: []string{"--no-alt-screen"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-plan", aliases: []string{"--no-plan"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-subagents", aliases: []string{"--no-subagents"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--oauth", aliases: []string{"--oauth"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--permission-mode", aliases: []string{"--permission-mode"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--rules", aliases: []string{"--rules"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--sandbox", aliases: []string{"--sandbox"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--system-prompt-override", aliases: []string{"--system-prompt-override", "--system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--verbatim", aliases: []string{"--verbatim"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--cwd", aliases: []string{"--cwd"}, arity: arityValue, scope: scopeCommon, kind: kindCWD},
	{name: "--debug", aliases: []string{"--debug"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--debug-file", aliases: []string{"--debug-file"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--continue", aliases: []string{"-c", "--continue"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--resume", aliases: []string{"-r", "--resume"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--session-id", aliases: []string{"-s", "--session-id"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--fork-session", aliases: []string{"--fork-session"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--restore-code", aliases: []string{"--restore-code"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--worktree", aliases: []string{"-w", "--worktree"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--worktree-ref", aliases: []string{"--worktree-ref", "--ref"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--single", aliases: []string{"-p", "--single"}, arity: arityFlag, scope: scopeExec, kind: kindMode},
	{name: "--output-format", aliases: []string{"--output-format"}, arity: arityValue, scope: scopeExec, kind: kindOutput},
	{name: "--max-turns", aliases: []string{"--max-turns"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--tools", aliases: []string{"--tools"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--include-partial-messages", aliases: []string{"--include-partial-messages"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--json-schema", aliases: []string{"--json-schema"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
})
