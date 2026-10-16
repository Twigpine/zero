//go:build !windows

package credstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// acquireFileLock takes an exclusive advisory lock (flock) so a read-modify-write
// of the credential file is serialized against every other one — across
// processes AND across goroutines, since flock is held per open file
// description and two opens in one process contend exactly as two processes do.
//
// THE LOCK FILE IS SEPARATE FROM THE DATA FILE, and that is not tidiness. write
// publishes by os.Rename, which replaces the inode; a lock taken on the data
// file would be attached to an inode that the next writer has already replaced,
// so every writer would appear to hold it. The lock lives on a file nothing
// renames.
func (s *Store) acquireFileLock(exclusive bool) (func() error, error) {
	file, err := openCredentialLock(s.lockPath())
	if err != nil {
		return nil, err
	}
	// Writers take LOCK_EX; readers take LOCK_SH so they run concurrently with
	// each other but still serialize against a writer's publish (see Get).
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	// LOCK_NB plus a deadline rather than a blocking flock: a blocking wait has
	// no way to report contention, and the sibling provider-write lock in
	// internal/config fails a busy transaction rather than hanging on it. See
	// credentialLockTimeout.
	deadline := time.Now().Add(credentialLockTimeout)
	for {
		err := unix.Flock(int(file.Fd()), how|unix.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("credstore: lock: %w", err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, credentialLockBusyError(file.Name())
		}
		time.Sleep(credentialLockRetryInterval)
	}
	return func() error {
		// Close alone drops the flock, so the explicit unlock is belt-and-braces —
		// but its failure is reported rather than swallowed, because a cleanup that
		// did not complete must not be indistinguishable from one that did. The
		// callers join this into their result, so it annotates a successful write
		// instead of masking it.
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		closeErr := file.Close()
		if err := errors.Join(unlockErr, closeErr); err != nil {
			return fmt.Errorf("credstore: release lock: %w", err)
		}
		return nil
	}, nil
}

// openCredentialLock walks from the filesystem root with handle-relative,
// no-follow opens. A path check followed by os.OpenFile would leave a race in
// which an attacker replaces a checked directory with a symlink before use.
func openCredentialLock(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("credstore: lock path: %w", err)
	}
	parts := splitAbsolutePath(absolute)
	if len(parts) == 0 {
		return nil, fmt.Errorf("credstore: unsafe lock path %q", path)
	}
	directoryParts := parts[:len(parts)-1]
	lockName := parts[len(parts)-1]

	rootFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("credstore: open filesystem root: %w", err)
	}
	currentFD := rootFD
	defer func() {
		if currentFD >= 0 {
			_ = unix.Close(currentFD)
		}
	}()

	var currentStat unix.Stat_t
	if err := unix.Fstat(currentFD, &currentStat); err != nil {
		return nil, fmt.Errorf("credstore: inspect filesystem root: %w", err)
	}
	if err := validateCredentialDirectory(string(filepath.Separator), &currentStat, false); err != nil {
		return nil, err
	}

	const maxSymlinks = 40
	symlinks := 0
	for index := 0; index < len(directoryParts); {
		part := directoryParts[index]
		nextFD, openErr := unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if err := unix.Mkdirat(currentFD, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, fmt.Errorf("credstore: create lock directory %q: %w", part, err)
			}
			nextFD, openErr = unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			var linkStat unix.Stat_t
			if err := unix.Fstatat(currentFD, part, &linkStat, unix.AT_SYMLINK_NOFOLLOW); err == nil && linkStat.Mode&unix.S_IFMT == unix.S_IFLNK {
				euid := uint32(os.Geteuid())
				trustedLinkOwner := linkStat.Uid == 0 || linkStat.Uid == euid
				trustedParentOwner := currentStat.Uid == 0 || currentStat.Uid == euid
				parentIsSafe := currentStat.Mode&0o022 == 0 || currentStat.Mode&unix.S_ISVTX != 0
				if !trustedLinkOwner || !trustedParentOwner || !parentIsSafe {
					return nil, fmt.Errorf("credstore: unsafe lock path %q contains an untrusted symlink", absolute)
				}
				symlinks++
				if symlinks > maxSymlinks {
					return nil, fmt.Errorf("credstore: unsafe lock path %q has too many symlinks", absolute)
				}
				target, err := readlinkAt(currentFD, part)
				if err != nil {
					return nil, fmt.Errorf("credstore: inspect lock path symlink %q: %w", part, err)
				}
				targetParts, absoluteTarget, err := safeSymlinkParts(target)
				if err != nil {
					return nil, fmt.Errorf("credstore: unsafe lock path %q: %w", absolute, err)
				}
				directoryParts = append(targetParts, directoryParts[index+1:]...)
				index = 0
				if absoluteTarget {
					if err := unix.Close(currentFD); err != nil {
						return nil, fmt.Errorf("credstore: close traversed directory: %w", err)
					}
					currentFD, err = unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
					if err != nil {
						return nil, fmt.Errorf("credstore: reopen filesystem root: %w", err)
					}
					if err := unix.Fstat(currentFD, &currentStat); err != nil {
						return nil, fmt.Errorf("credstore: inspect filesystem root: %w", err)
					}
				}
				continue
			}
			return nil, fmt.Errorf("credstore: open lock directory %q: %w", part, openErr)
		}

		var nextStat unix.Stat_t
		if err := unix.Fstat(nextFD, &nextStat); err != nil {
			_ = unix.Close(nextFD)
			return nil, fmt.Errorf("credstore: inspect lock directory %q: %w", part, err)
		}
		finalDirectory := index == len(directoryParts)-1
		if err := validateCredentialDirectory(part, &nextStat, finalDirectory); err != nil {
			_ = unix.Close(nextFD)
			return nil, err
		}
		if err := unix.Close(currentFD); err != nil {
			_ = unix.Close(nextFD)
			return nil, fmt.Errorf("credstore: close traversed directory: %w", err)
		}
		currentFD = nextFD
		currentStat = nextStat
		index++
	}

	lockFD, err := openCredentialLockFile(currentFD, lockName)
	if err != nil {
		var stat unix.Stat_t
		if statErr := unix.Fstatat(currentFD, lockName, &stat, unix.AT_SYMLINK_NOFOLLOW); statErr == nil && stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, fmt.Errorf("credstore: unsafe lock path %q is a symlink", absolute)
		}
		return nil, fmt.Errorf("credstore: open lock: %w", err)
	}
	var lockStat unix.Stat_t
	if err := unix.Fstat(lockFD, &lockStat); err != nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("credstore: inspect lock: %w", err)
	}
	if lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Nlink != 1 || lockStat.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("credstore: unsafe lock path %q has unexpected type, link count, or owner", absolute)
	}
	if lockStat.Mode&0o077 != 0 {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("credstore: unsafe permissions on lock path %q: mode %#o", absolute, lockStat.Mode&0o777)
	}
	if err := unix.Close(currentFD); err != nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("credstore: close lock directory: %w", err)
	}
	currentFD = -1
	return os.NewFile(uintptr(lockFD), absolute), nil
}

func openCredentialLockFile(directoryFD int, name string) (int, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var fd int
		fd, err = credentialLockOpenat(directoryFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if !errors.Is(err, unix.ENOENT) {
			return fd, err
		}
		// Darwin can transiently report ENOENT when several goroutines race to
		// create the same O_CREAT|O_NOFOLLOW file. The open parent keeps retries
		// bound to the directory that was already validated above.
		time.Sleep(time.Millisecond)
	}
	return -1, err
}

var credentialLockOpenat = unix.Openat

func validateCredentialDirectory(path string, stat *unix.Stat_t, final bool) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("credstore: unsafe lock path %q is not a directory", path)
	}
	euid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != euid {
		return fmt.Errorf("credstore: unsafe lock path %q is owned by uid %d", path, stat.Uid)
	}
	if final && stat.Uid != euid {
		return fmt.Errorf("credstore: unsafe lock path %q is not owned by the current user", path)
	}
	if stat.Mode&0o022 != 0 && (stat.Uid != 0 || stat.Mode&unix.S_ISVTX == 0) {
		return fmt.Errorf("credstore: unsafe permissions on lock directory %q: mode %#o", path, stat.Mode&0o7777)
	}
	return nil
}

func splitAbsolutePath(path string) []string {
	trimmed := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	if trimmed == "" || trimmed == "." {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}

func safeSymlinkParts(target string) ([]string, bool, error) {
	absolute := filepath.IsAbs(target)
	parts := splitAbsolutePath(target)
	if !absolute {
		cleaned := filepath.Clean(target)
		if cleaned == "." {
			return nil, false, nil
		}
		parts = strings.Split(cleaned, string(filepath.Separator))
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false, fmt.Errorf("symlink target %q is not traversal-safe", target)
		}
	}
	return parts, absolute, nil
}

func readlinkAt(directoryFD int, name string) (string, error) {
	size := 256
	for {
		buffer := make([]byte, size)
		count, err := unix.Readlinkat(directoryFD, name, buffer)
		if err != nil {
			return "", err
		}
		if count < len(buffer) {
			return string(buffer[:count]), nil
		}
		size *= 2
	}
}
