//go:build !windows

package sandbox

import "os"

// createRuntimeDirIdentified keeps the pathname form off Windows, where the
// elevated-installer-versus-unelevated-renamer split this closes does not apply.
func createRuntimeDirIdentified(path string) (string, bool, error) {
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", false, err
	}
	identity, ok := runtimeIdentityAfterCreate(path)
	return identity, ok, nil
}
