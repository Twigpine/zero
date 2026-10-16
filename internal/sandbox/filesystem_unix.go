//go:build darwin || linux

package sandbox

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// pathsShareFilesystem reports whether two paths live on the same filesystem.
//
// A path that does not exist YET is answered from its nearest existing
// ancestor: a write root the sandbox creates lands on whatever filesystem its
// parent is on, so "not created yet" is a knowable answer rather than an unknown
// one. That distinction matters because the profile carries roots for every
// platform — /private/tmp and /var/folders are macOS spellings that simply do
// not exist on Linux, and reading each of them as "cannot tell" would refuse
// every file-backed token on Linux.
//
// known is false only when a path's whole ancestor chain is uninspectable. That
// really is unknown, and an uninspectable root is not evidence of separation:
// callers reason about where a hard link COULD be created, so they must treat it
// as "could".
func pathsShareFilesystem(left, right string) (shared bool, known bool) {
	leftDevice, leftKnown := pathFilesystemID(left)
	if !leftKnown {
		return false, false
	}
	rightDevice, rightKnown := pathFilesystemID(right)
	if !rightKnown {
		return false, false
	}
	return leftDevice == rightDevice, true
}

// pathFilesystemID returns the device ID owning path, walking up to the nearest
// existing ancestor for a path that has not been created yet.
func pathFilesystemID(path string) (uint64, bool) {
	current := filepath.Clean(path)
	for {
		var stat unix.Stat_t
		if err := unix.Stat(current, &stat); err == nil {
			return uint64(stat.Dev), true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return 0, false
		}
		current = parent
	}
}

// pathHardLinkCount reports the number of directory entries that name the file's
// inode. A count above one proves an alias already exists somewhere the planner
// cannot enumerate, so callers must fail closed rather than mask one pathname.
//
// A missing file returns fs.ErrNotExist and count 0: an absent token has no
// inode to alias, and the pathname reservation that survives rotation is the
// lexical rule's job, not this one's. Any other inspection failure is returned
// as an error so the caller fails closed instead of reading it as "no alias".
// A non-regular file reports 0 with no error: link counts on a directory or a
// symlink do not describe an alias for the token's contents.
func pathHardLinkCount(path string) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, nil
	}
	return uint64(stat.Nlink), nil
}
