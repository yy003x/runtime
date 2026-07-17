package agentrun

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"agent-runtime/internal/provider"
)

type requestFingerprintPayload struct {
	ProjectID         string
	RunType           string
	RunID             string
	Caller            string
	SessionID         string
	TurnID            string
	ExecutionID       string
	ProviderProfile   string
	Provider          string
	ProviderConfig    map[string]any
	CWD               string
	Prompt            string
	PromptFile        string
	RawCLIArgs        []string
	DeadlineSeconds   int
	ResultSchema      string
	ExecutionMode     string
	ProviderOverrides map[string]any
	AllowedActions    []string
	ForbiddenActions  []string
}

func fingerprintRequest(request Request, prompt string, profile provider.Config) (string, error) {
	payload := requestFingerprintPayload{
		ProjectID: request.ProjectID, RunType: request.RunType, RunID: request.RunID, Caller: request.Caller,
		SessionID: request.SessionID, TurnID: request.TurnID, ExecutionID: request.ExecutionID,
		ProviderProfile: request.ProviderProfile, Provider: request.Provider, ProviderConfig: profile.Raw,
		CWD: request.CWD, Prompt: prompt, PromptFile: request.PromptFile,
		RawCLIArgs: request.RawCLIArgs, DeadlineSeconds: request.DeadlineSeconds,
		ResultSchema: request.ResultSchema, ExecutionMode: request.ExecutionMode,
		ProviderOverrides: request.ProviderOverrides, AllowedActions: request.AllowedActions,
		ForbiddenActions: request.ForbiddenActions,
	}
	return fingerprintValue(payload)
}

func fingerprintValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint request: %w", err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func (s *Service) existingRun(paths Paths, desired Request) (RunSummary, bool, error) {
	existingRequest, requestErr := s.store.ReadRequest(paths)
	existingStatus, statusErr := s.store.ReadStatus(paths)
	if os.IsNotExist(requestErr) && os.IsNotExist(statusErr) {
		return RunSummary{}, false, nil
	}
	if requestErr != nil || statusErr != nil {
		return RunSummary{}, true, fmt.Errorf("idempotency conflict for run_id %s: existing run is incomplete", desired.RunID)
	}
	if existingRequest.RequestFingerprint == "" {
		return summary(paths, existingStatus, false), true, fmt.Errorf("idempotency conflict for run_id %s: legacy request has no fingerprint; use --force or a new run_id", desired.RunID)
	}
	if existingRequest.RequestFingerprint != desired.RequestFingerprint {
		return summary(paths, existingStatus, false), true, fmt.Errorf("idempotency conflict for run_id %s: request differs from existing run", desired.RunID)
	}
	return summary(paths, existingStatus, true), true, nil
}
