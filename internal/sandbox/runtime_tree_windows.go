//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// ensureRuntimeTreeDir creates the owned tail beneath a physical base, one
// component at a time, from retained handles.
//
// Every open and create goes through the NtCreateFile helpers with
// OBJ_DONT_REPARSE and FILE_OPEN_REPARSE_POINT, so a junction planted at any
// component is seen as the link it is and refused rather than descended. No
// component name is resolved twice, so there is no interval for a swap to land
// in.
//
// No chmod. On Windows os.Chmod only toggles READONLY, and against a junction it
// lands on the link rather than the target, so it bought nothing here. The
// directory's protection is the ACL that elevated setup applied, which this
// descent deliberately does not touch.
func ensureRuntimeTreeDir(physicalBase string, tail []string) error {
	parent, err := openWindowsACLDirectoryNoFollow(physicalBase)
	if err != nil {
		return fmt.Errorf("open the sandbox runtime base %s: %w", physicalBase, err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	for _, name := range tail {
		child, _, createErr := createWindowsACLChildDirectory(parent, name)
		if createErr != nil {
			existing, openErr := openWindowsACLChildDirectory(parent, name)
			if openErr != nil {
				return fmt.Errorf("prepare the sandbox runtime component %s beneath %s: %w", name, physicalBase, createErr)
			}
			child = existing
		}
		_ = windows.CloseHandle(parent)
		parent = child
	}
	return nil
}
