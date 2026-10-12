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

// commitFileContents binds an overwrite to the file identity and bytes that
// the caller observed. A create uses exclusive creation. An overwrite opens the
// observed object without truncation, verifies identity/content through that
// handle, then truncates and writes the same handle. A path replacement before
// or during commit therefore fails instead of publishing stale rich evidence.
//
// expectedInfo nil means the caller observed a missing path. expectedContent
// may be nil for an existing but unreadable file; that path may still be
// overwritten, but callers must omit rich before/after evidence.
func commitFileContents(path string, expectedInfo os.FileInfo, expectedContent *string, content string) error {
	if fileWriteBeforeCommit != nil {
		fileWriteBeforeCommit(path)
	}

	if expectedInfo == nil {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		openedInfo, err := fileWriteStat(file)
		if err != nil {
			_ = file.Close()
			return err
		}
		return writeAndVerifyFileIdentity(path, file, openedInfo, content, false)
	}

	flags := os.O_WRONLY
	if expectedContent != nil {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0)
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
	pathInfo, err := os.Stat(path)
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
	return writeAndVerifyFileIdentity(path, file, openedInfo, content, true)
}

func writeAndVerifyFileIdentity(path string, file *os.File, openedInfo os.FileInfo, content string, truncate bool) error {
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
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		return fmt.Errorf("%w: path identity changed", errFileChangedDuringWrite)
	}
	return nil
}
