//go:build !windows

package sandbox

import "golang.org/x/sys/unix"

// createRuntimeTailHandleRelative creates the owned tail through the same
// no-follow boundary the lease uses.
//
// This was an os.Mkdir loop over full pathnames, on the reasoning that the
// elevated-installer-versus-unelevated-renamer split the Windows descent closes
// does not apply here. That is true of the elevation asymmetry and false of the
// substitution: the fallback root is a predictable name under a temp directory
// every local account can write to, so another user can put a link at an owned
// component and have this loop create the rest of the tree inside it. Same
// descent, same refusal, one implementation per platform rather than one
// protected platform.
func createRuntimeTailHandleRelative(base string, tail []string) ([]windowsCreatedRuntimeDir, error) {
	created, parent, err := createRuntimeTailRetainingFD(base, tail)
	if parent >= 0 {
		_ = unix.Close(parent)
	}
	return created, err
}
