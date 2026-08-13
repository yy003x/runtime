package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	runtimetmux "github.com/yy003x/runtime/internal/infrastructure/tmux"
)

type fakeTmuxManager struct {
	windows       []runtimetmux.Window
	startRequest  *runtimetmux.StartRequest
	startResult   runtimetmux.StartResult
	startErr      error
	sentID        string
	sentInput     string
	attachedID    string
	interruptedID string
	stoppedIDs    []string
}

func (manager *fakeTmuxManager) Start(
	_ context.Context,
	request runtimetmux.StartRequest,
) (runtimetmux.StartResult, error) {
	manager.startRequest = &request
	result := manager.startResult
	if result.Window.Binding == nil && request.Invocation.Binding != nil {
		binding := *request.Invocation.Binding
		result.Window.Binding = &binding
	}
	return result, manager.startErr
}

func (manager *fakeTmuxManager) List(
	context.Context,
) ([]runtimetmux.Window, error) {
	return manager.windows, nil
}

func (manager *fakeTmuxManager) Send(
	_ context.Context,
	tmuxID string,
	input string,
) (runtimetmux.ActionResult, error) {
	manager.sentID = tmuxID
	manager.sentInput = input
	return runtimetmux.ActionResult{
		TmuxID: tmuxID, Action: "send", Accepted: true,
	}, nil
}

func (manager *fakeTmuxManager) Attach(
	_ context.Context,
	tmuxID string,
	_ runtimetmux.TTYFiles,
) error {
	manager.attachedID = tmuxID
	return nil
}

func (manager *fakeTmuxManager) Interrupt(
	_ context.Context,
	tmuxID string,
) (runtimetmux.ActionResult, error) {
	manager.interruptedID = tmuxID
	return runtimetmux.ActionResult{
		TmuxID: tmuxID, Action: "interrupt", Accepted: true,
	}, nil
}

func (manager *fakeTmuxManager) Stop(
	_ context.Context,
	tmuxID string,
) (runtimetmux.ActionResult, error) {
	manager.stoppedIDs = append(manager.stoppedIDs, tmuxID)
	return runtimetmux.ActionResult{
		TmuxID: tmuxID, Action: "stop", Accepted: true,
	}, nil
}

func tmuxInputFile(t *testing.T, value string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
