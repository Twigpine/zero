//go:build linux

package pathjail

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(root *os.Root, oldname, newname string) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open directory for rename: %w", err)
	}
	defer directory.Close()
	if err := unix.Renameat2(int(directory.Fd()), oldname, int(directory.Fd()), newname, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("rename %s to %s: %w", oldname, newname, err)
	}
	return nil
}
