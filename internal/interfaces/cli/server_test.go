//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/profile"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

func TestMain(main *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case runtimetmux.HelperCommandName:
			if err := runtimetmux.RunHelper(os.Args[2:]); err != nil {
				_, _ = os.Stderr.WriteString(err.Error() + "\n")
				os.Exit(1)
			}
			os.Exit(0)
		case sessionTerminalHelperCommandName:
			if err := runSessionTerminalHelperVNext(os.Args[2:]); err != nil {
				_, _ = os.Stderr.WriteString(err.Error() + "\n")
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	if os.Getenv("SN_CLI_TEST_SERVER_HOLD") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(main.Run())
}

func TestServerStartReturnsStablePIDForThirdPartyManagement(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(executable, paths.ServerBinary); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SN_CLI_TEST_SERVER_HOLD", "1")
	var jsonOutput bytes.Buffer
	if err := startServer(
		paths, newCLIOutput(true, &jsonOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	var started struct {
		SchemaVersion   int  `json:"schema_version"`
		ContractVersion int  `json:"contract_version"`
		Running         bool `json:"running"`
		PID             int  `json:"pid"`
	}
	if err := json.Unmarshal(jsonOutput.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.SchemaVersion != cliOutputSchemaVersion ||
		started.ContractVersion != cliOutputContractVersion ||
		!started.Running || started.PID <= 0 {
		t.Fatalf("start result=%#v", started)
	}
	var humanOutput bytes.Buffer
	if err := startServer(
		paths, newCLIOutput(false, &humanOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		humanOutput.String(), "pid="+strconv.Itoa(started.PID),
	) {
		t.Fatalf("already-running output=%q", humanOutput.String())
	}
	var statusOutput bytes.Buffer
	if err := serverStatus(
		paths, newCLIOutput(true, &statusOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Running bool `json:"running"`
		PID     int  `json:"pid"`
	}
	if err := json.Unmarshal(statusOutput.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.PID != started.PID {
		t.Fatalf("status=%#v start=%#v", status, started)
	}
	process, err := os.FindProcess(started.PID)
	if err != nil {
		t.Fatal(err)
	}
	_ = process.Signal(syscall.SIGTERM)
	_ = os.Remove(paths.ServerPIDFile)
}

func TestServerRunningRequiresMatchingLeaseAndProcessIdentity(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	identity, err := processIdentityForPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	record := serverPIDRecord{
		SchemaVersion:     serverPIDSchemaVersion,
		PID:               os.Getpid(),
		Binary:            paths.ServerBinary,
		ProcessStart:      identity.StartToken,
		ProcessExecutable: identity.Executable,
		StartedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}

	running, _, err := serverRunning(paths)
	if err == nil || running {
		t.Fatalf("live pid without a held lease must fail closed: running=%v err=%v", running, err)
	}

	record.ProcessStart += "-stale"
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}
	running, _, err = serverRunning(paths)
	if err != nil || running {
		t.Fatalf("stale pid identity must not report running: running=%v err=%v", running, err)
	}
	record.ProcessStart = identity.StartToken
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}

	lease, acquired, err := tryAcquireFileLock(paths.ServerLeaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected to acquire server lease")
	}
	defer releaseFileLock(lease)

	running, pid, err := serverRunning(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !running || pid != os.Getpid() {
		t.Fatalf("running=%v pid=%d", running, pid)
	}

	record.ProcessStart += "-stale"
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}
	if running, _, err := serverRunning(paths); err == nil || running {
		t.Fatalf("identity mismatch must fail closed: running=%v err=%v", running, err)
	}
}

func TestServerRunningRejectsUnsupportedPIDRecord(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ServerPIDFile, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if running, _, err := serverRunning(paths); err == nil || running {
		t.Fatalf("unsupported pid record must fail closed: running=%v err=%v", running, err)
	}
}

func TestServerLifecycleLockRejectsConcurrentOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.lock")
	first, acquired, err := tryAcquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected first lifecycle lock")
	}
	defer releaseFileLock(first)

	second, acquired, err := tryAcquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		releaseFileLock(second)
		t.Fatal("concurrent lifecycle lock unexpectedly succeeded")
	}
}

func TestServerLockRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if file, acquired, err := tryAcquireFileLock(link); err == nil {
		if acquired {
			releaseFileLock(file)
		}
		t.Fatal("symlink lock path must be rejected")
	}
}

func TestServerInfoPublishesCurrentContract(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	writeVNextModel(
		t, paths.ConfigDir, "api-cx",
		"https://example.invalid/v1/chat/completions",
	)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runServerNamespaceVNext(
		paths, []string{"info"}, newCLIOutput(true, &stdout, &stderr),
	); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SchemaVersion   int                 `json:"schema_version"`
		ContractVersion int                 `json:"contract_version"`
		Namespaces      []string            `json:"namespaces"`
		Capabilities    map[string][]string `json:"capabilities"`
		ConfiguredAddr  string              `json:"configured_address"`
		Profiles        []struct {
			ID      string          `json:"id"`
			Kind    profile.Kind    `json:"kind"`
			Command json.RawMessage `json:"command"`
			Model   json.RawMessage `json:"model"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != 1 || payload.ContractVersion != 4 ||
		strings.Join(payload.Namespaces, ",") != strings.Join(fixedNamespaces, ",") ||
		len(payload.Capabilities["agent"]) == 0 ||
		payload.ConfiguredAddr != "127.0.0.1:8080" ||
		len(payload.Profiles) != 2 {
		t.Fatalf("payload=%#v", payload)
	}
	for _, capability := range payload.Capabilities["run"] {
		if capability == "resume" {
			t.Fatalf(
				"stock server must not advertise Kernel-extension resume: %#v",
				payload.Capabilities["run"],
			)
		}
	}
	for _, entry := range payload.Profiles {
		if entry.ID == "" ||
			entry.Kind != profile.KindCommand &&
				entry.Kind != profile.KindModel {
			t.Fatalf("invalid profile entry=%#v", entry)
		}
		switch entry.Kind {
		case profile.KindCommand:
			if len(entry.Command) == 0 || len(entry.Model) != 0 {
				t.Fatalf("invalid CLI profile entry=%#v", entry)
			}
		case profile.KindModel:
			if len(entry.Model) == 0 || len(entry.Command) != 0 {
				t.Fatalf("invalid API profile entry=%#v", entry)
			}
		}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]struct{}{
		"schema_version": {}, "contract_version": {}, "version": {},
		"runtime_home": {}, "profiles": {}, "run_database": {},
		"configured_address": {}, "namespaces": {}, "capabilities": {},
	}
	for name := range raw {
		if _, exists := allowed[name]; !exists {
			t.Fatalf("unexpected server info field %q", name)
		}
	}
	if len(raw) != len(allowed) {
		t.Fatalf("server info fields=%v", raw)
	}

	stdout.Reset()
	if err := runServerNamespaceVNext(
		paths, []string{"info"}, newCLIOutput(false, &stdout, &stderr),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "Runtime: sn-cli ") ||
		!strings.Contains(
			stdout.String(), "Configured address: 127.0.0.1:8080",
		) ||
		strings.Contains(stdout.String(), "sn-server sn-cli") {
		t.Fatalf("human output=%q", stdout.String())
	}
}

func TestServerStatusDoesNotClaimConfiguredAddressAsRuntimeFact(t *testing.T) {
	paths := prepareVNextHome(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := serverStatus(
		paths, newCLIOutput(true, &stdout, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["address"]; exists {
		t.Fatalf("status published invocation address: %s", stdout.String())
	}
	if _, exists := payload["configured_address"]; exists {
		t.Fatalf("status published configured address: %s", stdout.String())
	}
	stdout.Reset()
	if err := serverStatus(
		paths, newCLIOutput(false, &stdout, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "Address:") ||
		strings.Contains(stdout.String(), "address=") {
		t.Fatalf("human status published invocation address: %q", stdout.String())
	}
}

func TestServerUpdateRejectsActionLocalJSON(t *testing.T) {
	_, err := parseUpdateOptions([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "unknown update argument --json") {
		t.Fatalf("error=%v", err)
	}
}

func TestServerStatefulActionsRejectTrailingArgumentsBeforeBootstrap(
	t *testing.T,
) {
	for _, action := range []string{"info", "doctor", "start", "status", "stop"} {
		t.Run(action, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "not-created")
			paths, err := layout.FromHome(home)
			if err != nil {
				t.Fatal(err)
			}
			err = runServerNamespaceVNext(
				paths,
				[]string{action, "--unexpected"},
				newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{}),
			)
			if err == nil ||
				!strings.Contains(err.Error(), "does not accept arguments") {
				t.Fatalf("error=%v", err)
			}
			if _, statErr := os.Stat(home); !os.IsNotExist(statErr) {
				t.Fatalf(
					"invalid request touched Runtime home: stat error=%v",
					statErr,
				)
			}
		})
	}
}

func TestParseUpdateOptionsRejectsDuplicateAndConflictingModes(t *testing.T) {
	for _, args := range [][]string{
		{"--check", "--check"},
		{"--dry-run", "--dry-run"},
		{"--version", "v1.2.3", "--version", "v2.0.0"},
		{"--check", "--dry-run"},
		{"--check", "--version", "v1.2.3"},
		{"--version", "--dry-run"},
	} {
		if _, err := parseUpdateOptions(args); err == nil {
			t.Fatalf("parseUpdateOptions(%q) unexpectedly succeeded", args)
		}
	}
	options, err := parseUpdateOptions(
		[]string{"--dry-run", "--version", "v1.2.3"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !options.dryRun || options.targetVersion != "v1.2.3" {
		t.Fatalf("options=%#v", options)
	}
}

func TestUpgradePrivateOptionsRejectDuplicateAndMissingValues(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{})
	for _, args := range [][]string{
		{"--resources", "one", "--resources", "two"},
		{"--resources", "--unknown"},
	} {
		if err := runUpgradeCheck(paths, args, output); err == nil {
			t.Fatalf("runUpgradeCheck(%q) unexpectedly succeeded", args)
		}
	}
	for _, args := range [][]string{
		{"--payload", "one", "--payload", "two", "--target-home", paths.Home},
		{"--payload", "--target-home", paths.Home},
		{"--payload", "one", "--target-home", paths.Home, "--command-link", "--x"},
		{
			"--payload", "one", "--target-home", paths.Home,
			"--overwrite-configs", "--overwrite-configs",
		},
		{
			"--payload", "one", "--target-home", paths.Home,
			"--local-source-install", "--local-source-install",
		},
		{
			"--payload", "one", "--target-home", paths.Home,
			"--local-source-install", "--overwrite-configs",
		},
	} {
		if err := runUpgradeActivate(paths, args, output); err == nil {
			t.Fatalf("runUpgradeActivate(%q) unexpectedly succeeded", args)
		}
	}
}

func TestActivationCommandLinkMustBeOutsideRuntimeHome(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"bin", "configs"} {
		if err := os.MkdirAll(filepath.Join(home, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, directory, "sn-cli")
		err := validateActivationCommandLink(
			home, link, filepath.Join(home, "bin", "sn-cli"),
		)
		if err == nil ||
			!strings.Contains(err.Error(), "outside the Runtime home") {
			t.Fatalf("link=%s error=%v", link, err)
		}
	}
	external := filepath.Join(t.TempDir(), "sn-cli")
	externalParent, err := filepath.EvalSymlinks(filepath.Dir(external))
	if err != nil {
		t.Fatal(err)
	}
	external = filepath.Join(externalParent, "sn-cli")
	if err := validateActivationCommandLink(
		home, external, filepath.Join(home, "bin", "sn-cli"),
	); err != nil {
		t.Fatalf("external command link rejected: %v", err)
	}
}

func TestUpgradeActivationKeepsDurableCommandLinkReservationOnFailure(
	t *testing.T,
) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	payload := filepath.Join(root, "invalid-payload")
	commandDir := filepath.Join(root, "command")
	for _, directory := range []string{home, payload, commandDir} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	commandLink := filepath.Join(commandDir, "sn-cli")
	err = runUpgradeActivate(
		paths,
		[]string{
			"--payload", payload,
			"--target-home", home,
			"--command-link", commandLink,
		},
		newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{}),
	)
	if err == nil {
		t.Fatal("invalid payload unexpectedly activated")
	}
	expectedTarget := filepath.Join(home, "bin", "sn-cli")
	if got, readErr := os.Readlink(commandLink); readErr != nil ||
		got != expectedTarget {
		t.Fatalf(
			"failed activation lost durable command link: target=%q error=%v",
			got, readErr,
		)
	}
}

func TestServerDoctorDependencyErrorNamesMissingInputs(t *testing.T) {
	err := serverDoctorDependencyError(
		[]string{"cx-deep"}, []string{"cx-invalid"},
		[]string{"MODEL_API_KEY"},
	)
	message := err.Error()
	if !strings.Contains(message, "cx-deep") ||
		!strings.Contains(message, "cx-invalid") ||
		!strings.Contains(message, "MODEL_API_KEY") {
		t.Fatalf("message=%q", message)
	}
}

func TestServerDoctorReportsSelectedToolEnvironmentWithoutRemoteCall(
	t *testing.T,
) {
	paths := prepareVNextHome(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeVNextModel(
		t, paths.ConfigDir, "api-tool",
		"https://example.invalid/v1/chat/completions",
	)
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"agent":{"tools":["web_search"]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.ToolsDir, "web_search.json"),
		[]byte(`{
		  "schema_version":1,
		  "name":"web_search",
		  "effect":"read_only",
		  "description":"fixture web search",
		  "input_schema":{
		    "type":"object",
		    "properties":{"search_query":{"type":"string"}},
		    "required":["search_query"],
		    "additionalProperties":false
		  },
		  "executor":{
		    "type":"mcp",
		    "endpoint":"https://example.invalid/mcp",
		    "remote_tool":"web_search_prime",
		    "headers":{"Authorization":"Bearer ${Z_AI_API_KEY}"},
		    "timeout":"30s",
		    "max_response_bytes":1048576
		  }
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_API_KEY", "fixture-model-key")
	t.Setenv("Z_AI_API_KEY", "")
	var stdout bytes.Buffer
	err := serverDoctor(paths, newCLIOutput(false, &stdout, &bytes.Buffer{}))
	if err == nil || !strings.Contains(err.Error(), "Z_AI_API_KEY") ||
		!strings.Contains(stdout.String(), "Missing auth environment: Z_AI_API_KEY") {
		t.Fatalf("stdout=%q error=%v", stdout.String(), err)
	}
}
