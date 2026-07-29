package activation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yy003x/runtime/internal/layout"
)

// resetRuntimeState removes only the incompatible Session and durable Run
// state owned by Runtime. It is intentionally idempotent because a committed
// activation may need to resume this step from its durable journal.
func resetRuntimeState(target string) error {
	paths, err := runtimeStateResetPaths(target)
	if err != nil {
		return err
	}
	if err := validateRuntimeStateResetPaths(paths); err != nil {
		return err
	}
	for _, directory := range []string{
		paths.SessionsDir,
		filepath.Join(paths.StateDir, "session-locks"),
		filepath.Join(paths.StateDir, "session-invocations"),
	} {
		if err := removeRuntimeStateDirectory(directory); err != nil {
			return err
		}
	}
	for _, path := range []string{
		paths.RunDBFile + "-wal",
		paths.RunDBFile + "-shm",
		paths.RunDBFile + "-journal",
		paths.RunDBFile,
	} {
		if err := removeRuntimeStateFile(path); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeStateReset(target string) error {
	paths, err := runtimeStateResetPaths(target)
	if err != nil {
		return err
	}
	return validateRuntimeStateResetPaths(paths)
}

func runtimeStateResetPaths(target string) (layout.Paths, error) {
	paths, err := layout.FromHome(target)
	if err != nil {
		return layout.Paths{}, err
	}
	if paths.Home != target {
		return layout.Paths{}, fmt.Errorf(
			"runtime state reset target is not canonical",
		)
	}
	return paths, nil
}

func validateRuntimeStateResetPaths(paths layout.Paths) error {
	for _, directory := range []string{
		paths.SessionsDir,
		filepath.Join(paths.StateDir, "session-locks"),
		filepath.Join(paths.StateDir, "session-invocations"),
	} {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf(
				"runtime state path must be a directory, not a symlink: %s",
				directory,
			)
		}
	}
	for _, path := range []string{
		paths.RunDBFile + "-wal",
		paths.RunDBFile + "-shm",
		paths.RunDBFile + "-journal",
		paths.RunDBFile,
	} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf(
				"runtime state path must be a regular file, not a symlink: %s",
				path,
			)
		}
	}
	return nil
}

func removeRuntimeStateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf(
			"runtime state path must be a directory, not a symlink: %s",
			path,
		)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove runtime state directory %s: %w", path, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("persist runtime state directory removal: %w", err)
	}
	return nil
}

func removeRuntimeStateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf(
			"runtime state path must be a regular file, not a symlink: %s",
			path,
		)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove runtime state file %s: %w", path, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("persist runtime state file removal: %w", err)
	}
	return nil
}
