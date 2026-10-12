package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Gitlawb/zero/internal/fsutil"
)

const (
	statusTempPrefix  = ".daemon-status-"
	statusTempPattern = statusTempPrefix + "*"
)

func openStatusRoot(path string) (*os.Root, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open status directory: %w", err)
	}
	if err := validateStatusRoot(root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// writeStatusFileAtomicallyRoot publishes through a caller-owned, already
// bound directory root. The returned boolean crosses the commit boundary: once
// true, err is a durability/cleanup warning and the complete document is live.
func writeStatusFileAtomicallyRoot(
	root *os.Root,
	statusName string,
	data []byte,
	perm os.FileMode,
	beforeReplace func(),
	replace func(root *os.Root, src, dst string) error,
	syncParent func(root *os.Root) error,
) (committed bool, returnErr error) {
	if err := validateStatusRoot(root); err != nil {
		return false, err
	}

	temp, tempName, err := createStatusTemp(root, perm)
	if err != nil {
		return false, err
	}
	closed := false
	defer func() {
		if !closed {
			if err := temp.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close temporary status file during cleanup: %w", err))
			}
			closed = true
		}
		if err := root.Remove(tempName); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary status file: %w", err))
		}
	}()

	if err := temp.Chmod(perm); err != nil {
		return false, fmt.Errorf("set temporary status file permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return false, fmt.Errorf("write temporary status file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return false, fmt.Errorf("sync temporary status file: %w", err)
	}
	closeErr := temp.Close()
	closed = true
	if closeErr != nil {
		return false, fmt.Errorf("close temporary status file: %w", closeErr)
	}
	if beforeReplace != nil {
		beforeReplace()
	}
	if err := prepareStatusReplacement(root, tempName, statusName); err != nil {
		return false, fmt.Errorf("prepare status replacement: %w", err)
	}

	rename := root.Rename
	if replace != nil {
		rename = func(src, dst string) error { return replace(root, src, dst) }
	}
	// On Windows an existing reader that omitted FILE_SHARE_DELETE can block
	// this replacement even though an in-place write would succeed. Keep the
	// atomic publication boundary: RenameWithRetry bounds transient sharing
	// failures to ten short attempts, and a persistent failure leaves the old
	// status document intact instead of exposing a partially written one.
	var committedWarning error
	if err := fsutil.RenameWithRetry(tempName, statusName, rename); err != nil {
		var committedReplacement *fsutil.CommittedReplacementCleanupError
		if !errors.As(err, &committedReplacement) {
			return false, fmt.Errorf("replace status file: %w", err)
		}
		committedWarning = fmt.Errorf("clean up replaced status file: %w", err)
	}
	committed = true
	if syncParent == nil {
		syncParent = syncStatusRoot
	}
	if err := syncParent(root); err != nil {
		committedWarning = errors.Join(committedWarning, fmt.Errorf("sync status directory: %w", err))
	}
	if committedWarning != nil {
		return committed, committedWarning
	}
	return committed, nil
}

func validateStatusRoot(root *os.Root) error {
	info, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect status directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("status directory is not a directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("status directory permissions are %04o, want no group/other write access", info.Mode().Perm())
	}
	if err := checkStatusDirOwner(root, info); err != nil {
		return err
	}
	return nil
}

func createStatusTemp(root *os.Root, perm os.FileMode) (*os.File, string, error) {
	for range 100 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("generate temporary status file name: %w", err)
		}
		name := statusTempPrefix + hex.EncodeToString(suffix[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return file, name, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, "", fmt.Errorf("create temporary status file: %w", err)
	}
	return nil, "", fmt.Errorf("create temporary status file: exhausted unique names")
}

// syncStatusRoot makes the replacement directory entry durable through the
// same bound root used for creation and replacement. Windows directory sync is
// best-effort because Go cannot fsync a directory there.
func syncStatusRoot(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
