//go:build unix

package planmode

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPlanUnderBase opens rel under base with a true no-follow walk:
// openat(O_NOFOLLOW|O_DIRECTORY) for every intermediate component and
// openat(O_NOFOLLOW|O_RDONLY) for the final name. Unlike os.Root.Open, a
// final-component O_NOFOLLOW failure is mapped to a hard refusal rather than
// followed via checkSymlink when the target remains inside the base.
func openPlanUnderBase(base, rel, displayPath string) (*os.File, error) {
	parts, err := relComponents(rel)
	if err != nil {
		return nil, err
	}

	// O_NOFOLLOW on the base as well as on every component under it: see
	// errPlanBaseSymlink for why a link here defeats the whole walk. It applies
	// to the final component only, so a legitimately symlinked ~/.config above
	// the storage root is still fine.
	dirfd, err := openatRetry(unix.AT_FDCWD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if isNoFollowErr(err) || isSymlinkDisguisedAsENOTDIR(unix.AT_FDCWD, base, err) {
			return nil, errPlanBaseSymlink(base)
		}
		return nil, err
	}
	// Own dirfd until the final file is successfully handed to os.NewFile.
	// Intermediate replacements close the previous fd.
	defer func() {
		if dirfd >= 0 {
			_ = unix.Close(dirfd)
		}
	}()

	for i := 0; i < len(parts)-1; i++ {
		next, err := openatRetry(dirfd, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if isNoFollowErr(err) || isSymlinkDisguisedAsENOTDIR(dirfd, parts[i], err) {
				return nil, errPlanSymlink(displayPath)
			}
			return nil, err
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}

	final := parts[len(parts)-1]
	// O_NONBLOCK so a planted FIFO cannot hang the open; the regular-file
	// check below still rejects non-regular targets after open succeeds.
	fd, err := openatRetry(dirfd, final, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if isNoFollowErr(err) {
			return nil, errPlanSymlink(displayPath)
		}
		return nil, err
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		_ = unix.Close(fd)
		return nil, errPlanSymlink(displayPath)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("plan file %s is not a regular file", displayPath)
	}

	// Transfer ownership of fd to *os.File; prevent deferred Close of dirfd
	// from touching it. dirfd is still closed by the deferred cleanup.
	f := os.NewFile(uintptr(fd), displayPath)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("plan file %s: invalid file descriptor", displayPath)
	}
	return f, nil
}

func openatRetry(dirfd int, path string, flags int, mode uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, path, flags, mode)
		if err == syscall.EINTR {
			continue
		}
		return fd, err
	}
}

// isNoFollowErr reports whether err is the platform-specific errno returned
// when openat(..., O_NOFOLLOW) hits a symlink (ELOOP on most Unix, EMLINK on
// FreeBSD/Dragonfly).
func isNoFollowErr(err error) bool {
	return err == syscall.ELOOP || err == syscall.EMLINK
}

// isSymlinkDisguisedAsENOTDIR reports whether err is the ENOTDIR that
// openat(..., O_DIRECTORY|O_NOFOLLOW) returns on Linux and Darwin when name
// is actually a symlink: the kernel never dereferences the symlink to see
// the O_DIRECTORY mismatch it would otherwise report as ELOOP/EMLINK (what
// isNoFollowErr checks). A genuine non-symlink, non-directory component (a
// plain file blocking the path) also returns ENOTDIR, so this disambiguates
// with a no-follow stat instead of trusting the errno alone.
func isSymlinkDisguisedAsENOTDIR(dirfd int, name string, err error) bool {
	if err != syscall.ENOTDIR {
		return false
	}
	var st unix.Stat_t
	if statErr := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFLNK
}
