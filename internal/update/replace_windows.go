//go:build windows

package update

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/Gitlawb/zero/internal/config"
	"golang.org/x/sys/windows"
)

const (
	restoreRenameRetryAttempts = 10
	restoreRenameRetryDelay    = 100 * time.Millisecond
)

// renameOpenFileWithRetry renames the object file refers to onto newPath,
// retrying while something transient (an on-access scanner, a stale handle from
// the previous process image) still occupies the destination. The retry is on
// the handle-bound rename rather than a pathname rename: the destination is in a
// directory the threat principal can write, so re-resolving the SOURCE pathname
// between attempts would let a substitute be restored instead.
func renameOpenFileWithRetry(file *os.File, newPath string) error {
	var lastErr error
	for attempt := 0; attempt < restoreRenameRetryAttempts; attempt++ {
		if err := renameRecoveryFileByHandle(file, newPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(restoreRenameRetryDelay)
	}
	return lastErr
}

// This file is where ErrTargetPossiblyTampered (declared in apply.go, so
// cross-platform callers can test for it) is produced. It is wrapped into the
// error promote returns when a promotion attempt fails AND restoring the
// original binary to targetPath also fails. That combination means a principal
// who can write in the installation directory occupied targetPath in the gap
// the updater opened by renaming the running binary aside, and Windows would
// not let anything — including the restore — replace it:
// MOVEFILE_REPLACE_EXISTING cannot force past another handle's share-mode lock.
// This is not an ordinary failed update (the previous version simply staying in
// place); the executable path may now hold attacker-controlled bytes, so a
// caller must surface it as a security-relevant condition, not the same "try
// again later" failure as a stalled download.
//
// It is also returned by promote when a PREVIOUS run left that state behind and
// nobody has resolved it yet — see the refusal there.

// restoreOriginalBinary moves the object file refers to — the binary promote
// moved aside from targetPath, held open since before that rename — back onto
// targetPath after a failed promotion. Everything here is bound to that handle:
// oldPath is only ever used to NAME the object for the operator and to site the
// recovery marker, never re-resolved to decide what gets restored. A failed
// immediate restore is surfaced; the aside copy must not be queued as a reboot
// source because its pathname can be replaced before reboot under the
// writable-directory threat model.
func restoreOriginalBinary(file *os.File, oldPath string, targetPath string) error {
	err := renameOpenFileWithRetry(file, targetPath)
	if err == nil {
		err = verifyPromotedTarget(file, targetPath)
	}
	if err == nil {
		return nil
	}
	// The error this produces tells the operator their original is preserved at
	// oldPath. Record that so a later promotion refuses to treat the recovery
	// copy as the ordinary destination for another aside rename.
	if markErr := markOldBinaryPreserved(oldPath); markErr != nil {
		// The marker could not be established, so nothing on disk identifies
		// oldPath as the recovery copy. Move it to a distinct name and report that
		// authoritative location to the operator.
		if kept, keepErr := keepUnmarkedRecoveryCopy(file, oldPath); keepErr == nil {
			return fmt.Errorf(
				"%w: %v (the recovery marker could not be written: %v; the last binary this updater verified was moved to the distinct recovery path %s)",
				ErrTargetPossiblyTampered, err, markErr, kept,
			)
		} else if kept != "" {
			// The move succeeded but the post-move verification did not, so
			// oldPath is already vacated — point at kept, the path the
			// bytes actually landed at, not the path that no longer holds them.
			return fmt.Errorf(
				"%w: %v (the recovery marker could not be written: %v; the last binary this updater verified was moved to %s but could not be verified there: %v)",
				ErrTargetPossiblyTampered, err, markErr, kept, keepErr,
			)
		} else {
			return fmt.Errorf(
				"%w: %v (the recovery marker could not be written: %v — manually copy %s somewhere safe now)",
				ErrTargetPossiblyTampered, err, markErr, oldPath,
			)
		}
	}
	return fmt.Errorf("%w: %v", ErrTargetPossiblyTampered, err)
}

// oldBinaryPreservedSuffix names the marker written beside a "<path>.old" that
// a failed restore left as the last known-good binary.
const oldBinaryPreservedSuffix = ".keep"

// markOldBinaryPreserved records that oldPath must survive routine cleanup.
//
// The marker is created with the same exclusive, no-follow semantics as a
// staging file, and never truncates. Its pathname is predictable, so under the
// writable-install-directory threat model a lower-privileged writer can
// pre-create it as a hard link or reparse point; opening that with O_TRUNC
// would let the elevated updater write through it into a file of the attacker's
// choosing. CREATE_NEW plus FILE_FLAG_OPEN_REPARSE_POINT plus the same
// fresh-regular-file check refuses that object instead of writing to it.
//
// An existing marker is success, not a rewrite: its contents carry no state
// beyond "this .old is the last verified binary", so there is nothing to update
// and nothing worth opening a pre-existing object for. That also means an entry
// planted at the name by someone else reads as marked, which is the fail-safe
// direction: it makes the next run PRESERVE the recovery copy.
//
// A var so a test can force the failure branch. Every way to make the real
// CreateFile fail here (an unwritable directory, an over-length name) either
// takes the recovery copy down with it or is too platform-fragile to assert on.
var markOldBinaryPreserved = func(oldPath string) error {
	markerPath := oldPath + oldBinaryPreservedSuffix
	if _, err := os.Lstat(markerPath); err == nil {
		return nil
	}
	pathPtr, err := windows.UTF16PtrFromString(markerPath)
	if err != nil {
		return err
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
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			// Something already occupies the marker name. Whatever it is, this
			// function's only job is to make the next run preserve oldPath, and a
			// present name does that — without this process writing through an
			// object it did not create.
			return nil
		}
		return fmt.Errorf("create recovery marker %s: %w", markerPath, err)
	}
	marker := os.NewFile(uintptr(handle), markerPath)
	if err := verifyFreshRegularFile(handle, markerPath); err != nil {
		_ = deleteFileByHandle(marker)
		_ = marker.Close()
		return err
	}
	writeErr := writeRecoveryMarker(marker)
	// A partial marker must not be left beside a recovery copy that the caller
	// then relocates. Delete the object this process created through its own
	// handle — never by pathname, which the threat principal can point at
	// something else once the handle is released.
	var deleteErr error
	if writeErr != nil {
		deleteErr = deleteFileByHandle(marker)
	}
	closeErr := marker.Close()
	if writeErr == nil {
		// A close failure still leaves the fully written marker in place, and
		// its presence is the entire state this function records.
		_ = closeErr
		return nil
	}
	// Below, the write failed. Each remaining branch decides between reporting
	// that failure and conservatively claiming success — and only a confirmed
	// absence of the marker earns the report, because claiming success keeps
	// oldPath as the authoritative recovery location, which is the safe answer.
	if deleteErr != nil {
		// The partial marker could not be removed, so it is still there.
		return nil
	}
	if _, statErr := os.Lstat(markerPath); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
		// Something occupies the marker name, or its state cannot be established.
		return nil
	}
	return fmt.Errorf("write recovery marker %s: %w", markerPath, writeErr)
}

var writeRecoveryMarker = func(marker *os.File) error {
	_, err := marker.WriteString("The update at this path failed and could not restore the original binary.\n" +
		"The file without this marker's .keep suffix is the last binary this updater verified.\n")
	return err
}

// keepUnmarkedRecoveryCopy moves oldPath out from under routine cleanup when its
// marker could not be established, returning the path it now lives at.
//
// The name is unpredictable for the same reason the staging name is: this runs
// in a directory a lower-privileged principal may be able to write, and a fixed
// recovery name could be pre-created there to make this rename fail or land
// somewhere chosen by someone else. Being unpredictable also means being opaque,
// which is why the caller's error names the path.
func keepUnmarkedRecoveryCopy(file *os.File, oldPath string) (string, error) {
	suffix, err := randomStagingSuffix()
	if err != nil {
		return "", fmt.Errorf("%w: choose recovery path: %v", ErrTargetPossiblyTampered, err)
	}
	kept := filepath.Join(filepath.Dir(oldPath), filepath.Base(oldPath)+"."+suffix+".recovery")
	if _, statErr := os.Lstat(kept); statErr == nil {
		return "", fmt.Errorf("%w: recovery path %s already exists", ErrTargetPossiblyTampered, kept)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("%w: inspect recovery path %s: %v", ErrTargetPossiblyTampered, kept, statErr)
	}
	if err := renameRecoveryFileByHandle(file, kept); err != nil {
		return "", fmt.Errorf("%w: move verified recovery copy to %s: %v", ErrTargetPossiblyTampered, kept, err)
	}
	if err := verifyPromotedTarget(file, kept); err != nil {
		// The rename already reported success, so oldPath is no longer a
		// reliable location for the caller to fall back to — surface kept
		// anyway so the operator is pointed at the actual (if unverified)
		// destination instead of a path the move already vacated.
		return kept, fmt.Errorf("%w: verify recovery copy at %s: %v", ErrTargetPossiblyTampered, kept, err)
	}
	return kept, nil
}

func openRecoveryCopy(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	if err := verifyFreshRegularFile(handle, path); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

var renameRecoveryFileByHandle = renameOpenFile

// clearOldBinaryPreserved models the operator accepting the installed binary
// by deleting the recovery marker. The recovery copy itself remains preserved.
func clearOldBinaryPreserved(oldPath string) {
	_ = os.Remove(oldPath + oldBinaryPreservedSuffix)
}

// oldBinaryPreserved reports whether a failed restore marked oldPath as the last
// known-good binary.
//
// Only a definite "the marker is not there" answers false. An Lstat that fails
// for any other reason (permissions, a transient sharing error) leaves the
// question open, and the conservative answer to an open question here is to keep
// the copy: deleting it is irreversible, while keeping it costs one stale file.
func oldBinaryPreserved(oldPath string) bool {
	_, err := os.Lstat(oldPath + oldBinaryPreservedSuffix)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

type recoveryCleanupRecord struct {
	Path          string `json:"path"`
	VolumeSerial  uint32 `json:"volumeSerial"`
	FileIndexHigh uint32 `json:"fileIndexHigh"`
	FileIndexLow  uint32 `json:"fileIndexLow"`
	// Unresolved marks a failed restore. Preflight consults these records
	// alongside the sibling .keep marker so deleting that marker cannot
	// silence the next run's tamper refusal (#868). Cleanup records from a
	// successful promotion leave this false.
	Unresolved bool `json:"unresolved,omitempty"`
}

type recoveryCleanupQueue struct {
	Records []recoveryCleanupRecord `json:"records"`
}

// recoveryCleanupStateDir is per-user (%AppData% via UserConfigDir). The
// recovery copy and its .keep marker live in the installation directory, which
// is per-machine. A second Windows account therefore does not see the first
// account's unresolved record; the sibling marker is the machine-visible
// signal. A machine-wide store would need a writable shared location and ACL
// rules this updater does not own, so the record stays in the existing
// per-user update-recovery directory.
var recoveryCleanupStateDir = func() (string, error) {
	root, err := config.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "zero", "update-recovery"), nil
}

func recoveryCleanupRecordPath(targetPath string) (string, error) {
	dir, err := recoveryCleanupStateDir()
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(targetPath)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	return filepath.Join(dir, fmt.Sprintf("%x.json", digest)), nil
}

func validUpdaterRecoveryPath(targetPath string, recoveryPath string) bool {
	if !strings.EqualFold(filepath.Clean(filepath.Dir(targetPath)), filepath.Clean(filepath.Dir(recoveryPath))) {
		return false
	}
	name := strings.ToLower(filepath.Base(recoveryPath))
	prefix := strings.ToLower(filepath.Base(targetPath)) + ".zero-update-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".old") {
		return false
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".old")
	if len(suffix) != 32 {
		return false
	}
	for _, character := range suffix {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

// prepareRecoveryCleanup opens only the exact object vouched for by trusted
// per-user state from the previous successful promotion. Recovery discovery is
// intentionally broad and filename-based so suspicious state fails closed, but
// destructive cleanup never treats an install-directory name as provenance.
type recoveryCleanupCandidate struct {
	file   *os.File
	record recoveryCleanupRecord
}

func loadRecoveryCleanupQueue(targetPath string) (recoveryCleanupQueue, error) {
	recordPath, err := recoveryCleanupRecordPath(targetPath)
	if err != nil {
		return recoveryCleanupQueue{}, err
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return recoveryCleanupQueue{}, nil
		}
		return recoveryCleanupQueue{}, fmt.Errorf("read recovery cleanup record %s: %w", recordPath, err)
	}
	var queue recoveryCleanupQueue
	if err := json.Unmarshal(data, &queue); err == nil && queue.Records != nil {
		return queue, nil
	}
	var legacy recoveryCleanupRecord
	if json.Unmarshal(data, &legacy) == nil && legacy.Path != "" {
		return recoveryCleanupQueue{Records: []recoveryCleanupRecord{legacy}}, nil
	}
	return recoveryCleanupQueue{}, fmt.Errorf("parse recovery cleanup record %s", recordPath)
}

func prepareRecoveryCleanup(targetPath string) []recoveryCleanupCandidate {
	queue, err := loadRecoveryCleanupQueue(targetPath)
	if err != nil {
		// Unreadable trusted state is not "nothing to clean": skip destructive
		// cleanup rather than treat the queue as empty.
		return nil
	}
	var candidates []recoveryCleanupCandidate
	// Records outlive a single attempt so a temporarily locked copy is still
	// deleted on a later run, but a record that can never become actionable has
	// to be retired here or the backlog grows without bound.
	retained := queue.Records[:0]
	for _, record := range queue.Records {
		// Not a name this updater could have produced: unusable as provenance
		// no matter what appears at it later.
		if !validUpdaterRecoveryPath(targetPath, record.Path) {
			continue
		}
		// Definitively gone — an operator removed it, or a delete this updater
		// requested completed once the last handle closed.
		if _, err := os.Lstat(record.Path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		retained = append(retained, record)
		if oldBinaryPreserved(record.Path) || record.Unresolved {
			continue
		}
		file, err := openRecoveryCopy(record.Path)
		if err != nil {
			continue
		}
		identity, err := recoveryFileIdentity(file)
		if err != nil || identity.VolumeSerial != record.VolumeSerial ||
			identity.FileIndexHigh != record.FileIndexHigh || identity.FileIndexLow != record.FileIndexLow {
			_ = file.Close()
			continue
		}
		candidates = append(candidates, recoveryCleanupCandidate{file: file, record: record})
	}
	if len(retained) != len(queue.Records) {
		queue.Records = retained
		_ = writeRecoveryCleanupQueue(targetPath, queue)
	}
	return candidates
}

type recoveryIdentity struct {
	VolumeSerial  uint32
	FileIndexHigh uint32
	FileIndexLow  uint32
}

func recoveryFileIdentity(file *os.File) (recoveryIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return recoveryIdentity{}, err
	}
	return recoveryIdentity{
		VolumeSerial:  info.VolumeSerialNumber,
		FileIndexHigh: info.FileIndexHigh,
		FileIndexLow:  info.FileIndexLow,
	}, nil
}

// appendRecoveryCleanupRecord adds the copy this promotion moved aside to the
// per-user cleanup backlog. The identity comes from the handle the caller held
// across the aside rename, so a pathname substituted afterwards can never be
// recorded as updater-owned: the next run reopens recoveryPath and only deletes
// it if it is still that same object.
func appendRecoveryCleanupRecord(targetPath string, recoveryPath string, identity recoveryIdentity) error {
	return appendRecoveryCleanupRecordWithState(targetPath, recoveryPath, identity, false)
}

// appendUnresolvedRecoveryRecord records that a failed restore left recoveryPath
// as the last verified binary. Unlike the sibling .keep marker in the
// installation directory, this lives in per-user state an install-directory
// writer cannot delete. Preflight consults it alongside the marker (#868).
//
// A var so a test can force the failure branch. Making the real per-user store
// unwritable also takes down cleanup-queue writes this path does not own.
var appendUnresolvedRecoveryRecord = func(targetPath string, recoveryPath string, identity recoveryIdentity) error {
	return appendRecoveryCleanupRecordWithState(targetPath, recoveryPath, identity, true)
}

// persistFailedRestoreSignal keeps a durable fail-closed preflight signal after
// restoreOriginalBinary has already written the sibling .keep marker. The
// preferred signal is the per-user unresolved record. If that write fails, the
// marker is the only remaining protection and an install-directory writer can
// delete it; relocate the last verified copy to a .recovery name so the next
// preflight still refuses (#868).
//
// Relocation is bound to the object still named by asidePath. restoreOriginalBinary
// may already have moved the verified bytes back onto targetPath (the live
// binary) or to a .recovery name; keepUnmarkedRecoveryCopy renames through the
// handle without re-checking that path, so compensation must not run unless
// verifyPromotedTarget says the handle still is the aside copy. Making a failed
// record write worse by pulling the restored executable out of targetPath is
// not an acceptable fallback.
func persistFailedRestoreSignal(file *os.File, targetPath string, asidePath string, identity recoveryIdentity, restoreErr error) error {
	if err := appendUnresolvedRecoveryRecord(targetPath, asidePath, identity); err == nil {
		return restoreErr
	} else if verifyPromotedTarget(file, asidePath) != nil {
		return fmt.Errorf("%w (unresolved recovery record could not be written: %v)", restoreErr, err)
	} else if kept, keepErr := keepUnmarkedRecoveryCopy(file, asidePath); keepErr == nil {
		return fmt.Errorf("%w (unresolved recovery record could not be written: %v; the last binary this updater verified was moved to the distinct recovery path %s)", restoreErr, err, kept)
	} else if kept != "" {
		return fmt.Errorf("%w (unresolved recovery record could not be written: %v; the last binary this updater verified was moved to %s but could not be verified there: %v)", restoreErr, err, kept, keepErr)
	} else {
		return fmt.Errorf("%w (unresolved recovery record could not be written: %v)", restoreErr, err)
	}
}

func appendRecoveryCleanupRecordWithState(targetPath string, recoveryPath string, identity recoveryIdentity, unresolved bool) error {
	if !validUpdaterRecoveryPath(targetPath, recoveryPath) {
		return fmt.Errorf("invalid updater recovery path %s", recoveryPath)
	}
	record := recoveryCleanupRecord{
		Path:          recoveryPath,
		VolumeSerial:  identity.VolumeSerial,
		FileIndexHigh: identity.FileIndexHigh,
		FileIndexLow:  identity.FileIndexLow,
		Unresolved:    unresolved,
	}
	queue, err := loadRecoveryCleanupQueue(targetPath)
	if err != nil {
		return err
	}
	queue.Records = append(queue.Records, record)
	return writeRecoveryCleanupQueue(targetPath, queue)
}

// unresolvedRecordedRecoveryPaths returns live recovery copies vouched for by
// per-user unresolved-state records. Absence of the sibling .keep marker does
// not drop them: that marker is attacker-deletable in the install directory.
func unresolvedRecordedRecoveryPaths(targetPath string) ([]string, error) {
	queue, err := loadRecoveryCleanupQueue(targetPath)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, record := range queue.Records {
		if !record.Unresolved || !validUpdaterRecoveryPath(targetPath, record.Path) {
			continue
		}
		if _, err := os.Lstat(record.Path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		paths = append(paths, record.Path)
	}
	return paths, nil
}

var (
	syncRecoveryCleanupFile = (*os.File).Sync
	moveRecoveryCleanupFile = windows.MoveFileEx
)

func replaceRecoveryCleanupFile(sourcePath string, targetPath string) error {
	return moveRecoveryCleanupFile(
		windows.StringToUTF16Ptr(sourcePath),
		windows.StringToUTF16Ptr(targetPath),
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func writeRecoveryCleanupQueue(targetPath string, queue recoveryCleanupQueue) error {
	data, err := json.Marshal(queue)
	if err != nil {
		return err
	}
	recordPath, err := recoveryCleanupRecordPath(targetPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(recordPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, filepath.Base(recordPath)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	// This queue may carry an unresolved fail-closed promotion signal, not merely
	// an advisory cleanup backlog. Flush its complete contents before publishing,
	// then use Windows' write-through replacement so success is not reported in
	// the crash window between the namespace update and durable storage.
	if err := syncRecoveryCleanupFile(temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceRecoveryCleanupFile(temporaryPath, recordPath)
}

type fileDispositionInfo struct {
	DeleteFile byte
}

// deleteFileByHandle marks the object file refers to for deletion, which takes
// effect when its last handle closes. It never resolves a pathname, so it can
// only ever delete the object this process already holds open.
func deleteFileByHandle(file *os.File) error {
	info := fileDispositionInfo{DeleteFile: 1}
	return windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
}

func closeRecoveryCleanupCandidates(candidates []recoveryCleanupCandidate) {
	for index := range candidates {
		if candidates[index].file == nil {
			continue
		}
		_ = candidates[index].file.Close()
		candidates[index].file = nil
	}
}

// cleanupSupersededRecoveryCopies marks the exact pre-promotion objects for
// deletion only after the replacement has been verified. The handles deny
// delete sharing, so their entries cannot be substituted in the meantime.
//
// A record is only retired once its object is actually gone. A copy another
// process holds open (an on-access scanner, an operator's editor) stays in the
// backlog and is retried by a later promotion, so transient locks cannot leak
// full binaries.
func cleanupSupersededRecoveryCopies(targetPath string, candidates []recoveryCleanupCandidate) {
	defer closeRecoveryCleanupCandidates(candidates)
	queue, err := loadRecoveryCleanupQueue(targetPath)
	if err != nil {
		return
	}
	deleted := make(map[recoveryCleanupRecord]bool)
	for index := range candidates {
		candidate := &candidates[index]
		if err := deleteFileByHandle(candidate.file); err != nil {
			continue
		}
		// FileDispositionInfo removes the entry when the last handle closes, so
		// releasing ours is what makes the deletion observable.
		_ = candidate.file.Close()
		candidate.file = nil
		if _, err := os.Lstat(candidate.record.Path); errors.Is(err, os.ErrNotExist) {
			deleted[candidate.record] = true
		}
	}
	remaining := queue.Records[:0]
	for _, record := range queue.Records {
		if !deleted[record] {
			remaining = append(remaining, record)
		}
	}
	queue.Records = remaining
	_ = writeRecoveryCleanupQueue(targetPath, queue)
}
