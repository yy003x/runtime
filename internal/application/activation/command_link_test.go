package activation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureCommandLinkCreatesAndPreservesExactLink(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")

	if err := EnsureCommandLink(link, target); err != nil {
		t.Fatalf("create command link: %v", err)
	}
	if err := ValidateCommandLink(link, target); err != nil {
		t.Fatalf("validate command link: %v", err)
	}
	if err := EnsureCommandLink(link, target); err != nil {
		t.Fatalf("preserve command link: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read command link: %v", err)
	}
	if got != target {
		t.Fatalf("command link target = %q, want %q", got, target)
	}
}

func TestValidateCommandLinkDoesNotCreateMissingLink(t *testing.T) {
	link := filepath.Join(canonicalTestDir(t), "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	if err := ValidateCommandLink(link, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("validation created command link: %v", err)
	}
}

func TestEnsureCommandLinkNeverReplacesExistingEntry(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{
			name: "regular file",
			setup: func(path string) error {
				return os.WriteFile(path, []byte("keep"), 0o600)
			},
		},
		{
			name: "directory",
			setup: func(path string) error {
				return os.Mkdir(path, 0o700)
			},
		},
		{
			name: "different symlink",
			setup: func(path string) error {
				return os.Symlink("/different/runtime/bin/sn-cli", path)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := canonicalTestDir(t)
			link := filepath.Join(parent, "sn-cli")
			if err := test.setup(link); err != nil {
				t.Fatal(err)
			}
			if err := EnsureCommandLink(
				link, filepath.Join(t.TempDir(), "bin", "sn-cli"),
			); err == nil {
				t.Fatal("expected existing entry rejection")
			}
		})
	}
}

func TestEnsureCommandLinkRejectsSymlinkParent(t *testing.T) {
	parent := canonicalTestDir(t)
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(parent, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}
	err := EnsureCommandLink(
		filepath.Join(linkParent, "sn-cli"),
		filepath.Join(t.TempDir(), "bin", "sn-cli"),
	)
	if err == nil {
		t.Fatal("expected symlink parent rejection")
	}
}

func TestEnsureCommandLinkRejectsSymlinkAncestor(t *testing.T) {
	parent := canonicalTestDir(t)
	realAncestor := filepath.Join(parent, "real")
	realParent := filepath.Join(realAncestor, "bin")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realAncestor, alias); err != nil {
		t.Fatal(err)
	}
	err := EnsureCommandLink(
		filepath.Join(alias, "bin", "sn-cli"),
		filepath.Join(t.TempDir(), "bin", "sn-cli"),
	)
	if err == nil {
		t.Fatal("expected symlink ancestor rejection")
	}
	if _, statErr := os.Lstat(
		filepath.Join(realParent, "sn-cli"),
	); !os.IsNotExist(statErr) {
		t.Fatalf("symlink ancestor was followed: %v", statErr)
	}
}

func TestCommandLinkReservationBlocksConcurrentOccupationAndPersists(
	t *testing.T,
) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	reservation, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/other/runtime/bin/sn-cli", link); !os.IsExist(err) {
		t.Fatalf("reservation did not occupy command link: %v", err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("released durable link changed: target=%q error=%v", got, err)
	}
	if _, err := os.Stat(
		filepath.Join(parent, commandLinkOwnerName("sn-cli")),
	); err != nil {
		t.Fatalf("durable owner missing after release: %v", err)
	}
}

func TestCommandLinkReservationPreservesPreexistingExactLink(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	reservation, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("preexisting link changed: target=%q error=%v", got, err)
	}
}

func TestCommandLinkDurableOwnerSerializesInstallers(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	first, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveCommandLink(link, target); err == nil ||
		!strings.Contains(err.Error(), "in progress") {
		t.Fatalf("concurrent reservation error=%v", err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandLinkReservationDetectsReplacement(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	reservation, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Commit(); err == nil {
		t.Fatal("reservation accepted a replaced command link")
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("replacement was removed: target=%q error=%v", got, err)
	}
}

func TestCommandLinkReservationReleaseNeverRemovesReplacement(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	reservation, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	previousHook := commandLinkReservationTestHook
	commandLinkReservationTestHook = func(phase string) error {
		if phase != "before_release_verify" {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink(target, link)
	}
	t.Cleanup(func() {
		commandLinkReservationTestHook = previousHook
	})
	releaseErr := reservation.Release()
	commandLinkReservationTestHook = previousHook
	if releaseErr == nil {
		t.Fatal("release accepted a replaced command link")
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("release removed replacement: target=%q error=%v", got, err)
	}
}

func TestCommandLinkReservationRecoversAfterOwnerPublicationCrash(
	t *testing.T,
) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	injected := errors.New("simulated owner publication crash")
	previousHook := commandLinkReservationTestHook
	commandLinkReservationTestHook = func(phase string) error {
		if phase == "after_owner_persisted" {
			return injected
		}
		return nil
	}
	_, err := ReserveCommandLink(link, target)
	commandLinkReservationTestHook = previousHook
	if !errors.Is(err, injected) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(
		filepath.Join(parent, commandLinkOwnerName("sn-cli")),
	); err != nil {
		t.Fatalf("crash lost durable owner: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("pre-link crash published final link: %v", err)
	}
	retry, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatalf("retry durable reservation: %v", err)
	}
	if err := retry.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("retry link target=%q error=%v", got, err)
	}
}

func TestCommandLinkReservationRecoversAfterTempOwnerCrash(
	t *testing.T,
) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	injected := errors.New("simulated temp owner crash")
	previousHook := commandLinkReservationTestHook
	commandLinkReservationTestHook = func(phase string) error {
		if phase == "after_owner_temp_persisted" {
			return injected
		}
		return nil
	}
	_, err := ReserveCommandLink(link, target)
	commandLinkReservationTestHook = previousHook
	if !errors.Is(err, injected) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Lstat(
		filepath.Join(parent, commandLinkOwnerName("sn-cli")),
	); !os.IsNotExist(err) {
		t.Fatalf("temp crash published final owner: %v", err)
	}
	retry, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatalf("retry after temp owner crash: %v", err)
	}
	if err := retry.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandLinkReservationRejectsVisibleParentReplacement(t *testing.T) {
	root := canonicalTestDir(t)
	parent := filepath.Join(root, "command")
	oldParent := filepath.Join(root, "command-before-replacement")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	reservation, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, oldParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Commit(); err == nil ||
		!strings.Contains(err.Error(), "parent identity changed") {
		t.Fatalf("commit error=%v", err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("visible replacement changed: target=%q error=%v", got, err)
	}
}

func TestCommandLinkOwnerNeverOverwritesUnrelatedEntry(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string, string) error
	}{
		{
			name: "regular_file",
			setup: func(ownerPath string, _ string) error {
				return os.WriteFile(ownerPath, []byte("sentinel"), 0o600)
			},
		},
		{
			name: "hardlink",
			setup: func(ownerPath string, sentinel string) error {
				return os.Link(sentinel, ownerPath)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := canonicalTestDir(t)
			link := filepath.Join(parent, "sn-cli")
			target := filepath.Join(t.TempDir(), "bin", "sn-cli")
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(
				sentinel, []byte("sentinel"), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			ownerPath := filepath.Join(
				parent, commandLinkOwnerName("sn-cli"),
			)
			if err := test.setup(ownerPath, sentinel); err != nil {
				t.Fatal(err)
			}
			if _, err := ReserveCommandLink(link, target); err == nil {
				t.Fatal("unrelated owner entry was accepted")
			}
			value, err := os.ReadFile(sentinel)
			if err != nil || string(value) != "sentinel" {
				t.Fatalf("sentinel changed: value=%q error=%v", value, err)
			}
			if test.name == "regular_file" {
				value, err = os.ReadFile(ownerPath)
				if err != nil || string(value) != "sentinel" {
					t.Fatalf(
						"owner entry changed: value=%q error=%v",
						value, err,
					)
				}
			}
		})
	}
}

func TestCommandLinkOwnerAtomicPublishNeverReplacesLateEntry(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	ownerPath := filepath.Join(parent, commandLinkOwnerName("sn-cli"))
	previousHook := commandLinkReservationTestHook
	commandLinkReservationTestHook = func(phase string) error {
		if phase != "after_owner_temp_persisted" {
			return nil
		}
		return os.WriteFile(ownerPath, []byte("late-sentinel"), 0o600)
	}
	_, err := ReserveCommandLink(link, target)
	commandLinkReservationTestHook = previousHook
	if err == nil || !strings.Contains(err.Error(), "without replacement") {
		t.Fatalf("late owner error=%v", err)
	}
	value, readErr := os.ReadFile(ownerPath)
	if readErr != nil || string(value) != "late-sentinel" {
		t.Fatalf("late owner changed: value=%q error=%v", value, readErr)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("late owner race published final link: %v", err)
	}
}

func TestCommandLinkOwnerRequiresStrictJSON(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		expected string
	}{
		{
			name: "duplicate",
			value: `{"schema_version":1,"schema_version":1,` +
				`"link_name":"sn-cli","target":"/runtime/bin/sn-cli"}`,
			expected: "duplicate field",
		},
		{
			name: "unknown",
			value: `{"schema_version":1,"link_name":"sn-cli",` +
				`"target":"/runtime/bin/sn-cli","extra":true}`,
			expected: "unknown field",
		},
		{
			name:     "null",
			value:    `{"schema_version":1,"link_name":"sn-cli","target":null}`,
			expected: "must not be null",
		},
		{
			name: "trailing",
			value: `{"schema_version":1,"link_name":"sn-cli",` +
				`"target":"/runtime/bin/sn-cli"} []`,
			expected: "after top-level value",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := canonicalTestDir(t)
			ownerPath := filepath.Join(
				parent, commandLinkOwnerName("sn-cli"),
			)
			if err := os.WriteFile(
				ownerPath, []byte(test.value), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(parent, "sn-cli")
			target := filepath.Join(t.TempDir(), "bin", "sn-cli")
			if _, err := ReserveCommandLink(link, target); err == nil ||
				!strings.Contains(err.Error(), test.expected) {
				t.Fatalf("non-strict durable owner error=%v", err)
			}
			value, err := os.ReadFile(ownerPath)
			if err != nil || string(value) != test.value {
				t.Fatalf(
					"invalid owner changed: value=%q error=%v",
					value, err,
				)
			}
			if _, err := os.Lstat(link); !os.IsNotExist(err) {
				t.Fatalf("invalid owner published final link: %v", err)
			}
		})
	}
}

func TestCommandLinkReservationRejectsInPlaceOwnerMutation(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(*CommandLinkReservation) error
	}{
		{name: "commit", run: (*CommandLinkReservation).Commit},
		{name: "release", run: (*CommandLinkReservation).Release},
	} {
		t.Run(operation.name, func(t *testing.T) {
			for _, mutation := range []string{
				"truncate", "content_rewrite", "chmod", "hardlink",
			} {
				t.Run(mutation, func(t *testing.T) {
					parent := canonicalTestDir(t)
					link := filepath.Join(parent, "sn-cli")
					target := filepath.Join(
						t.TempDir(), "bin", "sn-cli",
					)
					reservation, err := ReserveCommandLink(link, target)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						_ = reservation.Release()
					})
					ownerPath := filepath.Join(
						parent, commandLinkOwnerName("sn-cli"),
					)
					ownerBefore, err := os.Lstat(ownerPath)
					if err != nil {
						t.Fatal(err)
					}
					linkBefore, err := os.Lstat(link)
					if err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "truncate":
						err = os.Truncate(ownerPath, 0)
					case "content_rewrite":
						err = os.WriteFile(
							ownerPath,
							[]byte(
								`{"schema_version":1,`+
									`"link_name":"sn-cli",`+
									`"target":"/other/runtime/bin/sn-cli"}`+
									"\n",
							),
							0o600,
						)
					case "chmod":
						err = os.Chmod(ownerPath, 0o644)
					case "hardlink":
						err = os.Link(
							ownerPath,
							filepath.Join(parent, "owner-hardlink"),
						)
					default:
						t.Fatalf("unknown mutation %q", mutation)
					}
					if err != nil {
						t.Fatal(err)
					}
					ownerAfterMutation, err := os.Lstat(ownerPath)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(ownerBefore, ownerAfterMutation) {
						t.Fatal("test mutation replaced the owner inode")
					}

					if err := operation.run(reservation); err == nil ||
						!strings.Contains(err.Error(), "owner") {
						t.Fatalf(
							"%s accepted %s owner mutation: %v",
							operation.name, mutation, err,
						)
					}
					ownerAfter, err := os.Lstat(ownerPath)
					if err != nil {
						t.Fatalf("owner was removed: %v", err)
					}
					if !os.SameFile(ownerBefore, ownerAfter) {
						t.Fatal("owner mutation was replaced")
					}
					linkAfter, err := os.Lstat(link)
					if err != nil {
						t.Fatalf("command link was removed: %v", err)
					}
					if !os.SameFile(linkBefore, linkAfter) {
						t.Fatal("command link was replaced")
					}
					if got, err := os.Readlink(link); err != nil ||
						got != target {
						t.Fatalf(
							"command link target=%q error=%v",
							got, err,
						)
					}
				})
			}
		})
	}
}

func TestCommandLinkOwnerIgnoresPartialTempFromCrashedWriter(t *testing.T) {
	parent := canonicalTestDir(t)
	ownerName := commandLinkOwnerName("sn-cli")
	partial := filepath.Join(parent, ownerName+".pending-crashed")
	if err := os.WriteFile(partial, []byte(`{"schema_`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	reservation, err := ReserveCommandLink(link, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Commit(); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(partial)
	if err != nil || string(value) != `{"schema_` {
		t.Fatalf("partial temp changed: value=%q error=%v", value, err)
	}
}

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	value, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(value)
}
