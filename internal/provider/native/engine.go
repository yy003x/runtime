package native

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Observer func(Snapshot)

type Engine struct {
	store    *FileStore
	client   Client
	contexts ContextManager
	config   Config
	observer Observer
}

func NewEngine(store *FileStore, client Client, config Config, observer Observer) *Engine {
	if config.MaxRounds <= 0 {
		config.MaxRounds = 10
	}
	if config.TokenBudget <= 0 {
		config.TokenBudget = 128000
	}
	if config.LLMTimeout <= 0 {
		config.LLMTimeout = 5 * time.Second
	}
	return &Engine{store: store, client: client, config: config, observer: observer}
}

func (e *Engine) Start(ctx context.Context, runID string, initial Context) (Snapshot, error) {
	if _, err := e.store.Load(); err == nil {
		return Snapshot{}, fmt.Errorf("native run already exists: %s", runID)
	}
	now := time.Now().UTC()
	snapshot := Snapshot{
		RunID: runID, State: StateCreated, MaxRounds: e.config.MaxRounds,
		Context: initial, CreatedAt: now, UpdatedAt: now,
	}
	e.appendEvent(&snapshot, "run_created", StateCreated, StateCreated, "run created", "")
	if err := e.save(snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := e.transition(&snapshot, StateRunning, "run_started", "run started", ""); err != nil {
		return Snapshot{}, err
	}
	return e.loop(ctx, snapshot)
}

func (e *Engine) Resume(ctx context.Context, patch *ContextPatch) (Snapshot, error) {
	snapshot, err := e.store.Load()
	if err != nil {
		return Snapshot{}, err
	}
	if patch != nil {
		if snapshot.State != StateWaitingHuman {
			return Snapshot{}, fmt.Errorf("%w: patch resume requires waiting_human, got %s", ErrInvalidTransition, snapshot.State)
		}
		next, err := e.contexts.ApplyPatch(snapshot.Context, *patch)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Context = next
		snapshot.LastError = ""
		e.appendEvent(&snapshot, "human_patched", snapshot.State, snapshot.State, "human patched context", "")
	} else if snapshot.State != StateBlocked && snapshot.State != StateStopped {
		return Snapshot{}, fmt.Errorf("%w: continue requires blocked or stopped, got %s", ErrInvalidTransition, snapshot.State)
	}
	if err := e.transition(&snapshot, StateRunning, "state_transition", "run continued", ""); err != nil {
		return Snapshot{}, err
	}
	return e.loop(ctx, snapshot)
}

func (e *Engine) loop(ctx context.Context, snapshot Snapshot) (Snapshot, error) {
	for snapshot.State == StateRunning {
		if snapshot.Round >= snapshot.MaxRounds {
			if err := e.transition(&snapshot, StateCompleted, "completed", "max rounds reached", ""); err != nil {
				return snapshot, err
			}
			break
		}
		messages, err := e.contexts.BuildPrompt(snapshot.Context, e.config.TokenBudget)
		if err != nil {
			_ = e.transition(&snapshot, StateFailed, "state_transition", "context build failed", err.Error())
			return snapshot, err
		}
		if err := e.transition(&snapshot, StateWaitingLLM, "llm_request_started", "llm request started", ""); err != nil {
			return snapshot, err
		}
		response, control, callErr := e.generate(ctx, Request{RunID: snapshot.RunID, Round: snapshot.Round, Messages: messages})
		if control != nil {
			if err := e.applyControl(&snapshot, *control); err != nil {
				return snapshot, err
			}
			break
		}
		if ctx.Err() != nil {
			_ = e.transition(&snapshot, StateCancelled, "cancelled", "run context cancelled", ctx.Err().Error())
			return snapshot, ctx.Err()
		}
		if callErr != nil {
			message := "llm request failed"
			if errors.Is(callErr, context.DeadlineExceeded) {
				message = "llm timeout"
			} else if errors.Is(callErr, ErrUpstream) {
				message = "llm upstream error"
			}
			if err := e.transition(&snapshot, StateWaitingHuman, "llm_request_failed", message, callErr.Error()); err != nil {
				return snapshot, err
			}
			break
		}
		snapshot.Context.Messages = append(snapshot.Context.Messages, response.Message)
		snapshot.Round++
		snapshot.LastError = ""
		e.appendEvent(&snapshot, "llm_request_ended", StateWaitingLLM, StateRunning, "llm request completed", "")
		if response.Done || snapshot.Round >= snapshot.MaxRounds {
			if err := e.transition(&snapshot, StateCompleted, "completed", "run completed", ""); err != nil {
				return snapshot, err
			}
			break
		}
		if err := e.transition(&snapshot, StateRunning, "state_transition", "next round scheduled", ""); err != nil {
			return snapshot, err
		}
	}
	return CloneSnapshot(snapshot), nil
}

func (e *Engine) generate(ctx context.Context, request Request) (Response, *Control, error) {
	callCtx, cancel := context.WithTimeout(ctx, e.config.LLMTimeout)
	defer cancel()
	type outcome struct {
		response Response
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := e.client.Generate(callCtx, request)
		done <- outcome{response: response, err: err}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			return result.response, nil, result.err
		case <-ctx.Done():
			return Response{}, nil, ctx.Err()
		case <-callCtx.Done():
			return Response{}, nil, callCtx.Err()
		case <-ticker.C:
			control, ok, err := e.store.TakeControl()
			if err != nil {
				return Response{}, nil, err
			}
			if ok {
				cancel()
				return Response{}, &control, nil
			}
		}
	}
}

func (e *Engine) applyControl(snapshot *Snapshot, control Control) error {
	state := StateBlocked
	event := "blocked"
	message := control.Reason
	if message == "" {
		message = control.Action
	}
	switch control.Action {
	case "block":
		state = StateBlocked
	case "stop":
		state, event = StateStopped, "stopped"
	case "cancel":
		state, event = StateCancelled, "cancelled"
	default:
		return fmt.Errorf("unknown native control action: %s", control.Action)
	}
	return e.transition(snapshot, state, event, message, "")
}

func (e *Engine) transition(snapshot *Snapshot, next State, eventType, message, eventErr string) error {
	previous := snapshot.State
	if err := ValidateTransition(previous, next); err != nil {
		return err
	}
	snapshot.State = next
	if eventErr != "" {
		snapshot.LastError = eventErr
	}
	e.appendEvent(snapshot, eventType, previous, next, message, eventErr)
	return e.save(*snapshot)
}

func (e *Engine) appendEvent(snapshot *Snapshot, eventType string, from, to State, message, eventErr string) {
	now := time.Now().UTC()
	snapshot.UpdatedAt = now
	snapshot.Events = append(snapshot.Events, Event{
		Sequence: int64(len(snapshot.Events) + 1), Type: eventType, Time: now,
		FromState: from, ToState: to, Round: snapshot.Round, Message: message, Error: eventErr,
	})
}

func (e *Engine) save(snapshot Snapshot) error {
	if err := e.store.Save(snapshot); err != nil {
		return fmt.Errorf("save native snapshot: %w", err)
	}
	if e.observer != nil {
		e.observer(CloneSnapshot(snapshot))
	}
	return nil
}

func ControlRun(store *FileStore, action, reason string) (Snapshot, error) {
	snapshot, err := store.Load()
	if err != nil {
		return Snapshot{}, err
	}
	if IsTerminal(snapshot.State) {
		return snapshot, nil
	}
	if snapshot.State == StateRunning || snapshot.State == StateWaitingLLM {
		if err := store.WriteControl(Control{Action: action, Reason: reason}); err != nil {
			return Snapshot{}, err
		}
		return snapshot, nil
	}
	engine := NewEngine(store, nil, Config{}, nil)
	if err := engine.applyControl(&snapshot, Control{Action: action, Reason: reason}); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func FinalText(snapshot Snapshot) string {
	for index := len(snapshot.Context.Messages) - 1; index >= 0; index-- {
		if snapshot.Context.Messages[index].Role == "assistant" {
			return snapshot.Context.Messages[index].Content
		}
	}
	return ""
}
