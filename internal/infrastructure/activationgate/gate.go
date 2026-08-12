// Package activationgate reads the filesystem barrier shared by every public
// Runtime entrypoint while an activation transaction is live or recoverable.
package activationgate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	guardFileName   = "activation.guard.json"
	journalFileName = "activation.journal.json"
)

// RequireOpen rejects operations while activation owns the Runtime home. The
// journal is itself a barrier so a crash cannot expose partially activated
// state after the guard has disappeared.
func RequireOpen(stateDir string) error {
	for _, name := range []string{guardFileName, journalFileName} {
		path := filepath.Join(stateDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("Runtime activation barrier is invalid at %s", path)
		}
		return fmt.Errorf(
			"Runtime home is undergoing activation or requires activation recovery",
		)
	}
	return nil
}
