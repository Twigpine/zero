//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// snapshotRuntimeStampBound keeps the pathname form off Windows. The split it
// closes there is specific to an elevated installer racing an unelevated
// renamer; the same eager identity capture still applies.
//
// The three-state result is NOT Windows-specific, though: "read it and there was
// nothing" and "could not read it" are different facts on every platform, and
// only the first may authorize a compensating delete. A permission or I/O error
// here stops setup rather than being recorded as proven absence.
func snapshotRuntimeStampBound(root string, name string) (identity string, identified bool, prior []byte, state runtimeStampState, err error) {
	identity, identified = runtimeDirIdentity(root)
	path := filepath.Join(root, name)
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return identity, identified, nil, runtimeStampAbsent, nil
		}
		return identity, identified, nil, runtimeStampUnknown, fmt.Errorf("read the sandbox runtime stamp at %s: %w", path, readErr)
	}
	return identity, identified, data, runtimeStampPresent, nil
}
