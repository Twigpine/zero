//go:build !windows

package planmode

import "path/filepath"

// resolvePhysical returns path with every symlink component resolved. On
// non-Windows systems filepath.EvalSymlinks resolves every link type the
// platform has, so it is the whole implementation.
func resolvePhysical(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// pathIsReparsePoint is a Windows concept; nothing here reports one.
func pathIsReparsePoint(string) bool { return false }
