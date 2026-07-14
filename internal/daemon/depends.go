package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type dependencyProcess struct {
	mu        sync.Mutex
	command   *exec.Cmd
	config    Dependency
	execution ExecutionEnvironment
	owners    map[string]struct{}
	healthy   bool
	lastError string
	stopping  bool
	restarts  int
}

func (s *Server) ensureDependencies(ctx context.Context, owner string, dependencies []Dependency, execution ExecutionEnvironment) ([]string, error) {
	names := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		dependency.Command = strings.TrimSpace(dependency.Command)
		if dependency.Command == "" {
			continue
		}
		if err := s.ensureDependency(ctx, owner, dependency, execution); err != nil {
			if dependency.Optional {
				s.logger.Printf("optional dependency %q unavailable: %v", dependency.Command, err)
				continue
			}
			s.releaseDependencies(owner)
			return nil, err
		}
		names = append(names, dependency.Command)
	}
	return names, nil
}

func (s *Server) ensureDependency(ctx context.Context, owner string, dependency Dependency, execution ExecutionEnvironment) error {
	s.mu.Lock()
	if existing := s.dependencies[dependency.Command]; existing != nil {
		existing.mu.Lock()
		existing.owners[owner] = struct{}{}
		healthy, lastError := existing.healthy, existing.lastError
		existing.mu.Unlock()
		s.mu.Unlock()
		if !healthy {
			return fmt.Errorf("dependency %q is not healthy: %s", dependency.Command, lastError)
		}
		return nil
	}
	process := &dependencyProcess{config: dependency, execution: execution, owners: map[string]struct{}{owner: {}}}
	s.dependencies[dependency.Command] = process
	s.mu.Unlock()

	if err := s.startDependency(ctx, process); err != nil {
		process.mu.Lock()
		process.lastError = err.Error()
		process.mu.Unlock()
		s.mu.Lock()
		delete(s.dependencies, dependency.Command)
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Server) startDependency(ctx context.Context, process *dependencyProcess) error {
	commandText, err := s.executionCommand(process.config.Command, process.execution)
	if err != nil {
		return err
	}
	command := exec.CommandContext(context.Background(), "sh", "-c", commandText)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdin = nil
	command.Stdout = nil
	var stderr io.ReadCloser
	if !process.config.Silent {
		stderr, err = command.StderrPipe()
		if err != nil {
			return err
		}
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start dependency %q: %w", process.config.Command, err)
	}
	process.mu.Lock()
	process.command = command
	process.healthy = false
	process.lastError = "starting"
	process.mu.Unlock()
	if stderr != nil {
		go s.forwardDependencyStderr(stderr, process.config.Command)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	if err := waitDependency(ctx, process.config, exited); err != nil {
		stopProcessGroup(command.Process)
		return fmt.Errorf("dependency %q: %w", process.config.Command, err)
	}
	process.mu.Lock()
	process.healthy = true
	process.lastError = ""
	process.mu.Unlock()
	go s.watchDependency(process, exited)
	return nil
}

func waitDependency(ctx context.Context, dependency Dependency, exited <-chan error) error {
	if dependency.WaitHTTP == "" && dependency.WaitTCP == "" {
		select {
		case err := <-exited:
			return fmt.Errorf("exited during startup: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var err error
		if dependency.WaitHTTP != "" {
			err = probeHTTP(deadline, dependency.WaitHTTP)
		} else {
			err = probeTCP(deadline, dependency.WaitTCP)
		}
		if err == nil {
			return nil
		}
		select {
		case exitErr := <-exited:
			return fmt.Errorf("exited before ready: %v", exitErr)
		case <-deadline.Done():
			return fmt.Errorf("health check timed out: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func probeTCP(ctx context.Context, address string) error {
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return connection.Close()
}

func probeHTTP(ctx context.Context, rawURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 500 * time.Millisecond}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	return nil
}

func (s *Server) watchDependency(process *dependencyProcess, exited <-chan error) {
	err := <-exited
	process.mu.Lock()
	process.healthy = false
	process.lastError = fmt.Sprint(err)
	stopping := process.stopping
	restart := process.config.Restart
	owners := len(process.owners)
	process.mu.Unlock()
	if stopping || !restart || owners == 0 {
		s.removeDependency(process.config.Command, process)
		return
	}
	for attempt := 1; attempt <= 10; attempt++ {
		process.mu.Lock()
		if process.stopping || len(process.owners) == 0 {
			process.mu.Unlock()
			break
		}
		process.restarts = attempt
		process.mu.Unlock()
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		if startErr := s.startDependency(context.Background(), process); startErr == nil {
			return
		} else {
			process.mu.Lock()
			process.lastError = startErr.Error()
			process.mu.Unlock()
		}
	}
	s.removeDependency(process.config.Command, process)
}

func (s *Server) releaseDependencies(owner string) {
	s.mu.Lock()
	processes := make([]*dependencyProcess, 0)
	for _, process := range s.dependencies {
		process.mu.Lock()
		delete(process.owners, owner)
		if len(process.owners) == 0 {
			process.stopping = true
			processes = append(processes, process)
		}
		process.mu.Unlock()
	}
	s.mu.Unlock()
	for _, process := range processes {
		process.mu.Lock()
		command := process.command
		process.mu.Unlock()
		if command != nil {
			stopProcessGroup(command.Process)
		}
		s.removeDependency(process.config.Command, process)
	}
}

func (s *Server) removeDependency(name string, expected *dependencyProcess) {
	s.mu.Lock()
	if s.dependencies[name] == expected {
		delete(s.dependencies, name)
		s.lastActive = time.Now()
	}
	s.mu.Unlock()
}

func (s *Server) stopDependencies() {
	s.mu.Lock()
	processes := make([]*dependencyProcess, 0, len(s.dependencies))
	for _, process := range s.dependencies {
		processes = append(processes, process)
	}
	s.dependencies = map[string]*dependencyProcess{}
	s.mu.Unlock()
	for _, process := range processes {
		process.mu.Lock()
		process.stopping = true
		command := process.command
		process.mu.Unlock()
		if command != nil {
			stopProcessGroup(command.Process)
		}
	}
}

func (s *Server) dependencyStatusLocked() []DependencyStatus {
	statuses := make([]DependencyStatus, 0, len(s.dependencies))
	for _, process := range s.dependencies {
		process.mu.Lock()
		status := DependencyStatus{Command: process.config.Command, Healthy: process.healthy, Restart: process.config.Restart, Optional: process.config.Optional, Owners: len(process.owners), Error: process.lastError}
		if process.command != nil && process.command.Process != nil {
			status.PID = process.command.Process.Pid
		}
		process.mu.Unlock()
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Command < statuses[j].Command })
	return statuses
}

func (s *Server) forwardDependencyStderr(reader io.Reader, name string) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		s.logger.Printf("depends %s: %s", name, scanner.Text())
	}
}
