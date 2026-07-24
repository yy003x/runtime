package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/provider"
)

// runUnrecordedProfile executes an explicit batch or direct API/native request
// without creating Run or Session artifacts. CLI profile arguments are native
// command arguments and are never parsed as Runtime options.
func runUnrecordedProfile(cfg *config.Config, profile provider.Config, args []string) (int, error) {
	prompt := ""
	rawArgs := []string(nil)
	overrides := map[string]any{}
	var err error
	switch profile.Type {
	case provider.TypeCLI:
		rawArgs = append([]string(nil), args...)
		if stdinHasPrompt() {
			if err := applyStdinPrompt(&prompt, ""); err != nil {
				return 1, fmt.Errorf("profile %q: %w", profile.ID, err)
			}
		}
	case provider.TypeAPI:
		prompt, overrides, err = parseAPIProviderArgs(args)
		if err == nil {
			err = applyStdinPrompt(&prompt, "")
		}
	case provider.TypeNative:
		prompt, err = parseSinglePromptArgs("native", args)
		if err == nil {
			err = applyStdinPrompt(&prompt, "")
		}
	default:
		err = fmt.Errorf("unsupported provider type %q", profile.Type)
	}
	if err != nil {
		return 1, fmt.Errorf("profile %q: %w", profile.ID, err)
	}
	if profile.Type != provider.TypeCLI && strings.TrimSpace(prompt) == "" {
		return 1, fmt.Errorf("profile %q: prompt is required", profile.ID)
	}
	return executeUnrecordedProfile(cfg, profile, prompt, rawArgs, overrides)
}

func executeUnrecordedProfile(cfg *config.Config, profile provider.Config, prompt string, rawArgs []string, overrides map[string]any) (int, error) {
	if profile.Type == provider.TypeCLI && profile.CLI != nil && profile.CLI.Executor == provider.ExecutorTmux {
		return 1, fmt.Errorf("profile %q uses tmux; use 'session run %s' or 'session open %s'", profile.ID, profile.ID, profile.ID)
	}

	service := agentrun.New(cfg.Home)
	loadedProfiles, err := service.Profiles()
	if err != nil {
		return 1, err
	}
	executionContext := context.Background()
	cancel := func() {}
	timeout := profile.TimeoutSeconds
	if timeout == 0 {
		timeout = service.DefaultDeadline
	}
	if timeout > 0 {
		executionContext, cancel = context.WithTimeout(executionContext, time.Duration(timeout)*time.Second)
	}
	defer cancel()
	profiles := map[string]provider.Config{profile.ID: profile}
	if profile.Type == provider.TypeNative || profile.Type == provider.TypeAPI && profile.API != nil && profile.API.Runtime != nil && profile.API.Runtime.Enabled {
		profiles = loadedProfiles
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	paths := cfg.Paths
	if paths.Home == "" {
		paths, err = layout.FromHome(cfg.Home)
		if err != nil {
			return 1, err
		}
	}

	directID := "direct-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	snapshotFile := ""
	stateful := profile.Type == provider.TypeNative || profile.Type == provider.TypeAPI && profile.API != nil && profile.API.Runtime != nil && profile.API.Runtime.Enabled
	if stateful {
		temporaryRoot := paths.TmpDir
		if temporaryRoot == "" {
			temporaryRoot = os.TempDir()
		}
		if err := os.MkdirAll(temporaryRoot, 0o700); err != nil {
			return 1, err
		}
		temporary, tempErr := os.MkdirTemp(temporaryRoot, "direct-")
		if tempErr != nil {
			return 1, tempErr
		}
		defer os.RemoveAll(temporary)
		snapshotFile = filepath.Join(temporary, "snapshot.json")
	}

	selected, err := provider.Select(profile)
	if err != nil {
		return 1, err
	}
	prepared, err := selected.Prepare(executionContext, profile, provider.Request{
		Prompt: prompt, RawCLIArgs: rawArgs, Overrides: overrides, CWD: cwd,
		Daemon: service.DaemonClient(), Profiles: profiles, RunID: directID, SnapshotFile: snapshotFile,
		PersonaDir: paths.PersonaDir, SkillDir: paths.SkillsDir, ToolDir: paths.ToolsDir,
		MemoryFile: paths.MemoryFile, MemoryCandidateFile: paths.MemoryCandidatesFile,
	})
	if err != nil {
		return 1, err
	}
	sink := &directTerminalSink{finalText: profile.Type != provider.TypeCLI}
	result, err := selected.Execute(executionContext, prepared, sink)
	if flushErr := sink.flushResult(result); err == nil && flushErr != nil {
		err = flushErr
	}
	if err != nil {
		return 1, err
	}
	if result.State == "blocked" {
		if strings.TrimSpace(result.BlockedReason) != "" {
			fmt.Fprintln(os.Stderr, result.BlockedReason)
		}
		return 1, nil
	}
	if result.State == "cancelled" || result.ExitCode < 0 {
		return 1, nil
	}
	return result.ExitCode, nil
}

func parseSinglePromptArgs(owner string, args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		if strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("%s prompt must not be empty", owner)
		}
		return args[0], nil
	default:
		return "", fmt.Errorf("%s prompt must be one quoted positional argument or stdin", owner)
	}
}

func parseAPIProviderArgs(args []string) (string, map[string]any, error) {
	overrides := map[string]any{}
	prompt := ""
	for index := 0; index < len(args); index++ {
		name := args[index]
		switch name {
		case "--model":
			index++
			if index >= len(args) || strings.TrimSpace(args[index]) == "" {
				return "", nil, fmt.Errorf("--model requires value")
			}
			overrides["model"] = args[index]
		case "--max-tokens":
			index++
			if index >= len(args) {
				return "", nil, fmt.Errorf("--max-tokens requires value")
			}
			value, err := strconv.Atoi(args[index])
			if err != nil || value <= 0 {
				return "", nil, fmt.Errorf("--max-tokens must be a positive integer")
			}
			overrides["max_tokens"] = value
		case "--temperature":
			index++
			if index >= len(args) {
				return "", nil, fmt.Errorf("--temperature requires value")
			}
			value, err := strconv.ParseFloat(args[index], 64)
			if err != nil {
				return "", nil, fmt.Errorf("--temperature must be a number")
			}
			overrides["temperature"] = value
		case "--stream":
			overrides["stream"] = true
		case "--no-stream":
			overrides["stream"] = false
		default:
			if strings.HasPrefix(name, "-") {
				return "", nil, fmt.Errorf("unknown API provider option: %s", name)
			}
			if index != len(args)-1 {
				return "", nil, fmt.Errorf("API prompt must be the final quoted positional argument")
			}
			prompt = name
		}
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = ""
	}
	return prompt, overrides, nil
}

func parseSessionProviderInput(profile provider.Config, args []string, externalPrompt bool) (string, []string, map[string]any, error) {
	switch profile.Type {
	case provider.TypeCLI:
		if externalPrompt {
			return "", append([]string(nil), args...), map[string]any{}, nil
		}
		if len(args) == 0 {
			return "", nil, nil, fmt.Errorf("CLI session prompt is required as the final argument, --prompt-file, or stdin")
		}
		prompt := args[len(args)-1]
		if strings.TrimSpace(prompt) == "" {
			return "", nil, nil, fmt.Errorf("CLI session prompt must not be empty")
		}
		return prompt, append([]string(nil), args[:len(args)-1]...), map[string]any{}, nil
	case provider.TypeAPI:
		prompt, overrides, err := parseAPIProviderArgs(args)
		if err != nil {
			return "", nil, nil, err
		}
		if externalPrompt && strings.TrimSpace(prompt) != "" {
			return "", nil, nil, fmt.Errorf("positional prompt, --prompt-file, and stdin are mutually exclusive")
		}
		if !externalPrompt && strings.TrimSpace(prompt) == "" {
			return "", nil, nil, fmt.Errorf("API session prompt is required as the final argument, --prompt-file, or stdin")
		}
		return prompt, nil, overrides, nil
	case provider.TypeNative:
		prompt, err := parseSinglePromptArgs("native", args)
		if err != nil {
			return "", nil, nil, err
		}
		if externalPrompt && strings.TrimSpace(prompt) != "" {
			return "", nil, nil, fmt.Errorf("positional prompt, --prompt-file, and stdin are mutually exclusive")
		}
		if !externalPrompt && strings.TrimSpace(prompt) == "" {
			return "", nil, nil, fmt.Errorf("native session prompt is required as one quoted argument, --prompt-file, or stdin")
		}
		return prompt, nil, map[string]any{}, nil
	default:
		return "", nil, nil, fmt.Errorf("unsupported provider type %q", profile.Type)
	}
}

type directTerminalSink struct {
	mu        sync.Mutex
	stdout    bool
	stderr    bool
	finalText bool
}

func (s *directTerminalSink) Stdout(value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(value) == 0 {
		return nil
	}
	if s.finalText {
		return nil
	}
	s.stdout = true
	_, err := os.Stdout.Write(value)
	return err
}

func (s *directTerminalSink) Stderr(value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(value) == 0 {
		return nil
	}
	s.stderr = true
	_, err := os.Stderr.Write(value)
	return err
}

func (*directTerminalSink) Event(provider.Event) error             { return nil }
func (*directTerminalSink) StatusPatch(provider.StatusPatch) error { return nil }

func (s *directTerminalSink) flushResult(result provider.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalText {
		value := result.FinalText
		if strings.TrimSpace(value) == "" {
			value = result.Stdout
		}
		if value != "" {
			if _, err := os.Stdout.WriteString(value); err != nil {
				return err
			}
			if !strings.HasSuffix(value, "\n") {
				if _, err := os.Stdout.WriteString("\n"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if !s.stdout && result.Stdout != "" {
		if _, err := os.Stdout.WriteString(result.Stdout); err != nil {
			return err
		}
	}
	if !s.stderr && result.Stderr != "" {
		if _, err := os.Stderr.WriteString(result.Stderr); err != nil {
			return err
		}
	}
	return nil
}
