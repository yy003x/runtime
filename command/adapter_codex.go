package command

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type codexAdapter struct{}

func (codexAdapter) Name() string { return "codex" }

func (codexAdapter) Build(request BuildRequest) (Invocation, error) {
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
	parsed, err := codexOptions.parse(prepared.args)
	if err != nil {
		return Invocation{}, err
	}
	parsed = normalizeCodexConfigSelectors(parsed)
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
		argv = append(argv, "-c", "model_reasoning_effort="+string(effort))
	} else if effortOption != nil {
		argv = append(argv, effortOption.tokens...)
	}
	if request.Resume != nil {
		if request.Mode != ModeInteractive {
			return Invocation{}, fmt.Errorf("resume is only supported in interactive mode")
		}
		argv = append(argv, "resume")
		if *request.Resume != "" {
			argv = append(argv, *request.Resume)
		}
	}
	if request.Mode == ModeExec {
		argv = append(argv, "exec")
		argv = append(argv, plan.exec...)
		if request.OutputProtocol == OutputCanonical {
			argv = append(argv, "--ephemeral", "--json")
		} else {
			for _, item := range plan.stateless {
				argv = append(argv, item.tokens...)
			}
			if len(plan.output) > 1 {
				return Invocation{}, fmt.Errorf("--json is configured multiple times")
			}
			if len(plan.output) == 1 {
				argv = append(argv, plan.output[0].tokens...)
			}
		}
	}
	argv, err = appendPrompt(argv, request)
	if err != nil {
		return Invocation{}, err
	}
	return finishInvocation(request, prepared, argv)
}

func (codexAdapter) Decode(stdout []byte) (CanonicalOutput, error) {
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64<<10), maxCanonicalRecordBytes+1)
	terminalCount := 0
	terminalSeen := false
	assistant := ""
	assistantSeen := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
			Item    struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return CanonicalOutput{}, fmt.Errorf("decode Codex JSONL: %w", err)
		}
		switch event.Type {
		case "error", "turn.failed":
			return CanonicalOutput{}, fmt.Errorf("Codex canonical output reported %s", event.Type)
		case "turn.completed":
			terminalCount++
			terminalSeen = true
		case "item.completed":
			if terminalSeen {
				return CanonicalOutput{}, fmt.Errorf("Codex item completed after terminal")
			}
			if event.Item.Type == "agent_message" {
				if len(event.Item.Text) > maxCanonicalRecordBytes {
					return CanonicalOutput{}, fmt.Errorf("Codex assistant result exceeds 1048576 bytes")
				}
				assistant = event.Item.Text
				assistantSeen = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return CanonicalOutput{}, fmt.Errorf("decode Codex JSONL: record exceeds 1048576 bytes: %w", err)
	}
	if terminalCount != 1 {
		return CanonicalOutput{}, fmt.Errorf(
			"Codex canonical output requires exactly one turn.completed, got %d",
			terminalCount,
		)
	}
	if !assistantSeen {
		return CanonicalOutput{}, fmt.Errorf("Codex canonical output has no assistant result")
	}
	return CanonicalOutput{Assistant: assistant}, nil
}

var codexOptions = newOptionParser([]optionSpec{
	{name: "--config", aliases: []string{"-c", "--config"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--enable", aliases: []string{"--enable"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--disable", aliases: []string{"--disable"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--strict-config", aliases: []string{"--strict-config"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--image", aliases: []string{"-i", "--image"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--model", aliases: []string{"-m", "--model"}, arity: arityValue, scope: scopeCommon, kind: kindModel},
	{name: "--oss", aliases: []string{"--oss"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--local-provider", aliases: []string{"--local-provider"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--profile", aliases: []string{"-p", "--profile"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--sandbox", aliases: []string{"-s", "--sandbox"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--dangerously-bypass-approvals-and-sandbox", aliases: []string{"--dangerously-bypass-approvals-and-sandbox"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--dangerously-bypass-hook-trust", aliases: []string{"--dangerously-bypass-hook-trust"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--cd", aliases: []string{"-C", "--cd"}, arity: arityValue, scope: scopeCommon, kind: kindCWD},
	{name: "--add-dir", aliases: []string{"--add-dir"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--ask-for-approval", aliases: []string{"-a", "--ask-for-approval"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--search", aliases: []string{"--search"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "exec", aliases: []string{"exec", "e"}, arity: arityFlag, scope: scopeExec, kind: kindMode},
	{name: "--skip-git-repo-check", aliases: []string{"--skip-git-repo-check"}, arity: arityFlag, scope: scopeExec, kind: kindGeneric},
	{name: "--ephemeral", aliases: []string{"--ephemeral"}, arity: arityFlag, scope: scopeExec, kind: kindStateless},
	{name: "--ignore-user-config", aliases: []string{"--ignore-user-config"}, arity: arityFlag, scope: scopeExec, kind: kindGeneric},
	{name: "--ignore-rules", aliases: []string{"--ignore-rules"}, arity: arityFlag, scope: scopeExec, kind: kindGeneric},
	{name: "--color", aliases: []string{"--color"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--json", aliases: []string{"--json"}, arity: arityFlag, scope: scopeExec, kind: kindOutput},
	{name: "--output-last-message", aliases: []string{"-o", "--output-last-message"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--output-schema", aliases: []string{"--output-schema"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "resume", aliases: []string{"resume"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "fork", aliases: []string{"fork"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
})

func normalizeCodexConfigSelectors(values []parsedOption) []parsedOption {
	result := make([]parsedOption, len(values))
	copy(result, values)
	for index := range result {
		if result[index].spec.name != "--config" ||
			len(result[index].values) != 1 {
			continue
		}
		key, _, exists := strings.Cut(result[index].values[0], "=")
		if !exists {
			continue
		}
		spec := *result[index].spec
		switch key {
		case "model":
			spec.kind = kindModel
		case "model_reasoning_effort":
			spec.kind = kindEffort
		default:
			continue
		}
		result[index].spec = &spec
	}
	return result
}
