package activation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/profile"
	"github.com/yy003x/runtime/pkg/session"
)

func TestMain(main *testing.M) {
	if os.Getenv("SN_ACTIVATION_TEST_HOLD") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("SN_ACTIVATION_TEST_CANDIDATE") == "1" {
		if len(os.Args) == 3 {
			switch {
			case os.Args[1] == "profile" && os.Args[2] == "check":
				home := os.Getenv(layout.HomeEnv)
				if _, err := profile.Load(
					filepath.Join(home, "configs"),
					profileid.ReservedNamespaces()...,
				); err == nil {
					os.Exit(0)
				}
				os.Exit(65)
			case os.Args[1] == "server" && os.Args[2] == "info":
				os.Exit(0)
			}
		}
		os.Exit(64)
	}
	if os.Getenv("SN_ACTIVATION_TEST_REJECT_CANDIDATE") == "1" {
		os.Exit(64)
	}
	os.Exit(main.Run())
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

func TestLoadManifestRejectsDuplicateFields(t *testing.T) {
	resources := filepath.Join(t.TempDir(), "resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	value := `{
		"schema_version":1,
		"schema_version":1,
		"activation_epoch":4,
		"contract_version":6,
		"session_schema_version":3,
		"run_schema_version":6
	}`
	if err := os.WriteFile(
		filepath.Join(resources, "release.json"),
		[]byte(value), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadManifest(resources); err == nil ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("error=%v", err)
	}
}

func TestReadJournalRejectsPreviousSchema(t *testing.T) {
	journalPath, journal, _ := preparedTransaction(t)
	journal.SchemaVersion = 2
	if err := writeJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(journalPath); err == nil ||
		!strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("previous journal schema error=%v", err)
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
	if result.ActivationEpoch != 4 || result.ContractVersion != 6 ||
		result.SessionSchemaVersion != 3 ||
		result.RunSchemaVersion != 6 {
		t.Fatalf("result=%#v", result)
	}
	if strings.Join(result.ResourceFiles, ",") !=
		"release.json,schema/profile.schema.json,schema/runtime.schema.json,schema/tool.schema.json,tmux.conf" {
		t.Fatalf("resource files=%v", result.ResourceFiles)
	}
	for _, path := range []string{
		filepath.Join(target, "bin", "sn-cli"),
		filepath.Join(target, "bin", "sn-server"),
		filepath.Join(target, "configs", "cx.json"),
		filepath.Join(target, "tools", "web_search.json"),
		filepath.Join(target, "runtime.json"),
		filepath.Join(target, "resources", "release.json"),
		filepath.Join(target, "resources", "schema", "tool.schema.json"),
		filepath.Join(target, "resources", "tmux.conf"),
		filepath.Join(target, "bin", "user-helper"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing activated path %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(
		filepath.Join(target, "resources", "tools"),
	); !os.IsNotExist(err) {
		t.Fatalf("payload tools leaked into active resources: %v", err)
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

func TestBuildDesiredHomePreservesToolsAndCopiesMissing(t *testing.T) {
	payload, target, _ := upgradeFixture(t)
	seedActiveHome(t, target)
	desired := filepath.Join(t.TempDir(), "desired")
	result, err := buildDesiredHome(target, payload, desired, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.CopiedTools, ",") != "web_fetch.json" {
		t.Fatalf("copied tools=%v", result.CopiedTools)
	}
	for path, want := range map[string]string{
		filepath.Join(desired, "tools", "web_search.json"): "active-web-search",
		filepath.Join(desired, "tools", "local-only.json"): "active-local-tool",
		filepath.Join(desired, "tools", "web_fetch.json"):  "payload-web-fetch",
	} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || !strings.Contains(string(data), want) {
			t.Fatalf("%s=%q err=%v", path, data, readErr)
		}
	}
}

func TestUpgradeActivateRejectsUnsupportedRunSchemaBeforeMutation(t *testing.T) {
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
	if _, err := database.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true,
	})
	if err == nil || !strings.Contains(err.Error(), "schema 999") {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(
		filepath.Join(target, "bin", "sn-cli"),
	); !os.IsNotExist(err) {
		t.Fatalf("unsupported-schema preflight mutated binary: %v", err)
	}
}

func TestUpgradeActivateRejectsMissingPayloadToolsBeforeMutation(t *testing.T) {
	payload, target, candidate := upgradeFixture(t)
	if err := os.RemoveAll(
		filepath.Join(payload, "resources", "tools"),
	); err != nil {
		t.Fatal(err)
	}
	_, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true,
	})
	if err == nil || !strings.Contains(err.Error(), "payload resources/tools") {
		t.Fatalf("missing tools error=%v", err)
	}
	assertActivationNotStarted(t, target)
}

func TestUpgradeActivateRejectsMissingPayloadManifestBeforeMutation(
	t *testing.T,
) {
	payload, target, candidate := upgradeFixture(t)
	if err := os.Remove(
		filepath.Join(payload, "release", "release.json"),
	); err != nil {
		t.Fatal(err)
	}
	_, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true,
	})
	if err == nil || !strings.Contains(err.Error(), "payload release manifest") {
		t.Fatalf("legacy manifest error=%v", err)
	}
	assertActivationNotStarted(t, target)
}

func TestUpgradeActivateRejectsUnexpectedPayloadLayoutBeforeMutation(
	t *testing.T,
) {
	for _, testCase := range []struct {
		name string
		path []string
	}{
		{name: "root_tools", path: []string{"tools", "legacy.json"}},
		{name: "root_runtime", path: []string{"runtime.json"}},
		{
			name: "resources_tmux",
			path: []string{"resources", "tmux.conf"},
		},
		{
			name: "resources_manifest",
			path: []string{"resources", "release.json"},
		},
		{
			name: "release_unknown",
			path: []string{"release", "legacy.json"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			payload, target, candidate := upgradeFixture(t)
			legacyPath := filepath.Join(
				append([]string{payload}, testCase.path...)...,
			)
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				legacyPath, []byte("legacy\n"), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, err := UpgradeActivate(context.Background(), UpgradeRequest{
				TargetHome: target, PayloadDir: payload,
				CandidateBinary: candidate, OverwriteConfig: true,
			})
			if err == nil || !strings.Contains(err.Error(), "unexpected entry") {
				t.Fatalf("unexpected payload layout error=%v", err)
			}
			assertActivationNotStarted(t, target)
		})
	}
}

func TestUpgradeActivateRejectsInvalidOrUnavailableToolsBeforeMutation(
	t *testing.T,
) {
	t.Run("invalid_payload_manifest", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		if err := os.WriteFile(
			filepath.Join(
				payload, "resources", "tools", "web_search.json",
			),
			[]byte(`{"unexpected":true}`), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeActivate(context.Background(), UpgradeRequest{
			TargetHome: target, PayloadDir: payload,
			CandidateBinary: candidate, OverwriteConfig: true,
		})
		if err == nil || !strings.Contains(err.Error(), "payload resources/tools") {
			t.Fatalf("invalid payload tools error=%v", err)
		}
		assertActivationNotStarted(t, target)
	})

	t.Run("unavailable_configured_tool", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		if err := os.WriteFile(
			filepath.Join(payload, "release", "runtime.json"),
			[]byte(`{"agent":{"tools":["missing_tool"]}}`), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeActivate(context.Background(), UpgradeRequest{
			TargetHome: target, PayloadDir: payload,
			CandidateBinary: candidate, OverwriteConfig: true,
		})
		if err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("unavailable payload tool error=%v", err)
		}
		assertActivationNotStarted(t, target)
	})

	t.Run("manifest_conflicts_with_builtin", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		if err := os.WriteFile(
			filepath.Join(
				payload, "resources", "tools", "read_file.json",
			),
			[]byte(toolFixture("read_file", "conflict")), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeActivate(context.Background(), UpgradeRequest{
			TargetHome: target, PayloadDir: payload,
			CandidateBinary: candidate, OverwriteConfig: true,
		})
		if err == nil || !strings.Contains(err.Error(), "built-in tool") {
			t.Fatalf("built-in manifest collision error=%v", err)
		}
		assertActivationNotStarted(t, target)
	})

	t.Run("invalid_preserved_tool", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		tools := filepath.Join(target, "tools")
		if err := os.MkdirAll(tools, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(tools, "local-only.json")
		if err := os.WriteFile(path, []byte(`{"unexpected":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeActivate(context.Background(), UpgradeRequest{
			TargetHome: target, PayloadDir: payload,
			CandidateBinary: candidate,
		})
		if err == nil || !strings.Contains(err.Error(), "active tools") {
			t.Fatalf("invalid active tools error=%v", err)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != `{"unexpected":true}` {
			t.Fatalf("active tool changed: %q %v", data, readErr)
		}
		for _, name := range []string{"bin", "resources", "state", "tmp"} {
			if _, statErr := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(statErr) {
				t.Fatalf("failed preflight created %s: %v", name, statErr)
			}
		}
	})
}

func TestUpgradeActivateRejectsUnsupportedRunSchemaManifestBeforeMutation(t *testing.T) {
	payload, target, candidate := upgradeFixture(t)
	manifestPath := filepath.Join(payload, "release", "release.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(
		string(manifest), `"run_schema_version":6`,
		`"run_schema_version":999`, 1,
	))
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true,
	})
	if err == nil || !strings.Contains(err.Error(), "run_schema=999") {
		t.Fatalf("error=%v", err)
	}
	assertActivationNotStarted(t, target)
}

func TestUpgradeActivateRejectsInvalidRuntimeBeforeMutation(t *testing.T) {
	t.Run("payload_runtime", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		if err := os.WriteFile(
			filepath.Join(payload, "release", "runtime.json"),
			[]byte(`{"definitely_invalid_runtime_field":true}`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		stopCalls := 0
		_, err := UpgradeActivate(
			context.Background(),
			UpgradeRequest{
				TargetHome: target, PayloadDir: payload,
				CandidateBinary: candidate, OverwriteConfig: true,
				LocalSourceInstall: true,
				InspectServer:      inspectStoppedServer,
				StopServer: func() error {
					stopCalls++
					return nil
				},
			},
		)
		if err == nil ||
			!strings.Contains(err.Error(), "payload release/runtime.json") {
			t.Fatalf("error=%v", err)
		}
		if stopCalls != 0 {
			t.Fatalf("invalid runtime stopped server %d time(s)", stopCalls)
		}
		assertActivationNotStarted(t, target)
	})

	t.Run("preserved_active_runtime", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		if err := os.WriteFile(
			filepath.Join(target, "runtime.json"),
			[]byte(`{"definitely_invalid_runtime_field":true}`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeActivate(
			context.Background(),
			UpgradeRequest{
				TargetHome: target, PayloadDir: payload,
				CandidateBinary: candidate,
			},
		)
		if err == nil ||
			!strings.Contains(err.Error(), "active runtime.json") {
			t.Fatalf("error=%v", err)
		}
		assertActivationNotStarted(t, target)
	})
}

func TestUpgradeActivateRejectsInvalidProfileBeforeMutation(t *testing.T) {
	payload, target, candidate := upgradeFixture(t)
	if err := os.WriteFile(
		filepath.Join(payload, "configs", "cx.json"),
		[]byte(`{"type":"cli","command":"unsupported"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	stopCalls := 0
	_, err := UpgradeActivate(
		context.Background(),
		UpgradeRequest{
			TargetHome: target, PayloadDir: payload,
			CandidateBinary: candidate, OverwriteConfig: true,
			LocalSourceInstall: true,
			InspectServer:      inspectStoppedServer,
			StopServer: func() error {
				stopCalls++
				return nil
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "candidate profile check") {
		t.Fatalf("error=%v", err)
	}
	if stopCalls != 0 {
		t.Fatalf("invalid profile stopped server %d time(s)", stopCalls)
	}
	assertActivationNotStarted(t, target)
}

func TestActivationRuntimeValidationRequiresRegularFiles(t *testing.T) {
	t.Run("payload_missing", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		if err := os.Remove(
			filepath.Join(payload, "release", "runtime.json"),
		); err != nil {
			t.Fatal(err)
		}
		stopCalls := 0
		_, err := UpgradeActivate(
			context.Background(),
			UpgradeRequest{
				TargetHome: target, PayloadDir: payload,
				CandidateBinary: candidate, OverwriteConfig: true,
				LocalSourceInstall: true,
				InspectServer:      inspectStoppedServer,
				StopServer: func() error {
					stopCalls++
					return nil
				},
			},
		)
		if err == nil ||
			!strings.Contains(err.Error(), "payload release/runtime.json") {
			t.Fatalf("error=%v", err)
		}
		if stopCalls != 0 {
			t.Fatalf("missing runtime stopped server %d time(s)", stopCalls)
		}
		assertActivationNotStarted(t, target)
	})

	t.Run("preserved_active_symlink", func(t *testing.T) {
		payload, target, candidate := upgradeFixture(t)
		external := filepath.Join(t.TempDir(), "runtime.json")
		if err := os.WriteFile(external, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(
			external, filepath.Join(target, "runtime.json"),
		); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeActivate(
			context.Background(),
			UpgradeRequest{
				TargetHome: target, PayloadDir: payload,
				CandidateBinary: candidate,
			},
		)
		if err == nil || !strings.Contains(err.Error(), "active runtime.json") {
			t.Fatalf("error=%v", err)
		}
		assertActivationNotStarted(t, target)
	})

	t.Run("staged_missing", func(t *testing.T) {
		payload, target, _ := upgradeFixture(t)
		manifest, _, err := LoadManifest(
			filepath.Join(payload, "release"),
		)
		if err != nil {
			t.Fatal(err)
		}
		desired := filepath.Join(t.TempDir(), "desired")
		if _, err := buildDesiredHome(
			target, payload, desired, true,
		); err != nil {
			t.Fatal(err)
		}
		if err := validateDesiredHomeContracts(desired, manifest); err != nil {
			t.Fatalf("complete staged home was rejected: %v", err)
		}
		if err := os.Remove(filepath.Join(desired, "runtime.json")); err != nil {
			t.Fatal(err)
		}
		if err := validateDesiredHomeContracts(
			desired, manifest,
		); err == nil || !strings.Contains(err.Error(), "staged runtime.json") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestValidateActiveHomeShapeRejectsSymlinkLogRoot(t *testing.T) {
	target := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(target, "logs")); err != nil {
		t.Fatal(err)
	}
	err := validateActiveHomeShape(target)
	if err == nil || !strings.Contains(err.Error(), "active logs") {
		t.Fatalf("error=%v", err)
	}
}

func TestUpgradeActivateRejectsIncompleteResourcesBeforeMutation(
	t *testing.T,
) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "missing_tmux_config",
			mutate: func(t *testing.T, payload string) {
				if err := os.Remove(
					filepath.Join(payload, "release", "tmux.conf"),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing_tool_schema",
			mutate: func(t *testing.T, payload string) {
				if err := os.Remove(filepath.Join(
					payload, "resources", "schema", "tool.schema.json",
				)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "schema_symlink",
			mutate: func(t *testing.T, payload string) {
				path := filepath.Join(
					payload, "resources", "schema",
					"profile.schema.json",
				)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "schema.json")
				if err := os.WriteFile(
					target, []byte("{}\n"), 0o600,
				); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed_schema_json",
			mutate: func(t *testing.T, payload string) {
				if err := os.WriteFile(
					filepath.Join(
						payload, "resources", "schema",
						"profile.schema.json",
					),
					[]byte(`{"type":`), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "uncompilable_schema",
			mutate: func(t *testing.T, payload string) {
				if err := os.WriteFile(
					filepath.Join(
						payload, "resources", "schema",
						"runtime.schema.json",
					),
					[]byte(`{"type":17}`), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong_schema_identity",
			mutate: func(t *testing.T, payload string) {
				if err := os.WriteFile(
					filepath.Join(
						payload, "resources", "schema",
						"profile.schema.json",
					),
					[]byte(`{
						"$schema":"https://json-schema.org/draft/2020-12/schema",
						"$id":"https://example.invalid/profile.schema.json",
						"title":"Runtime Profile",
						"oneOf":[{},{}]
					}`), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			payload, target, candidate := upgradeFixture(t)
			testCase.mutate(t, payload)
			_, err := UpgradeActivate(
				context.Background(),
				UpgradeRequest{
					TargetHome: target, PayloadDir: payload,
					CandidateBinary: candidate, OverwriteConfig: true,
				},
			)
			if err == nil {
				t.Fatal("incomplete resources were accepted")
			}
			assertActivationNotStarted(t, target)
		})
	}
}

func TestActiveExecutionPreflightAcceptsCompleteSettledFacts(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	sessionID := "session_" + strings.Repeat("1", 32)
	executionID := "execution_" + strings.Repeat("2", 32)
	turnID := "turn_" + strings.Repeat("3", 32)
	runID := "run_" + strings.Repeat("4", 32)
	sessionDir := filepath.Join(sessionsDir, sessionID)
	executionsDir := filepath.Join(sessionDir, "executions")
	if err := os.MkdirAll(executionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sessionData, err := json.Marshal(session.Session{
		SchemaVersion:   session.SchemaVersion,
		ID:              sessionID,
		Interface:       session.InterfaceManaged,
		State:           session.SessionIdle,
		Retention:       session.RetentionEphemeral,
		CreatedAt:       now,
		UpdatedAt:       now,
		MessageCount:    1,
		EventCount:      1,
		LastProfileID:   "api-cx",
		LastProfileKind: profile.KindModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	executionData, err := json.Marshal(session.Execution{
		SchemaVersion: session.SchemaVersion,
		ID:            executionID,
		SessionID:     sessionID,
		TurnID:        turnID,
		RunID:         runID,
		ProfileID:     "api-cx",
		ProfileKind:   profile.KindModel,
		State:         session.ExecutionSettled,
		Outcome:       session.OutcomeCompleted,
		RequestDigest: strings.Repeat("a", 64),
		ConfigDigest:  strings.Repeat("b", 64),
		Stdout:        session.StreamObservation{ObservedBytes: 2},
		Stderr:        session.StreamObservation{},
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		filepath.Join(sessionDir, "session.json"):         sessionData,
		filepath.Join(executionsDir, executionID+".json"): executionData,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := assertNoActiveSessionExecutions(sessionsDir); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSourceInstallReplacesConfigsAndResetsRuntimeState(
	t *testing.T,
) {
	payload, target, candidate := upgradeFixture(t)
	seedActiveHome(t, target)
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	seedRuntimeStateForReset(t, target)
	stopCalls := 0
	result, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true, LocalSourceInstall: true,
		InspectServer: inspectStoppedServer,
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
		filepath.Join(target, "tools", "local-only.json"),
		filepath.Join(target, "sessions"),
		filepath.Join(target, "state", "session-locks"),
		filepath.Join(target, "state", "session-invocations"),
		filepath.Join(target, "state", "session-mutations"),
		filepath.Join(target, "state", "session-trash-moves"),
		filepath.Join(target, "state", "native-tui-invocations"),
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
	if data, err := os.ReadFile(
		filepath.Join(target, "tools", "web_search.json"),
	); err != nil || !strings.Contains(string(data), "payload-web-search") {
		t.Fatalf("source tool was not installed: %q %v", data, err)
	}
}

func TestLocalSourceInstallValidatesCandidateBeforeStoppingOrResetting(
	t *testing.T,
) {
	payload, target, candidate := upgradeFixture(t)
	seedActiveHome(t, target)
	seedRuntimeStateForReset(t, target)
	t.Setenv("SN_ACTIVATION_TEST_REJECT_CANDIDATE", "1")
	stopCalls := 0
	_, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true, LocalSourceInstall: true,
		InspectServer: inspectStoppedServer,
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

func TestLocalSourceInstallChecksTmuxBeforeStoppingServer(t *testing.T) {
	payload, target, candidate := upgradeFixture(t)
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	socket := tmuxSocketForHome(target)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socket) })
	stopCalls := 0
	_, err := UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true, LocalSourceInstall: true,
		InspectServer: inspectStoppedServer,
		StopServer: func() error {
			stopCalls++
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Tmux socket") {
		t.Fatalf("error=%v", err)
	}
	if stopCalls != 0 {
		t.Fatalf("Tmux blocker stopped managed server %d time(s)", stopCalls)
	}
}

func TestDefaultTmuxSessionPreflightMatchesRuntimeHome(t *testing.T) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	tmuxTmp, err := os.MkdirTemp("/tmp", "sn-activation-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmuxTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", tmuxTmp)
	t.Cleanup(func() {
		_ = exec.Command(path, "-L", "default", "kill-server").Run()
		_ = os.RemoveAll(tmuxTmp)
	})
	target, err := layout.CanonicalHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(
		path, "-L", "default", "new-session", "-d", "-s", "sn-session",
	).CombinedOutput(); err != nil {
		t.Fatalf("create default tmux session: %v: %s", err, output)
	}
	setOwner := func(home string) {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(home))))
		data, err := json.Marshal(map[string]any{"full_home_digest": digest})
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.RawURLEncoding.EncodeToString(data)
		if output, err := exec.Command(
			path, "-L", "default", "set-option", "-q", "-t", "sn-session",
			"@sn_runtime_session", encoded,
		).CombinedOutput(); err != nil {
			t.Fatalf("set default tmux owner: %v: %s", err, output)
		}
	}
	setOwner(target)
	if err := assertNoDefaultTmuxSession(target); err == nil {
		t.Fatal("owned default tmux session did not block activation")
	}
	setOwner(t.TempDir())
	if err := assertNoDefaultTmuxSession(target); err != nil {
		t.Fatalf("foreign default tmux session blocked activation: %v", err)
	}
}

func TestLocalSourceInstallChecksExtraTargetProcessBeforeStoppingServer(
	t *testing.T,
) {
	payload, target, candidate := upgradeFixture(t)
	t.Setenv("SN_ACTIVATION_TEST_CANDIDATE", "1")
	bin := filepath.Join(target, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	targetCLI := filepath.Join(bin, "sn-cli")
	if err := os.Link(executable, targetCLI); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(targetCLI)
	command.Env = append(os.Environ(), "SN_ACTIVATION_TEST_HOLD=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	if _, err := processStartToken(command.Process.Pid); err != nil {
		t.Fatal(err)
	}

	stopCalls := 0
	_, err = UpgradeActivate(context.Background(), UpgradeRequest{
		TargetHome: target, PayloadDir: payload, CandidateBinary: candidate,
		OverwriteConfig: true, LocalSourceInstall: true,
		InspectServer: inspectStoppedServer,
		StopServer: func() error {
			stopCalls++
			return nil
		},
	})
	if err == nil ||
		!strings.Contains(err.Error(), "target Runtime binary is still running") {
		t.Fatalf("error=%v", err)
	}
	if stopCalls != 0 {
		t.Fatalf("target-process blocker stopped server %d time(s)", stopCalls)
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

func TestActivationTransactionRollsBackBehindActivationBarriers(
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
	if err := activationgate.RequireOpen(
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
	for _, name := range []string{"configs", "tools", "tmp"} {
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
			"PRAGMA user_version = 4",
		} {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if err := preflightRunDatabase(path, 4); err == nil ||
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
		if err := preflightRunDatabase(path, 4); err == nil ||
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
	if err := installActivationBarriers(journalPath, &journal); err != nil {
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
		filepath.Join(payload, "resources", "tools"),
		filepath.Join(payload, "release"),
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
		filepath.Join(payload, "release", "runtime.json"), "{}\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "tools", "web_fetch.json"),
		toolFixture("web_fetch", "payload-web-fetch"), 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "tools", "web_search.json"),
		toolFixture("web_search", "payload-web-search"), 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "schema", "profile.schema.json"),
		`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"$id":"https://github.com/yy003x/runtime/resources/schema/profile.schema.json",
			"title":"Runtime Profile",
			"oneOf":[{"type":"object"},{"type":"object"}]
		}`+"\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "schema", "runtime.schema.json"),
		`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"$id":"https://github.com/yy003x/runtime/resources/schema/runtime.schema.json",
			"title":"SN Runtime Configuration",
			"type":"object",
			"additionalProperties":false,
			"properties":{"agent":{},"scheduler":{},"run":{}}
		}`+"\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "resources", "schema", "tool.schema.json"),
		`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"$id":"https://github.com/yy003x/runtime/resources/schema/tool.schema.json",
			"title":"Runtime Tool",
			"type":"object",
			"additionalProperties":false,
			"properties":{}
		}`+"\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "release", "tmux.conf"),
		"set-option -g status off\n", 0o600,
	)
	writeFixture(
		filepath.Join(payload, "release", "release.json"),
		"{\"schema_version\":1,\"activation_epoch\":4,\"contract_version\":6,\"session_schema_version\":3,\"run_schema_version\":6}\n",
		0o600,
	)
	return payload, target, candidate
}

func inspectStoppedServer() (ManagedServerProcess, error) {
	return ManagedServerProcess{}, nil
}

func assertActivationNotStarted(t *testing.T, target string) {
	t.Helper()
	for _, name := range []string{"bin", "configs", "tools", "resources", "state", "tmp"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf(
				"failed preflight created %s: %v",
				filepath.Join(target, name), err,
			)
		}
	}
}

func seedActiveHome(t *testing.T, target string) {
	t.Helper()
	for _, directory := range []string{
		filepath.Join(target, "bin"),
		filepath.Join(target, "configs"),
		filepath.Join(target, "tools"),
		filepath.Join(target, "resources"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{
		filepath.Join(target, "bin", "sn-cli"):            "active-cli",
		filepath.Join(target, "bin", "sn-server"):         "active-server",
		filepath.Join(target, "bin", "user-helper"):       "preserve-me",
		filepath.Join(target, "configs", "old.json"):      "active-profile",
		filepath.Join(target, "tools", "web_search.json"): toolFixture("web_search", "active-web-search"),
		filepath.Join(target, "tools", "local-only.json"): toolFixture("local-only", "active-local-tool"),
		filepath.Join(target, "runtime.json"):             "active-runtime",
		filepath.Join(target, "resources", "old.txt"):     "active-resource",
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

func toolFixture(name, description string) string {
	return `{"schema_version":1,"name":"` + name +
		`","effect":"read_only","description":"` + description +
		`","input_schema":{"type":"object","properties":{},"additionalProperties":false},` +
		`"executor":{"type":"mcp","endpoint":"https://example.invalid/mcp",` +
		`"remote_tool":"` + name + `","headers":{},"timeout":"1s",` +
		`"max_response_bytes":1024}}` + "\n"
}

func seedRuntimeStateForReset(t *testing.T, target string) {
	t.Helper()
	for _, directory := range []string{
		filepath.Join(target, "sessions", "_system"),
		filepath.Join(target, "state", "session-locks"),
		filepath.Join(target, "state", "session-invocations"),
		filepath.Join(target, "state", "session-mutations"),
		filepath.Join(target, "state", "session-trash-moves"),
		filepath.Join(target, "state", "native-tui-invocations"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(target, "sessions", "_system", "index.json"),
		[]byte("{\"schema_version\":3,\"sessions\":[]}\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(target, "state", "session-locks", "index.lock"),
		filepath.Join(
			target, "state", "session-invocations", ".invocation-current.json",
		),
		filepath.Join(
			target, "state", "session-mutations",
			"session_fixture.json",
		),
		filepath.Join(
			target, "state", "session-trash-moves",
			"session_fixture.json",
		),
		filepath.Join(
			target, "state", "native-tui-invocations",
			"execution_fixture.json",
		),
	} {
		if err := os.WriteFile(path, []byte("runtime-state\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(target, "state", "runtime.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version = 4"); err != nil {
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
		if err := os.WriteFile(path, []byte("runtime-state\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
