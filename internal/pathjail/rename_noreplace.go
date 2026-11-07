package pathjail

import (
	"fmt"
	"os"
	"path/filepath"
)

// RenameNoReplace renames oldname to newname within the directory bound by
// root, and refuses to replace an existing newname with the same atomic
// guarantee the kernel gives a create's exclusive open. A caller that loses
// the race receives fs.ErrExist and nothing at newname changes.
//
// Both names must be single path components: they are resolved relative to
// root's directory handle, so a separator (or an ancestor link) must not be
// able to redirect the syscall outside the checked root. Callers with a path
// in a subdirectory open that directory with Root.OpenRoot first.
//
// Platforms whose kernel cannot publish without replacing fall back to
// ErrUnsupported, which lets the caller use a no-replace hard link instead.
func RenameNoReplace(root *os.Root, oldname, newname string) error {
	for _, name := range []string{oldname, newname} {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return fmt.Errorf("%w: rename name %q must be a single path component", ErrEscapes, name)
		}
	}
	return renameNoReplace(root, oldname, newname)
}
