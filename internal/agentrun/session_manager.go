package agentrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/capability"
	"github.com/yy003x/runtime/internal/provider"
)

type SessionManager struct {
	service *Service
	store   *SessionStore
}

func NewSessionManager(service *Service) *SessionManager {
	return &SessionManager{service: service, store: NewSessionStore(service.paths.SessionsDir, service.paths.HistoryDir, service.paths.SessionStateDir)}
}

func (m *SessionManager) Store() *SessionStore { return m.store }

func (m *SessionManager) EnsureSession(sessionID, projectID, cwd, title string, decision RecordDecision) (SessionRecord, error) {
	if decision.RecordMode == RecordOff {
		return SessionRecord{}, fmt.Errorf("cannot create a Session with record_mode=off")
	}
	if sessionID == "" {
		sessionID = newRunID(RunSession)
	}
	if existing, err := m.store.Get(sessionID); err == nil {
		if projectID != "" && existing.ProjectID != "" && existing.ProjectID != projectID {
			return SessionRecord{}, fmt.Errorf("session %s belongs to project %s", sessionID, existing.ProjectID)
		}
		return existing, nil
	}
	if decision.RecordMode != RecordFull {
		title = ""
	}
	return m.store.Create(SessionRecord{SessionID: sessionID, ProjectID: projectID, CWD: cwd, Title: truncateHistoryText(title, 120),
		RecordMode: decision.RecordMode, Retention: decision.Retention, CaptureQuality: decision.CaptureQuality})
}

func (m *SessionManager) BeginRun(
	request Request,
	prompt string,
	contextPrompt string,
	profile provider.Config,
	capacity provider.ContextCapacity,
	staticEstimate provider.StaticContextEstimate,
) (contextProjection, error) {
	if request.RecordMode == RecordOff || request.SessionID == "" {
		return contextProjection{}, nil
	}
	decision := RecordDecision{RecordMode: request.RecordMode, Retention: request.Retention, CaptureQuality: request.CaptureQuality}
	if _, err := m.EnsureSession(request.SessionID, request.ProjectID, request.CWD, prompt, decision); err != nil {
		return contextProjection{}, err
	}
	if _, err := m.store.ConfigureSession(request.SessionID, sessionRuntimeName(request.ExecutionKind, profile), request.ProviderProfile); err != nil {
		return contextProjection{}, err
	}
	manifest, projection, err := m.compileContext(
		request.SessionID,
		request.TurnID,
		request.CWD,
		profile,
		capacity,
		staticEstimate,
		prompt,
		contextPrompt,
		request.AllowedActions,
		request.ForbiddenActions,
	)
	policy, _ := json.Marshal(map[string]any{"allowed_actions": request.AllowedActions, "forbidden_actions": request.ForbiddenActions})
	manifest.PolicyDigest = digestBytes(policy)
	manifest.MemoryReads = append(manifest.MemoryReads, request.MemoryReads...)
	if _, err := m.store.AddTurn(request.SessionID, TurnRecord{
		TurnID: request.TurnID, Runtime: sessionRuntimeName(request.ExecutionKind, profile), Provider: request.Provider,
		Profile: request.ProviderProfile, Model: requestModel(request, profile), RecordMode: request.RecordMode,
		CaptureQuality: request.CaptureQuality,
	}, prompt, manifest); err != nil {
		return projection, err
	}
	execution := ExecutionRecord{ExecutionID: request.ExecutionID, Kind: request.ExecutionKind, Profile: request.ProviderProfile,
		Provider: request.Provider, State: StateRunning, CaptureQuality: request.CaptureQuality, RunIDs: []string{request.RunID}, TurnIDs: []string{request.TurnID}}
	if err := m.store.UpsertExecution(request.SessionID, execution); err != nil {
		return projection, err
	}
	if attemptErr := m.store.AddAttempt(request.SessionID, RunAttemptRecord{
		RunID: request.RunID, TurnID: request.TurnID,
		RunType: request.RunType, ExecutionID: request.ExecutionID, Attempt: 1,
		Provider: request.Provider, Profile: request.ProviderProfile,
	}); attemptErr != nil {
		return projection, attemptErr
	}
	return projection, err
}

func (m *SessionManager) CompleteRun(
	request Request,
	state, failureReason, output string,
	artifacts []map[string]any,
) error {
	if request.RecordMode == RecordOff || request.SessionID == "" {
		return nil
	}
	var resultRef *ResultRef
	if request.ResultFile != "" {
		if _, err := os.Stat(request.ResultFile); err == nil {
			digest, _ := digestFile(request.ResultFile)
			resultRef = &ResultRef{RunID: request.RunID, RunType: request.RunType, ResultFile: request.ResultFile, ResultDigest: digest}
		}
	}
	if err := m.store.CompleteRun(
		request.SessionID, request.TurnID, request.RunID, state, failureReason,
		output, artifacts, resultRef,
	); err != nil {
		return err
	}
	execution := ExecutionRecord{ExecutionID: request.ExecutionID, Kind: request.ExecutionKind, Profile: request.ProviderProfile,
		Provider: request.Provider, State: state, CaptureQuality: request.CaptureQuality, RunIDs: []string{request.RunID}, TurnIDs: []string{request.TurnID}, ResultRef: resultRef}
	return m.store.UpsertExecution(request.SessionID, execution)
}

func (m *SessionManager) ResumeRun(request Request) error {
	if request.RecordMode == RecordOff || request.SessionID == "" {
		return nil
	}
	if err := m.store.ResumeRun(request.SessionID, request.TurnID, request.RunID); err != nil {
		return err
	}
	return m.store.UpsertExecution(request.SessionID, ExecutionRecord{ExecutionID: request.ExecutionID, Kind: request.ExecutionKind,
		Profile: request.ProviderProfile, Provider: request.Provider, State: StateRunning, CaptureQuality: request.CaptureQuality,
		RunIDs: []string{request.RunID}, TurnIDs: []string{request.TurnID}})
}

func (m *SessionManager) CompileContext(
	sessionID, turnID, cwd string,
	profile provider.Config,
	prompt, contextPrompt string,
	allowed, forbidden []string,
) (ContextManifest, error) {
	capacity, err := profile.ResolveContextCapacity(nil)
	if err != nil {
		return ContextManifest{}, err
	}
	memoryFile, _ := m.MemoryPaths(sessionID)
	staticEstimate, err := provider.EstimateStaticContext(context.Background(), profile, provider.ContextEstimateRequest{
		Prompt: prompt, PersonaDir: m.service.PersonaDir,
		SkillDir: m.service.paths.SkillsDir, ToolDir: m.service.paths.ToolsDir,
		MemoryFile: memoryFile, Allowed: allowed, Forbidden: forbidden,
	})
	if err != nil {
		return ContextManifest{}, err
	}
	manifest, _, err := m.compileContext(
		sessionID, turnID, cwd, profile, capacity, staticEstimate,
		prompt, contextPrompt, allowed, forbidden,
	)
	return manifest, err
}

func (m *SessionManager) compileContext(
	sessionID, turnID, cwd string,
	profile provider.Config,
	capacity provider.ContextCapacity,
	staticEstimate provider.StaticContextEstimate,
	prompt, contextPrompt string,
	allowed, forbidden []string,
) (ContextManifest, contextProjection, error) {
	projection, projectionErr := m.projectContext(
		sessionID, turnID, profile.ID, capacity, staticEstimate, contextPrompt,
	)
	effectiveCapacity := projection.Capacity
	estimationComplete := len(staticEstimate.Unknown) == 0
	manifest := ContextManifest{SchemaVersion: SessionSchemaVersion, SessionID: sessionID, TurnID: turnID,
		CreatedAt: time.Now().UTC(), CWD: cwd, Profile: profile.ID, MessageRange: SequenceRange{After: 0, To: 0},
		ContextWindowTokens: effectiveCapacity.ContextWindowTokens, ReservedOutputTokens: effectiveCapacity.ReservedOutputTokens,
		InputBudgetTokens: effectiveCapacity.InputBudgetTokens, EstimatedInputTokens: projection.EstimatedInputTokens,
		CompactionAtTokens: effectiveCapacity.CompactionAtTokens, KeepRecentTurns: effectiveCapacity.KeepRecentTurns,
		SummaryEnabled: effectiveCapacity.SummaryEnabled, CapacitySource: effectiveCapacity.CapacitySource,
		Compacted: projection.Checkpoint != nil, PressureState: projection.PressureState,
		EstimationComplete: &estimationComplete, EstimatorSource: "utf8_heuristic_v1",
		StaticContextDigest: staticEstimate.Snapshot.Digest,
		CountedComponents:   append([]provider.ContextEstimateComponent(nil), staticEstimate.Counted...),
		UnknownComponents:   append([]provider.ContextUnknownComponent(nil), staticEstimate.Unknown...),
	}
	if len(projection.SourceMessages) > 0 {
		manifest.MessageRange.After = projection.SourceMessages[0].Sequence - 1
		manifest.MessageRange.To = projection.SourceMessages[len(projection.SourceMessages)-1].Sequence
	}
	if encoded, err := json.Marshal(projection.Messages); err == nil {
		manifest.MessageDigest = digestBytes(encoded)
	}
	if projection.Checkpoint != nil {
		path, digest, err := m.writeContextCheckpoint(*projection.Checkpoint)
		if err != nil {
			return ContextManifest{}, projection, err
		}
		manifest.SummaryRef, manifest.SummaryDigest = path, digest
		manifest.SummaryDigestKind = "stable_json_sha256"
		manifest.SummaryRange = projection.Checkpoint.CoveredMessageRange
	}
	if encoded, err := json.Marshal(profile.Raw); err == nil {
		manifest.ConfigDigest = digestBytes(encoded)
	}
	if projectionErr != nil {
		manifest.ProjectionError = projectionErr.Error()
		return manifest, projection, projectionErr
	}
	memoryFile, candidateFile := m.MemoryPaths(sessionID)
	registry := capability.NewRegistry(capability.RegistryConfig{
		SkillsDir: m.service.paths.SkillsDir, ToolsDir: m.service.paths.ToolsDir,
		MemoryFile: memoryFile, MemoryCandidatesFile: candidateFile,
	})
	skillManager := registry.Skills
	selected := map[string]capability.Skill{}
	if profile.API != nil && profile.API.Runtime != nil {
		for _, name := range profile.API.Runtime.Skills {
			if name == "*" {
				for _, skill := range skillManager.List() {
					selected[skill.Name] = skill
				}
				continue
			}
			if skill, err := skillManager.Get(name); err == nil {
				selected[skill.Name] = skill
			}
		}
		if profile.API.Runtime.AutoRouteSkills {
			if skill, ok := skillManager.Route(prompt); ok {
				selected[skill.Name] = skill
			}
		}
	}
	for _, skill := range selected {
		manifest.Skills = append(manifest.Skills, ContextAssetRef{ID: skill.Name, Digest: digestPath(skill.Path), Source: skill.Path})
	}
	sort.Slice(manifest.Skills, func(i, j int) bool { return manifest.Skills[i].ID < manifest.Skills[j].ID })
	loadsLocalTools := profile.Type == provider.TypeNative || profile.Type == provider.TypeAPI && profile.API != nil && profile.API.Runtime != nil && profile.API.Runtime.Enabled
	if loadsLocalTools {
		toolManager := registry.Tools
		for _, tool := range toolManager.Schemas() {
			if tool.Kind == "external" || !contextToolAllowed(tool.Name, tool.Capability, allowed, forbidden) {
				continue
			}
			encoded, _ := json.Marshal(tool.Schema)
			manifest.Tools = append(manifest.Tools, ContextAssetRef{ID: tool.Name, Digest: digestBytes(encoded), Source: tool.Kind})
		}
	}
	if profile.API != nil && profile.API.Runtime != nil && profile.API.Runtime.Memory != nil && profile.API.Runtime.Memory.Enabled {
		memory, err := registry.Memory()
		if err == nil {
			for _, item := range memory.Recall(prompt, "", profile.API.Runtime.Memory.TopK) {
				manifest.MemoryReads = append(manifest.MemoryReads, ContextMemoryRead{ID: item.ID, Type: item.Type, Digest: digestBytes([]byte(item.Content)), Source: item.Source})
			}
		}
	}
	return manifest, projection, nil
}

// ContextMessages 返回可跨 Provider 复用的规范化对话消息。
// transcript、tool event 和当前 Turn 输入不进入下一次模型上下文。
func (m *SessionManager) ContextMessages(
	sessionID, excludeTurnID string,
	profile provider.Config,
	currentPrompt string,
) ([]provider.NativeMessage, error) {
	capacity, err := profile.ResolveContextCapacity(nil)
	if err != nil {
		return nil, err
	}
	projection, err := m.projectContext(
		sessionID, excludeTurnID, profile.ID, capacity, provider.StaticContextEstimate{}, currentPrompt,
	)
	if err != nil {
		return nil, err
	}
	return projection.Messages, nil
}

func (m *SessionManager) contextSessionMessages(sessionID, excludeTurnID string) ([]SessionMessage, error) {
	messages, err := m.store.Messages(sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	values := make([]SessionMessage, 0, len(messages))
	for _, message := range messages {
		if message.TurnID == excludeTurnID || message.Kind != "" && message.Kind != "message" && message.Kind != "legacy_message" {
			continue
		}
		if message.Role != "user" && message.Role != "assistant" || strings.TrimSpace(message.Content) == "" {
			continue
		}
		values = append(values, message)
	}
	return values, nil
}

func (m *SessionManager) writeContextCheckpoint(checkpoint ContextCheckpoint) (string, string, error) {
	if err := validateRunID(checkpoint.TurnID); err != nil {
		return "", "", fmt.Errorf("checkpoint_ref_invalid: %w", err)
	}
	dir, err := m.store.sessionDir(checkpoint.SessionID)
	if err != nil {
		return "", "", err
	}
	contextDir := filepath.Join(dir, "context")
	checkpointDir := filepath.Join(contextDir, "checkpoints")
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
		return "", "", err
	}
	reference, err := checkpointRelativeRef(checkpoint.TurnID)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, filepath.FromSlash(reference))
	if err := writeJSONAtomic(path, checkpoint); err != nil {
		return "", "", err
	}
	digest := digestContextCheckpoint(checkpoint)
	manifest := ContextManifest{
		SummaryRef: reference, SummaryDigest: digest, SummaryDigestKind: checkpointDigestStableJSON,
	}
	if err := writeCheckpointCurrent(dir, checkpoint, manifest); err != nil {
		return "", "", err
	}
	return reference, digest, nil
}

// MemoryPaths 返回 Session 私有的 Runtime memory 路径。Workbench project/global
// memory 由调用方筛选后只读注入，Runtime 不直接读写 Workbench 项目目录。
func (m *SessionManager) MemoryPaths(sessionID string) (durable, candidates string) {
	if sessionID == "" {
		return "", ""
	}
	dir, err := m.store.sessionDir(sessionID)
	if err != nil {
		return "", ""
	}
	memoryDir := filepath.Join(dir, "memory")
	return filepath.Join(memoryDir, "working.json"), filepath.Join(memoryDir, "candidates.json")
}

func contextToolAllowed(name, capability string, allowed, forbidden []string) bool {
	if containsContextAction(forbidden, "*") || containsContextAction(forbidden, name) || capability != "" && containsContextAction(forbidden, capability) {
		return false
	}
	return containsContextAction(allowed, "*") || containsContextAction(allowed, name) || capability != "" && containsContextAction(allowed, capability)
}

func containsContextAction(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func DecideRecordPolicy(source, runType, executionKind, explicitSessionID, recordMode, retention string) (RecordDecision, error) {
	decision := RecordDecision{RecordMode: recordMode, Retention: retention, CaptureQuality: CaptureStructured}
	if decision.RecordMode == "" {
		decision.RecordMode = RecordFull
	}
	if decision.Retention == "" {
		decision.Retention = RetentionStandard
	}
	if executionKind == ExecutionTmux || executionKind == ExecutionTerminal {
		decision.CaptureQuality = CaptureTranscriptOnly
	}
	if runType == RunTask && explicitSessionID == "" && source != "http" {
		decision.Retention = RetentionEphemeral
		decision.Reason = "run without session intent"
	}
	if decision.Reason == "" {
		decision.Reason = "runtime default"
	}
	if !oneOf(decision.RecordMode, RecordFull, RecordMetadata, RecordOff) {
		return RecordDecision{}, fmt.Errorf("record_mode must be full|metadata|off")
	}
	if !oneOf(decision.Retention, RetentionEphemeral, RetentionStandard, RetentionPinned) {
		return RecordDecision{}, fmt.Errorf("retention must be ephemeral|standard|pinned")
	}
	return decision, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func sessionIDForRun(runID string) string {
	return relatedID("session", runID)
}

func turnIDForRun(runID string) string {
	return relatedID("turn", runID)
}

func executionIDForRun(runID string) string {
	return relatedID("execution", runID)
}

func relatedID(kind, runID string) string {
	candidate := kind + "-" + runID
	if len(candidate) <= 128 {
		return candidate
	}
	digest := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("%s-%x", kind, digest[:16])
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func digestPath(path string) string {
	if value, err := digestFile(path); err == nil {
		return value
	}
	return ""
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest)
}

func (m *SessionManager) ImportLegacyMemory() error {
	legacy := filepath.Join(m.service.StateDir, "memory.json")
	if _, err := os.Stat(m.service.paths.MemoryFile); err == nil {
		return nil
	}
	data, err := os.ReadFile(legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.service.paths.MemoryFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(m.service.paths.MemoryFile, data, 0o600)
}

func sessionRuntimeName(executionKind string, profile provider.Config) string {
	if executionKind == ExecutionTmux {
		return "tmux"
	}
	if executionKind == ExecutionTerminal {
		return "terminal"
	}
	if profile.Type == provider.TypeCLI {
		return "cli"
	}
	return "api"
}

func requestModel(request Request, profile provider.Config) string {
	if request.ModelOverride != "" {
		return request.ModelOverride
	}
	if profile.API != nil {
		return profile.API.Model
	}
	if profile.CLI != nil {
		return profile.CLI.Command.Model
	}
	if profile.Native != nil {
		return profile.Native.ModelProfile
	}
	return ""
}
