//go:build darwin || netbsd

package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

func isXattrNotFound(err error) bool {
	return errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA)
}
