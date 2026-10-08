//go:build !windows

package update

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const stagingDirPrefix = ".zero-stage-"

const stagingDirectoryCreateAttempts = 100

// openStagingDirectory is a test seam for the creation-to-open race.
var openStagingDirectory = func(parentFD int, name string) (int, error) {
	return unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// createStagedBinary creates a private directory next to targetPath, binds that
// directory and its parent to descriptors, and creates the staged file through
// the directory descriptor. No later staging operation re-walks the directory
// pathname.
func createStagedBinary(targetPath string) (*stagedBinary, error) {
	parentPath := filepath.Dir(targetPath)
	parentHandle, err := os.Open(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open staging parent: %w", err)
	}
	dirName, createdStat, err := createStagingDirectory(parentHandle)
	if err != nil {
		_ = parentHandle.Close()
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	dir := filepath.Join(parentPath, dirName)
	dirHandle, err := openAndVerifyStagingDirectory(parentHandle, dirName, createdStat)
	if err != nil {
		removeStagingDirectoryIfSame(parentHandle, dirName, createdStat)
		_ = parentHandle.Close()
		return nil, fmt.Errorf("open staging directory: %w", err)
	}
	path := filepath.Join(dir, filepath.Base(targetPath))
	file, err := createStagingFileAt(dirHandle, filepath.Base(path), path)
	if err != nil {
		(&stagedBinary{
			path:         path,
			dir:          dir,
			dirHandle:    dirHandle,
			parentHandle: parentHandle,
		}).discardPaths()
		return nil, err
	}
	return &stagedBinary{
		file:         file,
		path:         path,
		dir:          dir,
		dirHandle:    dirHandle,
		parentHandle: parentHandle,
	}, nil
}

// createStagingDirectory creates and stats the directory relative to the
// already-open installation directory. POSIX has no mkdir-and-open primitive,
// so openAndVerifyStagingDirectory additionally verifies that the entry still
// names this object and is a private directory owned by this effective user.
func createStagingDirectory(parent *os.File) (string, unix.Stat_t, error) {
	for attempt := 0; attempt < stagingDirectoryCreateAttempts; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", unix.Stat_t{}, err
		}
		name := stagingDirPrefix + hex.EncodeToString(random[:])
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", unix.Stat_t{}, err
		}
		var created unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &created, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			// Nothing else knows this name yet, so a failure here would otherwise
			// leave an empty .zero-stage-* directory behind for good. Removing it
			// with rmdir semantics is safe without a prior identity check: the name
			// was minted from fresh randomness moments ago, and rmdir refuses
			// anything that is not an empty directory, so the worst an entry
			// substituted in that window can do is make this call fail.
			_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
			return "", unix.Stat_t{}, err
		}
		return name, created, nil
	}
	return "", unix.Stat_t{}, fmt.Errorf("could not allocate a unique staging directory")
}

func openAndVerifyStagingDirectory(parent *os.File, name string, createdStat unix.Stat_t) (*os.File, error) {
	fd, err := openStagingDirectory(int(parent.Fd()), name)
	if err != nil {
		return nil, err
	}
	handle := os.NewFile(uintptr(fd), name)
	var handleStat unix.Stat_t
	if err := unix.Fstat(fd, &handleStat); err != nil {
		_ = handle.Close()
		return nil, err
	}
	if handleStat.Dev != createdStat.Dev ||
		handleStat.Ino != createdStat.Ino ||
		handleStat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		handleStat.Mode&0o777 != 0o700 ||
		int(handleStat.Uid) != os.Geteuid() {
		_ = handle.Close()
		return nil, fmt.Errorf("staging directory entry was replaced before it could be bound")
	}
	return handle, nil
}

// removeStagingDirectoryIfSame removes only the empty directory entry observed
// after creation. It never follows the entry or recursively removes contents;
// a substituted or non-empty directory is preserved.
func removeStagingDirectoryIfSame(parent *os.File, name string, createdStat unix.Stat_t) {
	var current unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return
	}
	if current.Dev != createdStat.Dev || current.Ino != createdStat.Ino {
		return
	}
	_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

// createStagingFileAt creates name inside the directory dir is bound to.
// O_EXCL refuses any pre-existing entry — including a dangling symlink, which
// POSIX guarantees is not resolved — and O_NOFOLLOW refuses a symlink at the
// final component, so neither can redirect the verified bytes elsewhere.
func createStagingFileAt(dir *os.File, name string, displayPath string) (*os.File, error) {
	fd, err := unix.Openat(
		int(dir.Fd()),
		name,
		unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o755,
	)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", displayPath, err)
	}
	return os.NewFile(uintptr(fd), displayPath), nil
}

func (staged *stagedBinary) promote(targetPath string) error {
	if err := staged.file.Chmod(0o755); err != nil {
		return err
	}
	if err := staged.verifyStagedIdentity(); err != nil {
		return err
	}
	if err := unix.Renameat(
		int(staged.dirHandle.Fd()),
		filepath.Base(staged.path),
		int(staged.parentHandle.Fd()),
		filepath.Base(targetPath),
	); err != nil {
		return fmt.Errorf("rename staged binary onto %s: %w", targetPath, err)
	}
	staged.path = targetPath
	staged.promoted = true
	return nil
}

func (staged *stagedBinary) verifyStagedIdentity() error {
	var handleStat unix.Stat_t
	if err := unix.Fstat(int(staged.file.Fd()), &handleStat); err != nil {
		return fmt.Errorf("stat staged binary: %w", err)
	}
	var childStat unix.Stat_t
	if err := unix.Fstatat(
		int(staged.dirHandle.Fd()),
		filepath.Base(staged.path),
		&childStat,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("stat staged binary path %s: %w", staged.path, err)
	}
	if childStat.Ino != handleStat.Ino || childStat.Dev != handleStat.Dev {
		return fmt.Errorf("staged binary %s was replaced after it was written", staged.path)
	}
	return nil
}

// discardOpenObject has nothing to do here: discardPaths already removes the
// staged child through the descriptor bound to the private staging directory,
// which no other principal can write, so there is no pathname handoff to close.
func (staged *stagedBinary) discardOpenObject() {}

// discardPaths removes the child through the bound directory descriptor and
// removes the directory only while its original parent entry still names it.
func (staged *stagedBinary) discardPaths() {
	if staged.dirHandle != nil && !staged.promoted {
		_ = unix.Unlinkat(int(staged.dirHandle.Fd()), filepath.Base(staged.path), 0)
	}
	if staged.dirHandle != nil {
		var bound unix.Stat_t
		var current unix.Stat_t
		boundErr := unix.Fstat(int(staged.dirHandle.Fd()), &bound)
		currentErr := unix.Fstatat(
			int(staged.parentHandle.Fd()),
			filepath.Base(staged.dir),
			&current,
			unix.AT_SYMLINK_NOFOLLOW,
		)
		_ = staged.dirHandle.Close()
		if boundErr == nil &&
			currentErr == nil &&
			bound.Dev == current.Dev &&
			bound.Ino == current.Ino {
			_ = unix.Unlinkat(int(staged.parentHandle.Fd()), filepath.Base(staged.dir), unix.AT_REMOVEDIR)
		}
	}
	if staged.parentHandle != nil {
		_ = staged.parentHandle.Close()
	}
}
