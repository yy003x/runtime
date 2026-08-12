//go:build !darwin && !linux

package activation

import "fmt"

func renameAtNoReplace(
	_ int,
	_ string,
	_ int,
	_ string,
) error {
	return fmt.Errorf(
		"atomic no-replace command link owner publication is unsupported on this platform",
	)
}
