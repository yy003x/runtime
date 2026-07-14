package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketPathFallsBackForLongDaemonDirectory(t *testing.T) {
	config := Config{Dir: filepath.Join(t.TempDir(), strings.Repeat("long-path-", 20))}
	path := config.SocketPath()
	if len(path) > 96 || !strings.HasPrefix(path, "/tmp/agent-runtime-") {
		t.Fatalf("socket path %q is not a short fallback", path)
	}
}

func TestDaemonStaleDetectsExecutableChange(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "sn-cli")
	if err := os.WriteFile(executable, []byte("first"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Config{Dir: t.TempDir(), Version: "binary-test", Executable: executable})
	status := &Status{Version: "binary-test", BinaryPath: executable, BinaryMtimeNanos: info.ModTime().UnixNano()}
	if client.daemonStale(status) {
		t.Fatal("unchanged daemon binary was marked stale")
	}
	changed := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(executable, changed, changed); err != nil {
		t.Fatal(err)
	}
	if !client.daemonStale(status) {
		t.Fatal("changed daemon binary was not marked stale")
	}
}

func TestDaemonTmuxRegistrySurvivesRestart(t *testing.T) {
	requireTmux(t)
	daemonDir, err := os.MkdirTemp("/tmp", "agent-runtime-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(daemonDir) })
	config := Config{Root: t.TempDir(), Dir: daemonDir, Version: "test-v1", IdleTimeout: time.Minute}
	client, stop := startTestServer(t, config)
	session, err := client.StartTmux(context.Background(), TmuxStartRequest{
		ProcessID: "session/test", Session: "daemon-registry-test", CWD: t.TempDir(),
		Command: "while :; do sleep 60; done",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil || len(status.Processes) != 1 || status.Processes[0].Session != session {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	stop(false)

	client, stop = startTestServer(t, config)
	status, err = client.Status(context.Background())
	if err != nil || len(status.Processes) != 1 || !status.Processes[0].Alive {
		t.Fatalf("restarted status=%#v err=%v", status, err)
	}
	if err := client.KillTmux(context.Background(), "session/test", session); err != nil {
		t.Fatal(err)
	}
	status, err = client.Status(context.Background())
	if err != nil || len(status.Processes) != 0 {
		t.Fatalf("cleaned status=%#v err=%v", status, err)
	}
	stop(true)
}

func TestExecutionEnvironmentIsExplicit(t *testing.T) {
	server := NewServer(Config{Dir: t.TempDir(), Version: "test"})
	plain := "printf ordinary"
	got, err := server.executionCommand(plain, ExecutionEnvironment{})
	if err != nil || got != plain || server.proxy != nil {
		t.Fatalf("plain=%q proxy=%#v err=%v", got, server.proxy, err)
	}
	injected, err := server.executionCommand(plain, ExecutionEnvironment{AuditProxy: true, Shim: true, Dylib: "/tmp/interpose.dylib", Bypass: []string{"localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.proxy.Close()
	for _, value := range []string{"HTTP_PROXY", "PATH", "DYLD_INSERT_LIBRARIES", "NO_PROXY", plain} {
		if !strings.Contains(injected, value) {
			t.Fatalf("injected command %q missing %q", injected, value)
		}
	}
	if !server.proxy.Status().Enabled {
		t.Fatal("audit proxy must be enabled")
	}
}

func TestDependencyReleasedWithTmuxProcess(t *testing.T) {
	requireTmux(t)
	daemonDir, err := os.MkdirTemp("/tmp", "agent-runtime-dep-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(daemonDir) })
	config := Config{Root: t.TempDir(), Dir: daemonDir, Version: "dep-test", IdleTimeout: time.Minute}
	client, stop := startTestServer(t, config)
	defer stop(true)
	session, err := client.StartTmux(context.Background(), TmuxStartRequest{
		ProcessID: "task/dep", Session: "daemon-dependency-test", CWD: t.TempDir(),
		Command: "while :; do sleep 60; done",
		Depends: []Dependency{{Command: "sleep 30", Restart: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil || len(status.Dependencies) != 1 || !status.Dependencies[0].Healthy || status.Dependencies[0].Owners != 1 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if err := client.KillTmux(context.Background(), "task/dep", session); err != nil {
		t.Fatal(err)
	}
	status, err = client.Status(context.Background())
	if err != nil || len(status.Processes) != 0 || len(status.Dependencies) != 0 {
		t.Fatalf("released status=%#v err=%v", status, err)
	}
}

func TestStatusReleasesDependencyWhenTmuxDisappears(t *testing.T) {
	requireTmux(t)
	daemonDir, err := os.MkdirTemp("/tmp", "agent-runtime-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(daemonDir) })
	config := Config{Root: t.TempDir(), Dir: daemonDir, Version: "stale-test", IdleTimeout: time.Minute}
	client, stop := startTestServer(t, config)
	defer stop(true)
	session, err := client.StartTmux(context.Background(), TmuxStartRequest{
		ProcessID: "task/stale", Session: "daemon-stale-test", CWD: t.TempDir(),
		Command: "while :; do sleep 60; done", Depends: []Dependency{{Command: "sleep 30"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("tmux", "kill-session", "-t", session).Run(); err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil || len(status.Processes) != 0 || len(status.Dependencies) != 0 {
		t.Fatalf("stale status=%#v err=%v", status, err)
	}
}

func TestFailedRestartingDependencyIsNotRegistered(t *testing.T) {
	server := NewServer(Config{Dir: t.TempDir(), Version: "dependency-failure"})
	response := server.acquire(context.Background(), "lease/failure", []Dependency{{Command: "exit 1", Restart: true}}, ExecutionEnvironment{})
	if response.OK || response.Error == "" {
		t.Fatalf("response=%#v", response)
	}
	status := server.status(context.Background())
	if len(status.Processes) != 0 || len(status.Dependencies) != 0 {
		t.Fatalf("failed dependency remained registered: %#v", status)
	}
}

func TestAuditProxyRejectsUnsupportedOrDifferentUpstreams(t *testing.T) {
	if proxy, err := newAuditProxy([]string{"https://127.0.0.1:3128"}); err == nil {
		proxy.Close()
		t.Fatal("https upstream proxy was accepted")
	}
	server := NewServer(Config{Dir: t.TempDir(), Version: "proxy-test"})
	_, err := server.executionValues(ExecutionEnvironment{AuditProxy: true, Upstreams: []string{"http://127.0.0.1:3128"}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.proxy.Close()
	if _, err := server.executionValues(ExecutionEnvironment{AuditProxy: true, Upstreams: []string{"http://127.0.0.1:8080"}}); err == nil {
		t.Fatal("different upstream pool was silently reused")
	}
}

func TestDependencyHealthProbes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := probeTCP(context.Background(), listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer httpServer.Close()
	if err := probeHTTP(context.Background(), httpServer.URL); err != nil {
		t.Fatal(err)
	}
}

func startTestServer(t *testing.T, config Config) (*Client, func(bool)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := NewServer(config)
	go func() { done <- server.Run(ctx) }()
	client := NewClient(config)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("daemon exited during startup: %v", err)
		default:
		}
		if _, err := client.Status(context.Background()); err == nil {
			return client, func(cleanup bool) {
				_ = client.Shutdown(context.Background(), cleanup)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					cancel()
					t.Fatal("daemon did not stop")
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("daemon did not start")
	return nil, nil
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}
