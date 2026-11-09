package cli

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAEPTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, directory := range []string{"configs", "state", "bin"} {
		if err := os.Mkdir(filepath.Join(home, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(home, "bin", "codex")
	for _, file := range []struct {
		path    string
		content []byte
		mode    os.FileMode
	}{
		{script, []byte("#!/bin/sh\nsleep 60\n"), 0o700},
		{filepath.Join(home, "runtime.json"), []byte(`{"agent":{"tools":["read_file","list_directory"]}}`), 0o600},
		{filepath.Join(home, "configs", "cx.json"), fmt.Appendf(nil,
			`{"type":"cli","command":%q,"model":"fixture"}`, script), 0o600},
	} {
		if err := os.WriteFile(file.path, file.content, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

type aepTestProcess struct {
	command  *exec.Cmd
	stdin    io.WriteCloser
	messages chan map[string]any
	done     chan struct{}
	err      error
	stderr   bytes.Buffer
}

func startAEPTestProcess(t *testing.T, home string) *aepTestProcess {
	t.Helper()
	process := &aepTestProcess{
		command:  exec.Command(os.Args[0], "-test.run=^TestAEPServeOverStdio$", "-test.v=false"),
		messages: make(chan map[string]any, 256), done: make(chan struct{}),
	}
	process.command.Env = append(os.Environ(), "SN_CLI_AEP_HELPER=1", "SN_CLI_HOME="+home, "PATH=/usr/bin:/bin")
	process.command.Stderr = &process.stderr
	var err error
	process.stdin, err = process.command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := process.command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(process.done)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var value map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &value); err == nil {
				process.messages <- value
			} else {
				process.err = err
			}
		}
		if err := scanner.Err(); err != nil {
			process.err = err
		}
		if err := process.command.Wait(); err != nil {
			process.err = err
		}
	}()
	t.Cleanup(func() {
		_ = process.stdin.Close()
		select {
		case <-process.done:
		default:
			_ = process.command.Process.Kill()
			select {
			case <-process.done:
			case <-time.After(10 * time.Second):
				t.Error("AEP process did not exit")
			}
		}
	})
	process.request(t, "init", "initialize", map[string]any{})
	return process
}

func (process *aepTestProcess) request(t *testing.T, id, method string, params map[string]any) map[string]any {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.stdin.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	resultFor := func(value map[string]any) (map[string]any, bool) {
		if value["id"] != id {
			return nil, false
		}
		result, ok := value["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s failed: %#v", method, value)
		}
		return result, true
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case value := <-process.messages:
			if result, matched := resultFor(value); matched {
				return result
			}
		case <-process.done:
			// Wait 完成时最后一个响应可能已入队但尚未被 select 消费。
			for len(process.messages) != 0 {
				if result, matched := resultFor(<-process.messages); matched {
					return result
				}
			}
			t.Fatalf("AEP exited: %v stderr=%s", process.err, process.stderr.String())
		case <-deadline:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

func (process *aepTestProcess) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-process.done:
		return process.err
	case <-time.After(20 * time.Second):
		t.Fatal("AEP did not exit")
		return nil
	}
}

func TestAEPJoiningSharedQueuePreservesActiveRun(t *testing.T) {
	home := newAEPTestHome(t)
	first := startAEPTestProcess(t, home)
	start := first.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "sleep", "cwd": home},
	})
	runID := start["runId"].(string)
	db, err := sql.Open("sqlite", filepath.Join(home, "state", "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := db.QueryRow("SELECT state FROM runs WHERE run_id = ?", runID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not start: %s", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := startAEPTestProcess(t, home)
	second.request(t, "shutdown", "shutdown", map[string]any{})
	if err := second.wait(t); err != nil {
		t.Fatalf("second AEP: %v stderr=%s", err, second.stderr.String())
	}
	view := first.request(t, "get", "run.get", map[string]any{"runId": runID})
	if view["status"] != "running" {
		t.Fatalf("joining worker changed active run: %#v", view)
	}
	first.request(t, "shutdown", "shutdown", map[string]any{})
	if err := first.wait(t); err != nil {
		t.Fatalf("first AEP: %v stderr=%s", err, first.stderr.String())
	}
}

func TestAEPWorkerFailureStopsServingAndReturnsError(t *testing.T) {
	home := newAEPTestHome(t)
	process := startAEPTestProcess(t, home)
	db, err := sql.Open("sqlite", filepath.Join(home, "state", "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 只破坏临时测试数据库的领取表，触发真实 Worker/Claim 错误；stdin 保持打开。
	if _, err := db.Exec("ALTER TABLE queue RENAME TO unavailable_queue"); err != nil {
		t.Fatal(err)
	}
	if err := process.wait(t); err == nil {
		t.Fatalf("worker failure was hidden; stderr=%s", process.stderr.String())
	}
	if !strings.Contains(process.stderr.String(), "queue") {
		t.Fatalf("original worker error missing: %s", process.stderr.String())
	}
}

// TestAEPServeOverStdio 以子进程方式驱动真实 `sn-cli aep`：stdin/stdout 承载
// AEP NDJSON JSON-RPC，fake codex 脚本承接真实 managed Session 执行链
// （sqlite Run store + scheduler worker + terminal publish barrier）。
func TestAEPServeOverStdio(t *testing.T) {
	if os.Getenv("SN_CLI_AEP_HELPER") == "1" {
		os.Exit(Main([]string{"aep"}))
	}
	home := t.TempDir()
	temp := t.TempDir()
	binDir := filepath.Join(temp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, "codex")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' \
  '{"type":"turn.completed"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(home, "configs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// active home 布局中 runtime.json 位于根目录（release 映射约定）。
	if err := os.WriteFile(
		filepath.Join(home, "runtime.json"),
		[]byte(`{"agent":{"tools":["read_file","list_directory"]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "cx.json"),
		fmt.Appendf(nil, `{"type":"cli","command":%q,"model":"fixture"}`, script),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		os.Args[0], "-test.run=^TestAEPServeOverStdio$", "-test.v=false",
	)
	command.Env = append(
		os.Environ(),
		"SN_CLI_AEP_HELPER=1",
		"SN_CLI_HOME="+home,
		"PATH="+binDir+string(os.PathListSeparator)+"/usr/bin:/bin",
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	messages := make(chan map[string]any, 64)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var value map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
				t.Errorf("non-protocol stdout line %q: %v", scanner.Text(), err)
				continue
			}
			messages <- value
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("read stdout: %v", err)
		}
	}()
	var stderrText strings.Builder
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			stderrText.WriteString(scanner.Text())
			stderrText.WriteByte('\n')
		}
	}()

	send := func(id, method string, params map[string]any) {
		t.Helper()
		frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			frame["params"] = params
		}
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(match func(map[string]any) bool) map[string]any {
		deadline := time.After(30 * time.Second)
		for {
			select {
			case value := <-messages:
				if match(value) {
					return value
				}
			case <-deadline:
				t.Fatalf("timed out; stderr=%s", stderrText.String())
			}
		}
	}

	send("init", "initialize", map[string]any{})
	initResponse := waitFor(func(value map[string]any) bool {
		return value["id"] == "init"
	})
	if _, failed := initResponse["error"]; failed {
		t.Fatalf("initialize failed: %#v stderr=%s", initResponse, stderrText.String())
	}
	send("start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "say ok", "cwd": temp},
	})
	startResponse := waitFor(func(value map[string]any) bool {
		return value["id"] == "start"
	})
	startResult, ok := startResponse["result"].(map[string]any)
	if !ok {
		t.Fatalf("run.start failed: %#v stderr=%s", startResponse, stderrText.String())
	}
	runID, _ := startResult["runId"].(string)
	if runID == "" {
		t.Fatalf("run.start result=%#v", startResult)
	}
	completed := waitFor(func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.completed"
	})
	if params := completed["params"].(map[string]any); params["runId"] != runID {
		t.Fatalf("run.completed=%#v", completed)
	}
	send("result", "run.result", map[string]any{"runId": runID})
	resultResponse := waitFor(func(value map[string]any) bool {
		return value["id"] == "result"
	})
	final, ok := resultResponse["result"].(map[string]any)
	if !ok || final["status"] != "succeeded" || final["summary"] != "ok" {
		t.Fatalf("run.result response=%#v stderr=%s", resultResponse, stderrText.String())
	}
	send("shutdown", "shutdown", map[string]any{})
	shutdownResponse := waitFor(func(value map[string]any) bool {
		return value["id"] == "shutdown"
	})
	if _, failed := shutdownResponse["error"]; failed {
		t.Fatalf("shutdown failed: %#v", shutdownResponse)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sn-cli aep exited with error: %v stderr=%s", err, stderrText.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("sn-cli aep did not exit after shutdown; stderr=%s", stderrText.String())
	}
}
