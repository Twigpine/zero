//go:build unix

package planmode

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// readPlanFile opens path with O_NOFOLLOW so a symlink planted between the
// path resolution and the open cannot redirect the read, then reads from the
// resulting handle. A symlink final component fails open with ELOOP.
func readPlanFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if pathErr, ok := err.(*os.PathError); ok && pathErr.Err == unix.ELOOP {
			return nil, fmt.Errorf("plan file %s is a symlink; refusing to read through it", path)
		}
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}
