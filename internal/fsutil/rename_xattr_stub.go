//go:build !windows && !linux && !darwin && !freebsd && !netbsd

package fsutil

import "os"

func preserveXattrs(*os.File, string) error {
	return nil
}
