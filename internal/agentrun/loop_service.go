package agentrun

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-arch/internal/capability"
)

type LoopPaths struct {
	LoopDir     string
	RequestFile string
	StatusFile  string
	EventsFile  string
	OutputLog   string
}

type LoopStartOptions struct {
	LoopID          string
	ProjectID       string
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
	SchemaVersion   int       `json:"schema_version"`
	LoopID          string    `json:"loop_id"`
	ProjectID       string    `json:"project_id"`
	Input           string    `json:"input"`
	InputFile       string    `json:"input_file"`
	Actions         []Action  `json:"actions,omitempty"`
	PlannerProfile  string    `json:"planner_profile,omitempty"`
	MaxSteps        int       `json:"max_steps"`
	Capabilities    []string  `json:"capabilities"`
	Forbidden       []string  `json:"forbidden_actions"`
	CWD             string    `json:"cwd"`
	ResultSchema    string    `json:"result_schema"`
	DeadlineSeconds int       `json:"deadline_seconds"`
	RuntimeVersion  string    `json:"runtime_version"`
	CreatedAt       time.Time `json:"created_at"`
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

const LoopOutcomeDone = "done"

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
	if options.LoopID == "" {
		options.LoopID = newRunID("loop")
	}
	paths := s.loopPaths(options.LoopID)
	if !options.Force {
		var existing PersistentLoopStatus
		if err := readJSON(paths.StatusFile, &existing); err == nil {
			return existing, nil
		}
	}
	cwd, err := resolveCWD(options.CWD)
	if err != nil {
		return PersistentLoopStatus{}, err
	}
	if err := os.MkdirAll(paths.LoopDir, 0o755); err != nil {
		return PersistentLoopStatus{}, err
	}
	if options.Force {
		for _, path := range []string{paths.RequestFile, paths.StatusFile, paths.EventsFile, paths.OutputLog} {
			_ = os.Remove(path)
		}
	}
	request := LoopRequest{SchemaVersion: 1, LoopID: options.LoopID, ProjectID: options.ProjectID,
		Input: input, InputFile: options.InputFile, Actions: options.Actions, PlannerProfile: options.PlannerProfile,
		MaxSteps: options.MaxSteps, Capabilities: options.Capabilities, Forbidden: options.Forbidden,
		CWD: cwd, ResultSchema: options.ResultSchema, DeadlineSeconds: options.DeadlineSeconds,
		RuntimeVersion: s.RuntimeVersion, CreatedAt: time.Now().UTC()}
	if err := writeJSONAtomic(paths.RequestFile, request); err != nil {
		return PersistentLoopStatus{}, err
	}
	status := PersistentLoopStatus{SchemaVersion: 1, LoopID: options.LoopID, ProjectID: options.ProjectID,
		State: StateRunning, Phase: PhasePlanning, MaxSteps: options.MaxSteps,
		Observations: []Observation{}, Message: "loop started", UpdatedAt: time.Now().UTC()}
	status.Transitions = []LoopTransition{{Phase: PhasePlanning, At: status.UpdatedAt}}
	if err := writeJSONAtomic(paths.StatusFile, status); err != nil {
		return PersistentLoopStatus{}, err
	}
	s.registerLoop(paths, request, StateRunning)
	_ = appendLoopEvent(paths, options.LoopID, "loop.started", map[string]any{"max_steps": options.MaxSteps})
	return status, nil
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
	paths := s.loopPaths(loopID)
	var request LoopRequest
	var status PersistentLoopStatus
	if err := readJSON(paths.RequestFile, &request); err != nil {
		return status, fmt.Errorf("loop 不存在: %s", loopID)
	}
	if err := readJSON(paths.StatusFile, &status); err != nil {
		return status, fmt.Errorf("loop 不存在: %s", loopID)
	}
	if status.Outcome != "" {
		return status, nil
	}
	if ctx.Err() != nil {
		return s.finishLoop(paths, status, OutcomeCancelled, "cancelled")
	}
	if status.Step >= status.MaxSteps {
		return s.finishLoop(paths, status, OutcomeFailed, "loop 达到 max_steps")
	}
	status = updateLoop(paths, status, PhasePlanning, "planning")
	action, err := s.nextLoopAction(ctx, request, status)
	if err != nil {
		_ = appendLoopEvent(paths, loopID, "planner.invalid", map[string]any{"error": err.Error()})
		return s.finishLoop(paths, status, OutcomeFailed, "planner invalid")
	}
	status.ActionCursor++
	_ = appendLoopEvent(paths, loopID, "planner.action", map[string]any{"type": action.Type})
	if action.Type == "respond" {
		status.Output = action.Content
		_ = appendText(paths.OutputLog, action.Content+"\n")
		return s.finishLoop(paths, status, LoopOutcomeDone, "respond")
	}
	status = updateLoop(paths, status, PhaseExecuting, "executing")
	execution := s.executeLoopAction(ctx, request, action)
	encoded, _ := json.Marshal(map[string]any{"action": action, "result": execution})
	_ = appendText(paths.OutputLog, string(encoded)+"\n")
	status = updateLoop(paths, status, PhaseObserving, "observing")
	kind := "progress"
	if execution.Status != "ok" {
		kind = "blocked"
	}
	status.Observations = append(status.Observations, Observation{Kind: kind, Content: execution.Output})
	_ = appendLoopEvent(paths, loopID, "observe", map[string]any{"kind": kind})
	if kind == "blocked" {
		return s.finishLoop(paths, status, OutcomeBlocked, "blocked")
	}
	status.Step++
	status = updateLoop(paths, status, PhasePlanning, "ready")
	return status, nil
}

func (s *Service) LoopStatus(loopID string) (PersistentLoopStatus, error) {
	var status PersistentLoopStatus
	if err := readJSON(s.loopPaths(loopID).StatusFile, &status); err != nil {
		return status, fmt.Errorf("loop 不存在: %s", loopID)
	}
	return status, nil
}

func (s *Service) LoopLogs(loopID string, tail int) (LoopLogsResult, error) {
	paths := s.loopPaths(loopID)
	content, err := os.ReadFile(paths.OutputLog)
	if err != nil && !os.IsNotExist(err) {
		return LoopLogsResult{}, err
	}
	var request LoopRequest
	if err := readJSON(paths.RequestFile, &request); err != nil {
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
	if file, openErr := os.Open(paths.EventsFile); openErr == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var event map[string]any
			if json.Unmarshal(scanner.Bytes(), &event) == nil {
				events = append(events, event)
			}
		}
		_ = file.Close()
	}
	if len(events) > tail {
		events = events[len(events)-tail:]
	}
	return LoopLogsResult{LoopID: loopID, ProjectID: request.ProjectID, Content: strings.Join(lines, "\n"), Events: events}, nil
}

func (s *Service) LoopCancel(loopID string) (PersistentLoopStatus, error) {
	status, err := s.LoopStatus(loopID)
	if err != nil {
		return status, err
	}
	return s.finishLoop(s.loopPaths(loopID), status, OutcomeCancelled, "cancelled")
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
		CWD: request.CWD, Prompt: prompt, ExecutionMode: ModeCapture, DeadlineSeconds: request.DeadlineSeconds})
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
		manager := capability.NewToolManager()
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
			CWD: request.CWD, Prompt: prompt, ExecutionMode: ModeManaged, DeadlineSeconds: request.DeadlineSeconds, ResultSchema: request.ResultSchema})
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
	err := writeJSONAtomic(paths.StatusFile, status)
	s.updateLoopRegistry(paths, status)
	_ = appendLoopEvent(paths, status.LoopID, "loop.finished", map[string]any{"outcome": outcome})
	return status, err
}

func updateLoop(paths LoopPaths, status PersistentLoopStatus, phase, message string) PersistentLoopStatus {
	status.Phase, status.Message, status.UpdatedAt = phase, message, time.Now().UTC()
	status.Transitions = append(status.Transitions, LoopTransition{Phase: phase, At: status.UpdatedAt})
	_ = writeJSONAtomic(paths.StatusFile, status)
	_ = appendLoopEvent(paths, status.LoopID, "loop.phase", map[string]any{"phase": phase})
	return status
}

func (s *Service) loopPaths(loopID string) LoopPaths {
	dir := filepath.Join(s.RunsDir, "loop", dateFromRunID(loopID), loopID)
	return LoopPaths{LoopDir: dir, RequestFile: filepath.Join(dir, "request.json"), StatusFile: filepath.Join(dir, "status.json"), EventsFile: filepath.Join(dir, "events.jsonl"), OutputLog: filepath.Join(dir, "output.log")}
}

func appendLoopEvent(paths LoopPaths, loopID, eventType string, data map[string]any) error {
	sequence := 1
	if file, err := os.Open(paths.EventsFile); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			sequence++
		}
		_ = file.Close()
	}
	record := map[string]any{"schema_version": 1, "event_id": randomID(16), "loop_id": loopID, "type": eventType, "ts": time.Now().UTC(), "seq": sequence, "data": data}
	file, err := os.OpenFile(paths.EventsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(record)
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
