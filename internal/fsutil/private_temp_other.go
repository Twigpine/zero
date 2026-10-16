//go:build !linux && !darwin && !windows

package fsutil

import (
	"errors"
	"os"
)

func createPrivateFile(string) (*os.File, error) {
	return nil, errors.New("fsutil: private staging creation is unsupported on this platform")
}
func createPrivateDir(string) error {
	return errors.New("fsutil: private staging creation is unsupported on this platform")
}
