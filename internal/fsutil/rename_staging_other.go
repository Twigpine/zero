//go:build !windows

package fsutil

import "os"

// protectStaging is a no-op on platforms whose authorization metadata is
// already copied onto the staging file by WriteFileAtomic: Unix mode bits,
// owner, and extended attributes (including POSIX ACLs) travel through
// Chmod, preserveOwner, preserveXattrs, and preserveNativeACL. Only Windows
// needs an explicit DACL transfer before publication.
func protectStaging(*os.File, string) error {
	return nil
}
