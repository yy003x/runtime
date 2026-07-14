package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func (s *Server) startTmux(ctx context.Context, request *TmuxStartRequest) Response {
	if request == nil || request.ProcessID == "" || request.Session == "" || request.CWD == "" || request.Command == "" {
		return Response{Error: "tmux start requires process_id, session, cwd and command"}
	}
	s.mu.Lock()
	if existing, ok := s.processes[request.ProcessID]; ok && tmuxHas(ctx, existing.Session) {
		s.mu.Unlock()
		return Response{OK: true, Session: existing.Session, Alive: true}
	}
	s.mu.Unlock()

	dependencyNames, err := s.ensureDependencies(ctx, request.ProcessID, request.Depends, request.Execution)
	if err != nil {
		return Response{Error: err.Error()}
	}
	command, err := s.executionCommand(request.Command, request.Execution)
	if err != nil {
		s.releaseDependencies(request.ProcessID)
		return Response{Error: err.Error()}
	}
	output, err := commandContext(ctx, "new-session", "-d", "-s", request.Session, "-c", request.CWD, command).CombinedOutput()
	if err != nil {
		s.releaseDependencies(request.ProcessID)
		return Response{Error: fmt.Sprintf("tmux new-session: %v: %s", err, strings.TrimSpace(string(output)))}
	}
	process := managedProcess{ProcessStatus: ProcessStatus{
		ID: request.ProcessID, Kind: "tmux", Session: request.Session, Alive: true,
		AuditProxy: request.Execution.AuditProxy, Shim: request.Execution.Shim,
		Dylib: request.Execution.Dylib != "", StartedAt: time.Now().UTC(),
	}, Depends: dependencyNames}
	s.mu.Lock()
	s.processes[request.ProcessID] = process
	s.lastActive = time.Now()
	err = s.persistRegistryLocked()
	s.mu.Unlock()
	if err != nil {
		_ = commandContext(context.Background(), "kill-session", "-t", request.Session).Run()
		s.releaseDependencies(request.ProcessID)
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Session: request.Session, Alive: true}
}

func (s *Server) hasTmux(ctx context.Context, processID, session string) (bool, error) {
	session = s.resolveSession(processID, session)
	alive := tmuxHas(ctx, session)
	if !alive && processID != "" {
		s.removeProcess(processID)
	}
	return alive, nil
}

func (s *Server) captureTmux(ctx context.Context, processID, session string, tail int) (string, error) {
	session = s.resolveSession(processID, session)
	if tail <= 0 {
		tail = 120
	}
	output, err := commandContext(ctx, "capture-pane", "-p", "-t", session, "-S", fmt.Sprintf("-%d", tail)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (s *Server) sendTmux(ctx context.Context, processID, session, value string, submit, bracketed bool) error {
	session = s.resolveSession(processID, session)
	buffer := fmt.Sprintf("agent-runtime-%d", time.Now().UnixNano())
	load := commandContext(ctx, "load-buffer", "-b", buffer, "-")
	load.Stdin = strings.NewReader(value)
	if output, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = commandContext(context.Background(), "delete-buffer", "-b", buffer).Run() }()
	args := []string{"paste-buffer", "-d", "-b", buffer, "-t", session}
	if bracketed {
		args = append([]string{"paste-buffer", "-p"}, args[1:]...)
	}
	if output, err := commandContext(ctx, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if submit {
		if output, err := commandContext(ctx, "send-keys", "-t", session, "Enter").CombinedOutput(); err != nil {
			return fmt.Errorf("tmux send Enter: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (s *Server) interruptTmux(ctx context.Context, processID, session string) error {
	session = s.resolveSession(processID, session)
	return commandContext(ctx, "send-keys", "-t", session, "C-c").Run()
}

func (s *Server) killTmux(ctx context.Context, processID, session string) error {
	session = s.resolveSession(processID, session)
	var err error
	if tmuxHas(ctx, session) {
		err = commandContext(ctx, "kill-session", "-t", session).Run()
	}
	if processID != "" {
		s.removeProcess(processID)
	} else {
		s.removeProcessBySession(session)
	}
	return err
}

func (s *Server) resolveSession(processID, session string) string {
	if session != "" {
		return session
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processes[processID].Session
}

func (s *Server) removeProcess(processID string) {
	s.mu.Lock()
	delete(s.processes, processID)
	_ = s.persistRegistryLocked()
	s.lastActive = time.Now()
	s.mu.Unlock()
	s.releaseDependencies(processID)
}

func (s *Server) removeProcessBySession(session string) {
	s.mu.Lock()
	processID := ""
	for id, process := range s.processes {
		if process.Session == session {
			processID = id
			delete(s.processes, id)
			break
		}
	}
	_ = s.persistRegistryLocked()
	s.lastActive = time.Now()
	s.mu.Unlock()
	if processID != "" {
		s.releaseDependencies(processID)
	}
}

func tmuxAvailable() error {
	_, err := exec.LookPath("tmux")
	return err
}
