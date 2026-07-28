//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/layout"
)

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

func TestServerInfoPublishesVNextContractWithoutLegacyScheduler(t *testing.T) {
	paths := prepareVNextHome(t)
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
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != 1 || payload.ContractVersion != 2 ||
		strings.Join(payload.Namespaces, ",") != strings.Join(fixedNamespaces, ",") ||
		len(payload.Capabilities["agent"]) == 0 ||
		payload.ConfiguredAddr != "127.0.0.1:8080" {
		t.Fatalf("payload=%#v", payload)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["features"]; exists {
		t.Fatal("legacy features were published")
	}
	if _, exists := raw["scheduler"]; exists {
		t.Fatal("legacy scheduler was published")
	}
	if _, exists := raw["address"]; exists {
		t.Fatal("configured address was published as runtime address")
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
	cfg := &config.Config{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runUpdateVNext(
		cfg, []string{"--json"}, newCLIOutput(false, &stdout, &stderr),
	)
	if err == nil || !strings.Contains(err.Error(), "unknown update argument --json") {
		t.Fatalf("error=%v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestServerDoctorDependencyErrorNamesMissingInputs(t *testing.T) {
	err := serverDoctorDependencyError(
		[]string{"cx-deep"}, []string{"MODEL_API_KEY"},
	)
	message := err.Error()
	if !strings.Contains(message, "cx-deep") ||
		!strings.Contains(message, "MODEL_API_KEY") {
		t.Fatalf("message=%q", message)
	}
}
