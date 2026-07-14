package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type managedProcess struct {
	ProcessStatus
	Depends []string `json:"depends,omitempty"`
}

type Server struct {
	config    Config
	logger    *log.Logger
	startedAt time.Time

	mu           sync.Mutex
	listener     net.Listener
	processes    map[string]managedProcess
	dependencies map[string]*dependencyProcess
	proxy        *auditProxy
	lastActive   time.Time
	binaryPath   string
	binaryMtime  int64
}

func NewServer(config Config) *Server {
	config = config.normalized()
	binaryPath, _ := os.Executable()
	binaryMtime := int64(0)
	if info, err := os.Stat(binaryPath); err == nil {
		binaryMtime = info.ModTime().UnixNano()
	}
	return &Server{
		config:       config,
		logger:       log.New(os.Stderr, "[agent-runtime-daemon] ", log.LstdFlags|log.Lmsgprefix),
		startedAt:    time.Now(),
		processes:    map[string]managedProcess{},
		dependencies: map[string]*dependencyProcess{},
		lastActive:   time.Now(),
		binaryPath:   binaryPath,
		binaryMtime:  binaryMtime,
	}
}

func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.config.Dir, 0o700); err != nil {
		return fmt.Errorf("create daemon dir: %w", err)
	}
	if err := s.cleanStale(); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.config.SocketPath())
	if err != nil {
		return fmt.Errorf("listen daemon socket: %w", err)
	}
	s.listener = listener
	_ = os.Chmod(s.config.SocketPath(), 0o600)
	if err := s.writeIdentity(); err != nil {
		listener.Close()
		return err
	}
	if err := s.loadRegistry(ctx); err != nil {
		s.logger.Printf("load process registry: %v", err)
	}
	defer s.cleanup(false)

	accept := make(chan net.Conn)
	acceptErr := make(chan error, 1)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			accept <- connection
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-acceptErr:
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		case connection := <-accept:
			go s.handle(connection)
		case <-ticker.C:
			if s.shouldIdleExit() {
				s.logger.Printf("idle timeout reached")
				return nil
			}
		}
	}
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Minute))
	var request Request
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&request); err != nil {
		s.writeResponse(connection, Response{Error: err.Error()})
		return
	}
	token, err := os.ReadFile(s.config.TokenPath())
	if err != nil || request.Token != strings.TrimSpace(string(token)) {
		s.writeResponse(connection, Response{Error: "invalid auth token"})
		return
	}
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()

	var response Response
	switch request.Type {
	case MessageStatus:
		response = Response{OK: true, Status: s.status(context.Background())}
	case MessageShutdown:
		response = Response{OK: true}
		s.writeResponse(connection, response)
		if request.Cleanup {
			s.cleanupProcesses(context.Background())
		}
		_ = s.listener.Close()
		return
	case MessageAcquire:
		response = s.acquire(context.Background(), request.ProcessID, request.Depends, request.Execution)
	case MessageRelease:
		s.removeProcess(request.ProcessID)
		response = Response{OK: true}
	case MessageTmuxStart:
		response = s.startTmux(context.Background(), request.TmuxStart)
	case MessageTmuxHas:
		alive, callErr := s.hasTmux(context.Background(), request.ProcessID, request.Session)
		response = rpcResponse(callErr)
		response.Alive = alive
	case MessageTmuxCapture:
		output, callErr := s.captureTmux(context.Background(), request.ProcessID, request.Session, request.Tail)
		response = rpcResponse(callErr)
		response.Output = output
	case MessageTmuxSend:
		response = rpcResponse(s.sendTmux(context.Background(), request.ProcessID, request.Session, request.Text, request.Submit, request.Bracketed))
	case MessageTmuxInterrupt:
		response = rpcResponse(s.interruptTmux(context.Background(), request.ProcessID, request.Session))
	case MessageTmuxKill:
		response = rpcResponse(s.killTmux(context.Background(), request.ProcessID, request.Session))
	default:
		response = Response{Error: "unknown message type: " + string(request.Type)}
	}
	s.writeResponse(connection, response)
}

func (s *Server) status(ctx context.Context) *Status {
	s.mu.Lock()
	processes := make([]ProcessStatus, 0, len(s.processes))
	stale := make([]string, 0)
	for id, process := range s.processes {
		process.Alive = process.Kind != "tmux" || tmuxHas(ctx, process.Session)
		if !process.Alive {
			delete(s.processes, id)
			stale = append(stale, id)
			continue
		}
		processes = append(processes, process.ProcessStatus)
	}
	_ = s.persistRegistryLocked()
	sort.Slice(processes, func(i, j int) bool { return processes[i].ID < processes[j].ID })
	s.mu.Unlock()
	for _, processID := range stale {
		s.releaseDependencies(processID)
	}
	s.mu.Lock()
	dependencies := s.dependencyStatusLocked()
	proxyStatus := ProxyStatus{}
	if s.proxy != nil {
		proxyStatus = s.proxy.Status()
	}
	s.mu.Unlock()
	return &Status{
		Version: s.config.Version, BinaryPath: s.binaryPath, BinaryMtimeNanos: s.binaryMtime,
		PID: os.Getpid(), Socket: s.config.SocketPath(),
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()), Clients: len(processes),
		Processes: processes, Dependencies: dependencies, Proxy: proxyStatus,
	}
}

func (s *Server) shouldIdleExit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.processes) > 0 || len(s.dependencies) > 0 {
		return false
	}
	return time.Since(s.lastActive) >= s.config.IdleTimeout
}

func (s *Server) cleanStale() error {
	data, err := os.ReadFile(s.config.PIDPath())
	if err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if processAlive(pid) {
			connection, dialErr := net.DialTimeout("unix", s.config.SocketPath(), 200*time.Millisecond)
			if dialErr == nil {
				_ = connection.Close()
				return fmt.Errorf("daemon already running with pid %d", pid)
			}
			s.logger.Printf("cleaning stale daemon identity for unresponsive pid %d", pid)
		}
	}
	_ = os.Remove(s.config.SocketPath())
	_ = os.Remove(s.config.TokenPath())
	_ = os.Remove(s.config.PIDPath())
	return nil
}

func (s *Server) writeIdentity() error {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return err
	}
	if err := atomicWrite(s.config.TokenPath(), []byte(hex.EncodeToString(bytes)+"\n"), 0o600); err != nil {
		return err
	}
	return atomicWrite(s.config.PIDPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func (s *Server) loadRegistry(ctx context.Context) error {
	data, err := os.ReadFile(s.config.RegistryPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored []managedProcess
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, process := range stored {
		if process.ID != "" && tmuxHas(ctx, process.Session) {
			process.Alive = true
			s.processes[process.ID] = process
		}
	}
	return s.persistRegistryLocked()
}

func (s *Server) persistRegistryLocked() error {
	stored := make([]managedProcess, 0, len(s.processes))
	for _, process := range s.processes {
		stored = append(stored, process)
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].ID < stored[j].ID })
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.config.RegistryPath(), append(data, '\n'), 0o600)
}

func (s *Server) cleanup(cleanProcesses bool) {
	if cleanProcesses {
		s.cleanupProcesses(context.Background())
	}
	s.stopDependencies()
	if s.proxy != nil {
		s.proxy.Close()
	}
	_ = os.Remove(s.config.SocketPath())
	_ = os.Remove(s.config.TokenPath())
	_ = os.Remove(s.config.PIDPath())
}

func (s *Server) cleanupProcesses(ctx context.Context) {
	s.mu.Lock()
	processes := make([]managedProcess, 0, len(s.processes))
	for _, process := range s.processes {
		processes = append(processes, process)
	}
	s.mu.Unlock()
	for _, process := range processes {
		_ = s.killTmux(ctx, process.ID, process.Session)
	}
}

func (s *Server) writeResponse(writer io.Writer, response Response) {
	_ = json.NewEncoder(writer).Encode(response)
}

func rpcResponse(err error) Response {
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true}
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func commandContext(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", args...)
}

func tmuxHas(ctx context.Context, session string) bool {
	return session != "" && commandContext(ctx, "has-session", "-t", session).Run() == nil
}

func stopProcessGroup(process *os.Process) {
	if process == nil {
		return
	}
	_ = syscall.Kill(-process.Pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
}
