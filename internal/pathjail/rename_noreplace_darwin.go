//go:build darwin

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
	if err := unix.RenameatxNp(int(directory.Fd()), oldname, int(directory.Fd()), newname, unix.RENAME_EXCL); err != nil {
		return fmt.Errorf("rename %s to %s: %w", oldname, newname, err)
	}
	return nil
}
