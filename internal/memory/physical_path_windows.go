//go:build windows

package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// resolvePhysicalPath asks the opened directory handle for its final DOS path.
// filepath.EvalSymlinks cannot resolve children beneath a directory junction on
// Windows even though ordinary opens and git's working-directory discovery can.
func resolvePhysicalPath(handle *os.Root, relative, _ string) (string, error) {
	dir, err := handle.Open(relative)
	if err != nil {
		return "", err
	}
	defer dir.Close()

	const maxPathUTF16 = 32768
	size := uint32(260)
	for size <= maxPathUTF16 {
		buffer := make([]uint16, size)
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(dir.Fd()), &buffer[0], size, 0)
		if err != nil {
			return "", err
		}
		if n < size {
			resolved := windows.UTF16ToString(buffer[:n])
			switch {
			case strings.HasPrefix(resolved, `\\?\UNC\`):
				resolved = `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`)
			case strings.HasPrefix(resolved, `\\?\`):
				resolved = strings.TrimPrefix(resolved, `\\?\`)
			}
			return filepath.Clean(resolved), nil
		}
		if n >= maxPathUTF16 {
			break
		}
		size = n + 1
	}
	return "", fmt.Errorf("resolved path exceeds the Windows path limit")
}
