//go:build windows

package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestMain(m *testing.M) {
	stateDir, err := os.MkdirTemp("", "zero-update-recovery-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create updater test state directory: %v\n", err)
		os.Exit(1)
	}
	recoveryCleanupStateDir = func() (string, error) { return stateDir, nil }
	code := m.Run()
	_ = os.RemoveAll(stateDir)
	os.Exit(code)
}

// TestPromoteInstallsTheStagedObjectNotTheStagedPath is the regression test for
// the live handoff half of #742: randomizing the staging name and creating it
// exclusively stops PRE-creation, but not substitution after the verified bytes
// are written. Windows renames through the staging HANDLE, so a substituted entry
// at the staging pathname is simply not what gets promoted.
func TestPromoteInstallsTheStagedObjectNotTheStagedPath(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	staged, err := stageBinary(sourcePath, targetPath)
	if err != nil {
		t.Fatalf("stageBinary: %v", err)
	}
	discarded := false
	defer func() {
		if !discarded {
			staged.discard()
		}
	}()

	// Rehearse the strongest form of the substitution: the staging entry is
	// replaced wholesale between the write and the swap. (A real attacker also has
	// to get past the exclusive share mode this handle holds; the test does not,
	// which only makes the check stricter.)
	substituted := false
	if err := os.Remove(staged.path); err == nil {
		if err := os.WriteFile(staged.path, []byte("attacker-binary"), 0o755); err != nil {
			t.Fatalf("WriteFile substituted staging entry: %v", err)
		}
		substituted = true
	}

	promoteErr := staged.promote(targetPath)
	// The staging handle keeps the promoted (or, on failure, discarded) file open
	// with an exclusive share mode, so release it before reading installed bytes
	// (installBinary's deferred discard does the same).
	staged.discard()
	discarded = true

	if promoteErr != nil {
		// Fully unlinking the staging file (its directory entry removed, then a
		// new file recreated at that name) is not something every Windows build
		// honors a same-handle rename back into: verifyPromotedTarget's post-
		// rename identity check can find nothing at targetPath and fail the
		// promotion, which restoreOriginalBinary then recovers from by moving
		// the pre-update binary back. That is this test's real security
		// property holding — the attacker's substituted bytes are never
		// installed — just via the fail-closed path instead of the handle-
		// rename defeating the substitution outright. Only accept the failure
		// when it actually happened via a real substitution; any other promote
		// error is a genuine regression.
		if !substituted {
			t.Fatalf("promote failed without substitution: %v", promoteErr)
		}
		if installed, readErr := os.ReadFile(targetPath); readErr == nil && string(installed) == "attacker-binary" {
			t.Fatalf("attacker-controlled bytes were installed: %q", installed)
		}
		t.Logf("promote refused after full-unlink substitution (%v); attacker bytes were not installed", promoteErr)
		return
	}

	installed, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile installed: %v", err)
	}
	if string(installed) != "verified-binary" {
		t.Fatalf("installed binary = %q, want the verified bytes", installed)
	}
	if !substituted {
		t.Log("the staging entry could not be replaced (exclusive share mode); the handle-bound rename was still exercised")
	}
}

// TestPromoteRejectsALyingRenameByHandle is the regression test for a review
// finding on PR #751: SetFileInformationByHandle reporting success is not, on
// its own, proof the object actually ended up at targetPath. Some Windows
// versions have been observed accepting the rename call against a handle
// whose directory entry was substituted out from under it without the object
// actually moving — this simulates that by stubbing the rename to lie, since
// the real trigger condition is Windows-version-specific and not reliably
// reproducible on demand. Without verifyPromotedTarget, promote would return
// nil while targetPath silently ends up missing, reporting a successful
// update that actually stranded the user without an executable at all.
func TestPromoteRejectsALyingRenameByHandle(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	staged, err := stageBinary(sourcePath, targetPath)
	if err != nil {
		t.Fatalf("stageBinary: %v", err)
	}
	defer staged.discard()

	original := renameFileByHandle
	renameFileByHandle = func(file *os.File, target string) error {
		return nil // lie: report success without touching anything
	}
	defer func() { renameFileByHandle = original }()

	promoteErr := staged.promote(targetPath)
	if promoteErr == nil {
		t.Fatal("promote reported success for a rename that never actually happened, want an error")
	}
	if !strings.Contains(promoteErr.Error(), "unreachable") {
		t.Fatalf("error = %q, want it to explain the target is unreachable", promoteErr.Error())
	}
	installed, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(installed) != "old-binary" {
		t.Fatalf("target = %q, want the original binary restored", installed)
	}
	if _, err := os.Stat(targetPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old leftover survived a successful restore: %v", err)
	}
}

// A promotion failure followed by a blocked restore is security-relevant all
// the way through installBinary; its contextual wrappers must not erase the
// sentinel that callers of Apply use to distinguish possible path tampering.
func TestInstallBinaryPreservesPossibleTamperingError(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	original := renameFileByHandle
	var conflicting windows.Handle
	renameFileByHandle = func(_ *os.File, target string) error {
		pathPtr, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return err
		}
		conflicting, err = windows.CreateFile(pathPtr, windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			return fmt.Errorf("create conflicting target: %w", err)
		}
		return errors.New("injected promotion failure")
	}
	t.Cleanup(func() {
		renameFileByHandle = original
		if conflicting != 0 {
			_ = windows.CloseHandle(conflicting)
		}
	})
	originalMark := markOldBinaryPreserved
	markOldBinaryPreserved = func(string) error { return errors.New("injected marker failure") }
	t.Cleanup(func() { markOldBinaryPreserved = originalMark })
	const suffix = "deadbeefdeadbeefdeadbeefdeadbeef"
	stubRandomStagingSuffix(t, suffix)

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want it to wrap ErrTargetPossiblyTampered", err)
	}
	if strings.Contains(err.Error(), "original preserved at "+targetPath+".old") {
		t.Fatalf("installBinary error falsely claims the relocated copy remains at .old: %v", err)
	}
	expectedRecovery := targetPath + ".zero-update-" + suffix + ".old." + suffix + ".recovery"
	if !strings.Contains(err.Error(), expectedRecovery) {
		t.Fatalf("installBinary error = %v, want the authoritative relocated recovery path", err)
	}
}

// TestPromoteRefusesWhileRecoveryCopyIsMarked covers jatmn's #751 P1: skipping
// the pre-rename cleanup was not enough to protect a marked recovery copy.
// os.Rename uses MOVEFILE_REPLACE_EXISTING on Windows, so renaming the running
// binary aside overwrote the last verified copy with the unverified bytes the
// earlier failure left at the target — and if this promotion then failed,
// restoreOriginalBinary could only move those unverified bytes back.
func TestPromoteRefusesWhileRecoveryCopyIsMarked(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	oldPath := targetPath + ".old"
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile old binary: %v", err)
	}
	if err := markOldBinaryPreserved(oldPath); err != nil {
		t.Fatalf("markOldBinaryPreserved: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want a refusal wrapping ErrTargetPossiblyTampered", err)
	}
	// The whole point: the known-good copy is still there afterwards.
	got, readErr := os.ReadFile(oldPath)
	if readErr != nil {
		t.Fatalf("recovery copy was destroyed by the retry: %v", readErr)
	}
	if string(got) != "known-good" {
		t.Fatalf("recovery copy = %q, want the last verified binary", got)
	}
	// And the error names both moves that end the state.
	for _, want := range []string{oldPath, targetPath, oldPath + oldBinaryPreservedSuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to name %q", err, want)
		}
	}

	// Clearing the marker is the operator accepting the installed binary; the
	// next promotion proceeds normally without destroying the recovery copy.
	clearOldBinaryPreserved(oldPath)
	stubRandomStagingSuffix(t, "deadbeef")
	if err := installBinary(sourcePath, targetPath); err != nil {
		t.Fatalf("installBinary after the operator cleared the marker: %v", err)
	}
	if data, err := os.ReadFile(targetPath); err != nil || string(data) != "verified-binary" {
		t.Fatalf("target = %q err=%v, want the verified bytes installed", data, err)
	}
	if data, err := os.ReadFile(oldPath); err != nil || string(data) != "known-good" {
		t.Fatalf("recovery copy = %q err=%v, want known-good after retry", data, err)
	}
}

// TestPreflightRefusesWhenRecoveryMarkerIsDeletedButTrustedRecordRemains is the
// #868 regression: after a failed restore, the sibling .keep marker is the
// only on-disk signal in the install directory that the aside copy is the last
// verified binary. A writer in that directory can delete it. The identity-bound
// per-user record written on the same failure path must still cause refusal.
func TestPreflightRefusesWhenRecoveryMarkerIsDeletedButTrustedRecordRemains(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	const suffix = "cafecafecafecafecafecafecafecafe"
	stubRandomStagingSuffix(t, suffix)
	original := renameFileByHandle
	var conflicting windows.Handle
	renameFileByHandle = func(_ *os.File, target string) error {
		pathPtr, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return err
		}
		conflicting, err = windows.CreateFile(pathPtr, windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			return fmt.Errorf("create conflicting target: %w", err)
		}
		return errors.New("injected promotion failure")
	}
	t.Cleanup(func() {
		renameFileByHandle = original
		if conflicting != 0 {
			_ = windows.CloseHandle(conflicting)
		}
	})

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	if conflicting != 0 {
		_ = windows.CloseHandle(conflicting)
		conflicting = 0
	}
	// Restore the production rename so a missed preflight refusal would
	// actually install, rather than fail for the injected reason.
	renameFileByHandle = original

	recoveryPath := targetPath + ".zero-update-" + suffix + ".old"
	if got, readErr := os.ReadFile(recoveryPath); readErr != nil || string(got) != "old-binary" {
		t.Fatalf("recovery copy = %q err=%v, want the last verified binary", got, readErr)
	}
	if !oldBinaryPreserved(recoveryPath) {
		t.Fatal("failed restore must still write the sibling marker")
	}
	queue, loadErr := loadRecoveryCleanupQueue(targetPath)
	if loadErr != nil {
		t.Fatalf("loadRecoveryCleanupQueue: %v", loadErr)
	}
	if len(queue.Records) != 1 || !queue.Records[0].Unresolved {
		t.Fatalf("trusted records = %+v, want one unresolved entry", queue.Records)
	}

	clearOldBinaryPreserved(recoveryPath)
	if oldBinaryPreserved(recoveryPath) {
		t.Fatal("marker deletion did not take effect")
	}

	err = installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("retry after marker deletion = %v, want refusal from the trusted record", err)
	}
	if !strings.Contains(err.Error(), recoveryPath) {
		t.Fatalf("error = %v, want it to name recovery path %s", err, recoveryPath)
	}
	if got, readErr := os.ReadFile(recoveryPath); readErr != nil || string(got) != "old-binary" {
		t.Fatalf("recovery copy after refusal = %q err=%v, want it left intact", got, readErr)
	}
}

// TestPreflightRefusesWhenRecordWriteFailsAndRecoveryMarkerIsDeleted is the
// #868 follow-up: if the per-user unresolved record cannot be written, the
// sibling .keep marker is the only remaining install-directory signal. A
// writer there can delete it. The failed-restore path must still leave a
// durable fail-closed signal so the next promotion refuses.
func TestPreflightRefusesWhenRecordWriteFailsAndRecoveryMarkerIsDeleted(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	const suffix = "feedfeedfeedfeedfeedfeedfeedfeed"
	stubRandomStagingSuffix(t, suffix)
	originalRename := renameFileByHandle
	var conflicting windows.Handle
	renameFileByHandle = func(_ *os.File, target string) error {
		pathPtr, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return err
		}
		conflicting, err = windows.CreateFile(pathPtr, windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			return fmt.Errorf("create conflicting target: %w", err)
		}
		return errors.New("injected promotion failure")
	}
	originalAppend := appendUnresolvedRecoveryRecord
	appendUnresolvedRecoveryRecord = func(string, string, recoveryIdentity) error {
		return errors.New("injected record write failure")
	}
	t.Cleanup(func() {
		renameFileByHandle = originalRename
		appendUnresolvedRecoveryRecord = originalAppend
		if conflicting != 0 {
			_ = windows.CloseHandle(conflicting)
		}
	})

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	if conflicting != 0 {
		_ = windows.CloseHandle(conflicting)
		conflicting = 0
	}
	// Restore production seams so a missed preflight refusal would actually
	// install, rather than fail for the injected reason.
	renameFileByHandle = originalRename
	appendUnresolvedRecoveryRecord = originalAppend

	asidePath := targetPath + ".zero-update-" + suffix + ".old"
	expectedRecovery := asidePath + "." + suffix + ".recovery"
	if got, readErr := os.ReadFile(expectedRecovery); readErr != nil || string(got) != "old-binary" {
		t.Fatalf("relocated recovery copy = %q err=%v, want the last verified binary at %s", got, readErr, expectedRecovery)
	}
	if !strings.Contains(err.Error(), expectedRecovery) {
		t.Fatalf("installBinary error = %v, want the authoritative relocated recovery path", err)
	}
	if _, statErr := os.Lstat(asidePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("aside path %s still occupied after relocation: %v", asidePath, statErr)
	}
	queue, loadErr := loadRecoveryCleanupQueue(targetPath)
	if loadErr != nil {
		t.Fatalf("loadRecoveryCleanupQueue: %v", loadErr)
	}
	for _, record := range queue.Records {
		if record.Unresolved {
			t.Fatalf("unresolved record was written despite injected failure: %+v", record)
		}
	}

	clearOldBinaryPreserved(asidePath)
	if oldBinaryPreserved(asidePath) {
		t.Fatal("marker deletion did not take effect")
	}

	err = installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("retry after record-write failure and marker deletion = %v, want refusal", err)
	}
	if !strings.Contains(err.Error(), expectedRecovery) {
		t.Fatalf("error = %v, want it to name relocated recovery path %s", err, expectedRecovery)
	}
	if got, readErr := os.ReadFile(expectedRecovery); readErr != nil || string(got) != "old-binary" {
		t.Fatalf("recovery copy after refusal = %q err=%v, want it left intact", got, readErr)
	}
}

// TestPersistFailedRestoreSignalDoesNotRelocateLiveBinary is the compensation
// identity check: when the per-user record write fails, persistFailedRestoreSignal
// must not rename through the handle unless that object is still the aside copy.
// If restore already put the verified bytes back at targetPath, relocating would
// remove the binary the user is running.
func TestPersistFailedRestoreSignalDoesNotRelocateLiveBinary(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	asidePath := targetPath + ".zero-update-0123456789abcdef0123456789abcdef.old"
	if err := os.WriteFile(targetPath, []byte("restored-live-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(asidePath, []byte("not-the-handle"), 0o755); err != nil {
		t.Fatalf("WriteFile aside: %v", err)
	}

	live, err := openRecoveryCopy(targetPath)
	if err != nil {
		t.Fatalf("open live binary: %v", err)
	}
	defer func() { _ = live.Close() }()
	identity, err := recoveryFileIdentity(live)
	if err != nil {
		t.Fatalf("recoveryFileIdentity: %v", err)
	}

	originalAppend := appendUnresolvedRecoveryRecord
	appendUnresolvedRecoveryRecord = func(string, string, recoveryIdentity) error {
		return errors.New("injected record write failure")
	}
	t.Cleanup(func() { appendUnresolvedRecoveryRecord = originalAppend })

	restoreErr := fmt.Errorf("%w: restore blocked", ErrTargetPossiblyTampered)
	err = persistFailedRestoreSignal(live, targetPath, asidePath, identity, restoreErr)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("error = %v, want the wrapped restore error", err)
	}
	if strings.Contains(err.Error(), ".recovery") {
		t.Fatalf("error = %v, compensation must not claim a relocation", err)
	}

	if err := live.Close(); err != nil {
		t.Fatalf("close live recovery handle: %v", err)
	}
	got, readErr := os.ReadFile(targetPath)
	if readErr != nil || string(got) != "restored-live-binary" {
		t.Fatalf("target = %q err=%v, want the live binary left in place", got, readErr)
	}
	if got, readErr := os.ReadFile(asidePath); readErr != nil || string(got) != "not-the-handle" {
		t.Fatalf("aside = %q err=%v, want the unrelated aside left in place", got, readErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".recovery") {
			t.Fatalf("compensation relocated an object to %s", entry.Name())
		}
	}
}

func TestWriteRecoveryCleanupQueuePropagatesSyncFailure(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	recordPath, err := recoveryCleanupRecordPath(targetPath)
	if err != nil {
		t.Fatalf("recoveryCleanupRecordPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(recordPath, []byte(`{"records":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile existing record: %v", err)
	}

	syncErr := errors.New("injected FlushFileBuffers failure")
	originalSync := syncRecoveryCleanupFile
	originalMove := moveRecoveryCleanupFile
	syncRecoveryCleanupFile = func(*os.File) error { return syncErr }
	moveCalled := false
	moveRecoveryCleanupFile = func(*uint16, *uint16, uint32) error {
		moveCalled = true
		return nil
	}
	t.Cleanup(func() {
		syncRecoveryCleanupFile = originalSync
		moveRecoveryCleanupFile = originalMove
	})

	err = writeRecoveryCleanupQueue(targetPath, recoveryCleanupQueue{Records: []recoveryCleanupRecord{{Path: "unpublished"}}})
	if !errors.Is(err, syncErr) {
		t.Fatalf("writeRecoveryCleanupQueue error = %v, want sync failure", err)
	}
	if moveCalled {
		t.Fatal("recovery record was replaced after its content flush failed")
	}
	got, readErr := os.ReadFile(recordPath)
	if readErr != nil || string(got) != `{"records":[]}` {
		t.Fatalf("record after sync failure = %q err=%v, want prior durable state", got, readErr)
	}
}

func TestReplaceRecoveryCleanupFileUsesWriteThrough(t *testing.T) {
	originalMove := moveRecoveryCleanupFile
	var gotFlags uint32
	moveRecoveryCleanupFile = func(_ *uint16, _ *uint16, flags uint32) error {
		gotFlags = flags
		return nil
	}
	t.Cleanup(func() { moveRecoveryCleanupFile = originalMove })

	if err := replaceRecoveryCleanupFile(`C:\\state\\record.tmp`, `C:\\state\\record.json`); err != nil {
		t.Fatalf("replaceRecoveryCleanupFile: %v", err)
	}
	wantFlags := uint32(windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH)
	if gotFlags != wantFlags {
		t.Fatalf("MoveFileEx flags = %#x, want %#x", gotFlags, wantFlags)
	}
}

// TestPreflightFailsClosedWhenRecoveryRecordIsUnreadable: a truncated or
// unparseable per-user state file is not "nothing to report". The file exists
// to remember that something went wrong; refusing to parse it must refuse the
// next promotion rather than silently switch the refusal off.
func TestPreflightFailsClosedWhenRecoveryRecordIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("installed"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	recordPath, err := recoveryCleanupRecordPath(targetPath)
	if err != nil {
		t.Fatalf("recoveryCleanupRecordPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(recordPath, []byte("{truncated"), 0o600); err != nil {
		t.Fatalf("WriteFile truncated record: %v", err)
	}

	if _, err := loadRecoveryCleanupQueue(targetPath); err == nil {
		t.Fatal("loadRecoveryCleanupQueue accepted truncated JSON, want a parse error")
	}
	if err := preflightRecoveryState(targetPath); !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("preflight = %v, want fail-closed ErrTargetPossiblyTampered", err)
	}

	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	err = installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary = %v, want refusal when recovery state is unreadable", err)
	}
	if got, readErr := os.ReadFile(targetPath); readErr != nil || string(got) != "installed" {
		t.Fatalf("target = %q err=%v, want the installed binary left in place", got, readErr)
	}
}

// TestLoadRecoveryCleanupQueueMissingIsEmpty keeps a missing state file as
// "nothing to report", distinct from an unreadable file that must fail closed.
func TestLoadRecoveryCleanupQueueMissingIsEmpty(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	queue, err := loadRecoveryCleanupQueue(targetPath)
	if err != nil {
		t.Fatalf("missing record: %v, want empty queue not an error", err)
	}
	if len(queue.Records) != 0 {
		t.Fatalf("records = %+v, want none", queue.Records)
	}
}

// TestSuccessfulPromoteCleanupRecordDoesNotBlockLaterUpdate pins the success
// path for #868: a verified promotion still writes a cleanup record, that
// record is not unresolved, and the next update proceeds.
func TestSuccessfulPromoteCleanupRecordDoesNotBlockLaterUpdate(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	if err := installBinary(sourcePath, targetPath); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	queue, loadErr := loadRecoveryCleanupQueue(targetPath)
	if loadErr != nil {
		t.Fatalf("loadRecoveryCleanupQueue: %v", loadErr)
	}
	if len(queue.Records) != 1 {
		t.Fatalf("cleanup records after success = %d, want 1", len(queue.Records))
	}
	if queue.Records[0].Unresolved {
		t.Fatal("successful promotion must not record unresolved recovery state")
	}
	if err := preflightRecoveryState(targetPath); err != nil {
		t.Fatalf("preflight after successful promotion: %v", err)
	}

	nextSource := filepath.Join(t.TempDir(), "newer-binary")
	if err := os.WriteFile(nextSource, []byte("next-verified"), 0o755); err != nil {
		t.Fatalf("WriteFile next source: %v", err)
	}
	if err := installBinary(nextSource, targetPath); err != nil {
		t.Fatalf("second installBinary after success: %v", err)
	}
	installed, err := os.ReadFile(targetPath)
	if err != nil || string(installed) != "next-verified" {
		t.Fatalf("installed binary = %q err=%v, want next-verified", installed, err)
	}
}

// TestPrepareRecoveryCleanupSkipsUnresolvedRecords keeps a failed-restore copy
// out of destructive cleanup even if the sibling marker is already gone.
func TestPrepareRecoveryCleanupSkipsUnresolvedRecords(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	recoveryPath := targetPath + ".zero-update-0123456789abcdef0123456789abcdef.old"
	if err := os.WriteFile(recoveryPath, []byte("last-verified"), 0o755); err != nil {
		t.Fatalf("WriteFile recovery: %v", err)
	}
	original, err := openRecoveryCopy(recoveryPath)
	if err != nil {
		t.Fatalf("openRecoveryCopy: %v", err)
	}
	identity, err := recoveryFileIdentity(original)
	_ = original.Close()
	if err != nil {
		t.Fatalf("recoveryFileIdentity: %v", err)
	}
	if err := appendUnresolvedRecoveryRecord(targetPath, recoveryPath, identity); err != nil {
		t.Fatalf("appendUnresolvedRecoveryRecord: %v", err)
	}

	candidates := prepareRecoveryCleanup(targetPath)
	if len(candidates) != 0 {
		t.Fatalf("cleanup candidates = %d, want unresolved copies skipped", len(candidates))
	}
	closeRecoveryCleanupCandidates(candidates)
	if got, err := os.ReadFile(recoveryPath); err != nil || string(got) != "last-verified" {
		t.Fatalf("unresolved recovery copy = %q err=%v, want it left untouched", got, err)
	}
	if got := recordedRecoveryCleanupCount(t, targetPath); got != 1 {
		t.Fatalf("recorded entries = %d, want the unresolved record retained", got)
	}
}

func TestPromoteRefusesRetryAfterInterruptedAside(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	oldPath := targetPath + ".old"
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile recovery copy: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target was unexpectedly created: %v", err)
	}
	if data, err := os.ReadFile(oldPath); err != nil || string(data) != "known-good" {
		t.Fatalf("recovery copy = %q err=%v, want known-good", data, err)
	}
}

func TestPromoteRefusesMarkedRandomAsideRecovery(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	canonicalOld := targetPath + ".old"
	randomOld := targetPath + ".deadbeef.old"
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(canonicalOld, []byte("stale-older-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile canonical recovery: %v", err)
	}
	if err := os.WriteFile(randomOld, []byte("last-known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile random recovery: %v", err)
	}
	if err := markOldBinaryPreserved(randomOld); err != nil {
		t.Fatalf("mark random recovery: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	for _, want := range []string{randomOld, randomOld + oldBinaryPreservedSuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want marked random recovery path %s", err, want)
		}
	}
	if got, err := os.ReadFile(randomOld); err != nil || string(got) != "last-known-good" {
		t.Fatalf("random recovery = %q err=%v, want last-known-good", got, err)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != "unverified" {
		t.Fatalf("target = %q err=%v, want refusal before promotion", got, err)
	}
}

func TestPromoteRefusesAmbiguousRecoveryWhenTargetIsMissing(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	canonicalOld := targetPath + ".old"
	randomOld := targetPath + ".deadbeef.old"
	if err := os.WriteFile(canonicalOld, []byte("older-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile canonical recovery: %v", err)
	}
	if err := os.WriteFile(randomOld, []byte("last-running-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile random recovery: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	for _, want := range []string{"ambiguous", canonicalOld, randomOld} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target was unexpectedly created: %v", err)
	}
}

func TestVerifyPromotedTargetRejectsDifferentRegularFile(t *testing.T) {
	dir := t.TempDir()
	stagedPath := filepath.Join(dir, "staged.exe")
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(stagedPath, []byte("verified"), 0o755); err != nil {
		t.Fatalf("WriteFile staged: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("attacker"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	staged, err := os.Open(stagedPath)
	if err != nil {
		t.Fatalf("Open staged: %v", err)
	}
	defer func() { _ = staged.Close() }()

	if err := verifyPromotedTarget(staged, targetPath); err == nil {
		t.Fatal("verifyPromotedTarget accepted a different regular file at targetPath")
	}
}

func TestVerifyPromotedTargetRejectsAHardLinkedName(t *testing.T) {
	dir := t.TempDir()
	stagedPath := filepath.Join(dir, "staged.exe")
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(stagedPath, []byte("verified"), 0o755); err != nil {
		t.Fatalf("WriteFile staged: %v", err)
	}
	if err := os.Link(stagedPath, targetPath); err != nil {
		t.Fatalf("Link target: %v", err)
	}
	staged, err := os.Open(stagedPath)
	if err != nil {
		t.Fatalf("Open staged: %v", err)
	}
	defer func() { _ = staged.Close() }()

	if err := verifyPromotedTarget(staged, targetPath); err == nil {
		t.Fatal("verifyPromotedTarget accepted a second hard-linked name without a completed rename")
	}
}

func TestInstallBinaryThroughReparsePointAncestor(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir real install directory: %v", err)
	}
	linkedDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	targetPath := filepath.Join(linkedDir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	if err := installBinary(sourcePath, targetPath); err != nil {
		t.Fatalf("installBinary through reparse-point ancestor: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(realDir, "zero.exe"))
	if err != nil {
		t.Fatalf("ReadFile installed: %v", err)
	}
	if string(installed) != "verified-binary" {
		t.Fatalf("installed binary = %q, want the verified bytes", installed)
	}
}

// TestInstallBinaryInstallsVerifiedBytes is the success control for the ordinary
// path: the staged bytes land at the target, the running binary is preserved at
// an updater-owned recovery path, and no staging artifact survives.
func TestInstallBinaryInstallsVerifiedBytes(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	if err := installBinary(sourcePath, targetPath); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	installed, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile installed: %v", err)
	}
	if string(installed) != "verified-binary" {
		t.Fatalf("installed binary = %q, want the verified bytes", installed)
	}
	recoveries, err := existingRecoveryPaths(targetPath)
	if err != nil {
		t.Fatalf("existingRecoveryPaths: %v", err)
	}
	if len(recoveries) != 1 || !strings.Contains(filepath.Base(recoveries[0]), ".zero-update-") {
		t.Fatalf("recovery paths = %v, want one namespaced updater recovery", recoveries)
	}
	if old, err := os.ReadFile(recoveries[0]); err != nil {
		t.Fatalf("the replaced binary must be preserved as the recovery copy: %v", err)
	} else if string(old) != "old-binary" {
		t.Fatalf("preserved binary = %q, want the previous one", old)
	}
	assertNoStagingLeftovers(t, dir)
}

func TestInstallBinaryPreservesArbitraryOldFilesDuringCleanup(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("version-0"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	manualBackup := targetPath + ".before-manual-patch.old"
	if err := os.WriteFile(manualBackup, []byte("manual-backup"), 0o755); err != nil {
		t.Fatalf("WriteFile manual backup: %v", err)
	}
	// Also cover a name that resembles the updater namespace but does not carry
	// the exact 128-bit hexadecimal suffix generated by randomStagingSuffix.
	lookalike := targetPath + ".zero-update-not-owned.old"
	if err := os.WriteFile(lookalike, []byte("lookalike-backup"), 0o755); err != nil {
		t.Fatalf("WriteFile lookalike backup: %v", err)
	}
	plantedNamespaced := targetPath + ".zero-update-00000000000000000000000000000000.old"
	if err := os.WriteFile(plantedNamespaced, []byte("planted-namespaced-backup"), 0o755); err != nil {
		t.Fatalf("WriteFile planted namespaced backup: %v", err)
	}

	for version := 1; version <= 3; version++ {
		sourcePath := filepath.Join(t.TempDir(), "new-binary")
		if err := os.WriteFile(sourcePath, []byte(fmt.Sprintf("version-%d", version)), 0o755); err != nil {
			t.Fatalf("WriteFile source: %v", err)
		}
		if err := installBinary(sourcePath, targetPath); err != nil {
			t.Fatalf("installBinary version %d: %v", version, err)
		}
	}

	for path, want := range map[string]string{
		manualBackup:      "manual-backup",
		lookalike:         "lookalike-backup",
		plantedNamespaced: "planted-namespaced-backup",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("backup %s = %q err=%v, want %q", path, got, err, want)
		}
	}
}

// TestRecoveryCleanupRefusesSubstitutedAside pins the provenance rule that
// makes handle-bound cleanup safe: a recorded recovery path whose object has
// been swapped out underneath the record is never opened as a cleanup
// candidate, so the substitute is not deleted on the next promotion.
func TestRecoveryCleanupRefusesSubstitutedAside(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	recoveryPath := targetPath + ".zero-update-0123456789abcdef0123456789abcdef.old"
	if err := os.WriteFile(recoveryPath, []byte("moved-aside-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile recovery: %v", err)
	}
	original, err := openRecoveryCopy(recoveryPath)
	if err != nil {
		t.Fatalf("openRecoveryCopy: %v", err)
	}
	identity, err := recoveryFileIdentity(original)
	_ = original.Close()
	if err != nil {
		t.Fatalf("recoveryFileIdentity: %v", err)
	}
	if err := appendRecoveryCleanupRecord(targetPath, recoveryPath, identity); err != nil {
		t.Fatalf("appendRecoveryCleanupRecord: %v", err)
	}
	if err := os.Rename(recoveryPath, recoveryPath+".displaced"); err != nil {
		t.Fatalf("Rename recovery: %v", err)
	}
	if err := os.WriteFile(recoveryPath, []byte("substituted-file"), 0o755); err != nil {
		t.Fatalf("WriteFile substitute: %v", err)
	}

	candidates := prepareRecoveryCleanup(targetPath)
	if len(candidates) != 0 {
		t.Fatalf("cleanup candidates = %d, want the substituted aside to be refused", len(candidates))
	}
	cleanupSupersededRecoveryCopies(targetPath, candidates)
	if got, err := os.ReadFile(recoveryPath); err != nil || string(got) != "substituted-file" {
		t.Fatalf("substituted file = %q err=%v, want it left untouched", got, err)
	}
}

// TestAppendRecoveryCleanupRecordRejectsForeignPath keeps the trusted record
// from ever vouching for a name this updater could not have created, which is
// what stops cleanup from deleting an operator's own backup.
func TestAppendRecoveryCleanupRecordRejectsForeignPath(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	foreign := targetPath + ".before-manual-patch.old"
	if err := os.WriteFile(foreign, []byte("operator-backup"), 0o755); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}
	if err := appendRecoveryCleanupRecord(targetPath, foreign, recoveryIdentity{}); err == nil {
		t.Fatal("appendRecoveryCleanupRecord accepted a path this updater never created")
	}
	recordPath, err := recoveryCleanupRecordPath(targetPath)
	if err != nil {
		t.Fatalf("recoveryCleanupRecordPath: %v", err)
	}
	if _, err := os.Lstat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trusted cleanup record was written for a foreign backup: %v", err)
	}
}

// TestRecoveryCleanupRetiresRecordsForVanishedCopies keeps the backlog that
// makes transient-lock retries possible from becoming its own unbounded growth:
// a record whose object an operator already removed can never become
// actionable, so it must not be carried forever.
func TestRecoveryCleanupRetiresRecordsForVanishedCopies(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("version-0"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	source := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(source, []byte("version-1"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	if err := installBinary(source, targetPath); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	recoveries, err := existingRecoveryPaths(targetPath)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("recovery paths = %v err=%v, want exactly one", recoveries, err)
	}
	if got := recordedRecoveryCleanupCount(t, targetPath); got != 1 {
		t.Fatalf("recorded cleanup entries = %d, want 1", got)
	}
	// The operator removes the recovery copy themselves.
	if err := os.Remove(recoveries[0]); err != nil {
		t.Fatalf("Remove recovery copy: %v", err)
	}

	closeRecoveryCleanupCandidates(prepareRecoveryCleanup(targetPath))

	if got := recordedRecoveryCleanupCount(t, targetPath); got != 0 {
		t.Fatalf("recorded cleanup entries = %d after the copy vanished, want the record retired", got)
	}
}

func recordedRecoveryCleanupCount(t *testing.T, targetPath string) int {
	t.Helper()
	queue, err := loadRecoveryCleanupQueue(targetPath)
	if err != nil {
		t.Fatalf("loadRecoveryCleanupQueue: %v", err)
	}
	return len(queue.Records)
}

func TestInstallBinaryBoundsRecoveryCopiesAcrossRepeatedUpgrades(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("version-0"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	for version := 1; version <= 4; version++ {
		contents := fmt.Sprintf("version-%d", version)
		sourcePath := filepath.Join(t.TempDir(), "new-binary")
		if err := os.WriteFile(sourcePath, []byte(contents), 0o755); err != nil {
			t.Fatalf("WriteFile source: %v", err)
		}
		if err := installBinary(sourcePath, targetPath); err != nil {
			t.Fatalf("installBinary version %d: %v", version, err)
		}
		recoveries, err := existingRecoveryPaths(targetPath)
		if err != nil {
			t.Fatalf("existingRecoveryPaths: %v", err)
		}
		if len(recoveries) != 1 {
			t.Fatalf("recovery count after version %d = %d (%v), want 1", version, len(recoveries), recoveries)
		}
		previous, err := os.ReadFile(recoveries[0])
		if err != nil {
			t.Fatalf("ReadFile recovery after version %d: %v", version, err)
		}
		wantPrevious := fmt.Sprintf("version-%d", version-1)
		if string(previous) != wantPrevious {
			t.Fatalf("recovery after version %d = %q, want %q", version, previous, wantPrevious)
		}
	}
}

func TestInstallBinaryRetainsLockedCleanupRecordUntilLaterRetry(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("version-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	install := func(version string) {
		source := filepath.Join(t.TempDir(), "new-binary")
		if err := os.WriteFile(source, []byte(version), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := installBinary(source, targetPath); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
	}
	install("version-1")
	recoveries, _ := existingRecoveryPaths(targetPath)
	lockedPath, err := windows.UTF16PtrFromString(recoveries[0])
	if err != nil {
		t.Fatal(err)
	}
	locked, err := windows.CreateFile(lockedPath, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	install("version-2")
	_ = windows.CloseHandle(locked)
	install("version-3")
	recoveries, _ = existingRecoveryPaths(targetPath)
	if len(recoveries) != 1 {
		t.Fatalf("recovery backlog after lock clears = %v, want only newest recovery", recoveries)
	}
}

func TestPromoteRestoresOriginalObjectWhenAsidePathIsSubstituted(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("known-good"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, err := createStagedBinary(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.discard()
	if _, err := staged.file.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	originalRename := renameFileByHandle
	originalRecoveryRename := renameRecoveryFileByHandle
	var substitutionErr error
	call := 0
	renameRecoveryFileByHandle = func(file *os.File, path string) error {
		call++
		if err := originalRecoveryRename(file, path); err != nil {
			return err
		}
		if call == 1 {
			substitutionErr = os.Rename(path, path+".stolen")
		}
		return nil
	}
	renameFileByHandle = func(_ *os.File, _ string) error {
		return errors.New("injected promotion failure")
	}
	defer func() {
		renameFileByHandle = originalRename
		renameRecoveryFileByHandle = originalRecoveryRename
	}()
	if err := staged.promote(targetPath); err == nil {
		t.Fatal("promote succeeded despite injected failure")
	}
	contents, err := os.ReadFile(targetPath)
	if err != nil || string(contents) != "known-good" {
		t.Fatalf("restored target = %q, %v; want original object", contents, err)
	}
	if substitutionErr == nil {
		t.Fatal("aside pathname substitution succeeded while recovery handle was retained")
	}
}

func TestInstallBinaryRefusesRelocatedRecoveryCopy(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	recoveryPath := targetPath + ".old.deadbeef.recovery"
	if err := os.WriteFile(recoveryPath, []byte("last-verified"), 0o755); err != nil {
		t.Fatalf("WriteFile recovery: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-release"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	if !strings.Contains(err.Error(), recoveryPath) {
		t.Fatalf("installBinary error = %v, want recovery path %s", err, recoveryPath)
	}
	if got, readErr := os.ReadFile(targetPath); readErr != nil || string(got) != "unverified" {
		t.Fatalf("target = %q err=%v, want unchanged unverified bytes", got, readErr)
	}
	if got, readErr := os.ReadFile(recoveryPath); readErr != nil || string(got) != "last-verified" {
		t.Fatalf("recovery = %q err=%v, want last-verified", got, readErr)
	}
}

func TestInstallBinaryRefusesRecoveryRelocatedFromRandomizedAside(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	recoveryPath := targetPath + ".aside.old.relocation.recovery"
	if err := os.WriteFile(recoveryPath, []byte("last-verified"), 0o755); err != nil {
		t.Fatalf("WriteFile recovery: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-release"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	err := installBinary(sourcePath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("installBinary error = %v, want ErrTargetPossiblyTampered", err)
	}
	if got, readErr := os.ReadFile(recoveryPath); readErr != nil || string(got) != "last-verified" {
		t.Fatalf("recovery = %q err=%v, want last-verified", got, readErr)
	}
}

func TestPromotionLockSerializesSameTarget(t *testing.T) {
	targetPath := filepath.Join(t.TempDir(), "zero.exe")
	releaseFirst, err := acquirePromotionLock(targetPath)
	if err != nil {
		t.Fatalf("acquire first promotion lock: %v", err)
	}

	started := make(chan struct{})
	acquired := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(started)
		release, err := acquirePromotionLock(strings.ToUpper(targetPath))
		if err != nil {
			secondResult <- err
			return
		}
		close(acquired)
		<-releaseSecond
		release()
		secondResult <- nil
	}()
	<-started
	select {
	case <-acquired:
		releaseFirst()
		close(releaseSecond)
		<-secondResult
		t.Fatal("second promotion acquired the same target lock before release")
	case err := <-secondResult:
		releaseFirst()
		t.Fatalf("acquire second promotion lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case <-acquired:
		close(releaseSecond)
		if err := <-secondResult; err != nil {
			t.Fatalf("release second promotion lock: %v", err)
		}
	case err := <-secondResult:
		t.Fatalf("acquire second promotion lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second promotion did not acquire the target lock after release")
	}
}

// TestInstallBinaryCleansUpWhenStagingFails covers the cleanup ordering: a
// failure after the staging file exists must not leave it behind, because each
// attempt now uses a fresh random name that the next attempt never reuses.
func TestInstallBinaryCleansUpWhenStagingFails(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	if err := installBinary(filepath.Join(t.TempDir(), "missing-source"), targetPath); err == nil {
		t.Fatal("installBinary with an unreadable source succeeded, want error")
	}
	assertNoStagingLeftovers(t, dir)
}

// TestDiscardDeletesTheStagedObjectThroughItsHandle pins that failure-path
// cleanup removes the object this updater staged rather than re-resolving the
// staging name after the handle is released.
func TestDiscardDeletesTheStagedObjectThroughItsHandle(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	staged, err := createStagedBinary(targetPath)
	if err != nil {
		t.Fatalf("createStagedBinary: %v", err)
	}
	stagingPath := staged.path

	staged.discard()

	if _, err := os.Lstat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged object survived discard: %v", err)
	}
	assertNoStagingLeftovers(t, dir)
}

// TestDiscardLeavesASubstitutedStagingEntryAlone is the impostor-survival
// counterpart of the POSIX promote tests: cleanup is bound to the staged
// object, so an entry a principal who can write the installation directory
// plants at the staging name once the handle is gone is never deleted. The
// substitution is staged by hand here because the exclusive, no-share handle
// makes it impossible while the updater still holds the object.
func TestDiscardLeavesASubstitutedStagingEntryAlone(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	staged, err := createStagedBinary(targetPath)
	if err != nil {
		t.Fatalf("createStagedBinary: %v", err)
	}
	stagingPath := staged.path
	// Run the handle-bound half of discard, then let the substitution win the
	// race that a pathname-based removal would lose.
	staged.discardOpenObject()
	if err := staged.file.Close(); err != nil {
		t.Fatalf("Close staged handle: %v", err)
	}
	const impostorContent = "attacker-owned file that cleanup must not delete"
	if err := os.WriteFile(stagingPath, []byte(impostorContent), 0o600); err != nil {
		t.Fatalf("WriteFile impostor: %v", err)
	}

	staged.discardPaths()

	got, err := os.ReadFile(stagingPath)
	if err != nil {
		t.Fatalf("cleanup removed the substituted staging entry: %v", err)
	}
	if string(got) != impostorContent {
		t.Fatalf("substituted entry = %q, want it left untouched", got)
	}
}
