//go:build darwin

package observability

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(root *os.Root, oldname, newname string) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open crash directory for publication: %w", err)
	}
	defer directory.Close()
	return unix.RenameatxNp(int(directory.Fd()), oldname, int(directory.Fd()), newname, unix.RENAME_EXCL)
}
