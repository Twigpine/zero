package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/fsutil"
)

var errFileChangedDuringWrite = errors.New("file changed on disk before the write committed")

// errSymlinkDestination refuses any write whose destination is, or has become,
// a symbolic link. Observation and validation inspect the link (Lstat) and the
// publication must replace the very object it validated: replacing the symlink
// itself would strand the pointed-to file with stale bytes while destroying the
// link. Failing closed is the only consistent contract.
var errSymlinkDestination = errors.New("refusing to write through a symbolic link")

// fileWriteBeforeCommit is a deterministic test hook. Production leaves it
// nil; tests use it to replace a path after observation but before opening the
// object that will actually be mutated.
var fileWriteBeforeCommit func(path string)

// fileCreateBeforeExclusivePublish is a deterministic test hook. It runs after
// commitFileContents has observed the path missing but before the exclusive
// publication, so a test can prove a file appearing in that window is refused
// rather than overwritten.
var fileCreateBeforeExclusivePublish func(path string)

// fileWriteStat is a deterministic test seam for proving that the opened-file
// identity is captured before the final preimage comparison. Production uses
// the file descriptor directly.
var fileWriteStat = func(file *os.File) (os.FileInfo, error) { return file.Stat() }

// commitFileContents binds an overwrite to the file identity and bytes that
// the caller observed, then publishes the new content through a private
// same-directory temporary file and an atomic replacement (fsutil.WriteFileAtomic).
func commitFileContents(path string, expectedInfo os.FileInfo, expectedContent *string, content string) (string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer root.Close()
	return commitRootedFileContents(root, path, filepath.Base(path), expectedInfo, expectedContent, content)
}

// commitRootedFileContents binds an overwrite to the file identity and bytes
// that the caller observed through an already-open *os.Root descriptor, then
// publishes the new content through a private same-directory temporary file
// and an atomic replacement.
//
// Because ancestor directories are verified relative to the root handle before
// publication, a parent swapped for a symlink between validation and this call
// is refused instead of redirecting the write. When the root directory itself
// was moved after openScopedWriteRoot, publication stays inside the rooted
// directory rather than following the dissenting pathname.
//
// The returned warning string is non-empty only when the replacement already
// committed but its staging/backup cleanup failed; the caller reports success
// and surfaces the warning. A non-nil error means nothing was published.
func commitRootedFileContents(root *os.Root, absolutePath, relativePath string, expectedInfo os.FileInfo, expectedContent *string, content string) (string, error) {
	if fileWriteBeforeCommit != nil {
		fileWriteBeforeCommit(absolutePath)
	}

	if err := verifyRootedAncestors(root, relativePath); err != nil {
		return "", err
	}

	pathInfo, statErr := root.Lstat(relativePath)
	if statErr == nil {
		if pathInfo.Mode()&os.ModeSymlink != 0 {
			return "", errSymlinkDestination
		}
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}

	if expectedInfo == nil {
		if statErr == nil {
			return "", errFileChangedDuringWrite
		}
		if fileCreateBeforeExclusivePublish != nil {
			fileCreateBeforeExclusivePublish(absolutePath)
		}
		if sameRootedDirectory(root, relativePath, absolutePath) {
			return publishFileContentsExclusive(absolutePath, content)
		}
		if !sameRootedDirectory(root, ".", root.Name()) {
			return publishRootedFileContents(root, relativePath, 0o644, content)
		}
		return "", errSymlinkDestination
	}

	file, err := root.OpenFile(relativePath, fileCommitOpenFlags(expectedContent), 0)
	if err != nil {
		return "", err
	}
	openedInfo, err := fileWriteStat(file)
	if err != nil {
		_ = file.Close()
		return "", err
	}
	if !os.SameFile(expectedInfo, openedInfo) {
		_ = file.Close()
		return "", errFileChangedDuringWrite
	}
	pathInfo, err = root.Lstat(relativePath)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, pathInfo) {
		_ = file.Close()
		return "", errFileChangedDuringWrite
	}
	if expectedContent != nil {
		current, readErr := io.ReadAll(file)
		if readErr != nil {
			_ = file.Close()
			return "", readErr
		}
		if string(current) != *expectedContent {
			_ = file.Close()
			return "", errFileChangedDuringWrite
		}
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if sameRootedDirectory(root, relativePath, absolutePath) {
		return publishFileContents(absolutePath, content)
	}
	if !sameRootedDirectory(root, ".", root.Name()) {
		return publishRootedFileContents(root, relativePath, openedInfo.Mode().Perm(), content)
	}
	return "", errFileChangedDuringWrite
}

func verifyRootedAncestors(root *os.Root, relativePath string) error {
	dir := filepath.Dir(filepath.Clean(relativePath))
	if dir == "." || dir == "" {
		return nil
	}
	current := ""
	for _, segment := range strings.Split(dir, string(filepath.Separator)) {
		if segment == "" || segment == "." {
			continue
		}
		if current == "" {
			current = segment
		} else {
			current = filepath.Join(current, segment)
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errSymlinkDestination
		}
	}
	return nil
}

func sameRootedDirectory(root *os.Root, relativePath, absolutePath string) bool {
	relDir := "."
	if relativePath != "." {
		relDir = filepath.Dir(relativePath)
	}
	absDir := absolutePath
	if relativePath != "." {
		absDir = filepath.Dir(absolutePath)
	}
	rootDirInfo, err := root.Lstat(relDir)
	if err != nil || rootDirInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	absDirInfo, err := os.Lstat(absDir)
	if err != nil || absDirInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return os.SameFile(rootDirInfo, absDirInfo)
}

func publishRootedFileContents(root *os.Root, relativePath string, perm os.FileMode, content string) (string, error) {
	if perm == 0 {
		perm = 0o644
	}
	dir := filepath.Dir(relativePath)
	for attempt := 0; attempt < 64; attempt++ {
		name := fmt.Sprintf(".zero-tmp-%d-%d", os.Getpid(), attempt)
		stagingRel := name
		if dir != "." {
			stagingRel = filepath.Join(dir, name)
		}
		file, err := root.OpenFile(stagingRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		if _, err := io.WriteString(file, content); err != nil {
			_ = file.Close()
			_ = root.Remove(stagingRel)
			return "", err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = root.Remove(stagingRel)
			return "", err
		}
		if err := file.Close(); err != nil {
			_ = root.Remove(stagingRel)
			return "", err
		}
		if err := root.Rename(stagingRel, relativePath); err != nil {
			_ = root.Remove(stagingRel)
			return "", err
		}
		return "", nil
	}
	return "", errors.New("exhausted rooted staging attempts")
}

// fileCommitOpenFlags selects the descriptor used to bind an overwrite to the
// observed object. When the caller has preimage bytes to compare, the descriptor
// must be readable (and writable, to prove the same authorization an in-place
// write would have required); when it does not, write access alone is enough.
func fileCommitOpenFlags(expectedContent *string) int {
	if expectedContent != nil {
		return os.O_RDWR
	}
	return os.O_WRONLY
}

// publishFileContents performs the atomic replacement and treats a committed
// replacement whose backup cleanup failed as a successful write, returning the
// warning instead of flipping the tool status to error.
func publishFileContents(path, content string) (string, error) {
	err := fsutil.WriteFileAtomic(path, []byte(content), 0o644)
	if err == nil {
		return "", nil
	}
	var committed *fsutil.CommittedReplacementCleanupError
	if errors.As(err, &committed) {
		return "replacement committed, but backup cleanup failed", nil
	}
	return "", err
}

// publishFileContentsExclusive creates the destination with an atomic no-replace
// publication. A destination that already exists, or that appears concurrently,
// is reported as errFileChangedDuringWrite instead of being overwritten. When
// the creation already committed but removing the staging hardlink failed, the
// warning is returned instead of turning the committed create into an error.
func publishFileContentsExclusive(path, content string) (string, error) {
	err := fsutil.WriteFileAtomicExclusive(path, []byte(content), 0o644)
	if err == nil {
		return "", nil
	}
	var committed *fsutil.CommittedReplacementCleanupError
	if errors.As(err, &committed) {
		return "replacement committed, but backup cleanup failed", nil
	}
	if errors.Is(err, os.ErrExist) {
		return "", errFileChangedDuringWrite
	}
	return "", err
}
