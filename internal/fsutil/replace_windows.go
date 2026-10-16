//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// replaceFileFlags is deliberately ZERO. Every REPLACEFILE_* flag ReplaceFileW
// accepts either defeats the reason this function exists or does nothing:
//
//   - REPLACEFILE_IGNORE_MERGE_ERRORS (0x2) and REPLACEFILE_IGNORE_ACL_ERRORS
//     (0x4). Microsoft documents BOTH with the same consequence: "if you specify
//     this flag and do not have WRITE_DAC access, the function succeeds but the
//     ACLs are not preserved." A silent success that publishes the temporary
//     file's inherited directory DACL over an explicitly restricted specialist —
//     exposing its system prompt — is precisely the failure this function exists
//     to prevent, so a merge failure MUST surface as an error and leave the
//     destination untouched. Passing 0x2 while omitting only 0x4 buys nothing:
//     ACL merging is part of the metadata merge 0x2 covers.
//   - REPLACEFILE_WRITE_THROUGH (0x1) is documented as "This value is not
//     supported", so it cannot be relied on to flush anything.
const replaceFileFlags = 0

const replaceBackupPattern = ".zero-replace-*.backup"

const (
	// Returned when the volume or redirector cannot provide ReplaceFileW's
	// DACL-preserving semantics. Existing destinations must fail closed.
	errorInvalidFunction = syscall.Errno(1)
	errorNotSupported    = syscall.Errno(50)

	// Partial-failure codes: ReplaceFileW got far enough to move or delete
	// something, so the on-disk state needs repair rather than a bare error. See
	// recoverPartialReplace.
	errorUnableToRemoveReplaced   = syscall.Errno(1175)
	errorUnableToMoveReplacement  = syscall.Errno(1176)
	errorUnableToMoveReplacement2 = syscall.Errno(1177)
)

var (
	replaceKernel32       = syscall.NewLazyDLL("kernel32.dll")
	replaceProcReplaceFil = replaceKernel32.NewProc("ReplaceFileW")
	moveFileExProc        = replaceKernel32.NewProc("MoveFileExW")
)

// publishExclusive publishes src as a new dst without replacing anything. With
// no MOVEFILE_REPLACE_EXISTING flag, MoveFileExW fails when dst exists (with
// ERROR_ALREADY_EXISTS or ERROR_FILE_EXISTS, which syscall.Errno maps to
// os.ErrExist), so a destination that appeared concurrently is refused rather
// than overwritten. MoveFileExW removes src on success, so the caller's
// deferred temporary cleanup becomes a no-op.
func publishExclusive(src, dst string) error {
	source, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileExProc.Call(
		uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(target)),
		0,
	)
	if result != 0 {
		return nil
	}
	if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
		return fmt.Errorf("create %s: MoveFileExW failed", dst)
	}
	if errors.Is(callErr, os.ErrExist) {
		return &os.PathError{Op: "create", Path: dst, Err: os.ErrExist}
	}
	return callErr
}

// replaceExisting publishes src over dst with ReplaceFileW rather than
// MoveFileEx (what os.Rename uses) to preserve destination metadata. The
// replacement is a freshly created temporary file, so it carries the directory's
// inherited DACL. Renaming it over the destination would therefore replace the
// destination's ACL - silently widening access to a file that had been restricted
// explicitly (os.File.Chmod cannot express that on Windows; Go only maps the
// owner-write bit). ReplaceFileW carries the replaced file's DACL and selected
// metadata over to the replacement instead.
//
// ReplaceFileW combines multiple filesystem steps and is not observer-atomic: an
// external reader can briefly see dst absent even when replacement succeeds.
// Callers that cannot tolerate that window must synchronize their own readers.
//
// No REPLACEFILE_* flag is passed at all — see replaceFileFlags for why each one
// would either silently lose the descriptor this function exists to preserve or
// do nothing. A merge failure therefore surfaces as an error, leaving the
// destination untouched and the caller free to clean up its temporary file,
// except in the partial-failure states recoverPartialReplace repairs.
func replaceExisting(src, dst string) error {
	return replaceExistingWith(src, dst, callReplaceFile, nil)
}

func replaceExistingWith(src, dst string, replace func(string, string, string) error, restore func(string, string) error) error {
	return replaceExistingWithCleanup(src, dst, replace, restore, removeReplaceBackup)
}

func replaceExistingWithCleanup(src, dst string, replace func(string, string, string) error, restore func(string, string) error, removeBackup func(string) error) error {
	info, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing to replace and no descriptor to preserve.
			return os.Rename(src, dst)
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace symlink destination: %s", dst)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrNonRegularDestination, dst)
	}

	backup, err := prepareReplaceBackup(dst)
	if err != nil {
		return fmt.Errorf("prepare replacement backup: %w", err)
	}
	callErr := replace(dst, src, backup)
	if callErr == nil {
		if err := removeBackup(backup); err != nil {
			return &CommittedReplacementCleanupError{BackupPath: backup, Cause: err}
		}
		return nil
	}
	if errors.Is(callErr, errorInvalidFunction) || errors.Is(callErr, errorNotSupported) {
		callErr = fmt.Errorf("DACL-preserving replacement is not supported for %s: %w", dst, callErr)
	}
	return recoverPartialReplace(callErr, dst, backup, restore)
}

func callReplaceFile(replacedPath, replacementPath, backupPath string) error {
	replaced, err := syscall.UTF16PtrFromString(replacedPath)
	if err != nil {
		return err
	}
	replacement, err := syscall.UTF16PtrFromString(replacementPath)
	if err != nil {
		return err
	}
	backup, err := syscall.UTF16PtrFromString(backupPath)
	if err != nil {
		return err
	}
	result, _, callErr := replaceProcReplaceFil.Call(
		uintptr(unsafe.Pointer(replaced)),
		uintptr(unsafe.Pointer(replacement)),
		uintptr(unsafe.Pointer(backup)),
		uintptr(replaceFileFlags),
		0,
		0,
	)
	if result != 0 {
		return nil
	}
	if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
		return fmt.Errorf("replace %s: ReplaceFileW failed", replacedPath)
	}
	return callErr
}

func prepareReplaceBackup(dst string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(dst), replaceBackupPattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		cleanupErr := removeReplaceBackup(path)
		return "", fmt.Errorf("close backup placeholder %s: %w (cleanup error: %v)", path, err, cleanupErr)
	}
	if err := removeReplaceBackup(path); err != nil {
		return "", fmt.Errorf("release backup path %s: %w", path, err)
	}
	return path, nil
}

func removeReplaceBackup(path string) error {
	var err error
	for i := 0; i < 10; i++ {
		err = os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if os.IsPermission(err) {
			// ReplaceFileW can leave the original's read-only attribute on the
			// backup. Chmod clears that bit on Windows without changing its DACL.
			_ = os.Chmod(path, 0o600)
		}
		if os.IsPermission(err) || isWindowsSharingOrLockViolation(err) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}
	return err
}

func cleanupReplaceBackup(callErr error, backup string) error {
	if err := removeReplaceBackup(backup); err != nil {
		return fmt.Errorf("%w (backup %s could not be removed: %v)", callErr, backup, err)
	}
	return callErr
}

// recoverPartialReplace restores the original after ReplaceFileW reports a
// partial failure, then returns the original error. Supplying a managed backup
// changes the dangerous failure states documented by Microsoft:
//
//   - ERROR_UNABLE_TO_MOVE_REPLACEMENT (1176) leaves both files under their
//     original names, so the failed write can discard src normally.
//   - ERROR_UNABLE_TO_MOVE_REPLACEMENT_2 (1177) leaves src in place and moves the
//     original destination to backup. Moving backup back to dst rolls the failed
//     overwrite back without losing the original content or its DACL.
//
// The replacement is left alone either way — nothing here deletes what it was
// handed — but the errors below deliberately say nothing about where it ended up,
// which is also why its path is not a parameter. Whether it still exists when the
// error surfaces is the caller's business: the specialist writer, the only caller
// today, removes its temporary file in a deferred cleanup, so an error promising
// a replacement "remains at" that path would send an operator looking for a file
// that was already gone. What these errors do describe is what this function
// itself left on disk — the state of dst, and of the managed backup holding the
// original.
func recoverPartialReplace(callErr error, dst, backup string, restore func(string, string) error) error {
	if !errors.Is(callErr, errorUnableToMoveReplacement2) {
		// For 1175, 1176, unsupported volumes, and ordinary failures, Windows
		// documents that dst remains at its original name. Remove any redundant
		// backup only after confirming that the destination still exists.
		if _, err := os.Lstat(dst); err == nil {
			return cleanupReplaceBackup(callErr, backup)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("replace %s: %w (inspect destination during recovery: %v)", dst, callErr, err)
		}
	}

	if _, err := os.Lstat(backup); err != nil {
		return fmt.Errorf("replace %s: %w (original backup expected at %s is unavailable: %v)", dst, callErr, backup, err)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("replace %s: %w (destination unexpectedly exists; the original also remains at backup %s)", dst, callErr, backup)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w (inspect destination before restoring backup %s: %v)", dst, callErr, backup, err)
	}
	if err := RenameWithRetry(backup, dst, restore); err != nil {
		// Only callErr is wrapped. Exposing a sharing violation from the exhausted
		// rollback would make the outer ReplaceWithRetry call retry after the
		// filesystem was already mutated.
		//
		// This is the one terminal state that needs a human: nothing is at dst, and
		// the original's only copy is under a name no tool looks for (the specialist
		// loader reads *.md and skips everything else), so it is invisible until
		// somebody moves it back. Say that outright rather than leaving an operator
		// to infer it from a bare error code.
		return fmt.Errorf(
			"replace %s: %w (rolling the original back failed: %v; %s no longer exists and the original survives only as %s, which must be moved back by hand to restore it)",
			dst, callErr, err, dst, backup,
		)
	}
	return fmt.Errorf("replace %s: %w (the original was restored)", dst, callErr)
}
