//go:build darwin || linux

package commandgolden

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yy003x/runtime/runtimetest/ptyx"
)

const capturedEnvKeys = "CODEX_HOME,CLAUDE_CONFIG_DIR,ANTHROPIC_BASE_URL,ANTHROPIC_MODEL,RUNTIME_GOLDEN_REMOVE_ME"

type testHarness struct {
	repoRoot string
	snCLI    string
	home     string
	userHome string
	fakeBin  string
	image    string
	baseEnv  []string
}

func TestCommandProfileGolden(t *testing.T) {
	harness := newHarness(t)
	baseline, err := LoadBaseline(filepath.Join(harness.repoRoot, "runtimetest", "commandgolden", "testdata", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("baseline source=%s installed=%s", baseline.SourceHead, baseline.InstalledBuild)

	t.Run("profiles_under_pty", func(t *testing.T) {
		for _, golden := range baseline.Profiles {
			golden := golden
			t.Run(golden.ID, func(t *testing.T) {
				userArgs := []string{"user value"}
				capturePath := filepath.Join(t.TempDir(), "capture.json")
				command := exec.Command(harness.snCLI, append([]string{golden.ID}, userArgs...)...)
				environment := map[string]string{
					"RUNTIME_GOLDEN_CAPTURE":  capturePath,
					"RUNTIME_GOLDEN_ENV_KEYS": capturedEnvKeys,
					"RUNTIME_GOLDEN_STDOUT":   "fake stdout\n",
					"RUNTIME_GOLDEN_STDERR":   "fake stderr\n",
				}
				if !isExecProfile(golden.ID) {
					environment["RUNTIME_GOLDEN_READ_STDIN"] = "line"
				}
				command.Env = harness.environment(environment)
				command.Dir = harness.repoRoot
				process, err := ptyx.Start(command)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Master.Close() })
				output := readPTY(process)
				if !isExecProfile(golden.ID) {
					if _, err := process.Master.Write([]byte("hello from pty\n")); err != nil {
						t.Fatal(err)
					}
				}
				if err := process.Cmd.Wait(); err != nil {
					result := <-output
					t.Fatalf("wait: %v output=%q read_err=%v", err, result.value, result.err)
				}
				if result := <-output; result.err != nil {
					t.Fatal(result.err)
				}
				capture, err := ReadCapture(capturePath)
				if err != nil {
					t.Fatal(err)
				}
				replacements := map[string]string{"HOME": harness.userHome, "IMAGE_PATH": harness.image}
				expectedArgv := make([]string, 0, len(golden.Argv)+len(userArgs))
				for _, argument := range golden.Argv {
					expectedArgv = append(expectedArgv, Expand(argument, replacements))
				}
				expectedArgv = append(expectedArgv, "--")
				expectedArgv = append(expectedArgv, userArgs...)
				if !reflect.DeepEqual(capture.Argv, expectedArgv) {
					t.Fatalf("argv=\n%q\nwant=\n%q", capture.Argv, expectedArgv)
				}
				expectedEnv := map[string]string{}
				for name, value := range golden.Env {
					expectedEnv[name] = Expand(value, replacements)
				}
				if !reflect.DeepEqual(capture.Env, expectedEnv) {
					t.Fatalf("env=%#v want=%#v missing=%v", capture.Env, expectedEnv, capture.Missing)
				}
				if capture.TTY.Stdin == isExecProfile(golden.ID) ||
					!capture.TTY.Stdout || !capture.TTY.Stderr {
					t.Fatalf("tty=%#v", capture.TTY)
				}
				if !isExecProfile(golden.ID) &&
					capture.Stdin != "hello from pty\n" {
					t.Fatalf("stdin=%q", capture.Stdin)
				}
				if golden.ID == "commit" || golden.ID == "cx-deep" || golden.ID == "cx-spark" {
					if !containsAdjacent(capture.Argv, "exec", "--skip-git-repo-check") {
						t.Fatalf("%s missing adjacent exec --skip-git-repo-check: %q", golden.ID, capture.Argv)
					}
				}
			})
		}
	})

	t.Run("implicit_and_explicit_profile_execute_identically", func(t *testing.T) {
		captures := make([]Capture, 0, 2)
		for _, prefix := range [][]string{
			{"commit"},
			{"profile", "commit"},
		} {
			capturePath := filepath.Join(t.TempDir(), "capture.json")
			args := append(
				append([]string(nil), prefix...),
				"--model", "override-model", "--effort", "high",
				"plan exactly once",
			)
			command := exec.Command(harness.snCLI, args...)
			command.Env = harness.environment(map[string]string{
				"RUNTIME_GOLDEN_CAPTURE": capturePath,
			})
			command.Dir = harness.repoRoot
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("run %q: %v stderr=%s", prefix, err, stderr.String())
			}
			capture, err := ReadCapture(capturePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(capture.Argv) == 0 ||
				capture.Argv[len(capture.Argv)-1] != "plan exactly once" {
				t.Fatalf("profile prompt was not delivered once: %q", capture.Argv)
			}
			captures = append(captures, capture)
		}
		if !reflect.DeepEqual(captures[0], captures[1]) {
			t.Fatalf("implicit=%#v explicit=%#v", captures[0], captures[1])
		}
	})

	t.Run("piped_stdin_is_appended_as_typed_prompt", func(t *testing.T) {
		capturePath := filepath.Join(t.TempDir(), "capture.json")
		command := exec.Command(harness.snCLI, "cx-deep")
		command.Env = harness.environment(map[string]string{
			"RUNTIME_GOLDEN_CAPTURE":    capturePath,
			"RUNTIME_GOLDEN_ENV_KEYS":   capturedEnvKeys,
			"RUNTIME_GOLDEN_READ_STDIN": "all",
		})
		command.Dir = harness.repoRoot
		command.Stdin = strings.NewReader("piped input")
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("run: %v stderr=%s", err, stderr.String())
		}
		capture, err := ReadCapture(capturePath)
		if err != nil {
			t.Fatal(err)
		}
		if len(capture.Argv) == 0 ||
			capture.Argv[len(capture.Argv)-1] != "piped input" ||
			capture.Stdin != "" || capture.TTY.Stdin ||
			capture.TTY.Stdout || capture.TTY.Stderr {
			t.Fatalf("capture=%#v", capture)
		}
	})

	t.Run("implicit_and_explicit_profiles_reject_native_argv", func(t *testing.T) {
		for _, prefix := range [][]string{
			{"cx-deep"},
			{"profile", "cx-deep"},
			{"--json", "cx-deep"},
			{"--json", "profile", "cx-deep"},
		} {
			capturePath := filepath.Join(t.TempDir(), "capture.json")
			args := append(
				append([]string(nil), prefix...),
				"--native-one", "value one",
			)
			command := exec.Command(harness.snCLI, args...)
			command.Env = harness.environment(map[string]string{
				"RUNTIME_GOLDEN_CAPTURE": capturePath,
			})
			command.Dir = harness.repoRoot
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) ||
				exitError.ExitCode() != 1 ||
				!strings.Contains(stderr.String(), "invalid_request") {
				t.Fatalf(
					"prefix=%q error=%v stdout=%q stderr=%q",
					prefix, err, stdout.String(), stderr.String(),
				)
			}
			if _, statErr := os.Stat(capturePath); !os.IsNotExist(statErr) {
				t.Fatalf("prefix=%q target unexpectedly executed: %v", prefix, statErr)
			}
		}
	})

	t.Run("target_exit_code_is_preserved", func(t *testing.T) {
		capturePath := filepath.Join(t.TempDir(), "capture.json")
		command := exec.Command(harness.snCLI, "cx")
		command.Env = harness.environment(map[string]string{
			"RUNTIME_GOLDEN_CAPTURE":   capturePath,
			"RUNTIME_GOLDEN_ENV_KEYS":  capturedEnvKeys,
			"RUNTIME_GOLDEN_EXIT_CODE": "23",
		})
		command.Dir = harness.repoRoot
		process, err := ptyx.Start(command)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Master.Close() })
		output := readPTY(process)
		waitErr := process.Cmd.Wait()
		result := <-output
		if result.err != nil {
			t.Fatal(result.err)
		}
		if code := ptyx.ExitCode(waitErr); code != 23 {
			t.Fatalf("exit code=%d err=%v output=%q", code, waitErr, result.value)
		}
	})

	t.Run("sigint_is_forwarded", func(t *testing.T) {
		directory := t.TempDir()
		capturePath := filepath.Join(directory, "capture.json")
		readyPath := filepath.Join(directory, "ready")
		command := exec.Command(harness.snCLI, "cx")
		command.Env = harness.environment(map[string]string{
			"RUNTIME_GOLDEN_CAPTURE":     capturePath,
			"RUNTIME_GOLDEN_ENV_KEYS":    capturedEnvKeys,
			"RUNTIME_GOLDEN_WAIT_SIGNAL": "1",
			"RUNTIME_GOLDEN_READY":       readyPath,
		})
		command.Dir = harness.repoRoot
		process, err := ptyx.Start(command)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = process.Signal(syscall.SIGKILL)
			_ = process.Master.Close()
		})
		output := readPTY(process)
		if err := WaitForFile(readyPath, 3*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := process.Cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		wait := make(chan error, 1)
		go func() { wait <- process.Cmd.Wait() }()
		select {
		case waitErr := <-wait:
			if code := ptyx.ExitCode(waitErr); code != 130 {
				t.Fatalf("exit code=%d err=%v", code, waitErr)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("sn-cli did not exit after SIGINT")
		}
		if result := <-output; result.err != nil {
			t.Fatal(result.err)
		}
		capture, err := ReadCapture(capturePath)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(capture.Signals, []string{"interrupt"}) {
			t.Fatalf("signals=%v", capture.Signals)
		}
	})

	t.Run("cx_image_missing_environment_fails_before_exec", func(t *testing.T) {
		capturePath := filepath.Join(t.TempDir(), "capture.json")
		command := exec.Command(harness.snCLI, "cx-image", "describe image")
		command.Env = removeEnvironment(
			harness.environment(map[string]string{
				"RUNTIME_GOLDEN_CAPTURE":  capturePath,
				"RUNTIME_GOLDEN_ENV_KEYS": capturedEnvKeys,
			}),
			"WB_RUNTIME_IMAGE_PATH",
		)
		command.Dir = harness.repoRoot
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		err := command.Run()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Fatalf("err=%v output=%q", err, output.String())
		}
		if !strings.Contains(output.String(), "WB_RUNTIME_IMAGE_PATH") {
			t.Fatalf("output=%q", output.String())
		}
		if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
			t.Fatalf("fake target executed: %v", err)
		}
	})

	t.Run("profile_env_can_unset_inherited_value", func(t *testing.T) {
		configPath := filepath.Join(harness.home, "configs", "env-unset.json")
		config := map[string]any{
			"type":    "cli",
			"command": "codex",
			"env": map[string]any{
				"RUNTIME_GOLDEN_REMOVE_ME": nil,
			},
		}
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		capturePath := filepath.Join(t.TempDir(), "capture.json")
		command := exec.Command(
			harness.snCLI, "env-unset", "--exec", "check environment",
		)
		command.Env = harness.environment(map[string]string{
			"RUNTIME_GOLDEN_CAPTURE":   capturePath,
			"RUNTIME_GOLDEN_ENV_KEYS":  capturedEnvKeys,
			"RUNTIME_GOLDEN_REMOVE_ME": "must-not-reach-target",
		})
		command.Dir = harness.repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("run: %v output=%q", err, output)
		}
		capture, err := ReadCapture(capturePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := capture.Env["RUNTIME_GOLDEN_REMOVE_ME"]; exists ||
			!contains(capture.Missing, "RUNTIME_GOLDEN_REMOVE_ME") {
			t.Fatalf("capture=%#v", capture)
		}
	})

	t.Run("command_invocation_creates_no_runtime_state", func(t *testing.T) {
		capturePath := filepath.Join(t.TempDir(), "capture.json")
		command := exec.Command(harness.snCLI, "cx-deep", "no state")
		command.Env = harness.environment(map[string]string{
			"RUNTIME_GOLDEN_CAPTURE":  capturePath,
			"RUNTIME_GOLDEN_ENV_KEYS": capturedEnvKeys,
		})
		command.Dir = harness.repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("run: %v output=%q", err, output)
		}
		for _, directory := range []string{
			"bin", "runs", "sessions", "history", "daemon",
			"state", "memory", "logs", "cache", "tmp",
		} {
			if _, err := os.Stat(filepath.Join(harness.home, directory)); !os.IsNotExist(err) {
				t.Fatalf("command invocation created %s: %v", directory, err)
			}
		}
	})
}

func newHarness(t *testing.T) testHarness {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	root := t.TempDir()
	snCLI := filepath.Join(root, "sn-cli")
	fakeTarget := filepath.Join(root, "fake-target")
	if err := Build(repoRoot, snCLI, "./cmd/sn-cli"); err != nil {
		t.Fatal(err)
	}
	if err := Build(repoRoot, fakeTarget, "./runtimetest/commandgolden/cmd/faketarget"); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "bin")
	for _, name := range []string{"codex", "claude"} {
		if err := CopyFile(fakeTarget, filepath.Join(fakeBin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "runtime-home")
	configDir := filepath.Join(home, "configs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceConfigDir := filepath.Join(repoRoot, "configs")
	configEntries, err := os.ReadDir(sourceConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range configEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := CopyFile(
			filepath.Join(sourceConfigDir, entry.Name()),
			filepath.Join(configDir, entry.Name()),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	userHome := filepath.Join(root, "user-home")
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(root, "image.png")
	if err := os.WriteFile(image, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseEnv := removeEnvironment(
		os.Environ(),
		"SN_CLI_HOME", "HOME", "PATH", "KMM_API_KEY", "Z_AI_API_KEY", "WB_RUNTIME_IMAGE_PATH",
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "RUNTIME_GOLDEN_CAPTURE",
		"RUNTIME_GOLDEN_ENV_KEYS", "RUNTIME_GOLDEN_READ_STDIN", "RUNTIME_GOLDEN_EXIT_CODE",
		"RUNTIME_GOLDEN_WAIT_SIGNAL", "RUNTIME_GOLDEN_READY", "RUNTIME_GOLDEN_REMOVE_ME",
	)
	baseEnv = append(baseEnv,
		"SN_CLI_HOME="+home,
		"HOME="+userHome,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KMM_API_KEY=fixture-key",
		"Z_AI_API_KEY=fixture-key",
		"WB_RUNTIME_IMAGE_PATH="+image,
	)
	return testHarness{
		repoRoot: repoRoot,
		snCLI:    snCLI,
		home:     home,
		userHome: userHome,
		fakeBin:  fakeBin,
		image:    image,
		baseEnv:  baseEnv,
	}
}

func (h testHarness) environment(overrides map[string]string) []string {
	values := append([]string(nil), h.baseEnv...)
	for name, value := range overrides {
		values = removeEnvironment(values, name)
		values = append(values, name+"="+value)
	}
	return values
}

type readResult struct {
	value []byte
	err   error
}

func readPTY(process *ptyx.Process) <-chan readResult {
	output := make(chan readResult, 1)
	go func() {
		value, err := process.ReadAll()
		output <- readResult{value: value, err: err}
	}()
	return output
}

func removeEnvironment(values []string, names ...string) []string {
	blocked := map[string]struct{}{}
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if _, exists := blocked[name]; !exists {
			result = append(result, value)
		}
	}
	return result
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsAdjacent(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func isExecProfile(id string) bool {
	switch id {
	case "commit", "cx-adv", "cx-deep", "cx-image", "cx-spark":
		return true
	default:
		return false
	}
}
