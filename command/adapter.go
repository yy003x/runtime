package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type Mode string

const (
	ModeInteractive Mode = "interactive"
	ModeExec        Mode = "exec"
)

type OutputProtocol string

const (
	OutputNative    OutputProtocol = "native"
	OutputCanonical OutputProtocol = "canonical"
)

type Overrides struct {
	Model  *string
	Effort *Effort
	CWD    *string
}

type BuildRequest struct {
	Mode                 Mode
	OutputProtocol       OutputProtocol
	Profile              Profile
	Overrides            Overrides
	ArgvPrompt           *string
	InheritedEnvironment []string
	InvocationBase       string
	Symbolic             bool
}

type Invocation struct {
	Path        string
	Argv        []string
	Environment []string
	CWD         string
}

type CanonicalOutput struct {
	Assistant string
}

type Adapter interface {
	Name() string
	Build(BuildRequest) (Invocation, error)
	Decode([]byte) (CanonicalOutput, error)
}

func Resolve(command string) (Adapter, error) {
	switch filepath.Base(command) {
	case "codex":
		return codexAdapter{}, nil
	case "claude":
		return claudeAdapter{}, nil
	default:
		return nil, fmt.Errorf("no command adapter for %q", filepath.Base(command))
	}
}

func Build(request BuildRequest) (Invocation, error) {
	if err := request.Profile.Validate(); err != nil {
		return Invocation{}, typedBuildError(err)
	}
	adapter, err := Resolve(request.Profile.Command)
	if err != nil {
		return Invocation{}, typedBuildError(err)
	}
	invocation, err := adapter.Build(request)
	if err != nil {
		return Invocation{}, typedBuildError(err)
	}
	return invocation, nil
}

func Decode(command string, stdout []byte) (CanonicalOutput, error) {
	adapter, err := Resolve(command)
	if err != nil {
		return CanonicalOutput{}, typedDecodeError(err)
	}
	output, err := adapter.Decode(stdout)
	if err != nil {
		return CanonicalOutput{}, typedDecodeError(err)
	}
	return output, nil
}

// CheckProfile validates all adapter plans without resolving environment,
// files, cwd, PATH, command existence, or argv budgets.
func CheckProfile(profile Profile) error {
	if err := profile.Validate(); err != nil {
		return typedBuildError(err)
	}
	placeholder := "__SN_CLI_SYMBOLIC_PROMPT__"
	for _, plan := range []struct {
		mode     Mode
		protocol OutputProtocol
	}{
		{ModeInteractive, OutputNative},
		{ModeExec, OutputNative},
		{ModeExec, OutputCanonical},
	} {
		if _, err := Build(BuildRequest{
			Mode:           plan.mode,
			OutputProtocol: plan.protocol,
			Profile:        profile,
			ArgvPrompt:     &placeholder,
			InvocationBase: "/__sn_cli_symbolic_cwd__",
			Symbolic:       true,
		}); err != nil {
			return fmt.Errorf(
				"%s/%s invocation plan: %w",
				plan.mode, plan.protocol, err,
			)
		}
	}
	return nil
}

type optionScope uint8

const (
	scopeCommon optionScope = iota
	scopeExec
)

type optionKind uint8

const (
	kindGeneric optionKind = iota
	kindModel
	kindEffort
	kindMode
	kindOutput
	kindStateless
	kindCWD
	kindCanonicalForbidden
	kindInput
)

const (
	arityFlag     = 0
	arityValue    = 1
	arityOptional = -1
	arityVariadic = -2
)

type optionSpec struct {
	name          string
	aliases       []string
	arity         int
	scope         optionScope
	kind          optionKind
	minValues     int
	forbidInCanon bool
}

type parsedOption struct {
	spec   *optionSpec
	tokens []string
	values []string
}

type optionParser struct {
	specs   []optionSpec
	aliases map[string]int
	modes   map[string]int
}

func newOptionParser(specs []optionSpec) optionParser {
	aliases := make(map[string]int)
	modes := make(map[string]int)
	for index := range specs {
		for _, alias := range specs[index].aliases {
			aliases[alias] = index
			if specs[index].kind == kindMode {
				modes[alias] = index
			}
		}
	}
	return optionParser{specs: specs, aliases: aliases, modes: modes}
}

func (parser optionParser) parse(args []string) ([]parsedOption, error) {
	result := make([]parsedOption, 0, len(args))
	for index := 0; index < len(args); index++ {
		token := args[index]
		if specIndex, exists := parser.modes[token]; exists {
			spec := &parser.specs[specIndex]
			result = append(result, parsedOption{
				spec: spec, tokens: []string{token},
			})
			continue
		}
		if token == "--" {
			return nil, fmt.Errorf("Profile args must not contain prompt terminator")
		}
		alias, attached, hasAttached := splitOptionToken(token)
		specIndex, exists := parser.aliases[alias]
		if !exists {
			return nil, fmt.Errorf("unsupported Profile argument %q", token)
		}
		spec := &parser.specs[specIndex]
		parsed := parsedOption{spec: spec, tokens: []string{token}}
		switch spec.arity {
		case arityFlag:
			if hasAttached {
				return nil, fmt.Errorf("%s does not accept a value", alias)
			}
		case arityValue:
			if hasAttached {
				if attached == "" {
					return nil, fmt.Errorf("%s requires a value", alias)
				}
				parsed.values = []string{attached}
			} else {
				index++
				if index >= len(args) ||
					args[index] == "--" ||
					parser.isRegisteredOption(args[index]) {
					return nil, fmt.Errorf("%s requires a value", alias)
				}
				parsed.tokens = append(parsed.tokens, args[index])
				parsed.values = []string{args[index]}
			}
		case arityOptional:
			if hasAttached {
				parsed.values = []string{attached}
			} else if index+1 < len(args) &&
				!parser.isRegisteredOption(args[index+1]) &&
				args[index+1] != "--" {
				index++
				parsed.tokens = append(parsed.tokens, args[index])
				parsed.values = []string{args[index]}
			}
		case arityVariadic:
			if hasAttached {
				parsed.values = append(parsed.values, attached)
			}
			for index+1 < len(args) &&
				args[index+1] != "--" &&
				!parser.isRegisteredOption(args[index+1]) {
				index++
				parsed.tokens = append(parsed.tokens, args[index])
				parsed.values = append(parsed.values, args[index])
			}
			if len(parsed.values) < spec.minValues {
				return nil, fmt.Errorf("%s requires at least %d value(s)", alias, spec.minValues)
			}
		default:
			return nil, fmt.Errorf("invalid adapter option arity")
		}
		result = append(result, parsed)
	}
	return result, nil
}

func (parser optionParser) isRegisteredOption(value string) bool {
	if _, exists := parser.modes[value]; exists {
		return true
	}
	alias, _, _ := splitOptionToken(value)
	_, exists := parser.aliases[alias]
	return exists
}

func splitOptionToken(value string) (string, string, bool) {
	if strings.HasPrefix(value, "--") {
		if name, attached, exists := strings.Cut(value, "="); exists {
			return name, attached, true
		}
	}
	return value, "", false
}

type optionPlan struct {
	common    []string
	exec      []string
	model     []parsedOption
	effort    []parsedOption
	output    []parsedOption
	stateless []parsedOption
	modeCount int
	input     []parsedOption
	forbidden []parsedOption
}

func classifyOptions(
	parsed []parsedOption,
	mode Mode,
	protocol OutputProtocol,
) (optionPlan, error) {
	var plan optionPlan
	for _, item := range parsed {
		if protocol == OutputCanonical &&
			(item.spec.forbidInCanon ||
				item.spec.kind == kindCanonicalForbidden) {
			return optionPlan{}, fmt.Errorf(
				"%s is incompatible with canonical output", item.spec.name,
			)
		}
		switch item.spec.kind {
		case kindModel:
			plan.model = append(plan.model, item)
		case kindEffort:
			plan.effort = append(plan.effort, item)
		case kindMode:
			plan.modeCount++
		case kindOutput:
			plan.output = append(plan.output, item)
		case kindStateless:
			plan.stateless = append(plan.stateless, item)
		case kindCWD:
			return optionPlan{}, fmt.Errorf(
				"%s is not allowed; use the typed cwd field", item.spec.name,
			)
		case kindInput:
			plan.input = append(plan.input, item)
		case kindCanonicalForbidden:
			plan.forbidden = append(plan.forbidden, item)
			if item.spec.scope == scopeCommon {
				plan.common = append(plan.common, item.tokens...)
			} else if mode == ModeExec {
				plan.exec = append(plan.exec, item.tokens...)
			}
		case kindGeneric:
			if item.spec.scope == scopeCommon {
				plan.common = append(plan.common, item.tokens...)
			} else if mode == ModeExec {
				plan.exec = append(plan.exec, item.tokens...)
			}
		}
	}
	return plan, nil
}

func effectiveTyped(request BuildRequest) (string, Effort, string, error) {
	model := request.Profile.Model
	if request.Overrides.Model != nil {
		model = *request.Overrides.Model
	}
	if model != "" {
		if err := validateTextToken("model", model, MaxTokenBytes, false); err != nil {
			return "", "", "", err
		}
	}
	effort := request.Profile.Effort
	if request.Overrides.Effort != nil {
		effort = *request.Overrides.Effort
	}
	if effort != "" {
		if _, err := ParseEffort(string(effort)); err != nil {
			return "", "", "", err
		}
	}
	cwd := request.Profile.CWD
	if request.Overrides.CWD != nil {
		cwd = *request.Overrides.CWD
	}
	return model, effort, cwd, nil
}

func appendPrompt(argv []string, request BuildRequest) ([]string, error) {
	if request.ArgvPrompt == nil {
		return argv, nil
	}
	if len(*request.ArgvPrompt) > MaxTokenBytes {
		return nil, &invocationLimitError{message: fmt.Sprintf(
			"prompt exceeds %d bytes", MaxTokenBytes,
		)}
	}
	if err := validateTextToken(
		"prompt", *request.ArgvPrompt, MaxTokenBytes, true,
	); err != nil {
		return nil, err
	}
	return append(argv, "--", *request.ArgvPrompt), nil
}

func oneSelector(
	name string,
	values []parsedOption,
	overridePresent bool,
) (*parsedOption, error) {
	if overridePresent {
		return nil, nil
	}
	if len(values) > 1 {
		return nil, fmt.Errorf("%s is configured multiple times", name)
	}
	if len(values) == 0 {
		return nil, nil
	}
	return &values[0], nil
}

const maxCanonicalRecordBytes = 1 << 20

func decodeSingleJSON(stdout []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode canonical JSON: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("canonical stdout contains multiple JSON documents")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode canonical JSON trailing data: %w", err)
	}
	return nil
}
