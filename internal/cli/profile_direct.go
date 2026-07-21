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

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/layout"
	"agent-runtime/internal/provider"
)

// runPromptProfile executes a one-shot profile without creating Run or Session
// artifacts. Persistent history is owned exclusively by the session namespace.
func runPromptProfile(cfg *config.Config, profile provider.Config, args []string) (int, error) {
	prompt, rawArgs, err := parseProfilePrompt(args)
	if err != nil {
		return 1, fmt.Errorf("profile %q: %w", profile.ID, err)
	}
	return executePromptProfile(cfg, profile, prompt, rawArgs)
}

func executePromptProfile(cfg *config.Config, profile provider.Config, prompt string, rawArgs []string) (int, error) {
	if profile.Type == provider.TypeCLI && profile.CLI != nil && profile.CLI.Executor == provider.ExecutorTmux {
		return 1, fmt.Errorf("profile %q uses tmux; use 'session run %s' or 'session open %s'", profile.ID, profile.ID, profile.ID)
	}

	service := agentrun.New(cfg.Home)
	profiles := map[string]provider.Config{profile.ID: profile}
	if profile.Type == provider.TypeNative || profile.Type == provider.TypeAPI && profile.API != nil && profile.API.Runtime != nil && profile.API.Runtime.Enabled {
		loadedProfiles, loadErr := service.Profiles()
		if loadErr != nil {
			return 1, loadErr
		}
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
	prepared, err := selected.Prepare(context.Background(), profile, provider.Request{
		Prompt: prompt, RawCLIArgs: rawArgs, Overrides: map[string]any{}, CWD: cwd,
		Daemon: service.DaemonClient(), Profiles: profiles, RunID: directID, SnapshotFile: snapshotFile,
		PersonaDir: paths.PersonaDir, SkillDir: paths.SkillsDir, ToolDir: paths.ToolsDir,
		MemoryFile: paths.MemoryFile, MemoryCandidateFile: paths.MemoryCandidatesFile,
	})
	if err != nil {
		return 1, err
	}
	sink := &directTerminalSink{finalText: profile.Type != provider.TypeCLI}
	result, err := selected.Execute(context.Background(), prepared, sink)
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

func parseProfilePrompt(args []string) (string, []string, error) {
	promptArgs, rawArgs := args, []string(nil)
	for index, value := range args {
		if value == "--" {
			promptArgs = args[:index]
			rawArgs = append([]string(nil), args[index+1:]...)
			break
		}
	}
	for _, value := range promptArgs {
		if value == "--help" || value == "-h" || value == "--version" {
			return "", nil, fmt.Errorf("target CLI arguments must follow --")
		}
	}
	prompt := strings.TrimSpace(strings.Join(promptArgs, " "))
	if err := applyStdinPrompt(&prompt, ""); err != nil {
		return "", nil, err
	}
	if prompt == "" {
		return "", nil, fmt.Errorf("prompt is required")
	}
	return prompt, rawArgs, nil
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
