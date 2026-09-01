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

	// O_NOFOLLOW on the base as well as on every component under it: see
	// errPlanBaseSymlink. MkdirAll above happily accepts a base whose final
	// component is a symlink to a directory, so without this the writer would
	// create and rename inside the link's target.
	dirfd, err := openatRetry(unix.AT_FDCWD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if isNoFollowErr(err) || isSymlinkDisguisedAsENOTDIR(unix.AT_FDCWD, base, err) {
			return errPlanBaseSymlink(base)
		}
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
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("write plan file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write plan file: %w", err)
	}

	if err := renameatRetry(dirfd, tmpName, dirfd, final); err != nil {
		return fmt.Errorf("replace plan file: %w", err)
	}
	_ = unix.Fsync(dirfd)
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
		if isSymlinkDisguisedAsENOTDIR(dirfd, name, err) {
			// Translate to ELOOP so the caller's isNoFollowErr check reaches
			// the same refusal every other platform's symlink hit gives.
			return -1, syscall.ELOOP
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

// stageContentUnderBase opens the validated dir descriptor with O_NOFOLLOW and
// creates a temporary staged plan file plus an exclusive companion lock file
// relative to that descriptor, ensuring containment cannot be bypassed by
// intermediate path swaps.
func stageContentUnderBase(dir, sessionID, content string) (string, func(), error) {
	dirfd, err := openatRetry(unix.AT_FDCWD, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open plan editor staging directory: %w", err)
	}
	defer func() {
		if dirfd >= 0 {
			_ = unix.Close(dirfd)
		}
	}()

	var st unix.Stat_t
	if err := unix.Fstat(dirfd, &st); err != nil {
		return "", nil, fmt.Errorf("stat plan editor staging directory: %w", err)
	}
	if (st.Mode & unix.S_IFMT) != unix.S_IFDIR {
		return "", nil, fmt.Errorf("plan editor staging directory is not a directory")
	}

	slug := slugify(sessionID)
	var leafName string
	var fd int = -1
	var lockFd int = -1
	for try := 0; try < 100; try++ {
		candidate := fmt.Sprintf("%s-%d-%d.md", slug, os.Getpid(), time.Now().UnixNano())
		lockCandidate := candidate + ".lock"

		cLockFd, err := openatRetry(dirfd, lockCandidate, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			continue
		}
		if err := unix.Flock(cLockFd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = unix.Close(cLockFd)
			_ = unix.Unlinkat(dirfd, lockCandidate, 0)
			continue
		}

		cFd, err := openatRetry(dirfd, candidate, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			_ = unix.Flock(cLockFd, unix.LOCK_UN)
			_ = unix.Close(cLockFd)
			_ = unix.Unlinkat(dirfd, lockCandidate, 0)
			continue
		}

		leafName = candidate
		fd = cFd
		lockFd = cLockFd
		break
	}
	if fd < 0 {
		return "", nil, fmt.Errorf("stage plan file for editor: failed to create unique temporary file")
	}

	stagedPath := filepath.Join(dir, leafName)
	lockPath := stagedPath + ".lock"

	file := os.NewFile(uintptr(fd), stagedPath)
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Flock(lockFd, unix.LOCK_UN)
		_ = unix.Close(lockFd)
		_ = os.Remove(stagedPath)
		_ = os.Remove(lockPath)
		return "", nil, fmt.Errorf("stage plan file for editor: invalid descriptor")
	}
	if _, err := file.WriteString(strings.TrimRight(content, "\n") + "\n"); err != nil {
		_ = file.Close()
		_ = unix.Flock(lockFd, unix.LOCK_UN)
		_ = unix.Close(lockFd)
		_ = os.Remove(stagedPath)
		_ = os.Remove(lockPath)
		return "", nil, fmt.Errorf("stage plan file for editor: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = unix.Flock(lockFd, unix.LOCK_UN)
		_ = unix.Close(lockFd)
		_ = os.Remove(stagedPath)
		_ = os.Remove(lockPath)
		return "", nil, fmt.Errorf("stage plan file for editor: %w", err)
	}

	cleanup := func() {
		_ = unix.Flock(lockFd, unix.LOCK_UN)
		_ = unix.Close(lockFd)
		_ = os.Remove(stagedPath)
		_ = os.Remove(lockPath)
	}
	return stagedPath, cleanup, nil
}

// tryReclaimStaleStagedFile attempts to reclaim an abandoned staged plan file.
// It verifies the filename matches the Zero staged format, opens the companion
// .lock file and attempts non-blocking exclusive flock. If the lock cannot be
// acquired (an editor is actively open), the file is preserved.
func tryReclaimStaleStagedFile(dir, leafName string) bool {
	if !strings.HasSuffix(leafName, ".md") {
		return false
	}
	dirfd, err := openatRetry(unix.AT_FDCWD, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(dirfd) }()

	lockName := leafName + ".lock"
	lockFd, err := openatRetry(dirfd, lockName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		defer func() { _ = unix.Close(lockFd) }()
		if err := unix.Flock(lockFd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return false
		}
		defer func() { _ = unix.Flock(lockFd, unix.LOCK_UN) }()
	}
	_ = unix.Unlinkat(dirfd, leafName, 0)
	_ = unix.Unlinkat(dirfd, lockName, 0)
	return true
}
