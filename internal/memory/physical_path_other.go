//go:build !windows

package memory

import (
	"os"
	"path/filepath"
)

func resolvePhysicalPath(_ *os.Root, _ string, path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
