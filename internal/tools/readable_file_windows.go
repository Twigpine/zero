//go:build windows

package tools

import "os"

func openReadableFile(path string) (*os.File, error) {
	return os.Open(path)
}
