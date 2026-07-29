package activation

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/layout"
)

func TestMain(main *testing.M) {
	if os.Getenv("SN_ACTIVATION_TEST_HOLD") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("SN_ACTIVATION_TEST_CANDIDATE") == "1" {
		if len(os.Args) == 3 &&
			((os.Args[1] == "profile" && os.Args[2] == "check") ||
				(os.Args[1] == "server" && os.Args[2] == "info")) {
			os.Exit(0)
		}
		os.Exit(64)
	}
	if os.Getenv("SN_ACTIVATION_TEST_REJECT_CANDIDATE") == "1" {
		os.Exit(64)
	}
	os.Exit(main.Run())
}

func TestLegacyProfileListGateAlwaysRejectsStagedContractV3Candidate(
	t *testing.T,
) {
	root := t.TempDir()
	resources := filepath.Join(root, "resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schema_version":1,"activation_epoch":2,"contract_version":3,"session_schema_version":2,"run_schema_version":2,"minimum_updater_epoch":2,"legacy_self_update":"blocked"}`)
	if err := os.WriteFile(filepath.Join(resources, "release.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "candidate")
	if err := os.WriteFile(binary, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "merged-home")
	if err := RequireLegacyProfileListGate(home, binary, resources); err == nil {
		t.Fatal("staged candidate was accepted without activation token")
	}
}

func TestLoadManifestRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(
		target,
		[]byte(`{"schema_version":1,"activation_epoch":1}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	resources := filepath.Join(root, "resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		target, filepath.Join(resources, "release.json"),
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadManifest(resources); err == nil {
		t.Fatal("release manifest symlink was accepted")
	}
}

func TestDurableRenamePersistsTargetBeforeSource(t *testing.T) {
	var calls []string
	err := durableRenameWith(
		"/active/bin",
		"/active/tmp/activation/backup/bin",
		func(source, target string) error {
			calls = append(calls, "rename:"+source+"->"+target)
			return nil
		},
		func(path string) error {
			calls = append(calls, "sync:"+path)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"rename:/active/bin->/active/tmp/activation/backup/bin",
		"sync:/active/tmp/activation/backup",
		"sync:/active",
	}
	if strings.Join(calls, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("calls=%#v", calls)
	}
}

func TestDurableRenameSameDirectorySyncsOnce(t *testing.T) {
	var synced []string
	err := durableRenameWith(
		"/active/.temporary",
		"/active/current",
		func(string, string) error { return nil },
		func(path string) error {
			synced = append(synced, path)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != "/active" {
		t.Fatalf("synced=%#v", synced)
	}
}

func TestUpgradeActivateCommitsCompletePayload(t *testing.T) {
	payload, target, candidate := upgradeFixture(t)
	seedActiveHome(t, target)
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	result, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ContractVersion != 3 ||
		result.SessionSchemaVersion != 2 ||
		result.RunSchemaVersion != 2 {
		t.Fatalf("result=%#v", result)
	}
	for _, path := range []string{
		filepath.Join(target, "bin", "sn-cli"),
		filepath.Join(target, "bin", "sn-server"),
		filepath.Join(target, "configs", "cx.json"),
		filepath.Join(target, "runtime.json"),
		filepath.Join(target, "resources", "release.json"),
		filepath.Join(target, "bin", "user-helper"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing activated path %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(
		filepath.Join(target, "commands"),
	); !os.IsNotExist(err) {
		t.Fatalf("obsolete commands directory remains: %v", err)
	}
	if _, err := os.Stat(
		filepath.Join(target, "state", activationGuardName),
	); !os.IsNotExist(err) {
		t.Fatalf("activation guard remains: %v", err)
	}
	if _, err := os.Stat(
		filepath.Join(target, "state", journalName),
	); !os.IsNotExist(err) {
		t.Fatalf("activation journal remains: %v", err)
	}
}

func TestUpgradeActivateRejectsOldRunSchemaBeforeMutation(t *testing.T) {
	payload, target, candidate := upgradeFixture(t)
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	databasePath := filepath.Join(target, "state", "runtime.db")
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true,
	})
	if err == nil || !strings.Contains(err.Error(), "schema 1") {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(
		filepath.Join(target, "bin", "sn-cli"),
	); !os.IsNotExist(err) {
		t.Fatalf("old-schema preflight mutated binary: %v", err)
	}
}

func TestLocalSourceInstallReplacesConfigsAndResetsOldRuntimeState(
	t *testing.T,
) {
	payload, target, candidate := upgradeFixture(t)
	seedActiveHome(t, target)
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	seedLegacyRuntimeState(t, target)
	stopCalls := 0
	result, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true, LocalSourceInstall: true,
		StopServer: func() error {
			stopCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopCalls != 1 {
		t.Fatalf("stop calls=%d, want 1", stopCalls)
	}
	if !result.RuntimeStateReset {
		t.Fatalf("result=%#v", result)
	}
	for _, path := range []string{
		filepath.Join(target, "configs", "old.json"),
		filepath.Join(target, "commands", "old.json"),
		filepath.Join(target, "sessions"),
		filepath.Join(target, "state", "session-locks"),
		filepath.Join(target, "state", "session-invocations"),
		filepath.Join(target, "state", "runtime.db"),
		filepath.Join(target, "state", "runtime.db-wal"),
		filepath.Join(target, "state", "runtime.db-shm"),
		filepath.Join(target, "state", "runtime.db-journal"),
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("local source install kept %s: %v", path, statErr)
		}
	}
	if _, err := os.Stat(
		filepath.Join(target, "configs", "cx.json"),
	); err != nil {
		t.Fatalf("source profile was not installed: %v", err)
	}
}

func TestLocalSourceInstallValidatesCandidateBeforeStoppingOrResetting(
	t *testing.T,
) {
	payload, target, candidate := upgradeFixture(t)
	seedActiveHome(t, target)
	seedLegacyRuntimeState(t, target)
	t.Setenv("SN_ACTIVATION_TEST_REJECT_CANDIDATE", "1")
	stopCalls := 0
	_, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true, LocalSourceInstall: true,
		StopServer: func() error {
			stopCalls++
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "candidate profile check") {
		t.Fatalf("error=%v", err)
	}
	if stopCalls != 0 {
		t.Fatalf("candidate failure stopped server %d time(s)", stopCalls)
	}
	for _, path := range []string{
		filepath.Join(target, "configs", "old.json"),
		filepath.Join(target, "sessions", "_system", "index.json"),
		filepath.Join(target, "state", "runtime.db"),
	} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("candidate failure changed %s: %v", path, statErr)
		}
	}
}

func TestUpgradeActivateRejectsLiveServerAndTmuxBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name  string
		block func(*testing.T, string) func()
	}{
		{
			name: "server_lease",
			block: func(t *testing.T, target string) func() {
				path := filepath.Join(
					target, "state", "sn-server.lease.lock",
				)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(
					path, os.O_CREATE|os.O_RDWR, 0o600,
				)
				if err != nil {
					t.Fatal(err)
				}
				if err := unix.Flock(
					int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB,
				); err != nil {
					t.Fatal(err)
				}
				return func() {
					_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
					_ = file.Close()
				}
			},
		},
		{
			name: "tmux_socket",
			block: func(t *testing.T, target string) func() {
				path := tmuxSocketForHome(target)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() { _ = os.Remove(path) }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, target, candidate := upgradeFixture(t)
			t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
			release := test.block(t, target)
			defer release()
			if _, err := UpgradeActivate(
				context.Background(),
				UpgradeRequest{
					TargetHome: target, PayloadDir: payload,
					CandidateBinary: candidate, OverwriteConfig: true,
				},
			); err == nil {
				t.Fatal("live state was accepted")
			}
			if _, err := os.Stat(
				filepath.Join(target, "bin", "sn-cli"),
			); !os.IsNotExist(err) {
				t.Fatalf("blocked activation mutated binary: %v", err)
			}
		})
	}
}

func TestActivationTransactionRollsBackBehindLegacyBarriers(
	t *testing.T,
) {
	journalPath, journal, _ := preparedTransaction(t)
	if err := os.RemoveAll(
		filepath.Join(journal.StageRoot, "desired", "resources"),
	); err != nil {
		t.Fatal(err)
	}
	if err := commitUpgradeTransaction(journalPath, &journal); err == nil {
		t.Fatal("commit unexpectedly succeeded without staged resources")
	}
	if err := verifyTransactionState(journal, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(journal.TargetHome, "state", activationGuardName),
		journalPath,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("rollback left %s: %v", path, err)
		}
	}
}

func TestActivationRollbackRestoresObsoleteCommands(t *testing.T) {
	journalPath, journal, _ := preparedTransaction(t)
	if err := os.Remove(
		filepath.Join(journal.StageRoot, "desired", "runtime.json"),
	); err != nil {
		t.Fatal(err)
	}
	if err := commitUpgradeTransaction(journalPath, &journal); err == nil {
		t.Fatal("commit unexpectedly succeeded without staged runtime.json")
	}
	data, err := os.ReadFile(
		filepath.Join(journal.TargetHome, "commands", "old.json"),
	)
	if err != nil || string(data) != "old-command" {
		t.Fatalf("obsolete commands rollback=%q err=%v", data, err)
	}
}

func TestActivationJournalAcceptsExistingSchema2CommandsArtifact(
	t *testing.T,
) {
	journalPath, journal, _ := preparedTransaction(t)
	commands, err := journalArtifact(&journal, obsoleteCommandsArtifact)
	if err != nil {
		t.Fatal(err)
	}
	commands.Remove = false
	commands.Staged = filepath.Join(
		journal.StageRoot, "desired", obsoleteCommandsArtifact,
	)
	if err := os.MkdirAll(commands.Staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(commands.Staged, "cx.json"),
		[]byte("{\"profile\":\"cx\"}\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	commands.NewDigest, err = treeDigest(commands.Staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	loaded, err := readJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	loadedCommands, err := journalArtifact(
		&loaded, obsoleteCommandsArtifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	if loadedCommands.Remove {
		t.Fatal("existing schema 2 commands artifact changed to removal")
	}
}

func TestActivationRecoveryFinalizesRolledBackJournalWithoutGuard(
	t *testing.T,
) {
	journalPath, journal, _ := preparedTransaction(t)
	if err := rollbackUpgradeTransaction(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	journal.Phase = "rolled_back"
	journal.OwnerStartToken = "reused-owner-token"
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := RequireNoGuard(
		filepath.Join(journal.TargetHome, "state"),
	); err == nil {
		t.Fatal("terminal rollback journal did not retain the entry barrier")
	}
	if err := recoverUpgradeTransaction(
		journal.TargetHome, journalPath,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("rolled-back recovery left journal: %v", err)
	}
	if err := verifyTransactionState(journal, false); err != nil {
		t.Fatal(err)
	}
}

func TestActivationRollbackFailureKeepsRuntimeGuarded(t *testing.T) {
	journalPath, journal, _ := preparedTransaction(t)
	configs, err := journalArtifact(&journal, "configs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(configs.Backup); err != nil {
		t.Fatal(err)
	}
	if err := rollbackUpgradeTransaction(
		journalPath, journal,
	); err == nil {
		t.Fatal("rollback accepted a missing original backup")
	}
	for _, path := range []string{
		filepath.Join(journal.TargetHome, "state", activationGuardName),
		journalPath,
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("failed rollback removed guard evidence %s: %v", path, err)
		}
	}
	if info, err := os.Lstat(
		filepath.Join(journal.TargetHome, "bin"),
	); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("failed rollback did not retain bin barrier: %v %#v", err, info)
	}
}

func TestActivationRecoveryRejectsChangedCommittedTarget(t *testing.T) {
	journalPath, journal, guard := preparedTransaction(t)
	if err := commitUpgradeTransaction(journalPath, &journal); err != nil {
		t.Fatal(err)
	}
	journal.OwnerStartToken = "reused-owner-token"
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(journal.TargetHome, "runtime.json"),
		[]byte("tampered"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := recoverUpgradeTransaction(
		journal.TargetHome, journalPath,
	); err == nil {
		t.Fatal("recovery accepted a changed committed target")
	}
	guardPath := filepath.Join(
		journal.TargetHome, "state", activationGuardName,
	)
	value, err := os.ReadFile(guardPath)
	if err != nil || string(value) != string(guard) {
		t.Fatalf("recovery did not preserve guard: %v", err)
	}
}

func TestActivationRecoveryFinalizesVerifiedCommit(t *testing.T) {
	journalPath, journal, _ := preparedTransaction(t)
	if err := commitUpgradeTransaction(journalPath, &journal); err != nil {
		t.Fatal(err)
	}
	journal.OwnerStartToken = "reused-owner-token"
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := recoverUpgradeTransaction(
		journal.TargetHome, journalPath,
	); err != nil {
		t.Fatal(err)
	}
	if err := verifyTransactionState(journal, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(journal.TargetHome, "state", activationGuardName),
		journalPath,
		journal.StageRoot,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("finalization left %s: %v", path, err)
		}
	}
}

func TestCommittedLocalSourceResetRemainsGuardedAndRecovers(
	t *testing.T,
) {
	journalPath, journal, guard := preparedTransaction(t)
	journal.ResetRuntimeState = true
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := commitUpgradeTransaction(journalPath, &journal); err != nil {
		t.Fatal(err)
	}
	journal.Phase = "committed"
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	sessionsPath := filepath.Join(journal.TargetHome, "sessions")
	if err := os.RemoveAll(sessionsPath); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	sentinel := filepath.Join(external, "sentinel")
	if err := os.WriteFile(sentinel, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, sessionsPath); err != nil {
		t.Fatal(err)
	}
	guardPath := filepath.Join(
		journal.TargetHome, "state", activationGuardName,
	)
	if err := finalizeUpgradeTransaction(
		journalPath, journal, guardPath, guard,
	); err == nil {
		t.Fatal("state reset accepted a symlink")
	}
	for _, path := range []string{guardPath, journalPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("failed reset removed recovery barrier %s: %v", path, err)
		}
	}
	if value, err := os.ReadFile(sentinel); err != nil ||
		string(value) != "safe" {
		t.Fatalf("external state changed: %q %v", value, err)
	}
	if err := os.Remove(sessionsPath); err != nil {
		t.Fatal(err)
	}
	if err := recoverUpgradeTransaction(
		journal.TargetHome, journalPath,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		guardPath, journalPath, journal.StageRoot, sessionsPath,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("recovery left %s: %v", path, err)
		}
	}
}

func TestActivationRecoveryRejectsMismatchedGuard(t *testing.T) {
	journalPath, journal, _ := preparedTransaction(t)
	guardPath := filepath.Join(
		journal.TargetHome, "state", activationGuardName,
	)
	if err := os.WriteFile(guardPath, []byte("other-owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal.OwnerStartToken = "reused-owner-token"
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := recoverUpgradeTransaction(
		journal.TargetHome, journalPath,
	); err == nil {
		t.Fatal("recovery accepted mismatched guard identity")
	}
	if _, err := os.Lstat(journalPath); err != nil {
		t.Fatalf("mismatched recovery removed journal: %v", err)
	}
}

func TestUpgradeActivateRejectsManagedPathSymlinksBeforeMutation(
	t *testing.T,
) {
	for _, name := range []string{"configs", "tmp"} {
		t.Run(name, func(t *testing.T) {
			payload, target, candidate := upgradeFixture(t)
			t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
			external := filepath.Join(t.TempDir(), "external")
			if err := os.MkdirAll(external, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(external, "sentinel")
			if err := os.WriteFile(sentinel, []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(
				external, filepath.Join(target, name),
			); err != nil {
				t.Fatal(err)
			}
			if _, err := UpgradeActivate(
				context.Background(), UpgradeRequest{
					TargetHome: target, PayloadDir: payload,
					CandidateBinary: candidate, OverwriteConfig: true,
				},
			); err == nil {
				t.Fatal("managed path symlink was accepted")
			}
			value, err := os.ReadFile(sentinel)
			if err != nil || string(value) != "safe" {
				t.Fatalf("external sentinel changed: %q %v", value, err)
			}
		})
	}
}

func TestRunPreflightRejectsUnknownStateAndSidecarSymlink(t *testing.T) {
	t.Run("unknown_state", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "runtime.db")
		database, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			"CREATE TABLE runs (state TEXT NOT NULL)",
			"CREATE TABLE queue (run_id TEXT)",
			"INSERT INTO runs(state) VALUES ('future_state')",
			"PRAGMA user_version = 2",
		} {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if err := preflightRunDatabase(path, 2); err == nil ||
			!strings.Contains(err.Error(), "unknown state") {
			t.Fatalf("unknown Run state error=%v", err)
		}
	})
	t.Run("sidecar_symlink", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "runtime.db")
		if err := os.WriteFile(path, []byte("not-used"), 0o600); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(root, "external")
		if err := os.WriteFile(external, []byte("safe"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, path+"-wal"); err != nil {
			t.Fatal(err)
		}
		if err := preflightRunDatabase(path, 2); err == nil ||
			!strings.Contains(err.Error(), "sidecar") {
			t.Fatalf("sidecar symlink error=%v", err)
		}
		value, err := os.ReadFile(external)
		if err != nil || string(value) != "safe" {
			t.Fatalf("sidecar sentinel changed: %q %v", value, err)
		}
	})
}

func TestProcessGateUsesFileIdentityAndStartToken(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "sn-cli")
	if err := os.Link(executable, targetPath); err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	target := processTarget{Path: targetPath, Info: targetInfo}
	command := exec.Command(executable)
	command.Env = append(
		os.Environ(), "SN_ACTIVATION_TEST_HOLD=1",
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	ownToken, err := processStartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ownExclusion := processExclusion{StartToken: ownToken}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = assertNoTargetProcesses(
			[]processTarget{target},
			map[int]processExclusion{os.Getpid(): ownExclusion},
		)
		if err != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("hardlinked target process was not detected")
	}
	if err := requireTargetCLIProcess(
		command.Process.Pid, target,
	); err != nil {
		t.Fatal(err)
	}
	token, err := processStartToken(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertNoTargetProcesses(
		[]processTarget{target},
		map[int]processExclusion{
			os.Getpid():         ownExclusion,
			command.Process.Pid: {StartToken: token},
		},
	); err != nil {
		t.Fatalf("exact coordinator exclusion failed: %v", err)
	}
	if err := assertNoTargetProcesses(
		[]processTarget{target},
		map[int]processExclusion{
			os.Getpid():         ownExclusion,
			command.Process.Pid: {StartToken: "reused"},
		},
	); err == nil {
		t.Fatal("changed coordinator start token was accepted")
	}
}

func TestProcessGateIgnoresProcessUsingReplacedTargetVnode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	targetPath := filepath.Join(root, "sn-cli")
	if err := os.Link(executable, targetPath); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(targetPath)
	command.Env = append(
		os.Environ(), "SN_ACTIVATION_TEST_HOLD=1",
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	if _, err := processStartToken(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	previousPath := filepath.Join(root, "sn-cli.previous")
	if err := os.Rename(targetPath, previousPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		targetPath, []byte("replacement\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	ownToken, err := processStartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := assertNoTargetProcesses(
		[]processTarget{{Path: targetPath, Info: targetInfo}},
		map[int]processExclusion{
			os.Getpid(): {StartToken: ownToken},
		},
	); err != nil {
		t.Fatalf("replaced target vnode was treated as running: %v", err)
	}
}

func TestProcessGateIgnoresReplacedTargetBehindActivationBarrier(
	t *testing.T,
) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(bin, "sn-cli")
	if err := os.Link(executable, targetPath); err != nil {
		t.Fatal(err)
	}
	commandDir := filepath.Join(root, "command")
	if err := os.Mkdir(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	commandLink := filepath.Join(commandDir, "sn-cli")
	if err := os.Symlink(targetPath, commandLink); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(commandLink)
	command.Env = append(
		os.Environ(), "SN_ACTIVATION_TEST_HOLD=1",
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	if _, err := processStartToken(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	oldBin := filepath.Join(home, "bin.old")
	if err := os.Rename(bin, oldBin); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		targetPath, []byte("replacement\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	stagedBin := filepath.Join(home, "bin.staged")
	if err := os.Rename(bin, stagedBin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("activation barrier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownToken, err := processStartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := assertNoTargetProcesses(
		[]processTarget{{Path: targetPath, Info: targetInfo}},
		map[int]processExclusion{
			os.Getpid(): {StartToken: ownToken},
		},
	); err != nil {
		t.Fatalf("barrier hid a provably replaced target vnode: %v", err)
	}
}

func preparedTransaction(
	t *testing.T,
) (string, transactionJournal, []byte) {
	t.Helper()
	payload, target, _ := upgradeFixture(t)
	target, err := layout.CanonicalHome(target)
	if err != nil {
		t.Fatal(err)
	}
	seedActiveHome(t, target)
	for _, directory := range []string{
		filepath.Join(target, "state"),
		filepath.Join(target, "tmp"),
		filepath.Join(target, "sessions"),
	} {
		if err := ensurePrivateDirectory(directory); err != nil {
			t.Fatal(err)
		}
	}
	stageRoot, err := os.MkdirTemp(
		filepath.Join(target, "tmp"), "activation-",
	)
	if err != nil {
		t.Fatal(err)
	}
	desired := filepath.Join(stageRoot, "desired")
	if _, err := buildDesiredHome(
		target, payload, desired, true,
	); err != nil {
		t.Fatal(err)
	}
	nonce, err := randomNonce()
	if err != nil {
		t.Fatal(err)
	}
	guard := []byte(
		`{"schema_version":2,"nonce":"` + nonce +
			`","owner_pid":1,"owner_start_token":"test"}` + "\n",
	)
	if err := prepareBarrierFiles(stageRoot, guard); err != nil {
		t.Fatal(err)
	}
	journal, err := newTransactionJournal(
		target, stageRoot, desired, nonce, "test-owner-token", guard, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(target, "state", journalName)
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := writeActivationGuard(
		filepath.Join(target, "state", activationGuardName), guard,
	); err != nil {
		t.Fatal(err)
	}
	if err := installLegacyBarriers(
		journalPath, &journal, guard,
	); err != nil {
		t.Fatal(err)
	}
	journal.Phase = "barriered"
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	return journalPath, journal, guard
}

func upgradeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	target := filepath.Join(root, "home")
	for _, directory := range []string{
		filepath.Join(payload, "configs"),
		filepath.Join(payload, "resources", "schema"),
		target,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(payload, "sn-cli")
	if err := os.Link(executable, candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(executable, filepath.Join(payload, "sn-server")); err != nil {
		t.Fatal(err)
	}
	writeFixture := func(path, value string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(
		filepath.Join(payload, "configs", "cx.json"),
		"{\"type\":\"cli\",\"command\":\"codex\"}\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "runtime.json"), "{}\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "schema", "profile.schema.json"),
		"{}\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "release.json"),
		"{\"schema_version\":1,\"activation_epoch\":2,\"contract_version\":3,\"session_schema_version\":2,\"run_schema_version\":2,\"minimum_updater_epoch\":2,\"legacy_self_update\":\"blocked\"}\n",
		0o600,
	)
	return payload, target, candidate
}

func seedActiveHome(t *testing.T, target string) {
	t.Helper()
	for _, directory := range []string{
		filepath.Join(target, "bin"),
		filepath.Join(target, "configs"),
		filepath.Join(target, "commands"),
		filepath.Join(target, "resources"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{
		filepath.Join(target, "bin", "sn-cli"):        "old-cli",
		filepath.Join(target, "bin", "sn-server"):     "old-server",
		filepath.Join(target, "bin", "user-helper"):   "preserve-me",
		filepath.Join(target, "configs", "old.json"):  "old-profile",
		filepath.Join(target, "commands", "old.json"): "old-command",
		filepath.Join(target, "runtime.json"):         "old-runtime",
		filepath.Join(target, "resources", "old.txt"): "old-resource",
	} {
		mode := os.FileMode(0o600)
		if strings.Contains(path, string(filepath.Separator)+"bin"+string(filepath.Separator)) {
			mode = 0o700
		}
		if err := os.WriteFile(path, []byte(value), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func seedLegacyRuntimeState(t *testing.T, target string) {
	t.Helper()
	for _, directory := range []string{
		filepath.Join(target, "sessions", "_system"),
		filepath.Join(target, "state", "session-locks"),
		filepath.Join(target, "state", "session-invocations"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(target, "sessions", "_system", "index.json"),
		[]byte("{\"schema_version\":1,\"sessions\":[]}\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(target, "state", "session-locks", "index.lock"),
		filepath.Join(
			target, "state", "session-invocations", ".invocation-old.json",
		),
	} {
		if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(target, "state", "runtime.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version = 1"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		databasePath + "-wal",
		databasePath + "-shm",
		databasePath + "-journal",
	} {
		if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
