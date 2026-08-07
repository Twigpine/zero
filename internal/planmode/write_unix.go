//go:build unix

package planmode

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// writePlanUnderBase creates missing intermediate directories under base with
// mkdirat and openat(O_NOFOLLOW|O_DIRECTORY), then writes content into a
// temporary sibling of the final name and renameat's it into place. Every
// component is opened relative to the previous handle with O_NOFOLLOW, so an
// intermediate symlink swap cannot redirect create/rename outside base.
func writePlanUnderBase(base, rel, displayPath, content string) error {
	parts, err := relComponents(rel)
	if err != nil {
		return err
	}

	dirfd, err := openatRetry(unix.AT_FDCWD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	defer func() {
		if dirfd >= 0 {
			_ = unix.Close(dirfd)
		}
	}()

	// Ensure every intermediate component exists as a real directory and is
	// not a symlink. Create missing components with mkdirat (which does not
	// follow a final-component symlink on the create itself); refuse EEXIST
	// targets that are not plain directories by retrying open with O_NOFOLLOW.
	for i := 0; i < len(parts)-1; i++ {
		next, err := ensureDirNoFollow(dirfd, parts[i])
		if err != nil {
			if isNoFollowErr(err) {
				return errPlanSymlinkWrite(displayPath)
			}
			return fmt.Errorf("create plan directory: %w", err)
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}

	// Owner-only on the immediate parent directory. fchmod acts on the open
	// handle so a rename race cannot point chmod at a different path.
	if err := unix.Fchmod(dirfd, 0o700); err != nil {
		return fmt.Errorf("restrict plan directory permissions: %w", err)
	}

	final := parts[len(parts)-1]
	// Refuse a final-component symlink: rename would replace the name itself
	// on Unix, but the durable plan contract is a plain file, not a link.
	if err := refuseSymlinkAt(dirfd, final, displayPath); err != nil {
		return err
	}

	tmpName := planTempName(final)
	fd, err := openatRetry(dirfd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if isNoFollowErr(err) {
			return errPlanSymlinkWrite(displayPath)
		}
		return fmt.Errorf("write plan file: %w", err)
	}

	// written gates cleanup: on failure close the raw fd (if still ours) and
	// unlink the temp leaf. os.NewFile takes ownership of fd, so after a
	// successful handoff only Unlinkat remains our job.
	written := false
	defer func() {
		if !written {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			_ = unix.Unlinkat(dirfd, tmpName, 0)
		}
	}()

	// Stream content through the fd via os.File so short writes are handled.
	file := os.NewFile(uintptr(fd), displayPath+" (tmp)")
	if file == nil {
		return fmt.Errorf("write plan file: invalid file descriptor")
	}
	fd = -1
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write plan file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write plan file: %w", err)
	}

	if err := renameatRetry(dirfd, tmpName, dirfd, final); err != nil {
		return fmt.Errorf("replace plan file: %w", err)
	}
	written = true
	return nil
}

// ensureDirNoFollow opens name under dirfd as a directory without following
// symlinks. If it is missing, mkdirat creates it, then openat is retried.
// Concurrent creators are handled by treating EEXIST as a successful create
// and reopening.
func ensureDirNoFollow(dirfd int, name string) (int, error) {
	for try := 0; try < 2; try++ {
		next, err := openatRetry(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == nil {
			return next, nil
		}
		if isNoFollowErr(err) {
			return -1, err
		}
		if err != syscall.ENOENT && !os.IsNotExist(err) {
			// EEXIST without open succeeding means a non-directory is present.
			return -1, err
		}
		if mkdirErr := unix.Mkdirat(dirfd, name, 0o700); mkdirErr != nil && mkdirErr != syscall.EEXIST {
			return -1, mkdirErr
		}
	}
	return -1, fmt.Errorf("create plan directory %s: exhausted retries", name)
}

// refuseSymlinkAt fails when name under dirfd is a symlink. Missing names are
// fine (the subsequent O_EXCL create will introduce the file).
func refuseSymlinkAt(dirfd int, name, displayPath string) error {
	var st unix.Stat_t
	err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if err == syscall.ENOENT || os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errPlanSymlinkWrite(displayPath)
	}
	return nil
}

func renameatRetry(olddirfd int, oldpath string, newdirfd int, newpath string) error {
	for {
		err := unix.Renameat(olddirfd, oldpath, newdirfd, newpath)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}
