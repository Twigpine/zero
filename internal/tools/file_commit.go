package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
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

// commitRootedFileContents binds an overwrite to the file identity and bytes
// that the caller observed, opening every component through an already-open
// *os.Root descriptor rather than re-resolving the absolute path. Because the
// ancestor directories are traversed relative to the root handle, a parent
// swapped for a symlink that escapes the workspace between validation and this
// call is refused by the kernel instead of redirecting the write.
//
// A create uses exclusive creation THROUGH THE ROOT. An overwrite opens the
// observed object without truncation, verifies identity/content through that
// handle, then truncates and writes the same handle. relativePath is the target
// expressed relative to root; absolutePath is retained only for the test hook
// and error reporting.
//
// expectedInfo nil means the caller observed a missing path. expectedContent
// may be nil for an existing but unreadable file; that path may still be
// overwritten, but callers must omit rich before/after evidence.
func commitRootedFileContents(root *os.Root, absolutePath, relativePath string, expectedInfo os.FileInfo, expectedContent *string, content string) error {
	if fileWriteBeforeCommit != nil {
		fileWriteBeforeCommit(absolutePath)
	}

	if expectedInfo == nil {
		file, err := root.OpenFile(relativePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		openedInfo, err := fileWriteStat(file)
		if err != nil {
			_ = file.Close()
			return err
		}
		return writeAndVerifyRootedFileIdentity(root, relativePath, file, openedInfo, content, false)
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
	return writeAndVerifyRootedFileIdentity(root, relativePath, file, openedInfo, content, true)
}

func writeAndVerifyRootedFileIdentity(root *os.Root, relativePath string, file *os.File, openedInfo os.FileInfo, content string, truncate bool) error {
	if truncate {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			_ = file.Close()
			return err
		}
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return err
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
