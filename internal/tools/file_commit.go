package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Twigpine/zero/internal/pathjail"
)

var errFileChangedDuringWrite = errors.New("file changed on disk before the write committed")

// fileWriteBeforeCommit is a deterministic test hook. Production leaves it
// nil; tests use it to replace a path after observation but before opening the
// object that will actually be mutated.
var fileWriteBeforeCommit func(path string)

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

// commitRootedFileContents binds an overwrite to the file identity and bytes
// that the caller observed, opening every component through an already-open
// *os.Root descriptor rather than re-resolving the absolute path. Because the
// ancestor directories are traversed relative to the root handle, a parent
// swapped for a symlink that escapes the workspace between validation and this
// call is refused by the kernel instead of redirecting the write.
//
// Both publishes are atomic against a concurrent reader: the replacement bytes
// are written whole to a side file in the target's directory and the target is
// swapped wholesale. A create reserves the name with the exclusive create it
// has always honored and hands it the finished bytes in one rename, so losing
// that reservation refuses instead of replacing the raced file; an overwrite
// publishes through a rename, which hands the path over in one step. A failure
// at any point before the publish removes the side file and leaves the
// observed bytes standing — the caller's file outlives a write that could not
// complete, and no partial state is ever visible at the path itself.
//
// relativePath is the target expressed relative to root; absolutePath is
// retained only for the test hook and error reporting.
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
	mode := openedInfo.Mode().Perm()
	if err := file.Close(); err != nil {
		return err
	}
	return commitRootedFileOverwrite(root, relativePath, openedInfo, mode, content)
}

// commitRootedFileCreate publishes a brand-new file whose absence the caller
// observed. Nothing the caller's request produces is ever written to the
// target's bytes in pieces: the name is reserved first with the exclusive
// create this path has always honored — empty, refusing to replace a file the
// race between observation and publication created, and receiving from the
// kernel the permission bits the mode and the process umask together grant —
// and the finished bytes are staged on a private side file beside the target
// that the reserved name is then handed in one rename. A concurrent reader
// sees the absent file, the empty reservation, or the whole file.
func commitRootedFileCreate(root *os.Root, relativePath, content string) error {
	tempName, err := writeRootedSideFile(root, relativePath, 0o600, content)
	if err != nil {
		return err
	}
	reserved, err := root.OpenFile(relativePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		_ = removeWriteSideFile(root, tempName)
		return fmt.Errorf("creating %s: %w", relativePath, err)
	}
	reservedInfo, err := reserved.Stat()
	if err != nil {
		_ = reserved.Close()
		// The reservation's identity could not be captured, so the reserved
		// name cannot be proved to be this call's own empty file and is left
		// alone; the failure is reported either way and the side file is
		// always removed.
		_ = removeWriteSideFile(root, tempName)
		return fmt.Errorf("creating %s: %w", relativePath, err)
	}
	discard := func(cause error) error {
		_ = reserved.Close()
		joined := []error{cause, removeWriteSideFile(root, tempName)}
		// The reservation is this call's artifact only while the name still
		// resolves to the file it opened; a name carrying something else
		// belongs to the hand that swapped it in and is not touched.
		pathInfo, statErr := root.Stat(relativePath)
		if statErr == nil && os.SameFile(reservedInfo, pathInfo) {
			if cleanupErr := removeWriteSideFile(root, relativePath); cleanupErr != nil {
				joined = append(joined, fmt.Errorf("removing the reserved empty file: %w", cleanupErr))
			}
		}
		return fmt.Errorf("creating %s: %w", relativePath, errors.Join(joined...))
	}
	if err := reserved.Close(); err != nil {
		return discard(err)
	}
	// The reserved create just told us the permissions the kernel granted the
	// name for this process; publishing them, instead of assigning the mode
	// outright, keeps a process umask as effective as it was for the exclusive
	// create this step replaces.
	if err := root.Chmod(tempName, reservedInfo.Mode().Perm()); err != nil {
		return discard(err)
	}
	pathInfo, err := root.Stat(relativePath)
	if err != nil || !os.SameFile(reservedInfo, pathInfo) {
		return discard(errFileChangedDuringWrite)
	}
	if err := root.Rename(tempName, relativePath); err != nil {
		return discard(err)
	}
	return nil
}

// commitRootedFileOverwrite publishes a replacement for an object whose
// identity and preimage just passed validation: the bytes are written to a
// side file beside the target, the observed identity is re-checked against the
// path as late as the rename allows, and a rename that runs through the root
// handle hands the path over in one step. A failure while the side file is
// being written — ENOSPC, a failing device — therefore costs a removed side
// file, not the observed file. The publish carries the observed file's
// permission bits over so the published file looks like the file it replaced,
// and the rename detaches this name from an inode shared by hard-link
// siblings instead of truncating the bytes out from under them.
func commitRootedFileOverwrite(root *os.Root, relativePath string, openedInfo os.FileInfo, mode os.FileMode, content string) error {
	tempName, err := writeRootedSideFile(root, relativePath, mode, content)
	if err != nil {
		return err
	}
	pathInfo, err := root.Stat(relativePath)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		_ = removeWriteSideFile(root, tempName)
		return fmt.Errorf("%w: path identity changed", errFileChangedDuringWrite)
	}
	if err := root.Rename(tempName, relativePath); err != nil {
		_ = removeWriteSideFile(root, tempName)
		return fmt.Errorf("writing %s: %w", relativePath, err)
	}
	return nil
}

// writeRootedSideFile writes content to a fresh side file beside the target,
// relative to the root handle, and leaves it closed and ready to publish. The
// caller owns removing the side file when the publish that follows does not
// commit. mode carries the permission bits the published file should have,
// because the side file is created with its own restrictive mode so a planted
// link at a predictable name would carry nothing usable.
func writeRootedSideFile(root *os.Root, relativePath string, mode os.FileMode, content string) (string, error) {
	temp, tempName, err := pathjail.CreateTemp(root, filepath.Dir(relativePath), "zero-write", ".tmp")
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", relativePath, err)
	}
	writeErr := func() error {
		if err := fileWriteTempWrite(temp, content); err != nil {
			_ = temp.Close()
			return err
		}
		if err := temp.Chmod(mode.Perm()); err != nil {
			_ = temp.Close()
			return err
		}
		return temp.Close()
	}()
	if writeErr != nil {
		_ = removeWriteSideFile(root, tempName)
		return "", fmt.Errorf("writing %s: %w", relativePath, writeErr)
	}
	return tempName, nil
}

// removeWriteSideFile removes a side file that never reached (or outlived) its
// publish, through the root handle. It shares the implementation with the
// structured patch side files.
func removeWriteSideFile(root *os.Root, name string) error {
	return removeStructuredPatchTemp(root, name)
}
