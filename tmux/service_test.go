package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/yy003x/runtime/contract"
)

const testTmuxConfig = `set-option -g update-environment ""
set-option -g exit-empty on
set-window-option -g remain-on-exit on
set-window-option -g automatic-rename off
set-window-option -g allow-rename off
`

func TestSourceTmuxConfigMatchesValidatedFixture(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "release", "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "# Runtime owns this dedicated tmux server. User configuration is never loaded.\n"+
		testTmuxConfig {
		t.Fatalf("release/tmux.conf drifted:\n%s", value)
	}
}

func TestUUIDv7RoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 29, 9, 8, 7, 654321000, time.UTC)
	id, err := newUUIDv7(now, strings.NewReader(strings.Repeat("a", 16)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseUUIDv7(id)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Equal(now.Truncate(time.Millisecond)) {
		t.Fatalf("parsed time = %s, want %s", parsed, now.Truncate(time.Millisecond))
	}
	if id[14] != '7' || id[19] < '8' || id[19] > 'b' {
		t.Fatalf("not UUIDv7-style: %s", id)
	}
}

func TestExactTargetEnvironmentReplacesTmuxReservedValues(t *testing.T) {
	configured := []string{
		"A=1", "TERM=bad", "TMUX=bad", "TMUX_PANE=bad",
	}
	current := []string{
		"TERM=xterm-256color", "TMUX=/tmp/socket,1,0", "TMUX_PANE=%4",
		"UNRELATED=not-inherited",
	}
	got := exactTargetEnvironment(configured, current)
	want := []string{
		"A=1", "TERM=xterm-256color", "TMUX=/tmp/socket,1,0", "TMUX_PANE=%4",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

func TestFramedSendHeadroomDoesNotWidenRawSendLimit(t *testing.T) {
	value := strings.Repeat("x", maxSendBytes+1)
	if err := validateSendInput(value, maxSendBytes); err == nil {
		t.Fatal("raw Tmux input exceeded its public limit")
	}
	if err := validateSendInput(value, maxFramedSendBytes); err != nil {
		t.Fatalf("valid carrier frame rejected: %v", err)
	}
	service := &Service{}
	if _, err := service.SendFramed(
		context.Background(), "unused", "two\nlines",
	); err == nil {
		t.Fatal("multi-line carrier frame was accepted")
	}
}

func TestManifestDigestDetectsMutation(t *testing.T) {
	manifest := launchManifest{
		SchemaVersion:      WindowSchemaVersion,
		OwnerUID:           os.Getuid(),
		Home:               "/tmp/example",
		Nonce:              strings.Repeat("a", 32),
		Path:               "/bin/echo",
		Argv:               []string{"echo", "ok"},
		Environment:        []string{"A=1"},
		CWD:                "/tmp",
		ExecutableIdentity: "identity",
		ReadyPath:          "/tmp/example/tmp/tmux/a.ready",
		GoPath:             "/tmp/example/tmp/tmux/a.go",
		StatusPath:         "/tmp/example/tmp/tmux/a.status",
		GateTimeoutMS:      1000,
	}
	digest, _, err := marshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ManifestDigest = digest
	if err := validateManifest(manifest); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	manifest.Argv[1] = "changed"
	if err := validateManifest(manifest); err == nil {
		t.Fatal("mutated manifest was accepted")
	}
}

func TestAttachRejectsNonTTYBeforeRegistryLookup(t *testing.T) {
	service := newTestService(t, false)
	err := service.Attach(
		context.Background(),
		"018f6f9e-0000-7000-8000-000000000000",
		TTYFiles{},
	)
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorInvalidRequest ||
		runtimeErr.Phase != contract.PhaseRequest {
		t.Fatalf("error = %#v", err)
	}
}

func TestCurrentTmuxSocket(t *testing.T) {
	socket, inside := currentTmuxSocket("/tmp/sn.sock,123,0")
	if !inside || socket != "/tmp/sn.sock" {
		t.Fatalf("socket=%q inside=%t", socket, inside)
	}
	if _, inside := currentTmuxSocket(""); inside {
		t.Fatal("empty TMUX reported as inside")
	}
}

func TestNewServiceRejectsPathsOutsideHome(t *testing.T) {
	home := t.TempDir()
	config := testConfig(t, home, false)
	config.ManifestDir = filepath.Join(t.TempDir(), "tmux")
	if _, err := NewService(config); err == nil {
		t.Fatal("outside manifest path was accepted")
	}
}

func TestCleanupStaleLaunchFilesIsBoundedAndFailClosed(t *testing.T) {
	service := newTestService(t, false)
	stale := filepath.Join(
		service.config.ManifestDir,
		"launch-"+strings.Repeat("a", 32)+".json",
	)
	recent := filepath.Join(
		service.config.ManifestDir,
		"launch-"+strings.Repeat("b", 32)+".ready",
	)
	for _, path := range []string{stale, recent} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := service.cleanupStaleLaunchFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale artifact remains: %v", err)
	}
	if _, err := os.Lstat(recent); err != nil {
		t.Fatalf("recent artifact removed: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(service.config.ManifestDir, "unexpected"),
		[]byte("x"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := service.cleanupStaleLaunchFiles(); err == nil {
		t.Fatal("unexpected launch artifact was accepted")
	}
}

func TestPrepareRejectsSymlinkedRuntimeParent(t *testing.T) {
	service := newTestService(t, false)
	tmpDir := filepath.Join(service.home, "tmp")
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, tmpDir); err != nil {
		t.Fatal(err)
	}
	if err := service.prepare(context.Background(), true); err == nil {
		t.Fatal("symlinked Runtime tmp directory was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was mutated: %#v", entries)
	}
}

func TestMarkerlessBootstrapRecoveryRequiresNoUserOptions(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	for _, test := range []struct {
		name          string
		foreignOption bool
		wantError     bool
	}{
		{name: "clean recovery"},
		{name: "foreign option", foreignOption: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, false)
			ctx := context.Background()
			if err := service.prepare(ctx, true); err != nil {
				t.Fatal(err)
			}
			incarnation := strings.Repeat("c", 32)
			result, err := service.run(ctx, CommandSpec{
				Path: service.tmuxPath,
				Args: []string{
					"-S", service.config.SocketFile,
					"-f", service.config.TmuxConfigFile,
					"new-session", "-d", "-s", SessionName,
					"-n", service.sentinelName(incarnation),
					"--", "/usr/bin/true",
				},
				Env: service.serverEnv,
			})
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("create markerless server: result=%#v err=%v", result, err)
			}
			if test.foreignOption {
				result, err = service.runTmux(
					ctx, nil, "set-option", "-gq", "@foreign", "yes",
				)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("set foreign option: result=%#v err=%v", result, err)
				}
			}
			marker, err := service.ensureServer(ctx)
			if test.wantError {
				if err == nil {
					t.Fatal("markerless server with foreign option was recovered")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if marker.FullHomeDigest != service.homeDigest ||
				marker.ServerIncarnation == incarnation {
				t.Fatalf("marker = %#v", marker)
			}
		})
	}
}

func TestOrphanWindowCanBeListedAndStopped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, false)
	ctx := context.Background()
	lock, err := acquireFileLock(service.config.LockFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.prepare(ctx, true); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	if _, err := service.ensureServer(ctx); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	tmuxID, err := newUUIDv7(time.Now(), nil)
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	result, err := service.runTmux(
		ctx, nil,
		"new-window", "-d", "-t", SessionName,
		"-n", windowName(tmuxID), "--", "/usr/bin/true",
	)
	lock.Close()
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("create orphan: result=%#v err=%v", result, err)
	}
	values, err := service.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].TmuxID != tmuxID ||
		values[0].State != StateOrphaned {
		t.Fatalf("orphan list = %#v", values)
	}
	if _, err := service.Stop(ctx, tmuxID); err != nil {
		t.Fatalf("stop orphan: %v", err)
	}
}

func TestFastExitTargetIsRegisteredBeforeExecution(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, true)
	result, err := service.Start(context.Background(), StartRequest{
		Invocation: Invocation{
			ProfileID: "fast", Path: "/usr/bin/true",
			Argv: []string{"true"}, Environment: []string{"ONLY_CONFIGURED=yes"},
			CWD: service.home, ConfigDigest: digestString("fast-config"),
			Binding: &Binding{
				Kind: "session", ID: "session_11111111111111111111111111111111",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.LaunchAccepted || result.Window.State != StateExited {
		t.Fatalf("result = %#v", result)
	}
	if result.Window.ExitCode == nil || *result.Window.ExitCode != 0 {
		t.Fatalf("exit code = %#v", result.Window.ExitCode)
	}
	if result.Window.Binding == nil || result.Window.Binding.Kind != "session" ||
		result.Window.Binding.ID != "session_11111111111111111111111111111111" {
		t.Fatalf("binding = %#v", result.Window.Binding)
	}
	if _, err := service.Stop(
		context.Background(), result.Window.TmuxID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestCooperativeTargetMustAcknowledgeReadiness(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, true)
	result, err := service.Start(context.Background(), StartRequest{
		Invocation: Invocation{
			ProfileID: "cooperative-fast", Path: "/usr/bin/true",
			Argv: []string{"true"}, Environment: []string{"ONLY_CONFIGURED=yes"},
			CWD: service.home, ConfigDigest: digestString("cooperative-fast-config"),
			CooperativeReady: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LaunchAccepted || result.Window.State != StateExited ||
		result.Window.LaunchError == nil ||
		!strings.Contains(
			result.Window.LaunchError.Message,
			"exited before readiness acknowledgement",
		) {
		t.Fatalf("result = %#v", result)
	}
	if _, err := service.Stop(
		context.Background(), result.Window.TmuxID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestBindingIsUniqueWithinDedicatedRegistry(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, true)
	binding := &Binding{
		Kind: "session", ID: "session_22222222222222222222222222222222",
	}
	request := StartRequest{Invocation: Invocation{
		ProfileID: "bound", Path: "/usr/bin/true", Argv: []string{"true"},
		Environment: []string{"ONLY_CONFIGURED=yes"}, CWD: service.home,
		ConfigDigest: digestString("bound-config"), Binding: binding,
	}}
	first, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "already belongs to window") {
		t.Fatalf("duplicate binding error = %v", err)
	}
	windows, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].TmuxID != first.Window.TmuxID {
		t.Fatalf("windows = %#v", windows)
	}
	if _, err := service.Stop(context.Background(), first.Window.TmuxID); err != nil {
		t.Fatal(err)
	}
}

func TestServiceDedicatedServerLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, true)
	targetPath := buildTestTarget(t, service.home)
	firstFact := filepath.Join(service.home, "first-target.json")
	first := startTestTarget(t, service, targetPath, firstFact, "first prompt")
	if !first.LaunchAccepted {
		t.Fatalf("first launch not accepted: %#v", first)
	}
	secondFact := filepath.Join(service.home, "second-target.json")
	second := startTestTarget(t, service, targetPath, secondFact, "")
	if !second.LaunchAccepted || first.Window.TmuxID == second.Window.TmuxID {
		t.Fatalf("unexpected second launch: %#v", second)
	}

	firstTarget := waitTargetFact(t, firstFact)
	if len(firstTarget.Argv) == 0 ||
		firstTarget.Argv[len(firstTarget.Argv)-1] != "first prompt" {
		t.Fatalf("target argv = %#v", firstTarget.Argv)
	}
	assertExactTargetEnvironment(t, firstTarget.Environment)
	_ = waitTargetFact(t, secondFact)

	values, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 ||
		values[0].TmuxID != first.Window.TmuxID ||
		values[1].TmuxID != second.Window.TmuxID {
		t.Fatalf("list = %#v", values)
	}
	if _, err := service.Send(
		context.Background(), first.Window.TmuxID, "safe paste\nsecond line",
	); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := service.Stop(
		context.Background(), first.Window.TmuxID,
	); err != nil {
		t.Fatalf("stop first: %v", err)
	}
	values, err = service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].TmuxID != second.Window.TmuxID {
		t.Fatalf("list after first stop = %#v", values)
	}
	if _, err := service.Interrupt(
		context.Background(), second.Window.TmuxID,
	); err != nil {
		t.Fatalf("interrupt second: %v", err)
	}
	waitWindowState(t, service, second.Window.TmuxID, StateExited)
	if _, err := service.Stop(
		context.Background(), second.Window.TmuxID,
	); err != nil {
		t.Fatalf("stop second: %v", err)
	}
	if _, err := service.Stop(
		context.Background(), second.Window.TmuxID,
	); err == nil {
		t.Fatal("stale tmux_id stop was accepted")
	}
	values, err = service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("final list = %#v", values)
	}
	entries, err := os.ReadDir(service.config.ManifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("launch artifacts remain: %#v", entries)
	}
}

func TestConcurrentFirstStartsShareOneOwnedServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, true)
	targetPath := buildTestTarget(t, service.home)
	type outcome struct {
		result StartResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			factPath := filepath.Join(
				service.home, fmt.Sprintf("concurrent-%d.json", index),
			)
			result, err := service.Start(
				context.Background(),
				StartRequest{Invocation: Invocation{
					ProfileID: "test-cli", Path: targetPath,
					Argv: []string{targetPath, fmt.Sprintf("prompt-%d", index)},
					Environment: []string{
						"ONLY_CONFIGURED=yes",
						"SN_TMUX_TARGET_FACT=" + factPath,
					},
					CWD: service.home,
					ConfigDigest: digestString(
						fmt.Sprintf("config-%d", index),
					),
				}},
			)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	results := make([]StartResult, 0, 2)
	for range 2 {
		current := <-outcomes
		if current.err != nil {
			t.Fatalf("concurrent start: %v", current.err)
		}
		if !current.result.LaunchAccepted {
			t.Fatalf("launch not accepted: %#v", current.result)
		}
		results = append(results, current.result)
	}
	if results[0].Window.TmuxID == results[1].Window.TmuxID {
		t.Fatalf("duplicate concurrent tmux_id: %#v", results)
	}
	windows, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("concurrent windows = %#v", windows)
	}
	for _, result := range results {
		if _, err := service.Stop(
			context.Background(), result.Window.TmuxID,
		); err != nil {
			t.Fatalf("stop concurrent window: %v", err)
		}
	}
}

func TestActionConditionalRejectsLateForeignLink(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	service := newTestService(t, true)
	targetPath := buildTestTarget(t, service.home)
	first := startTestTarget(
		t, service, targetPath,
		filepath.Join(service.home, "late-link-first.json"), "",
	)
	second := startTestTarget(
		t, service, targetPath,
		filepath.Join(service.home, "late-link-second.json"), "",
	)
	baseRunner := service.runner
	var once sync.Once
	var hookErr error
	service.runner = commandRunnerFunc(func(
		ctx context.Context,
		spec CommandSpec,
	) (CommandResult, error) {
		if containsArgument(spec.Args, "if-shell") {
			once.Do(func() {
				command := exec.Command(
					service.tmuxPath, "-S", service.config.SocketFile,
					"new-session", "-d", "-s", "foreign",
					"-t", SessionName,
				)
				if output, err := command.CombinedOutput(); err != nil {
					hookErr = fmt.Errorf(
						"create late foreign link: %w: %s", err, output,
					)
				}
			})
		}
		return baseRunner.Run(ctx, spec)
	})
	if _, err := service.Stop(
		context.Background(), first.Window.TmuxID,
	); err == nil {
		t.Fatal("stop accepted a window linked after validation")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	service.runner = baseRunner
	command := exec.Command(
		service.tmuxPath, "-S", service.config.SocketFile,
		"kill-session", "-t", "foreign",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("remove foreign session: %v: %s", err, output)
	}
	for _, value := range []StartResult{first, second} {
		if _, err := service.Stop(
			context.Background(), value.Window.TmuxID,
		); err != nil {
			t.Fatalf("stop restored window: %v", err)
		}
	}
}

func TestAttachUsesRealTTYAndDetachesCleanly(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	// Attach spawns a real tmux client on the PTY. tmux needs a usable terminfo
	// entry; CI runners have no TERM by default, which makes tmux fail with
	// "open terminal failed: terminal does not support clear".
	t.Setenv("TERM", "xterm-256color")
	service := newTestService(t, true)
	targetPath := buildTestTarget(t, service.home)
	started := startTestTarget(
		t, service, targetPath,
		filepath.Join(service.home, "attach-target.json"), "",
	)
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer terminal.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.Attach(
			ctx, started.Window.TmuxID,
			TTYFiles{
				Stdin: terminal, Stdout: terminal, Stderr: terminal,
			},
		)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := master.Write([]byte{0x02, 'd'}); err == nil {
			select {
			case attachErr := <-done:
				if attachErr != nil {
					t.Fatalf("attach: %v", attachErr)
				}
				if _, err := service.Stop(
					context.Background(), started.Window.TmuxID,
				); err != nil {
					t.Fatal(err)
				}
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	t.Fatal("tmux attach did not detach before timeout")
}

func TestHelperReportsGateIdentityFailureBeforeExec(t *testing.T) {
	service := newTestService(t, false)
	path, identity, err := executableIdentity("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("d", 32)
	paths := service.launchPaths(nonce)
	manifest := launchManifest{
		SchemaVersion: WindowSchemaVersion, OwnerUID: os.Getuid(),
		Home: service.home, Nonce: nonce, Path: path,
		Argv: []string{"true"}, Environment: []string{"ONLY_CONFIGURED=yes"},
		CWD: service.home, ExecutableIdentity: identity,
		ReadyPath: paths.ready, GoPath: paths.goFile,
		StatusPath: paths.status, GateTimeoutMS: 3000,
	}
	digest, _, err := marshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ManifestDigest = digest
	if err := writeJSONPrivate(
		paths.manifest, manifest, service.uid,
	); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		os.Args[0], "-test.run=^TestTmuxHelperProcess$", "--",
		HelperCommandName, "--manifest", paths.manifest,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ready readyFact
		if err := decodePrivateJSON(
			paths.ready, 64<<10, service.uid, &ready,
		); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("helper did not publish readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := writeJSONPrivate(
		paths.goFile,
		goFact{
			SchemaVersion: WindowSchemaVersion, Nonce: nonce,
			ManifestDigest: strings.Repeat("0", 64),
		},
		service.uid,
	); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("helper accepted mismatched launch gate")
	}
	var status helperStatus
	if err := decodePrivateJSON(
		paths.status, 64<<10, service.uid, &status,
	); err != nil {
		t.Fatal(err)
	}
	if status.Error == nil ||
		status.Error.Message != "Tmux launch gate identity mismatch" {
		t.Fatalf("helper status = %#v", status)
	}
	for _, consumed := range []string{
		paths.manifest, paths.ready, paths.goFile,
	} {
		if _, err := os.Lstat(consumed); !os.IsNotExist(err) {
			t.Fatalf("launch input remains at %s: %v", consumed, err)
		}
	}
}

func TestHelperRejectsActivationGuardBeforeReady(t *testing.T) {
	service := newTestService(t, false)
	path, identity, err := executableIdentity("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("e", 32)
	paths := service.launchPaths(nonce)
	manifest := launchManifest{
		SchemaVersion: WindowSchemaVersion, OwnerUID: os.Getuid(),
		Home: service.home, Nonce: nonce, Path: path,
		Argv: []string{"true"}, Environment: []string{"ONLY_CONFIGURED=yes"},
		CWD: service.home, ExecutableIdentity: identity,
		ReadyPath: paths.ready, GoPath: paths.goFile,
		StatusPath: paths.status, GateTimeoutMS: 3000,
	}
	digest, _, err := marshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ManifestDigest = digest
	if err := writeJSONPrivate(
		paths.manifest, manifest, service.uid,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(service.home, "state", "activation.guard.json"),
		[]byte("{}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := RunHelper([]string{"--manifest", paths.manifest}); err == nil {
		t.Fatal("Tmux helper accepted an active activation guard")
	}
	if _, err := os.Lstat(paths.ready); !os.IsNotExist(err) {
		t.Fatalf("helper published readiness while guarded: %v", err)
	}
}

type commandRunnerFunc func(
	context.Context,
	CommandSpec,
) (CommandResult, error)

func (function commandRunnerFunc) Run(
	ctx context.Context,
	spec CommandSpec,
) (CommandResult, error) {
	return function(ctx, spec)
}

func containsArgument(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestTmuxHelperProcess(t *testing.T) {
	index := indexOf(os.Args, HelperCommandName)
	if index < 0 {
		return
	}
	if err := RunHelper(os.Args[index+1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "tmux helper: %v\n", err)
		os.Exit(97)
	}
	os.Exit(0)
}

type targetFact struct {
	Argv        []string `json:"argv"`
	Environment []string `json:"environment"`
}

func newTestService(t *testing.T, helper bool) *Service {
	t.Helper()
	config := testConfig(t, t.TempDir(), helper)
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		service.forceKillServer(context.Background())
		_ = os.Remove(config.SocketFile)
	})
	return service
}

func testConfig(t *testing.T, home string, helper bool) Config {
	t.Helper()
	canonical, err := canonicalHome(home)
	if err != nil {
		t.Fatal(err)
	}
	home = canonical
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		filepath.Join(home, "state"),
		filepath.Join(home, "tmp", "tmux"),
		filepath.Join(home, "resources"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configFile := filepath.Join(home, "resources", "tmux.conf")
	if err := os.WriteFile(configFile, []byte(testTmuxConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	socketDir := filepath.Join(
		"/tmp", fmt.Sprintf("sn-cli-tmux-%d", os.Getuid()),
	)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socketDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("unsafe test socket directory: info=%v err=%v", info, err)
	}
	homeDigest := digestString(filepath.Clean(home))
	config := Config{
		Home: home, LockFile: filepath.Join(home, "state", "tmux.lock"),
		ManifestDir:    filepath.Join(home, "tmp", "tmux"),
		TmuxConfigFile: configFile, SocketDir: socketDir,
		SocketFile:   filepath.Join(socketDir, homeDigest[:16]+".sock"),
		ReadyTimeout: 5 * time.Second, GateTimeout: 10 * time.Second,
		CommandTimeout: 10 * time.Second,
	}
	if helper {
		config.HelperCommand = []string{
			os.Args[0], "-test.run=^TestTmuxHelperProcess$", "--",
			HelperCommandName,
		}
	}
	return config
}

func startTestTarget(
	t *testing.T,
	service *Service,
	targetPath string,
	factPath string,
	prompt string,
) StartResult {
	t.Helper()
	argv := []string{targetPath}
	if prompt != "" {
		argv = append(argv, prompt)
	}
	result, err := service.Start(context.Background(), StartRequest{
		Invocation: Invocation{
			ProfileID: "test-cli", Path: targetPath, Argv: argv,
			Environment: []string{
				"ONLY_CONFIGURED=yes",
				"SN_TMUX_TARGET_FACT=" + factPath,
			},
			CWD: service.home, ConfigDigest: digestString("test-config"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func waitTargetFact(t *testing.T, path string) targetFact {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var fact targetFact
			if err := json.Unmarshal(data, &fact); err != nil {
				t.Fatal(err)
			}
			return fact
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("target fact %s was not written", path)
	return targetFact{}
}

func assertExactTargetEnvironment(t *testing.T, values []string) {
	t.Helper()
	got := make(map[string]string, len(values))
	for _, value := range values {
		name, current, exists := strings.Cut(value, "=")
		if exists {
			got[name] = current
		}
	}
	for _, name := range []string{
		"ONLY_CONFIGURED", "SN_TMUX_TARGET_FACT",
		"TERM", "TMUX", "TMUX_PANE",
	} {
		if _, exists := got[name]; !exists {
			t.Fatalf("target environment lacks %s: %#v", name, values)
		}
	}
	for name := range got {
		switch name {
		case "ONLY_CONFIGURED", "SN_TMUX_TARGET_FACT",
			"TERM", "TMUX", "TMUX_PANE":
		default:
			t.Fatalf("unexpected inherited environment %s: %#v", name, values)
		}
	}
}

func buildTestTarget(t *testing.T, home string) string {
	t.Helper()
	sourceDir := filepath.Join(home, "target-source")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := `package main

import (
	"encoding/json"
	"os"
	"time"
)

func main() {
	data, err := json.Marshal(struct {
		Argv []string ` + "`json:\"argv\"`" + `
		Environment []string ` + "`json:\"environment\"`" + `
	}{os.Args, os.Environ()})
	if err != nil {
		os.Exit(91)
	}
	if err := os.WriteFile(
		os.Getenv("SN_TMUX_TARGET_FACT"), append(data, '\n'), 0600,
	); err != nil {
		os.Exit(92)
	}
	for {
		time.Sleep(time.Hour)
	}
}
`
	sourceFile := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(sourceFile, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "tmux-test-target")
	command := exec.Command("go", "build", "-o", target, sourceFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build target: %v\n%s", err, output)
	}
	return target
}

func waitWindowState(
	t *testing.T,
	service *Service,
	tmuxID string,
	state State,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := service.Show(context.Background(), tmuxID)
		if err == nil && value.State == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	value, err := service.Show(context.Background(), tmuxID)
	t.Fatalf("window state = %#v, err=%v; want %s", value, err, state)
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
