//go:build windows

package update

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createStagedBinary stages beside targetPath under an unpredictable name. Unlike
// POSIX this needs no private directory: Windows can rename an object through the
// very handle it was created with (see promote), so the staging pathname never has
// to be re-resolved and cannot be substituted.
func createStagedBinary(targetPath string) (*stagedBinary, error) {
	path, err := stagingFilePath(targetPath)
	if err != nil {
		return nil, err
	}
	file, err := createStagingFile(path)
	if err != nil {
		return nil, err
	}
	return &stagedBinary{file: file, path: path}, nil
}

// createStagingFile creates path exclusively and without following any
// reparse point that may already occupy it. CREATE_NEW alone can still
// resolve through an existing reparse point (symlink/junction) when deciding
// whether the target exists — if that reparse point's target is a real file
// writable by this (possibly elevated) process, CreateFile would open and
// truncate it instead. FILE_FLAG_OPEN_REPARSE_POINT makes CreateFile operate
// on the reparse point itself, so CREATE_NEW fails on it exactly like it
// would fail on a pre-existing regular file or hard link.
//
// DELETE access is requested alongside GENERIC_WRITE because promote renames
// this object through the handle, which requires it.
func createStagingFile(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_WRITE|windows.DELETE,
		0,
		nil,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	file := os.NewFile(uintptr(handle), path)
	if err := verifyFreshRegularFile(handle, path); err != nil {
		// Delete through the handle rather than by pathname: the object this
		// process just created is the only thing it may remove, and its name is
		// writable by the threat principal the moment the handle is released.
		_ = deleteFileByHandle(file)
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// verifyFreshRegularFile defends in depth against the handle unexpectedly
// referring to a reparse point, directory, or an object with other hard
// links: CREATE_NEW + FILE_FLAG_OPEN_REPARSE_POINT should already guarantee
// a brand-new regular file, but this catches any surprise before any of the
// verified release bytes are written through the handle.
func verifyFreshRegularFile(handle windows.Handle, path string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("stat new staging file %s: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("staging file %s is unexpectedly a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("staging file %s is unexpectedly a directory", path)
	}
	if info.NumberOfLinks > 1 {
		return fmt.Errorf("staging file %s unexpectedly has %d hard links", path, info.NumberOfLinks)
	}
	return nil
}

// promote makes the staged object the installed binary. Windows will not let a
// running executable be overwritten or deleted directly, but NTFS does allow
// renaming it aside — the same trick already used for locked config files in
// internal/cli/mcp_config.go's replaceMCPWritableConfigFile.
//
// The second rename goes through the staging HANDLE
// (SetFileInformationByHandle/FileRenameInfo) instead of the staging pathname.
// A pathname rename would re-resolve the staging entry, so a lower-privileged
// principal that can write in the installation directory could replace that entry
// after the verified bytes were written and have the updater install its file
// instead. Renaming the object the handle already refers to removes that handoff:
// there is no second lookup to win.
func (staged *stagedBinary) promote(targetPath string) error {
	releasePromotionLock, err := acquirePromotionLock(targetPath)
	if err != nil {
		return fmt.Errorf("lock binary promotion: %w", err)
	}
	defer releasePromotionLock()

	if err := preflightRecoveryStateLocked(targetPath); err != nil {
		return err
	}
	// Always use a namespaced unpredictable aside path. Besides avoiding any
	// existing recovery copy, this gives trusted cleanup state a narrow path
	// format to validate instead of accepting arbitrary *.old files.
	suffix, suffixErr := randomStagingSuffix()
	if suffixErr != nil {
		return fmt.Errorf("choose recovery path: %w", suffixErr)
	}
	asidePath := targetPath + ".zero-update-" + suffix + ".old"
	cleanupCandidates := prepareRecoveryCleanup(targetPath)
	cleanedCandidates := false
	defer func() {
		if !cleanedCandidates {
			closeRecoveryCleanupCandidates(cleanupCandidates)
		}
	}()
	original, openErr := openRecoveryCopy(targetPath)
	if openErr != nil {
		return fmt.Errorf("open running binary for recovery: %w", openErr)
	}
	defer func() { _ = original.Close() }()
	originalIdentity, identityErr := recoveryFileIdentity(original)
	if identityErr != nil {
		return fmt.Errorf("capture running binary identity: %w", identityErr)
	}
	if err := renameRecoveryFileByHandle(original, asidePath); err != nil {
		return fmt.Errorf("rename running binary aside: %w", err)
	}
	if err := verifyPromotedTarget(original, asidePath); err != nil {
		return fmt.Errorf("verify running binary aside: %w", err)
	}
	renameErr := renameFileByHandle(staged.file, targetPath)
	if renameErr == nil {
		if verifyErr := verifyPromotedTarget(staged.file, targetPath); verifyErr != nil {
			renameErr = fmt.Errorf("promoted object unreachable at %s: %w", targetPath, verifyErr)
		}
	}
	if renameErr != nil {
		if restoreErr := restoreOriginalBinary(original, asidePath, targetPath); restoreErr != nil {
			// Best-effort: the restore error is what the operator must see.
			// persistFailedRestoreSignal writes the per-user record when it
			// can, and otherwise relocates the last verified copy so deleting
			// the sibling .keep marker cannot silence the next preflight (#868).
			restoreErr = persistFailedRestoreSignal(original, targetPath, asidePath, originalIdentity, restoreErr)
			return fmt.Errorf("install new binary: %v; additionally failed to restore the original binary: %w", renameErr, restoreErr)
		}
		return fmt.Errorf("install new binary: %w", renameErr)
	}
	if err := appendRecoveryCleanupRecord(targetPath, asidePath, originalIdentity); err == nil {
		cleanupSupersededRecoveryCopies(targetPath, cleanupCandidates)
		cleanedCandidates = true
	}
	staged.path = targetPath
	staged.promoted = true
	return nil
}

func preflightRecoveryState(targetPath string) error {
	release, err := acquirePromotionLock(targetPath)
	if err != nil {
		return fmt.Errorf("lock binary recovery preflight: %w", err)
	}
	defer release()
	return preflightRecoveryStateLocked(targetPath)
}

func preflightRecoveryStateLocked(targetPath string) error {
	relocatedRecoveries, recoveryErr := relocatedRecoveryPaths(targetPath)
	if recoveryErr != nil {
		return fmt.Errorf("%w: inspect relocated recovery state for %s: %v", ErrTargetPossiblyTampered, targetPath, recoveryErr)
	}
	if len(relocatedRecoveries) != 0 {
		return fmt.Errorf(
			"%w: a previous update moved the last binary this updater verified to %s after recovery-marker creation failed; restore the correct recovery binary or remove it after verifying %s before updating again",
			ErrTargetPossiblyTampered, strings.Join(relocatedRecoveries, ", "), targetPath,
		)
	}
	// Refuse to promote while a previous failure left its recovery copy in place.
	//
	// Skipping the cleanup below is not enough to protect it: os.Rename uses
	// MOVEFILE_REPLACE_EXISTING on Windows, so renaming the running binary aside
	// would overwrite the last verified copy with whatever now occupies
	// targetPath — the very bytes the earlier failure could not verify. If this
	// promotion then failed, restoreOriginalBinary could only move those
	// unverified bytes back, and the known-good binary would be gone.
	//
	// Automation cannot resolve this safely on the operator's behalf: only they
	// can say whether the file at targetPath is theirs. So this fails closed and
	// says exactly which two moves end the state.
	markedRecoveries, recoveryErr := markedRecoveryPaths(targetPath)
	if recoveryErr != nil {
		return fmt.Errorf("%w: inspect previous recovery state for %s: %v", ErrTargetPossiblyTampered, targetPath, recoveryErr)
	}
	if len(markedRecoveries) != 0 {
		recoveryPaths := make([]string, 0, len(markedRecoveries))
		markerPaths := make([]string, 0, len(markedRecoveries))
		for _, recoveryPath := range markedRecoveries {
			recoveryPaths = append(recoveryPaths, recoveryPath)
			markerPaths = append(markerPaths, recoveryPath+oldBinaryPreservedSuffix)
		}
		return fmt.Errorf(
			"%w: a previous update could not restore the original binary. Recovery path(s) %s may hold the last binary this updater verified and %s may hold unverified content. "+
				"Move the correct recovery binary back over %s to restore it, or delete the recovery copy itself (not only its marker, %s) to accept the installed binary, then update again",
			ErrTargetPossiblyTampered, strings.Join(recoveryPaths, ", "), targetPath,
			targetPath, strings.Join(markerPaths, ", "),
		)
	}
	if _, targetErr := os.Lstat(targetPath); errors.Is(targetErr, os.ErrNotExist) {
		recoveryPaths, err := existingRecoveryPaths(targetPath)
		if err != nil {
			return fmt.Errorf("%w: inspect recovery copies for missing %s: %v", ErrTargetPossiblyTampered, targetPath, err)
		}
		switch len(recoveryPaths) {
		case 1:
			return fmt.Errorf(
				"%w: %s is missing and %s may be the only recoverable binary; move it back before updating again",
				ErrTargetPossiblyTampered, targetPath, recoveryPaths[0],
			)
		case 0:
			// Let the ordinary aside rename below report the missing target.
		default:
			return fmt.Errorf(
				"%w: %s is missing and multiple recovery binaries exist (%s); resolve the ambiguous recovery state before updating again",
				ErrTargetPossiblyTampered, targetPath, strings.Join(recoveryPaths, ", "),
			)
		}
	}
	return nil
}

// acquirePromotionLock serializes the full target-specific recovery transaction
// across updater processes. Without it, one updater could capture another's
// unmarked aside while that process is still trying to restore or mark it.
// Windows mutex ownership is thread-affine, so the caller remains pinned until
// the returned release function runs.
func acquirePromotionLock(targetPath string) (func(), error) {
	absolutePath, err := filepath.Abs(targetPath)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolutePath))))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("Local\\zero-update-%x", digest))
	if err != nil {
		return nil, err
	}

	runtime.LockOSThread()
	handle, createErr := windows.CreateMutex(nil, false, name)
	if createErr != nil && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		runtime.UnlockOSThread()
		return nil, createErr
	}
	if handle == 0 {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create target mutex returned an invalid handle")
	}
	event, waitErr := windows.WaitForSingleObject(handle, windows.INFINITE)
	if waitErr != nil || event != windows.WAIT_OBJECT_0 && event != windows.WAIT_ABANDONED {
		_ = windows.CloseHandle(handle)
		runtime.UnlockOSThread()
		if waitErr != nil {
			return nil, waitErr
		}
		return nil, fmt.Errorf("wait for target mutex returned %#x", event)
	}
	return func() {
		_ = windows.ReleaseMutex(handle)
		_ = windows.CloseHandle(handle)
		runtime.UnlockOSThread()
	}, nil
}

// existingRecoveryPaths returns every canonical or randomized aside path that
// could hold a binary moved away from targetPath by a previous promotion. The
// directory is attacker-writable under the threat model, so these names are
// only reasons to fail closed; their presence is never proof of file contents.
func existingRecoveryPaths(targetPath string) ([]string, error) {
	dir := filepath.Dir(targetPath)
	base := filepath.Base(targetPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	// NTFS is case-insensitive (case-preserving), so a recovery file can exist
	// on disk as e.g. "zero.exe.OLD" — fold both sides before matching, or it
	// silently drops out of every caller's fail-closed check below.
	lowerBase := strings.ToLower(base)
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if lowerName == lowerBase+".old" ||
			(strings.HasPrefix(lowerName, lowerBase+".") &&
				strings.HasSuffix(lowerName, ".old") &&
				len(lowerName) > len(lowerBase)+len("..old")) {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths, nil
}

// relocatedRecoveryPaths returns copies moved to the distinct recovery name
// after a failed restore could not establish a .keep marker. Unlike ordinary
// unmarked .old files, these are authoritative unresolved recovery state and
// must block every later promotion until the operator resolves them.
func relocatedRecoveryPaths(targetPath string) ([]string, error) {
	dir := filepath.Dir(targetPath)
	base := strings.ToLower(filepath.Base(targetPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := base + "."
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if !strings.HasPrefix(lowerName, prefix) || !strings.HasSuffix(lowerName, ".recovery") {
			continue
		}
		// The canonical relocation is <target>.old.<suffix>.recovery.
		// A failed promotion that already used a randomized aside can also
		// produce <target>.<aside-suffix>.old.<recovery-suffix>.recovery.
		middle := strings.TrimSuffix(strings.TrimPrefix(lowerName, prefix), ".recovery")
		if strings.HasPrefix(middle, "old.") && len(middle) > len("old.") ||
			strings.Contains(middle, ".old.") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths, nil
}

// markedRecoveryPaths finds recovery copies protected by a sibling .keep
// marker or by a trusted per-user unresolved-state record. A failed
// second-or-later update commonly uses a randomized aside because the
// canonical .old already exists. The trusted record is consulted even when
// the marker is gone: that marker lives in the attacker-writable install
// directory (#868).
func markedRecoveryPaths(targetPath string) ([]string, error) {
	recoveryPaths, err := existingRecoveryPaths(targetPath)
	if err != nil {
		return nil, err
	}
	var marked []string
	seen := make(map[string]bool)
	for _, recoveryPath := range recoveryPaths {
		if oldBinaryPreserved(recoveryPath) {
			marked = append(marked, recoveryPath)
			seen[strings.ToLower(recoveryPath)] = true
		}
	}
	recordedPaths, err := unresolvedRecordedRecoveryPaths(targetPath)
	if err != nil {
		return nil, err
	}
	for _, recorded := range recordedPaths {
		key := strings.ToLower(recorded)
		if seen[key] {
			continue
		}
		seen[key] = true
		marked = append(marked, recorded)
	}
	return marked, nil
}

// verifyPromotedTarget reports whether targetPath names the same object as the
// staged handle after a reported-successful rename. SetFileInformationByHandle
// returning success is not, on its own, proof the object ended up there: a
// handle whose directory entry was removed and replaced out from under it
// (the substitution createStagingFile's exclusive, no-share open defends
// against — see its doc comment) can leave some Windows versions accepting
// the rename call without it taking effect, which would otherwise let
// promote report success while targetPath is left missing entirely.
//
// A metadata-only open is not blocked by staged.file's exclusive data/delete
// sharing. Comparing the volume and file index is independent of path spelling,
// including 8.3 names, case, UNC prefixes, and reparse points in ancestors.
func verifyPromotedTarget(file *os.File, targetPath string) error {
	targetPathPtr, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	targetHandle, err := windows.CreateFile(
		targetPathPtr,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fmt.Errorf("open promoted target metadata: %w", err)
	}
	defer func() { _ = windows.CloseHandle(targetHandle) }()

	var stagedInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &stagedInfo); err != nil {
		return fmt.Errorf("query staged object identity: %w", err)
	}
	var targetInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(targetHandle, &targetInfo); err != nil {
		return fmt.Errorf("query promoted target identity: %w", err)
	}
	if targetInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("promoted target is unexpectedly a reparse point")
	}
	if targetInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("promoted target is unexpectedly a directory")
	}
	if stagedInfo.NumberOfLinks != 1 || targetInfo.NumberOfLinks != 1 {
		return fmt.Errorf("promoted object unexpectedly has %d hard links", targetInfo.NumberOfLinks)
	}
	if stagedInfo.VolumeSerialNumber != targetInfo.VolumeSerialNumber ||
		stagedInfo.FileIndexHigh != targetInfo.FileIndexHigh ||
		stagedInfo.FileIndexLow != targetInfo.FileIndexLow {
		return fmt.Errorf("target path does not name the staged object")
	}
	return nil
}

// discardOpenObject schedules the staged object for deletion through the handle
// it was created with, so failure-path cleanup removes exactly the object this
// updater staged. The pathname alternative (os.Remove after the handle is
// closed) re-resolves the staging entry, and a principal who can write in the
// installation directory can substitute that entry in the gap — the same
// handoff createStagingFile and promote are built to avoid. Deletion takes
// effect when the last handle closes, which discard does immediately after.
//
// If the request fails, the staged file is left behind rather than removed by
// name: a leaked file costs disk, deleting an attacker-chosen entry costs the
// operator a file they own.
func (staged *stagedBinary) discardOpenObject() {
	if staged.promoted || staged.file == nil {
		return
	}
	_ = deleteFileByHandle(staged.file)
}

func (staged *stagedBinary) discardPaths() {
	// POSIX-only state is present in the shared struct and always nil here.
	_ = staged.dir
	_ = staged.dirHandle
	_ = staged.parentHandle
	// Nothing is removed by pathname here; see discardOpenObject.
}

// fileRenameInfo mirrors FILE_RENAME_INFO. FileName is a variable-length WCHAR
// array that follows the header, so the buffer is sized by hand and the name is
// appended after fileRenameInfoHeaderSize bytes.
type fileRenameInfo struct {
	ReplaceIfExists bool
	RootDirectory   windows.Handle
	FileNameLength  uint32
}

// fileRenameInfoHeaderSize is the offset of FILE_RENAME_INFO's FileName member.
// It is derived rather than hardcoded because the RootDirectory pointer's
// alignment (and therefore the offset) differs between 32- and 64-bit Windows.
var fileRenameInfoHeaderSize = func() uintptr {
	var info fileRenameInfo
	return unsafe.Offsetof(info.FileNameLength) + unsafe.Sizeof(info.FileNameLength)
}()

// renameFileByHandle renames the object file refers to, not the object its
// current pathname resolves to. targetPath must be fully qualified.
//
// It is a package var, like stageBinary, so a test can simulate
// SetFileInformationByHandle reporting success without the rename actually
// taking effect — the exact failure mode verifyPromotedTarget defends
// against — without needing to reproduce whatever Windows-version-specific
// condition triggers it for real.
func renameOpenFile(file *os.File, targetPath string) error {
	name, err := windows.UTF16FromString(targetPath)
	if err != nil {
		return err
	}
	// FILE_RENAME_INFO declares FileName as NUL-terminated even though
	// FileNameLength excludes the terminator. Keep the terminator in the buffer:
	// omitting it makes the ordinary rename fail with ERROR_PATH_NOT_FOUND on the
	// supported Windows runner.
	buffer := make([]byte, int(fileRenameInfoHeaderSize)+len(name)*2)
	info := (*fileRenameInfo)(unsafe.Pointer(&buffer[0]))
	// ReplaceIfExists stays false: promote already renamed the running binary
	// aside, so a target that exists again means something raced the update, and
	// failing is better than clobbering whatever appeared there.
	info.FileNameLength = uint32((len(name) - 1) * 2)
	for index, unit := range name {
		binary.LittleEndian.PutUint16(buffer[int(fileRenameInfoHeaderSize)+index*2:], unit)
	}
	if err := windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileRenameInfo,
		&buffer[0],
		uint32(len(buffer)),
	); err != nil {
		return fmt.Errorf("rename staged binary onto %s: %w", targetPath, err)
	}
	return nil
}

var renameFileByHandle = renameOpenFile
