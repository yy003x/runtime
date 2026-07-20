package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if request.ReadyFile != "" {
		_ = os.Remove(request.ReadyFile)
		command = fmt.Sprintf("stty sane 2>/dev/null || :; : > %s || exit 125; %s", shellQuote(request.ReadyFile), command)
	}
	if request.RestartMaxAttempts > 0 || request.ExitFile != "" {
		command = supervisedTmuxCommand(command, request.RestartMaxAttempts, request.RestartDelaySeconds, request.ExitFile)
	}
	initialCommand := command
	if request.LogFile != "" {
		initialCommand = "while :; do sleep 3600; done"
	}
	output, err := commandContext(ctx, "new-session", "-d", "-s", request.Session, "-c", request.CWD, initialCommand).CombinedOutput()
	if err != nil {
		s.releaseDependencies(request.ProcessID)
		return Response{Error: fmt.Sprintf("tmux new-session: %v: %s", err, strings.TrimSpace(string(output)))}
	}
	if request.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(request.LogFile), 0o700); err != nil {
			_ = commandContext(context.Background(), "kill-session", "-t", request.Session).Run()
			s.releaseDependencies(request.ProcessID)
			return Response{Error: fmt.Sprintf("create tmux log directory: %v", err)}
		}
		pipeCommand := "cat >> " + shellQuote(request.LogFile)
		if pipeOutput, pipeErr := commandContext(ctx, "pipe-pane", "-o", "-t", request.Session, pipeCommand).CombinedOutput(); pipeErr != nil {
			_ = commandContext(context.Background(), "kill-session", "-t", request.Session).Run()
			s.releaseDependencies(request.ProcessID)
			return Response{Error: fmt.Sprintf("tmux pipe-pane: %v: %s", pipeErr, strings.TrimSpace(string(pipeOutput)))}
		}
		if respawnOutput, respawnErr := commandContext(ctx, "respawn-pane", "-k", "-t", request.Session, "-c", request.CWD, command).CombinedOutput(); respawnErr != nil {
			_ = commandContext(context.Background(), "kill-session", "-t", request.Session).Run()
			s.releaseDependencies(request.ProcessID)
			return Response{Error: fmt.Sprintf("tmux respawn-pane: %v: %s", respawnErr, strings.TrimSpace(string(respawnOutput)))}
		}
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

func supervisedTmuxCommand(command string, maxAttempts int, delaySeconds float64, exitFile string) string {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if delaySeconds < 0 {
		delaySeconds = 0
	}
	delay := strconv.FormatFloat(delaySeconds, 'f', -1, 64)
	writeExit := ""
	if exitFile != "" {
		writeExit = fmt.Sprintf("printf '%%s %%s\\n' \"$exit_code\" \"$attempt\" > %s; ", shellQuote(exitFile))
	}
	return fmt.Sprintf(
		"attempt=1; exit_code=0; while :; do /bin/sh -c %s; exit_code=$?; if [ \"$exit_code\" -eq 0 ] || [ \"$attempt\" -ge %d ]; then break; fi; printf '\\r\\n[sn-runtime] process exited with status %%s; restarting attempt %%s/%d\\r\\n' \"$exit_code\" \"$((attempt+1))\"; attempt=$((attempt+1)); sleep %s; done; %sexit \"$exit_code\"",
		shellQuote(command), maxAttempts, maxAttempts, delay, writeExit,
	)
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
		timer := time.NewTimer(30 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		submitByte := "0a"
		if bracketed {
			submitByte = "0d"
		}
		if output, err := commandContext(ctx, "send-keys", "-H", "-t", session, submitByte).CombinedOutput(); err != nil {
			return fmt.Errorf("tmux submit: %w: %s", err, strings.TrimSpace(string(output)))
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
