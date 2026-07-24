package agentrun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/capability"
)

type LoopPaths struct {
	LoopDir     string
	RequestFile string
	StatusFile  string
	EventsFile  string
	OutputLog   string
	ResultFile  string
}

type LoopStartOptions struct {
	LoopID          string
	ProjectID       string
	SessionID       string
	Input           string
	InputFile       string
	Actions         []Action
	PlannerProfile  string
	MaxSteps        int
	Capabilities    []string
	Forbidden       []string
	CWD             string
	ResultSchema    string
	DeadlineSeconds int
	Force           bool
}

type LoopRequest struct {
	SchemaVersion      int       `json:"schema_version"`
	LoopID             string    `json:"loop_id"`
	ProjectID          string    `json:"project_id"`
	SessionID          string    `json:"session_id,omitempty"`
	Input              string    `json:"input"`
	InputFile          string    `json:"input_file"`
	Actions            []Action  `json:"actions,omitempty"`
	PlannerProfile     string    `json:"planner_profile,omitempty"`
	MaxSteps           int       `json:"max_steps"`
	Capabilities       []string  `json:"capabilities"`
	Forbidden          []string  `json:"forbidden_actions"`
	CWD                string    `json:"cwd"`
	ResultSchema       string    `json:"result_schema"`
	DeadlineSeconds    int       `json:"deadline_seconds"`
	RuntimeVersion     string    `json:"runtime_version"`
	RequestFingerprint string    `json:"request_fingerprint,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type Observation struct {
	Kind    string `json:"kind"`
	Content any    `json:"content"`
}

type PersistentLoopStatus struct {
	SchemaVersion int              `json:"schema_version"`
	LoopID        string           `json:"loop_id"`
	ProjectID     string           `json:"project_id"`
	State         string           `json:"state"`
	Phase         string           `json:"phase"`
	Outcome       string           `json:"outcome,omitempty"`
	Step          int              `json:"step"`
	MaxSteps      int              `json:"max_steps"`
	Output        any              `json:"output,omitempty"`
	Observations  []Observation    `json:"observations"`
	ActionCursor  int              `json:"action_cursor"`
	Message       string           `json:"message"`
	Transitions   []LoopTransition `json:"transitions"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

const LoopOutcomeDone = OutcomeSucceeded

type LoopLogsResult struct {
	LoopID    string           `json:"loop_id"`
	ProjectID string           `json:"project_id"`
	Content   string           `json:"content"`
	Events    []map[string]any `json:"events"`
}

func (s *Service) LoopStart(options LoopStartOptions) (PersistentLoopStatus, error) {
	if len(options.Actions) == 0 && options.PlannerProfile == "" {
		return PersistentLoopStatus{}, fmt.Errorf("loop start 需要 actions 或 planner_profile")
	}
	input := options.Input
	if options.InputFile != "" {
		data, err := os.ReadFile(options.InputFile)
		if err != nil {
			return PersistentLoopStatus{}, err
		}
		if input != "" {
			input += "\n\n"
		}
		input += string(data)
	}
	if strings.TrimSpace(input) == "" {
		return PersistentLoopStatus{}, fmt.Errorf("loop input is required")
	}
	if options.MaxSteps <= 0 {
		options.MaxSteps = 10
	}
	if options.ProjectID == "" {
		options.ProjectID = s.DefaultProject
	}
	if options.SessionID != "" {
		session, err := NewSessionManager(s).Store().Get(options.SessionID)
		if err != nil {
			return PersistentLoopStatus{}, fmt.Errorf("loop session does not exist: %s", options.SessionID)
		}
		if session.ProjectID != "" && session.ProjectID != options.ProjectID {
			return PersistentLoopStatus{}, fmt.Errorf("session %s belongs to project %s", options.SessionID, session.ProjectID)
		}
	}
	if options.LoopID == "" {
		options.LoopID = newRunID("loop")
	}
	if err := validateRunID(options.LoopID); err != nil {
		return PersistentLoopStatus{}, err
	}
	cwd, err := resolveCWD(options.CWD)
	if err != nil {
		return PersistentLoopStatus{}, err
	}
	now := time.Now().UTC()
	request := LoopRequest{SchemaVersion: 1, LoopID: options.LoopID, ProjectID: options.ProjectID,
		SessionID: options.SessionID,
		Input:     input, InputFile: options.InputFile, Actions: options.Actions, PlannerProfile: options.PlannerProfile,
		MaxSteps: options.MaxSteps, Capabilities: options.Capabilities, Forbidden: options.Forbidden,
		CWD: cwd, ResultSchema: options.ResultSchema, DeadlineSeconds: options.DeadlineSeconds,
		RuntimeVersion: s.RuntimeVersion, CreatedAt: now}
	request.RequestFingerprint, err = fingerprintValue(struct {
		LoopID, ProjectID, SessionID, Input, InputFile, PlannerProfile, CWD, ResultSchema string
		Actions                                                                           []Action
		MaxSteps, DeadlineSeconds                                                         int
		Capabilities, Forbidden                                                           []string
	}{
		LoopID: request.LoopID, ProjectID: request.ProjectID, SessionID: request.SessionID, Input: request.Input, InputFile: request.InputFile,
		PlannerProfile: request.PlannerProfile, CWD: request.CWD, ResultSchema: request.ResultSchema,
		Actions: request.Actions, MaxSteps: request.MaxSteps, DeadlineSeconds: request.DeadlineSeconds,
		Capabilities: request.Capabilities, Forbidden: request.Forbidden,
	})
	if err != nil {
		return PersistentLoopStatus{}, err
	}
	runLock, err := s.acquireRunLock(context.Background(), options.LoopID)
	if err != nil {
		return PersistentLoopStatus{}, err
	}
	defer runLock.release()
	paths := s.loopPaths(options.LoopID)
	if !options.Force {
		var existingRequest LoopRequest
		var existingStatus PersistentLoopStatus
		requestErr := readLoopJSON(paths.RequestFile, &existingRequest)
		statusErr := readLoopJSON(paths.StatusFile, &existingStatus)
		if !(os.IsNotExist(requestErr) && os.IsNotExist(statusErr)) {
			if requestErr == nil && statusErr == nil && existingRequest.RequestFingerprint != "" && existingRequest.RequestFingerprint == request.RequestFingerprint {
				normalizeLoopStatus(&existingStatus)
				return existingStatus, nil
			}
			return existingStatus, fmt.Errorf("idempotency conflict for loop_id %s", options.LoopID)
		}
	}
	if err := os.MkdirAll(paths.LoopDir, 0o700); err != nil {
		return PersistentLoopStatus{}, err
	}
	if options.Force {
		for _, path := range []string{paths.RequestFile, paths.StatusFile, paths.EventsFile, paths.OutputLog, paths.ResultFile} {
			_ = os.Remove(path)
		}
	}
	if err := writeLoopJSON(paths.RequestFile, request); err != nil {
		return PersistentLoopStatus{}, err
	}
	status := PersistentLoopStatus{SchemaVersion: 1, LoopID: options.LoopID, ProjectID: options.ProjectID,
		State: StateRunning, Phase: PhasePlanning, MaxSteps: options.MaxSteps,
		Observations: []Observation{}, Message: "loop started", UpdatedAt: time.Now().UTC()}
	status.Transitions = []LoopTransition{{Phase: PhasePlanning, At: status.UpdatedAt}}
	if err := writeLoopJSON(paths.StatusFile, status); err != nil {
		return PersistentLoopStatus{}, err
	}
	s.registerLoop(paths, request, StateRunning)
	return status, appendLoopEvent(paths, options.LoopID, "loop.started", map[string]any{"max_steps": options.MaxSteps})
}

func (s *Service) LoopRun(ctx context.Context, options LoopStartOptions) (PersistentLoopStatus, error) {
	status, err := s.LoopStart(options)
	if err != nil {
		return status, err
	}
	for status.Outcome == "" {
		status, err = s.LoopStep(ctx, status.LoopID)
		if err != nil {
			return status, err
		}
	}
	return status, nil
}

func (s *Service) LoopStep(ctx context.Context, loopID string) (PersistentLoopStatus, error) {
	if err := validateRunID(loopID); err != nil {
		return PersistentLoopStatus{}, err
	}
	runLock, err := s.acquireRunLock(ctx, loopID)
	if err != nil {
		return PersistentLoopStatus{}, err
	}
	defer runLock.release()
	paths := s.loopPaths(loopID)
	var request LoopRequest
	var status PersistentLoopStatus
	if err := readLoopJSON(paths.RequestFile, &request); err != nil {
		return status, fmt.Errorf("loop 不存在: %s", loopID)
	}
	if err := readLoopJSON(paths.StatusFile, &status); err != nil {
		return status, fmt.Errorf("loop 不存在: %s", loopID)
	}
	normalizeLoopStatus(&status)
	if status.Outcome != "" {
		return status, nil
	}
	if ctx.Err() != nil {
		return s.finishLoop(paths, status, OutcomeCancelled, "cancelled")
	}
	if status.Step >= status.MaxSteps {
		return s.finishLoop(paths, status, OutcomeFailed, "loop 达到 max_steps")
	}
	if request.DeadlineSeconds > 0 && time.Now().After(request.CreatedAt.Add(time.Duration(request.DeadlineSeconds)*time.Second)) {
		return s.finishLoop(paths, status, OutcomeFailed, "loop deadline exceeded")
	}
	status, err = updateLoop(paths, status, PhasePlanning, "planning")
	if err != nil {
		return status, err
	}
	action, err := s.nextLoopAction(ctx, request, status)
	if err != nil {
		eventErr := appendLoopEvent(paths, loopID, "planner.invalid", map[string]any{"error": err.Error()})
		finished, finishErr := s.finishLoop(paths, status, OutcomeFailed, "planner invalid")
		return finished, errors.Join(eventErr, finishErr)
	}
	status.ActionCursor++
	if err := appendLoopEvent(paths, loopID, "planner.action", map[string]any{"type": action.Type}); err != nil {
		return status, err
	}
	if err := authorizeLoopAction(request, action); err != nil {
		eventErr := appendLoopEvent(paths, loopID, "action.denied", map[string]any{"type": action.Type, "error": err.Error()})
		finished, finishErr := s.finishLoop(paths, status, OutcomeBlocked, "blocked: "+err.Error())
		return finished, errors.Join(eventErr, finishErr)
	}
	if action.Type == "respond" {
		status.Output = action.Content
		if err := appendText(paths.OutputLog, action.Content+"\n"); err != nil {
			return status, err
		}
		return s.finishLoop(paths, status, LoopOutcomeDone, "respond")
	}
	status, err = updateLoop(paths, status, PhaseExecuting, "executing")
	if err != nil {
		return status, err
	}
	execution := s.executeLoopAction(ctx, request, action)
	encoded, err := json.Marshal(map[string]any{"action": action, "result": execution})
	if err != nil {
		return status, err
	}
	if err := appendText(paths.OutputLog, string(encoded)+"\n"); err != nil {
		return status, err
	}
	status, err = updateLoop(paths, status, PhaseObserving, "observing")
	if err != nil {
		return status, err
	}
	kind := "progress"
	if execution.Status != "ok" {
		kind = "blocked"
	}
	status.Observations = append(status.Observations, Observation{Kind: kind, Content: execution.Output})
	if err := appendLoopEvent(paths, loopID, "observe", map[string]any{"kind": kind}); err != nil {
		return status, err
	}
	if kind == "blocked" {
		return s.finishLoop(paths, status, OutcomeBlocked, "blocked")
	}
	status.Step++
	return updateLoop(paths, status, PhasePlanning, "ready")
}

func (s *Service) LoopStatus(loopID string) (PersistentLoopStatus, error) {
	var status PersistentLoopStatus
	if err := validateRunID(loopID); err != nil {
		return status, err
	}
	if err := readLoopJSON(s.loopPaths(loopID).StatusFile, &status); err != nil {
		return status, fmt.Errorf("loop 不存在: %s", loopID)
	}
	normalizeLoopStatus(&status)
	return status, nil
}

func (s *Service) LoopLogs(loopID string, tail int) (LoopLogsResult, error) {
	if err := validateRunID(loopID); err != nil {
		return LoopLogsResult{}, err
	}
	paths := s.loopPaths(loopID)
	content, err := os.ReadFile(paths.OutputLog)
	if err != nil && !os.IsNotExist(err) {
		return LoopLogsResult{}, err
	}
	var request LoopRequest
	if err := readLoopJSON(paths.RequestFile, &request); err != nil {
		return LoopLogsResult{}, fmt.Errorf("loop 不存在: %s", loopID)
	}
	if tail <= 0 {
		tail = 120
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	events := []map[string]any{}
	if err := withAdvisoryFileLock(paths.EventsFile+".lock", func() error {
		if file, openErr := os.Open(paths.EventsFile); openErr == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				var event map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
					_ = file.Close()
					return err
				}
				events = append(events, event)
			}
			return errors.Join(scanner.Err(), file.Close())
		} else if !os.IsNotExist(openErr) {
			return openErr
		}
		return nil
	}); err != nil {
		return LoopLogsResult{}, err
	}
	if len(events) > tail {
		events = events[len(events)-tail:]
	}
	return LoopLogsResult{LoopID: loopID, ProjectID: request.ProjectID, Content: strings.Join(lines, "\n"), Events: events}, nil
}

func (s *Service) LoopCancel(loopID string) (PersistentLoopStatus, error) {
	if err := validateRunID(loopID); err != nil {
		return PersistentLoopStatus{}, err
	}
	runLock, err := s.acquireRunLock(context.Background(), loopID)
	if err != nil {
		return PersistentLoopStatus{}, err
	}
	defer runLock.release()
	paths := s.loopPaths(loopID)
	var status PersistentLoopStatus
	err = readLoopJSON(paths.StatusFile, &status)
	if err != nil {
		return status, err
	}
	normalizeLoopStatus(&status)
	if status.Outcome != "" {
		return status, nil
	}
	return s.finishLoop(paths, status, OutcomeCancelled, "cancelled")
}

func (s *Service) nextLoopAction(ctx context.Context, request LoopRequest, status PersistentLoopStatus) (Action, error) {
	if len(request.Actions) > 0 {
		if status.ActionCursor >= len(request.Actions) {
			return Action{Type: "respond", Content: "loop actions completed"}, nil
		}
		action := request.Actions[status.ActionCursor]
		if !validAction(action) {
			return Action{}, fmt.Errorf("invalid action at index %d", status.ActionCursor)
		}
		return action, nil
	}
	messages, _ := json.Marshal(map[string]any{"input": request.Input, "observations": status.Observations})
	prompt := "Return exactly one JSON action object with type respond|tool|run_agent. Context:\n" + string(messages)
	run, err := s.Run(ctx, RunOptions{RunType: RunTask, Profile: request.PlannerProfile, ProjectID: request.ProjectID,
		SessionID: request.SessionID, CWD: request.CWD, Prompt: prompt, ExecutionMode: ModeCapture,
		DeadlineSeconds: request.DeadlineSeconds, Caller: "loop.planner"})
	if err != nil {
		return Action{}, err
	}
	result, err := s.ReadResult(RunTask, run.RunID)
	if err != nil {
		return Action{}, err
	}
	var action Action
	text := strings.TrimSpace(result.Summary)
	text = strings.TrimPrefix(strings.TrimSuffix(text, "```"), "```json")
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &action); err != nil {
		return Action{}, err
	}
	return action, nil
}

func (s *Service) executeLoopAction(ctx context.Context, request LoopRequest, action Action) ExecutionResult {
	if action.Type == "tool" {
		registry := capability.NewRegistry(capability.RegistryConfig{SkillsDir: s.paths.SkillsDir, ToolsDir: s.paths.ToolsDir})
		manager := registry.Tools
		output, err := manager.Call(action.Name, action.Arguments, request.Capabilities, request.Forbidden)
		if err != nil {
			return ExecutionResult{Status: "blocked", Output: err.Error()}
		}
		return ExecutionResult{Status: "ok", Output: output}
	}
	if action.Type == "run_agent" {
		profile := fmt.Sprint(action.Request["profile"])
		prompt := fmt.Sprint(action.Request["prompt"])
		if profile == "" || prompt == "" {
			return ExecutionResult{Status: "error", Output: "run_agent requires profile and prompt"}
		}
		run, err := s.Run(ctx, RunOptions{RunType: RunTask, Profile: profile, ProjectID: request.ProjectID,
			SessionID: request.SessionID, CWD: request.CWD, Prompt: prompt, ExecutionMode: ModeManaged,
			DeadlineSeconds: request.DeadlineSeconds, ResultSchema: request.ResultSchema, Caller: "loop.run_agent"})
		if err != nil {
			return ExecutionResult{Status: "error", Output: err.Error()}
		}
		return ExecutionResult{Status: "ok", Output: run}
	}
	return ExecutionResult{Status: "error", Output: "unsupported action"}
}

func (s *Service) finishLoop(paths LoopPaths, status PersistentLoopStatus, outcome, message string) (PersistentLoopStatus, error) {
	status.State, status.Phase, status.Outcome, status.Message = StateDone, PhaseTerminal, outcome, message
	if outcome == OutcomeFailed {
		status.State = StateFailed
	} else if outcome == OutcomeBlocked {
		status.State = StateBlocked
	} else if outcome == OutcomeCancelled {
		status.State = StateCancelled
	}
	status.UpdatedAt = time.Now().UTC()
	status.Transitions = append(status.Transitions, LoopTransition{Phase: PhaseTerminal, Outcome: outcome, At: status.UpdatedAt})
	summaryText := strings.TrimSpace(fmt.Sprint(status.Output))
	if status.Output == nil || summaryText == "" || summaryText == "<nil>" {
		summaryText = message
	}
	errorItems := []map[string]any{}
	if outcome != OutcomeSucceeded {
		errorItems = append(errorItems, map[string]any{"message": message, "outcome": outcome})
	}
	result := Result{SchemaVersion: 1, RunID: status.LoopID, Outcome: outcome, Summary: summaryText,
		Artifacts: []map[string]any{{"type": "log", "path": paths.OutputLog}}, Errors: errorItems,
		Validation: Validation{Commands: []string{}, Passed: outcome == OutcomeSucceeded}}
	if err := writeLoopJSON(paths.ResultFile, result); err != nil {
		return status, err
	}
	err := writeLoopJSON(paths.StatusFile, status)
	s.updateLoopRegistry(paths, status)
	eventErr := appendLoopEvent(paths, status.LoopID, "loop.finished", map[string]any{"outcome": outcome})
	return status, errors.Join(err, eventErr)
}

func updateLoop(paths LoopPaths, status PersistentLoopStatus, phase, message string) (PersistentLoopStatus, error) {
	status.Phase, status.Message, status.UpdatedAt = phase, message, time.Now().UTC()
	status.Transitions = append(status.Transitions, LoopTransition{Phase: phase, At: status.UpdatedAt})
	statusErr := writeLoopJSON(paths.StatusFile, status)
	eventErr := appendLoopEvent(paths, status.LoopID, "loop.phase", map[string]any{"phase": phase})
	return status, errors.Join(statusErr, eventErr)
}

func (s *Service) loopPaths(loopID string) LoopPaths {
	dir := filepath.Join(s.RunsDir, "loop", dateFromRunID(loopID), loopID)
	return LoopPaths{LoopDir: dir, RequestFile: filepath.Join(dir, "request.json"), StatusFile: filepath.Join(dir, "status.json"), EventsFile: filepath.Join(dir, "events.jsonl"), OutputLog: filepath.Join(dir, "output.log"), ResultFile: filepath.Join(dir, "result.json")}
}

func appendLoopEvent(paths LoopPaths, loopID, eventType string, data map[string]any) error {
	return withAdvisoryFileLock(paths.EventsFile+".lock", func() error {
		sequence := 1
		if file, err := os.Open(paths.EventsFile); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				sequence++
			}
			scanErr := scanner.Err()
			closeErr := file.Close()
			if scanErr != nil || closeErr != nil {
				return errors.Join(scanErr, closeErr)
			}
		}
		record := map[string]any{"schema_version": 1, "event_id": randomID(16), "loop_id": loopID, "type": eventType, "ts": time.Now().UTC(), "seq": sequence, "data": data}
		file, err := os.OpenFile(paths.EventsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer file.Close()
		return json.NewEncoder(file).Encode(record)
	})
}

func readLoopJSON(path string, value any) error {
	return withAdvisoryFileLock(path+".lock", func() error { return readJSON(path, value) })
}

func writeLoopJSON(path string, value any) error {
	return withAdvisoryFileLock(path+".lock", func() error { return writeJSONAtomic(path, value) })
}

func normalizeLoopStatus(status *PersistentLoopStatus) {
	if status.Outcome == "done" {
		status.Outcome = OutcomeSucceeded
	}
	for index := range status.Transitions {
		if status.Transitions[index].Outcome == "done" {
			status.Transitions[index].Outcome = OutcomeSucceeded
		}
	}
}

func authorizeLoopAction(request LoopRequest, action Action) error {
	if containsString(request.Forbidden, action.Type) {
		return fmt.Errorf("动作被 forbidden_actions 禁止: %s", action.Type)
	}
	if action.Type == "tool" && containsString(request.Forbidden, action.Name) {
		return fmt.Errorf("动作被 forbidden_actions 禁止: %s", action.Name)
	}
	if action.Type == "run_agent" {
		const capabilityName = "agent.run"
		if containsString(request.Forbidden, capabilityName) {
			return fmt.Errorf("动作被 forbidden_actions 禁止: %s", capabilityName)
		}
		if !containsString(request.Capabilities, capabilityName) {
			return fmt.Errorf("缺少 capability: %s", capabilityName)
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func appendText(path, text string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(text)
	return err
}
