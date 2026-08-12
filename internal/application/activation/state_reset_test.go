package activation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeStateResetRejectsAncestorSymlinkSwap(t *testing.T) {
	root := canonicalTestDir(t)
	ancestor := filepath.Join(root, "ancestor")
	home := filepath.Join(ancestor, "home")
	oldAncestor := filepath.Join(root, "ancestor-before-swap")
	externalAncestor := filepath.Join(root, "external")
	externalHome := filepath.Join(externalAncestor, "home")
	for _, directory := range []string{
		filepath.Join(home, "state"),
		filepath.Join(home, "sessions"),
		filepath.Join(externalHome, "state"),
		filepath.Join(externalHome, "sessions"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	owned := filepath.Join(home, "sessions", "owned")
	external := filepath.Join(externalHome, "sessions", "sentinel")
	if err := os.WriteFile(owned, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}

	previousHook := runtimeStateResetTestHook
	runtimeStateResetTestHook = func(phase string) error {
		if phase != "roots_opened" {
			return nil
		}
		if err := os.Rename(ancestor, oldAncestor); err != nil {
			return err
		}
		return os.Symlink(externalAncestor, ancestor)
	}
	t.Cleanup(func() {
		runtimeStateResetTestHook = previousHook
		_ = os.Remove(ancestor)
		_ = os.Rename(oldAncestor, ancestor)
	})

	err := resetRuntimeState(home)
	if err == nil || !strings.Contains(err.Error(), "changed during state reset") {
		t.Fatalf("error=%v", err)
	}
	for path, expected := range map[string]string{
		filepath.Join(oldAncestor, "home", "sessions", "owned"): "owned",
		external: "external",
	} {
		value, readErr := os.ReadFile(path)
		if readErr != nil || string(value) != expected {
			t.Fatalf("path %s changed: value=%q error=%v", path, value, readErr)
		}
	}
}

func TestRuntimeStateResetNeverFollowsEntrySwapSymlink(t *testing.T) {
	home := canonicalTestDir(t)
	state := filepath.Join(home, "state")
	sessions := filepath.Join(home, "sessions")
	originalSessions := filepath.Join(home, "sessions-before-swap")
	external := canonicalTestDir(t)
	sentinel := filepath.Join(external, "sentinel")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessions, "owned"), []byte("owned"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}

	previousHook := runtimeStateResetTestHook
	runtimeStateResetTestHook = func(phase string) error {
		if phase != "before_rename:sessions" {
			return nil
		}
		if err := os.Rename(sessions, originalSessions); err != nil {
			return err
		}
		return os.Symlink(external, sessions)
	}
	t.Cleanup(func() {
		runtimeStateResetTestHook = previousHook
	})

	err := resetRuntimeState(home)
	if err == nil || !strings.Contains(err.Error(), "changed before tombstoning") {
		t.Fatalf("error=%v", err)
	}
	value, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(value) != "safe" {
		t.Fatalf("external directory changed: value=%q error=%v", value, readErr)
	}
	if value, readErr := os.ReadFile(
		filepath.Join(originalSessions, "owned"),
	); readErr != nil || string(value) != "owned" {
		t.Fatalf("pinned original changed: value=%q error=%v", value, readErr)
	}
	if _, statErr := os.Lstat(sessions); !os.IsNotExist(statErr) {
		t.Fatalf("swapped symlink was followed or retained: %v", statErr)
	}
	if _, statErr := os.Lstat(
		filepath.Join(home, ".sn-reset-sessions", runtimeStateResetPayload),
	); statErr != nil {
		t.Fatalf("mismatched payload was not quarantined: %v", statErr)
	}
}

func TestRuntimeStateResetNeverDeletesMismatchedPayloadOnRetry(t *testing.T) {
	home := canonicalTestDir(t)
	state := filepath.Join(home, "state")
	sessions := filepath.Join(home, "sessions")
	originalSessions := filepath.Join(home, "sessions-before-swap")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessions, "original"), []byte("original"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	previousHook := runtimeStateResetTestHook
	runtimeStateResetTestHook = func(phase string) error {
		if phase != "before_rename:sessions" {
			return nil
		}
		if err := os.Rename(sessions, originalSessions); err != nil {
			return err
		}
		if err := os.Mkdir(sessions, 0o700); err != nil {
			return err
		}
		return os.WriteFile(
			filepath.Join(sessions, "replacement"),
			[]byte("replacement"), 0o600,
		)
	}
	err := resetRuntimeState(home)
	runtimeStateResetTestHook = nil
	t.Cleanup(func() {
		runtimeStateResetTestHook = previousHook
	})
	if err == nil || !strings.Contains(err.Error(), "changed before tombstoning") {
		t.Fatalf("first reset error=%v", err)
	}

	err = resetRuntimeState(home)
	if err == nil ||
		!strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("retry error=%v", err)
	}
	for path, expected := range map[string]string{
		filepath.Join(originalSessions, "original"): "original",
		filepath.Join(
			home, ".sn-reset-sessions",
			runtimeStateResetPayload, "replacement",
		): "replacement",
	} {
		value, readErr := os.ReadFile(path)
		if readErr != nil || string(value) != expected {
			t.Fatalf(
				"mismatched retry changed %s: value=%q error=%v",
				path, value, readErr,
			)
		}
	}
}

func TestRuntimeStateResetRecoversInterruptedOwnedTombstone(t *testing.T) {
	home := canonicalTestDir(t)
	state := filepath.Join(home, "state")
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(
		filepath.Join(sessions, "nested"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessions, "nested", "turn.json"),
		[]byte("{}"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("simulated crash")
	previousHook := runtimeStateResetTestHook
	runtimeStateResetTestHook = func(phase string) error {
		if phase == "after_rename:sessions" {
			return injected
		}
		return nil
	}
	err := resetRuntimeState(home)
	runtimeStateResetTestHook = nil
	t.Cleanup(func() {
		runtimeStateResetTestHook = previousHook
	})
	if !errors.Is(err, injected) {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Lstat(sessions); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted reset restored sessions unexpectedly: %v", statErr)
	}
	if _, statErr := os.Lstat(
		filepath.Join(home, ".sn-reset-sessions"),
	); statErr != nil {
		t.Fatalf("interrupted reset did not retain owned tombstone: %v", statErr)
	}

	if err := resetRuntimeState(home); err != nil {
		t.Fatalf("recover interrupted reset: %v", err)
	}
	for _, path := range []string{
		sessions,
		filepath.Join(home, ".sn-reset-sessions"),
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("recovered reset retained %s: %v", path, statErr)
		}
	}
}

func TestRuntimeStateResetRecoversUncommittedTombstoneReservation(
	t *testing.T,
) {
	for _, test := range []struct {
		name   string
		marker []byte
	}{
		{name: "empty"},
		{name: "partial_marker", marker: []byte("partial")},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := canonicalTestDir(t)
			if err := os.MkdirAll(
				filepath.Join(home, "state"), 0o700,
			); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(
				filepath.Join(home, "sessions"), 0o700,
			); err != nil {
				t.Fatal(err)
			}
			tombstone := filepath.Join(home, ".sn-reset-sessions")
			if err := os.Mkdir(tombstone, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.marker != nil {
				if err := os.WriteFile(
					filepath.Join(
						tombstone, runtimeStateResetMarkerName,
					),
					test.marker, 0o600,
				); err != nil {
					t.Fatal(err)
				}
			}
			if err := resetRuntimeState(home); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(tombstone); !os.IsNotExist(err) {
				t.Fatalf("uncommitted tombstone retained: %v", err)
			}
		})
	}
}

func TestRuntimeStateResetDoesNotFollowNestedSymlink(t *testing.T) {
	home := canonicalTestDir(t)
	state := filepath.Join(home, "state")
	sessions := filepath.Join(home, "sessions")
	external := canonicalTestDir(t)
	sentinel := filepath.Join(external, "sentinel")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(sessions, "external")); err != nil {
		t.Fatal(err)
	}

	if err := resetRuntimeState(home); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(sentinel)
	if err != nil || string(value) != "safe" {
		t.Fatalf("nested symlink target changed: value=%q error=%v", value, err)
	}
}
