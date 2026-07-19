package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Client struct {
	config Config
}

func NewClient(config Config) *Client {
	return &Client{config: config.normalized()}
}

func (c *Client) Config() Config { return c.config }

func (c *Client) EnsureRunning(ctx context.Context) (*Status, string, error) {
	status, err := c.Status(ctx)
	if err == nil {
		if !c.daemonStale(status) {
			return status, "", nil
		}
		if len(status.Processes) == 0 && len(status.Dependencies) == 0 && !status.Busy {
			_ = c.Shutdown(ctx, false)
			_ = c.waitStopped(ctx, 3*time.Second)
		} else {
			return status, "daemon version differs; active work keeps the current daemon", nil
		}
	}
	if err := c.start(); err != nil {
		return nil, "", err
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		status, lastErr = c.Status(ctx)
		if lastErr == nil {
			return status, "", nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, "", fmt.Errorf("daemon did not become ready: %w; log=%s", lastErr, c.config.LogPath())
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	response, err := c.send(ctx, Request{Type: MessageStatus})
	if err != nil {
		return nil, err
	}
	return response.Status, nil
}

func (c *Client) Shutdown(ctx context.Context, cleanup bool) error {
	_, err := c.send(ctx, Request{Type: MessageShutdown, Cleanup: cleanup})
	return err
}

func (c *Client) Stop(ctx context.Context, cleanup bool, timeout time.Duration) error {
	if err := c.Shutdown(ctx, cleanup); err != nil {
		return err
	}
	return c.waitStopped(ctx, timeout)
}

func (c *Client) Acquire(ctx context.Context, processID string, dependencies []Dependency, execution ExecutionEnvironment) (map[string]string, error) {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return nil, err
	}
	response, err := c.send(ctx, Request{Type: MessageAcquire, ProcessID: processID, Depends: dependencies, Execution: execution})
	if err != nil {
		return nil, err
	}
	return response.Environment, nil
}

func (c *Client) Release(ctx context.Context, processID string) error {
	_, err := c.send(ctx, Request{Type: MessageRelease, ProcessID: processID})
	return err
}

func (c *Client) StartTmux(ctx context.Context, request TmuxStartRequest) (string, error) {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return "", err
	}
	response, err := c.send(ctx, Request{Type: MessageTmuxStart, TmuxStart: &request})
	if err != nil {
		return "", err
	}
	return response.Session, nil
}

func (c *Client) HasTmux(ctx context.Context, processID, session string) (bool, error) {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return false, err
	}
	response, err := c.send(ctx, Request{Type: MessageTmuxHas, ProcessID: processID, Session: session})
	if err != nil {
		return false, err
	}
	return response.Alive, nil
}

func (c *Client) CaptureTmux(ctx context.Context, processID, session string, tail int) (string, error) {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return "", err
	}
	response, err := c.send(ctx, Request{Type: MessageTmuxCapture, ProcessID: processID, Session: session, Tail: tail})
	if err != nil {
		return "", err
	}
	return response.Output, nil
}

func (c *Client) SendTmux(ctx context.Context, processID, session, value string, submit, bracketed bool) error {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return err
	}
	_, err := c.send(ctx, Request{Type: MessageTmuxSend, ProcessID: processID, Session: session, Text: value, Submit: submit, Bracketed: bracketed})
	return err
}

func (c *Client) InterruptTmux(ctx context.Context, processID, session string) error {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return err
	}
	_, err := c.send(ctx, Request{Type: MessageTmuxInterrupt, ProcessID: processID, Session: session})
	return err
}

func (c *Client) KillTmux(ctx context.Context, processID, session string) error {
	if _, _, err := c.EnsureRunning(ctx); err != nil {
		return err
	}
	_, err := c.send(ctx, Request{Type: MessageTmuxKill, ProcessID: processID, Session: session})
	return err
}

func (c *Client) send(ctx context.Context, request Request) (*Response, error) {
	token, err := os.ReadFile(c.config.TokenPath())
	if err != nil {
		return nil, fmt.Errorf("read daemon token: %w", err)
	}
	request.Token = strings.TrimSpace(string(token))
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	connection, err := dialer.DialContext(ctx, "unix", c.config.SocketPath())
	if err != nil {
		return nil, fmt.Errorf("dial daemon %s: %w", c.config.SocketPath(), err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Minute))
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, fmt.Errorf("encode daemon request: %w", err)
	}
	var response Response
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&response); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if !response.OK {
		return &response, fmt.Errorf("daemon: %s", response.Error)
	}
	return &response, nil
}

func (c *Client) start() error {
	if err := os.MkdirAll(c.config.Dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.config.LogPath()), 0o700); err != nil {
		return err
	}
	executable, err := c.executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(c.config.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(executable, "daemon", "serve")
	command.Env = append(os.Environ(), "SN_CLI_HOME="+c.config.Home, "AGENT_RUNTIME_VERSION="+c.config.Version)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	return command.Process.Release()
}

func (c *Client) executable() (string, error) {
	if c.config.Executable != "" {
		return c.config.Executable, nil
	}
	self, _ := os.Executable()
	if filepath.Base(self) == "sn-cli" {
		return self, nil
	}
	candidate := filepath.Join(c.config.Home, "bin", "sn-cli")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, nil
	}
	if path, err := exec.LookPath("sn-cli"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("sn-cli executable not found; build it before starting daemon")
}

func (c *Client) daemonStale(status *Status) bool {
	if status == nil || status.Version != c.config.Version {
		return true
	}
	if self, err := os.Executable(); c.config.Executable == "" && err == nil && strings.HasSuffix(filepath.Base(self), ".test") {
		return false
	}
	executable, err := c.executable()
	if err != nil || status.BinaryPath == "" || status.BinaryMtimeNanos == 0 {
		return false
	}
	executable = canonicalPath(executable)
	running := canonicalPath(status.BinaryPath)
	info, err := os.Stat(executable)
	if err != nil {
		return false
	}
	return executable != running || info.ModTime().UnixNano() != status.BinaryMtimeNanos
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	return path
}

func (c *Client) waitStopped(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.config.SocketPath()); os.IsNotExist(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon did not stop within %s", timeout)
}
