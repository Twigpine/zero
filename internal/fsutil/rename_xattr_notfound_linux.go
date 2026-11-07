//go:build linux

package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

func isXattrNotFound(err error) bool {
	return errors.Is(err, unix.ENODATA)
}
