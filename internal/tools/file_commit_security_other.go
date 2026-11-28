//go:build !windows

package tools

import (
	"os"
	"path/filepath"

	"github.com/Twigpine/zero/internal/pathjail"
)

// fileSecurity is the access control an overwrite carries from the file it
// replaces. Only Windows has anything to carry here: elsewhere the permission
// bits are the access control this package preserves, and the staged file is
// given them directly.
type fileSecurity struct{}

func captureFileSecurity(*os.File) (*fileSecurity, error) { return nil, nil }

func applyFileSecurity(*os.File, *fileSecurity) error { return nil }

// createSideFile creates the side file an overwrite or a create stages its
// bytes in. The mode is the kernel's exclusive-create mode, so an overwrite's
// side file is owner-only from the moment it exists.
func createSideFile(root *os.Root, relativePath string, mode os.FileMode, _ *fileSecurity) (*os.File, string, error) {
	return pathjail.CreateTempMode(root, filepath.Dir(relativePath), "zero-write", ".tmp", mode)
}
