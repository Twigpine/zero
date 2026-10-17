package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Twigpine/zero/internal/pathjail"
)

var errFileChangedDuringWrite = errors.New("file changed on disk before the write committed")

// fileWriteBeforeCommit is a deterministic test hook. Production leaves it
// nil; tests use it to replace a path after observation but before opening the
// object that will actually be mutated.
var fileWriteBeforeCommit func(path string)

// fileWriteBeforePublish is a deterministic test hook that runs after the
// replacement bytes have been staged and before the destination's identity is
// re-checked for the publish. Tests use it to install a competing writer at the
// last point before publication.
var fileWriteBeforePublish func(path string)

// fileWriteStat is a deterministic test seam for proving that the opened-file
// identity is captured before the final preimage comparison. Production uses
// the file descriptor directly.
var fileWriteStat = func(file *os.File) (os.FileInfo, error) { return file.Stat() }

// fileWriteTempWrite is a deterministic test seam for a failure while the
// replacement bytes are written to the side file, after the observed file's
// identity and preimage have been accepted. Production writes through
// io.WriteString. The failure it stands in for (a full disk, a failing
// device) is exactly the moment a truncate-then-write has already destroyed
// the observed file.
var fileWriteTempWrite = func(file *os.File, content string) error {
	_, err := io.WriteString(file, content)
	return err
}

// fileWriteRename publishes the staged side file over the destination.
// Tests replace it to exercise the in-place fallback deterministically.
var fileWriteRename = func(root *os.Root, oldName, newName string) error {
	return root.Rename(oldName, newName)
}

// fileWriteNoReplaceRename publishes a create without replacing an existing
// name. Tests replace it to exercise the hard-link fallback that filesystems
// without a no-replace rename use.
var fileWriteNoReplaceRename = publishRootedNoReplace

// commitRootedFileContents binds an overwrite to the file identity and bytes
// that the caller observed, opening every component through an already-open
// *os.Root descriptor rather than re-resolving the absolute path. Because the
// ancestor directories are traversed relative to the root handle, a parent
// swapped for a symlink that escapes the workspace between validation and this
// call is refused by the kernel instead of redirecting the write.
//
// Both publishes are atomic against a concurrent reader: the replacement bytes
// are written whole to a side file in the target's directory and the target is
// swapped wholesale. A create publishes through a kernel no-replace rename, so
// losing the create race refuses instead of replacing the raced file. An
// overwrite publishes through a rename after re-checking that the destination
// still names the observed object; that check is the last operation before the
// rename, but no supported platform offers a compare-and-swap rename, so a
// writer that replaces the name between the check and the syscall can still be
// replaced. A failure at any point before the publish removes the side file
// and leaves the observed bytes standing — the caller's file outlives a write
// that could not complete, and no partial state is ever visible at the path
// itself.
//
// A final-component symlink keeps main's behaviour: the link stays a link and
// the resolved file is what is replaced. A hard link is detached by the rename
// instead of rewritten through the shared inode. Where the platform refuses a
// rename (notably Windows with an open reader), the write falls back to the
// previous in-place publish through a freshly validated handle; that is the
// only case in which the preimage is not protected by staging.
//
// relativePath is the target expressed relative to root; absolutePath is
// retained only for the test hooks and error reporting.
//
// expectedInfo nil means the caller observed a missing path. expectedContent
// may be nil for an existing but unreadable file; that path may still be
// overwritten, but callers must omit rich before/after evidence.
func commitRootedFileContents(root *os.Root, absolutePath, relativePath string, expectedInfo os.FileInfo, expectedContent *string, content string) error {
	if fileWriteBeforeCommit != nil {
		fileWriteBeforeCommit(absolutePath)
	}

	if expectedInfo == nil {
		return commitRootedFileCreate(root, relativePath, content)
	}

	flags := os.O_WRONLY
	if expectedContent != nil {
		flags = os.O_RDWR
	}
	file, err := root.OpenFile(relativePath, flags, 0)
	if err != nil {
		return err
	}
	openedInfo, err := fileWriteStat(file)
	if err != nil {
		_ = file.Close()
		return err
	}
	if !os.SameFile(expectedInfo, openedInfo) {
		_ = file.Close()
		return errFileChangedDuringWrite
	}
	pathInfo, err := root.Stat(relativePath)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		_ = file.Close()
		return errFileChangedDuringWrite
	}
	if expectedContent != nil {
		current, readErr := io.ReadAll(file)
		if readErr != nil {
			_ = file.Close()
			return readErr
		}
		if string(current) != *expectedContent {
			_ = file.Close()
			return errFileChangedDuringWrite
		}
	}

	// A device, FIFO or socket is not a file this path should replace with a
	// regular one, and the previous in-place write is the only publish that
	// keeps its semantics. Regular files (including symlink targets) go through
	// staging and rename.
	if !openedInfo.Mode().IsRegular() {
		return writeRootedFileInPlace(root, relativePath, file, openedInfo, expectedContent, content)
	}
	mode := openedInfo.Mode().Perm()
	publishPath, err := resolveRootedFinalPath(root, relativePath)
	if err != nil {
		_ = file.Close()
		return err
	}
	if publishPath != relativePath {
		targetInfo, statErr := root.Stat(publishPath)
		if statErr != nil || !os.SameFile(openedInfo, targetInfo) {
			_ = file.Close()
			return fmt.Errorf("%w: link target changed", errFileChangedDuringWrite)
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return commitRootedFileOverwrite(root, absolutePath, relativePath, publishPath, openedInfo, mode, expectedContent, content)
}

// commitRootedFileCreate publishes a brand-new file whose absence the caller
// observed. The bytes are written whole to a private side file beside the
// target and handed to the name with a kernel no-replace rename: the create is
// either absent or the whole file, a concurrent create is refused rather than
// replaced, and a write that cannot complete leaves no file at all.
func commitRootedFileCreate(root *os.Root, relativePath, content string) error {
	tempName, err := writeRootedSideFile(root, relativePath, 0o644, true, content)
	if err != nil {
		return err
	}
	if err := publishRootedCreate(root, tempName, relativePath); err != nil {
		return errors.Join(err, removeWriteSideFile(root, tempName))
	}
	return nil
}

// publishRootedCreate hands the staged side file to a name that must not
// already exist. RenameNoReplace is the primary primitive; a hard link is the
// no-replace fallback for filesystems whose rename cannot promise it. Neither
// fallback copies into a visible target, so a reader sees only the absent name
// or the complete file.
func publishRootedCreate(root *os.Root, tempName, relativePath string) error {
	renameErr := fileWriteNoReplaceRename(root, tempName, relativePath)
	if renameErr == nil {
		return nil
	}
	if errors.Is(renameErr, os.ErrExist) {
		return fmt.Errorf("creating %s: %w", relativePath, renameErr)
	}
	if !renameNoReplaceUnsupported(renameErr) {
		return fmt.Errorf("creating %s: %w", relativePath, renameErr)
	}
	if linkErr := root.Link(tempName, relativePath); linkErr != nil {
		return fmt.Errorf("creating %s: %w", relativePath, errors.Join(renameErr, linkErr))
	}
	if cleanupErr := removeWriteSideFile(root, tempName); cleanupErr != nil {
		return fmt.Errorf("creating %s: published, but removing the staging name failed: %w", relativePath, cleanupErr)
	}
	return nil
}

// publishRootedNoReplace performs the no-replace rename through a root handle
// bound to the target's own directory. Binding the directory first is what
// keeps the raw rename inside the workspace: a parent link swapped after the
// caller resolved the path is refused when the directory is opened, and both
// names are single components below that handle.
func publishRootedNoReplace(root *os.Root, tempName, relativePath string) error {
	parent := filepath.Dir(relativePath)
	if filepath.Dir(tempName) != parent {
		return fmt.Errorf("staging file %s is not beside %s", tempName, relativePath)
	}
	directory, err := root.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return pathjail.RenameNoReplace(directory, filepath.Base(tempName), filepath.Base(relativePath))
}

// renameNoReplaceUnsupported reports whether the platform or filesystem told
// us only that it cannot perform a no-replace rename, so the no-replace hard
// link is the right fallback. A collision is not an unsupported operation and
// is handled by the caller before this is consulted.
func renameNoReplaceUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL)
}

// commitRootedFileOverwrite publishes a replacement for an object whose
// identity and preimage just passed validation: the bytes are written to a
// side file beside the target, the observed identity is re-checked against the
// path as late as the rename allows, and a rename hands the path over in one
// step. A failure while the side file is being written — ENOSPC, a failing
// device — therefore costs a removed side file, not the observed file.
//
// If the platform refuses the rename (most commonly a Windows reader holding
// the destination open), the previous in-place publish is used after reopening
// and re-validating the same object. That fallback cannot be atomic, but it is
// the only way to write a file another process has open without delete
// sharing, and it is what this tool did before staging existed.
func commitRootedFileOverwrite(root *os.Root, absolutePath, relativePath, publishPath string, openedInfo os.FileInfo, mode os.FileMode, expectedContent *string, content string) error {
	tempName, err := writeRootedSideFile(root, publishPath, mode, false, content)
	if err != nil {
		return err
	}
	if fileWriteBeforePublish != nil {
		fileWriteBeforePublish(absolutePath)
	}
	if err := verifyRootedPublishTarget(root, publishPath, openedInfo); err != nil {
		_ = removeWriteSideFile(root, tempName)
		return err
	}
	if err := fileWriteRename(root, tempName, publishPath); err != nil {
		renameErr := fmt.Errorf("writing %s: atomic replacement refused: %w", relativePath, err)
		fallbackErr := rewriteRootedFileInPlace(root, relativePath, openedInfo, expectedContent, content)
		cleanupErr := removeWriteSideFile(root, tempName)
		if fallbackErr == nil {
			if cleanupErr != nil {
				return fmt.Errorf("%w: the file was written in place, but removing the staged replacement failed: %v", renameErr, cleanupErr)
			}
			return nil
		}
		return errors.Join(renameErr, fmt.Errorf("writing %s: in-place fallback failed: %w", relativePath, fallbackErr), cleanupErr)
	}
	return nil
}

// verifyRootedPublishTarget refuses to replace anything but the exact object
// whose identity and preimage were validated. It is deliberately the last
// operation before the rename; no portable rename offers a compare-and-swap,
// so this check is the strongest condition available before an atomic replace.
func verifyRootedPublishTarget(root *os.Root, publishPath string, openedInfo os.FileInfo) error {
	info, err := root.Lstat(publishPath)
	if err != nil || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !os.SameFile(openedInfo, info) {
		return fmt.Errorf("%w: path identity changed", errFileChangedDuringWrite)
	}
	return nil
}

// resolveRootedFinalPath follows final-component symlinks inside the root and
// returns the path of the file the caller's link resolves to. Publishing over
// that path preserves the link, unlike a rename over the link itself.
func resolveRootedFinalPath(root *os.Root, relativePath string) (string, error) {
	current := relativePath
	for links := 0; links < 255; links++ {
		info, err := root.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		target, err := root.Readlink(current)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
			return "", fmt.Errorf("%w: %s is an absolute link", errFileChangedDuringWrite, current)
		}
		next := filepath.Clean(filepath.Join(filepath.Dir(current), target))
		if next == ".." || strings.HasPrefix(next, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: %s points outside the workspace", errFileChangedDuringWrite, current)
		}
		current = next
	}
	return "", fmt.Errorf("%w: more than 255 links at %s", errFileChangedDuringWrite, relativePath)
}

// writeRootedSideFile writes content to a fresh side file beside the target,
// relative to the root handle, and leaves it closed and ready to publish. The
// caller owns removing the side file when the publish that follows does not
// commit.
//
// umaskDerived selects the permission contract. A create asks the kernel for
// 0o644 and remembers what the process umask actually granted; the bytes are
// made owner-only while they are staged, then the granted bits are restored.
// An overwrite stages 0o600 and finally carries the observed file's permission
// bits over, so the published file looks like the one it replaced.
func writeRootedSideFile(root *os.Root, relativePath string, mode os.FileMode, umaskDerived bool, content string) (string, error) {
	createMode := mode.Perm()
	if !umaskDerived {
		createMode = 0o600
	}
	temp, tempName, err := pathjail.CreateTempMode(root, filepath.Dir(relativePath), "zero-write", ".tmp", createMode)
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", relativePath, err)
	}
	granted := mode.Perm()
	if umaskDerived {
		info, statErr := temp.Stat()
		if statErr != nil {
			_ = temp.Close()
			_ = removeWriteSideFile(root, tempName)
			return "", fmt.Errorf("writing %s: %w", relativePath, statErr)
		}
		granted = info.Mode().Perm()
		if chmodErr := temp.Chmod(0o600); chmodErr != nil {
			_ = temp.Close()
			_ = removeWriteSideFile(root, tempName)
			return "", fmt.Errorf("writing %s: %w", relativePath, chmodErr)
		}
	}
	writeErr := func() error {
		if err := fileWriteTempWrite(temp, content); err != nil {
			return err
		}
		return temp.Chmod(granted.Perm())
	}()
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		_ = removeWriteSideFile(root, tempName)
		return "", fmt.Errorf("writing %s: %w", relativePath, errors.Join(writeErr, closeErr))
	}
	return tempName, nil
}

// writeRootedFileInPlace publishes through an already-validated handle. It is
// the non-regular and rename-refused fallback, so it keeps the previous
// truncate-and-write behaviour and restores the observed bytes if a write
// fails and the preimage is available.
func writeRootedFileInPlace(root *os.Root, relativePath string, file *os.File, openedInfo os.FileInfo, expectedContent *string, content string) error {
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		restoreErr := restoreRootedFile(file, expectedContent)
		closeErr := file.Close()
		return errors.Join(err, restoreErr, closeErr)
	}
	if err := file.Close(); err != nil {
		return err
	}
	pathInfo, err := root.Stat(relativePath)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		return fmt.Errorf("%w: path identity changed", errFileChangedDuringWrite)
	}
	return nil
}

// rewriteRootedFileInPlace reopens the observed path, re-validates identity
// and preimage through that handle, and only then falls back to an in-place
// write. A replacement installed during the refused publish is therefore not
// touched.
func rewriteRootedFileInPlace(root *os.Root, relativePath string, expectedInfo os.FileInfo, expectedContent *string, content string) error {
	flags := os.O_WRONLY
	if expectedContent != nil {
		flags = os.O_RDWR
	}
	file, err := root.OpenFile(relativePath, flags, 0)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !os.SameFile(expectedInfo, openedInfo) {
		_ = file.Close()
		return errFileChangedDuringWrite
	}
	pathInfo, err := root.Stat(relativePath)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		_ = file.Close()
		return errFileChangedDuringWrite
	}
	if expectedContent != nil {
		current, readErr := io.ReadAll(file)
		if readErr != nil {
			_ = file.Close()
			return readErr
		}
		if string(current) != *expectedContent {
			_ = file.Close()
			return errFileChangedDuringWrite
		}
	}
	return writeRootedFileInPlace(root, relativePath, file, openedInfo, expectedContent, content)
}

// restoreRootedFile puts the observed bytes back after a failed in-place
// write. The write already truncated the file, so this is best effort: the
// caller receives both the original failure and any restore failure.
func restoreRootedFile(file *os.File, preimage *string) error {
	if preimage == nil {
		return nil
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("restoring the preimage: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("restoring the preimage: %w", err)
	}
	if _, err := io.WriteString(file, *preimage); err != nil {
		return fmt.Errorf("restoring the preimage: %w", err)
	}
	return nil
}

// removeWriteSideFile removes a staged replacement that never reached (or did
// not survive) its publish, through the root handle.
func removeWriteSideFile(root *os.Root, name string) error {
	err := root.Remove(name)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// Windows refuses to delete a read-only file. The staged file only carries
	// that bit after its published mode was applied; clear it through the root
	// and retry. The name belongs to a random side file this commit created.
	if errors.Is(err, os.ErrPermission) {
		if chmodErr := root.Chmod(name, 0o600); chmodErr == nil {
			err = root.Remove(name)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				return nil
			}
		}
	}
	return err
}
