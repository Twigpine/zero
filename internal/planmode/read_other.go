//go:build !unix

package planmode

import (
	"fmt"
	"os"
)

// readPlanFile refuses a symlinked plan path via Lstat, then reads by name.
// O_NOFOLLOW is unavailable outside Unix; the Lstat check is the best portable
// fallback (directory is 0700, so the residual TOCTOU window is limited).
func readPlanFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("plan file %s is a symlink; refusing to read through it", path)
	}
	return os.ReadFile(path)
}
