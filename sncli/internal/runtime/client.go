package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-arch/internal/agentrun"
	"agent-arch/internal/provider"
)

// Client keeps the small interface used by the interactive REPL while routing
// every request to the Go runtime in this repository.
type Client struct {
	Root string
}

type RunOptions struct {
	Provider string
	Prompt   string
	CWD      string
	Sandbox  string
	Timeout  int
	Model    string
}

type RunResult struct {
	RunID           string            `json:"run_id"`
	Provider        string            `json:"provider"`
	Requested       string            `json:"requested_provider"`
	Outcome         string            `json:"outcome"`
	ReturnCode      int               `json:"returncode"`
	FinalText       string            `json:"final_text"`
	Artifacts       map[string]string `json:"artifacts"`
	BlockedReason   string            `json:"blocked_reason"`
	FailureReason   string            `json:"failure_reason"`
	DurationSeconds float64           `json:"duration_seconds"`
}

func (c Client) ProvidersJSON() ([]byte, error) {
	profiles, err := agentrun.New(c.Root).Profiles()
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"ok": true, "source": "configs", "profiles": profiles})
}

func (c Client) DoctorJSON() ([]byte, error) {
	service := agentrun.New(c.Root)
	profiles, err := service.Profiles()
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"ok": true, "runtime": service.RuntimeVersion, "runs_dir": service.RunsDir,
		"default_profile": service.DefaultProfile, "profile_count": len(profiles),
	})
}

func (c Client) Run(opts RunOptions) (*RunResult, error) {
	service := agentrun.New(c.Root)
	profileName := opts.Provider
	if profileName == "" {
		profileName = service.DefaultProfile
	}
	profiles, err := service.Profiles()
	if err != nil {
		return nil, err
	}
	profile, ok := provider.Resolve(profiles, profileName)
	if !ok {
		return nil, fmt.Errorf("unknown provider profile: %s", profileName)
	}
	overrides := map[string]any{}
	if opts.Model != "" {
		overrides["model"] = opts.Model
	}
	if opts.Sandbox != "" && profile.CLI != nil && profile.CLI.Driver == "codex" {
		overrides["sandbox_mode"] = opts.Sandbox
	}
	summary, runErr := service.Run(context.Background(), agentrun.RunOptions{
		RunType: agentrun.RunTask, Profile: profileName, Prompt: opts.Prompt, CWD: opts.CWD,
		ExecutionMode: agentrun.ModeCapture, DeadlineSeconds: opts.Timeout, ProviderOverrides: overrides,
	})
	result := &RunResult{
		RunID: summary.RunID, Requested: profileName, Provider: profile.ID,
		Outcome: outcomeFromState(summary.State), Artifacts: map[string]string{"run_dir": summary.RunDir},
	}
	if summary.RunID != "" {
		if contract, err := service.ReadResult(agentrun.RunTask, summary.RunID); err == nil {
			result.Outcome = contract.Outcome
			result.FinalText = contract.Summary
		}
		if status, err := service.Status(agentrun.RunTask, summary.RunID); err == nil {
			result.FailureReason = status.FailureReason
			if status.State == agentrun.StateBlocked {
				result.BlockedReason = status.FailureReason
			}
			if code, ok := status.ProviderStatus["returncode"].(float64); ok {
				result.ReturnCode = int(code)
			} else if code, ok := status.ProviderStatus["returncode"].(int); ok {
				result.ReturnCode = code
			}
		}
	}
	if runErr != nil {
		if result.FailureReason == "" {
			result.FailureReason = runErr.Error()
		}
		return result, runErr
	}
	if result.FinalText == "" && result.Outcome == agentrun.OutcomeSucceeded {
		return result, fmt.Errorf("runtime returned empty final text")
	}
	return result, nil
}

func outcomeFromState(state string) string {
	switch state {
	case agentrun.StateDone:
		return agentrun.OutcomeSucceeded
	case agentrun.StateBlocked:
		return agentrun.OutcomeBlocked
	case agentrun.StateCancelled:
		return agentrun.OutcomeCancelled
	default:
		return agentrun.OutcomeFailed
	}
}
