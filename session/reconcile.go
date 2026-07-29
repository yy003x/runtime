package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
)

type ReconcileOptions struct {
	Terminate          bool `json:"terminate"`
	AcknowledgeUnknown bool `json:"acknowledge_unknown"`
}

type ReconcileResult struct {
	SessionID   string    `json:"session_id"`
	TurnID      string    `json:"turn_id"`
	RunID       string    `json:"run_id"`
	ExecutionID string    `json:"execution_id"`
	State       TurnState `json:"state"`
	Resolved    bool      `json:"resolved"`
}

func (service *Service) reconcileStaleSessions() error {
	values, err := service.store.list(ListFilter{})
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.ActiveTurnID == "" {
			continue
		}
		if err := service.store.withLock(value.ID, func() error {
			current, err := service.store.loadSession(value.ID)
			if err != nil {
				return err
			}
			if current.ActiveTurnID == "" {
				return nil
			}
			turn, err := service.store.loadTurn(value.ID, current.ActiveTurnID)
			if err != nil {
				return err
			}
			if turn.State != TurnRunning {
				return nil
			}
			execution, executionErr := service.store.loadExecution(
				value.ID, turn.ExecutionID,
			)
			if executionErr == nil && execution.Process != nil &&
				execution.Process.OwnerPID > 0 &&
				execution.Process.OwnerStartToken != "" {
				alive, same, probeErr := probeProcess(
					execution.Process.OwnerPID,
					execution.Process.OwnerStartToken,
				)
				if probeErr != nil {
					return probeErr
				}
				if alive && same {
					return nil
				}
			} else if executionErr != nil &&
				!errors.Is(executionErr, os.ErrNotExist) {
				return executionErr
			}
			if current.State != SessionBlocked {
				current.State = SessionBlocked
				current.UpdatedAt = service.now().UTC()
				if err := service.store.writeSession(current); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return service.store.rebuildIndex()
}

func (service *Service) Reconcile(
	ctx context.Context,
	sessionID string,
	options ReconcileOptions,
) (ReconcileResult, *contract.RuntimeError) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return ReconcileResult{}, sessionRuntimeError(
			contract.ErrorInvalidRequest, err.Error(),
		)
	}
	if options.Terminate && options.AcknowledgeUnknown {
		return ReconcileResult{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"terminate and acknowledge_unknown are mutually exclusive",
		)
	}
	var result ReconcileResult
	var runtimeErr *contract.RuntimeError
	err := service.store.withLock(sessionID, func() error {
		sessionValue, err := service.store.loadSession(sessionID)
		if err != nil {
			return err
		}
		if sessionValue.ActiveTurnID == "" {
			var found bool
			result, found, err = service.lastReconciledResult(sessionID)
			if err != nil {
				return err
			}
			if found {
				return nil
			}
			runtimeErr = sessionRuntimeError(
				contract.ErrorConflict,
				fmt.Sprintf("session %s has no execution to reconcile", sessionID),
			)
			return nil
		}
		turn, err := service.store.loadTurn(sessionID, sessionValue.ActiveTurnID)
		if err != nil {
			return err
		}
		result = ReconcileResult{
			SessionID: sessionID, TurnID: turn.ID, RunID: turn.RunID,
			ExecutionID: turn.ExecutionID, State: turn.State,
		}
		if turn.State != TurnRunning || sessionValue.State != SessionBlocked {
			runtimeErr = sessionRuntimeError(
				contract.ErrorConflict,
				"Session execution is not awaiting reconciliation",
			)
			return nil
		}
		var execution Execution
		execution, err = service.store.loadExecution(
			sessionID, turn.ExecutionID,
		)
		if errors.Is(err, os.ErrNotExist) {
			execution = Execution{
				SchemaVersion: SchemaVersion,
				ID:            turn.ExecutionID, SessionID: sessionID, TurnID: turn.ID,
				RunID: turn.RunID, ProfileID: turn.ProfileID,
				ProfileKind:      turn.ProfileKind,
				State:            ExecutionSpawnIntent,
				RequestDigest:    turn.RequestDigest,
				ConfigDigest:     turn.ConfigDigest,
				BasePromptDigest: turn.BasePromptDigest,
				CWD:              turn.CWD, CreatedAt: turn.CreatedAt,
			}
			err = nil
		}
		if err != nil {
			return err
		}
		if !options.AcknowledgeUnknown {
			resolved, reconcileErr := reconcileProcess(ctx, execution, options.Terminate)
			if reconcileErr != nil {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict, reconcileErr.Error(),
				)
				return nil
			}
			if !resolved {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"execution identity remains live or ambiguous",
				)
				return nil
			}
		}
		now := service.now().UTC()
		unknown := sessionRuntimeError(
			contract.ErrorConflict,
			"Session execution ended with unknown outcome and was explicitly reconciled",
		)
		turn.State = TurnFailed
		turn.Error = unknown
		turn.UpdatedAt = now
		sessionValue.State = SessionIdle
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		execution.State = ExecutionSettled
		execution.Outcome = OutcomeUnknown
		execution.Error = unknown
		execution.UpdatedAt = now
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: "turn.reconciled", TurnID: turn.ID,
			RunID: turn.RunID, ExecutionID: turn.ExecutionID,
			State: string(TurnFailed), Error: unknown,
		}); err != nil {
			return err
		}
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		if err := service.store.writeSession(sessionValue); err != nil {
			return err
		}
		result.State = TurnFailed
		result.Resolved = true
		return nil
	})
	if err != nil {
		return ReconcileResult{}, sessionRuntimeError(
			contract.ErrorInternal, err.Error(),
		)
	}
	if runtimeErr != nil {
		return result, runtimeErr
	}
	_ = service.store.rebuildIndex()
	return result, nil
}

func (service *Service) lastReconciledResult(
	sessionID string,
) (ReconcileResult, bool, error) {
	root := filepath.Join(service.store.sessionDir(sessionID), "executions")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return ReconcileResult{}, false, nil
	}
	if err != nil {
		return ReconcileResult{}, false, err
	}
	var values []Execution
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		value, err := service.store.loadExecution(
			sessionID, strings.TrimSuffix(entry.Name(), ".json"),
		)
		if err != nil {
			return ReconcileResult{}, false, err
		}
		values = append(values, value)
	}
	sort.Slice(values, func(left, right int) bool {
		if values[left].UpdatedAt.Equal(values[right].UpdatedAt) {
			return values[left].ID > values[right].ID
		}
		return values[left].UpdatedAt.After(values[right].UpdatedAt)
	})
	if len(values) == 0 ||
		values[0].State != ExecutionSettled ||
		values[0].Outcome != OutcomeUnknown {
		return ReconcileResult{}, false, nil
	}
	turn, err := service.store.loadTurn(sessionID, values[0].TurnID)
	if err != nil {
		return ReconcileResult{}, false, err
	}
	if turn.State != TurnFailed {
		return ReconcileResult{}, false, fmt.Errorf(
			"unknown-outcome execution %s has inconsistent turn state %s",
			values[0].ID, turn.State,
		)
	}
	return ReconcileResult{
		SessionID: sessionID,
		TurnID:    turn.ID, RunID: turn.RunID, ExecutionID: values[0].ID,
		State: turn.State, Resolved: true,
	}, true, nil
}

func reconcileProcess(
	ctx context.Context,
	execution Execution,
	terminate bool,
) (bool, error) {
	if execution.ProfileKind != "cli" {
		return false, fmt.Errorf(
			"API executor outcome requires acknowledge_unknown",
		)
	}
	if execution.State == ExecutionSpawnIntent &&
		(execution.Process == nil || execution.Process.HelperPID == 0) {
		return true, nil
	}
	if execution.Process == nil {
		return false, fmt.Errorf("execution has no process identity")
	}
	process := *execution.Process
	if process.HelperPID <= 0 || process.PGID <= 0 || process.StartToken == "" {
		return false, fmt.Errorf("execution process identity is incomplete")
	}
	alive, same, err := probeProcess(process.HelperPID, process.StartToken)
	if err != nil {
		return false, err
	}
	groupAlive, err := probeProcessGroup(process.PGID)
	if err != nil {
		return false, err
	}
	if !alive {
		if groupAlive {
			return false, fmt.Errorf(
				"process group %d may still be live after its leader exited",
				process.PGID,
			)
		}
		return true, nil
	}
	if !same {
		return false, fmt.Errorf(
			"pid %d was reused; process identity is ambiguous",
			process.HelperPID,
		)
	}
	pgid, err := syscall.Getpgid(process.HelperPID)
	if err != nil {
		return false, fmt.Errorf("read process group: %w", err)
	}
	if pgid != process.PGID || pgid != process.HelperPID {
		return false, fmt.Errorf(
			"process group identity does not match the recorded group leader",
		)
	}
	if !terminate {
		return false, nil
	}
	if process.HelperPID == os.Getpid() {
		return false, fmt.Errorf("refusing to terminate the current process")
	}
	if err := syscall.Kill(-process.PGID, syscall.SIGTERM); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("terminate process group: %w", err)
	}
	gone, err := waitProcessGroup(ctx, process.PGID, 2*time.Second)
	if err != nil {
		return false, err
	}
	if gone {
		return true, nil
	}
	if err := syscall.Kill(-process.PGID, syscall.SIGKILL); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("kill process group: %w", err)
	}
	return waitProcessGroup(ctx, process.PGID, 2*time.Second)
}

func waitProcessGroup(
	ctx context.Context,
	pgid int,
	timeout time.Duration,
) (bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		alive, err := probeProcessGroup(pgid)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

func probeProcess(pid int, startToken string) (alive bool, same bool, err error) {
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("probe pid %d: %w", pid, err)
	}
	token, err := processStartToken(pid)
	if err != nil {
		return false, false, fmt.Errorf("read pid %d identity: %w", pid, err)
	}
	return true, token == startToken, nil
}

func probeProcessGroup(pgid int) (bool, error) {
	if err := syscall.Kill(-pgid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("probe process group %d: %w", pgid, err)
	}
	return true, nil
}
