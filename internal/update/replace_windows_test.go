//go:build windows

package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The replacement path itself (rename the running binary aside, then rename the
// staged object into place through its handle) is covered by
// TestInstallBinaryInstallsVerifiedBytes and
// TestPromoteInstallsTheStagedObjectNotTheStagedPath in
// stage_promote_windows_test.go, which exercise it through the staging handle the
// production code uses rather than a loose pathname.

func TestRenameOpenFileWithRetrySucceedsImmediately(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}
	file, err := openRecoveryCopy(src)
	if err != nil {
		t.Fatalf("openRecoveryCopy: %v", err)
	}
	defer func() { _ = file.Close() }()

	if err := renameOpenFileWithRetry(file, dst); err != nil {
		t.Fatalf("renameOpenFileWithRetry: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected dst to exist after rename: %v", err)
	}
}

// A permanently-failing rename (the destination is held by a conflicting
// exclusive handle for good) must exhaust its retries and surface the
// underlying error, rather than retrying forever.
func TestRenameOpenFileWithRetryFailsAfterExhaustingAttempts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	for _, path := range []string{src, dst} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	file, err := openRecoveryCopy(src)
	if err != nil {
		t.Fatalf("openRecoveryCopy: %v", err)
	}
	defer func() { _ = file.Close() }()
	blocker, err := openWithoutSharing(dst)
	if err != nil {
		t.Skipf("cannot hold the destination exclusively on this filesystem: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	if err := renameOpenFileWithRetry(file, dst); err == nil {
		t.Fatal("expected renameOpenFileWithRetry to fail against a permanently blocked destination")
	}
}

// TestRestoreOriginalBinaryFlagsPossibleTamperingWhenRestoreFails is the
// regression test for a review finding on PR #751: when a promotion attempt
// fails AND the restore of the preserved original also cannot get past
// whatever now occupies targetPath, that combination must be reported as a
// security-relevant condition (ErrTargetPossiblyTampered), not folded into
// the same generic error a stalled download would produce — the caller needs
// to be able to tell "try the update again later" apart from "verify what is
// at this path before running it again".
func TestRestoreOriginalBinaryFlagsPossibleTamperingWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "zero.exe.old")
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(oldPath, []byte("original"), 0o755); err != nil {
		t.Fatalf("WriteFile oldPath: %v", err)
	}

	// Simulate an attacker occupying targetPath with a lock MOVEFILE_REPLACE_EXISTING
	// cannot get past: an exclusive, no-share open.
	pathPtr, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	handle, err := windows.CreateFile(pathPtr, windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("CreateFile targetPath: %v", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	restoreErr := restoreOriginalBinary(openRecoveryHandle(t, oldPath), oldPath, targetPath)
	if restoreErr == nil {
		t.Fatal("restoreOriginalBinary succeeded despite a conflicting exclusive lock on targetPath, want an error")
	}
	if !errors.Is(restoreErr, ErrTargetPossiblyTampered) {
		t.Fatalf("error = %v, want it to wrap ErrTargetPossiblyTampered", restoreErr)
	}
}

// TestRestoreOriginalBinaryMarksPreservedCopy pins the other half: the path that
// reports "original preserved at <.old>" is the path that makes that true across
// runs.
func TestRestoreOriginalBinaryMarksPreservedCopy(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	oldPath := targetPath + ".old"
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile old binary: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	// Hold the target with no sharing so the restore rename cannot replace it,
	// which is the condition ErrTargetPossiblyTampered describes.
	blocker, err := openWithoutSharing(targetPath)
	if err != nil {
		t.Skipf("cannot hold the target exclusively on this filesystem: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	recovery := openRecoveryHandle(t, oldPath)
	err = restoreOriginalBinary(recovery, oldPath, targetPath)
	// Release the handle promote would hold, so these assertions can read the
	// files it was keeping unsubstitutable.
	_ = recovery.Close()
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("restore error = %v, want ErrTargetPossiblyTampered", err)
	}
	if !oldBinaryPreserved(oldPath) {
		t.Fatal("a failed restore must mark the preserved copy so later cleanup keeps it")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("recovery copy was removed after a failed restore: %v", err)
	}
}

// TestMarkOldBinaryPreservedRefusesPreCreatedLink covers jatmn's #751 finding
// that the marker was a predictable link-following truncate write: the path is
// fixed, so a lower-privileged writer in the install directory can pre-create it
// as a hard link (or reparse point) and have the elevated updater truncate and
// write through it into a file of their choosing.
func TestMarkOldBinaryPreservedRefusesPreCreatedLink(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			oldPath := filepath.Join(dir, "zero.exe.old")
			if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
				t.Fatalf("WriteFile old binary: %v", err)
			}
			victim := filepath.Join(t.TempDir(), "victim.txt")
			const victimContent = "attacker-chosen target that must not be written"
			if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
				t.Fatalf("WriteFile victim: %v", err)
			}

			markerPath := oldPath + oldBinaryPreservedSuffix
			var linkErr error
			switch kind {
			case "hardlink":
				linkErr = os.Link(victim, markerPath)
			case "symlink":
				linkErr = os.Symlink(victim, markerPath)
			}
			if linkErr != nil {
				t.Skipf("%s unsupported here: %v", kind, linkErr)
			}

			// Whatever this returns, the one thing it must not do is write through
			// the planted object. Reporting the marker as present is the safe
			// answer: it makes the next run PRESERVE the recovery copy.
			_ = markOldBinaryPreserved(oldPath)

			got, err := os.ReadFile(victim)
			if err != nil {
				t.Fatalf("ReadFile victim: %v", err)
			}
			if string(got) != victimContent {
				t.Fatalf("marker write followed the planted %s and wrote into %q: %q", kind, victim, got)
			}
			// The recovery copy itself is untouched, which is the marker's purpose.
			if _, err := os.Stat(oldPath); err != nil {
				t.Fatalf("recovery copy was removed while planting the marker: %v", err)
			}
		})
	}
}

func TestMarkOldBinaryPreservedRemovesPartialMarkerBeforeRelocation(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	oldPath := targetPath + ".old"
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile old binary: %v", err)
	}
	blocker, err := openWithoutSharing(targetPath)
	if err != nil {
		t.Skipf("cannot hold the target exclusively on this filesystem: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	originalWrite := writeRecoveryMarker
	writeRecoveryMarker = func(marker *os.File) error {
		_, _ = marker.WriteString("partial")
		return errors.New("injected marker write failure")
	}
	t.Cleanup(func() { writeRecoveryMarker = originalWrite })
	stubRandomStagingSuffix(t, "deadbeef")

	recovery := openRecoveryHandle(t, oldPath)
	err = restoreOriginalBinary(recovery, oldPath, targetPath)
	// Release the handle promote would hold, so these assertions can read the
	// files it was keeping unsubstitutable.
	_ = recovery.Close()
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("restore error = %v, want ErrTargetPossiblyTampered", err)
	}
	kept := oldPath + ".deadbeef.recovery"
	if !strings.Contains(err.Error(), kept) {
		t.Fatalf("error = %v, want relocated recovery path %s", err, kept)
	}
	if _, err := os.Lstat(oldPath + oldBinaryPreservedSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial marker survived relocation: %v", err)
	}
	if got, err := os.ReadFile(kept); err != nil || string(got) != "known-good" {
		t.Fatalf("relocated recovery = %q err=%v, want known-good", got, err)
	}
}

// TestRestoreOriginalBinaryKeepsRecoveryCopyWhenMarkingFails covers the half of
// CodeRabbit's marker finding that surfacing the failure alone does not: when no
// marker can be established, nothing on disk tells the next run to keep the
// copy, so it is moved out from under routine cleanup instead of being left at
// the ordinary "<target>.old" recovery name.
func TestRestoreOriginalBinaryKeepsRecoveryCopyWhenMarkingFails(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	oldPath := targetPath + ".old"
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile old binary: %v", err)
	}
	// Hold the target with no sharing so the restore rename fails.
	blocker, err := openWithoutSharing(targetPath)
	if err != nil {
		t.Skipf("cannot hold the target exclusively on this filesystem: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	// Force the marker to be unestablishable. Doing it through the seam rather
	// than by breaking the filesystem keeps oldPath itself intact, which is the
	// state this behavior is about.
	originalMark := markOldBinaryPreserved
	markOldBinaryPreserved = func(string) error { return errors.New("injected marker failure") }
	t.Cleanup(func() { markOldBinaryPreserved = originalMark })
	stubRandomStagingSuffix(t, "deadbeef")

	recovery := openRecoveryHandle(t, oldPath)
	err = restoreOriginalBinary(recovery, oldPath, targetPath)
	// Release the handle promote would hold, so these assertions can read the
	// files it was keeping unsubstitutable.
	_ = recovery.Close()
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("restore error = %v, want ErrTargetPossiblyTampered", err)
	}
	kept := oldPath + ".deadbeef.recovery"
	if !strings.Contains(err.Error(), kept) {
		t.Fatalf("error = %v, want it to name the path the copy was moved to", err)
	}
	got, readErr := os.ReadFile(kept)
	if readErr != nil {
		t.Fatalf("recovery copy was not kept: %v", readErr)
	}
	if string(got) != "known-good" {
		t.Fatalf("kept copy = %q, want the last verified binary", got)
	}
	if _, err := os.Lstat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old recovery path still exists after move: %v", err)
	}
}

// TestOldBinaryPreservedTreatsAnUnreadableMarkerAsPresent pins the conservative
// side of the marker check: only a definite "not there" allows the copy to be
// deleted, because deleting it is irreversible and keeping it costs a file.
func TestOldBinaryPreservedTreatsAnUnreadableMarkerAsPresent(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "zero.exe.old")
	if oldBinaryPreserved(oldPath) {
		t.Fatal("a genuinely absent marker must report not-preserved")
	}
	// A directory at the marker name is an entry Lstat can see; anything other
	// than a clean not-exist keeps the copy.
	if err := os.Mkdir(oldPath+oldBinaryPreservedSuffix, 0o700); err != nil {
		t.Fatalf("Mkdir marker: %v", err)
	}
	if !oldBinaryPreserved(oldPath) {
		t.Fatal("an entry at the marker name must count as preserved")
	}
}

// TestRestoreOriginalBinarySurfacesMarkerWriteFailure covers the #751 P3: the
// error promises the original is preserved at <target>.old, but if the marker
// cannot be written the next run's cleanup deletes exactly that file. The
// operator has to be told to act now rather than at their convenience.
func TestRestoreOriginalBinarySurfacesMarkerWriteFailure(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	oldPath := targetPath + ".old"
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile old binary: %v", err)
	}
	file := openRecoveryHandle(t, oldPath)
	// Neither the restore nor the marker nor the relocation can succeed, so the
	// recovery copy stays at oldPath and the operator must be pointed there.
	originalMark := markOldBinaryPreserved
	markOldBinaryPreserved = func(string) error { return errors.New("injected marker failure") }
	t.Cleanup(func() { markOldBinaryPreserved = originalMark })
	originalRename := renameRecoveryFileByHandle
	renameRecoveryFileByHandle = func(*os.File, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { renameRecoveryFileByHandle = originalRename })

	err := restoreOriginalBinary(file, oldPath, targetPath)
	_ = file.Close()
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("restore error = %v, want ErrTargetPossiblyTampered", err)
	}
	if !strings.Contains(err.Error(), "recovery marker could not be written") {
		t.Fatalf("error = %v, want it to disclose the failed marker", err)
	}
	if !strings.Contains(err.Error(), oldPath) {
		t.Fatalf("error = %v, want the path the operator must copy", err)
	}
	if strings.Contains(err.Error(), "later update") {
		t.Fatalf("error = %v, must not promise cleanup that no longer exists", err)
	}
	if got, readErr := os.ReadFile(oldPath); readErr != nil || string(got) != "known-good" {
		t.Fatalf("recovery copy at %s = %q err=%v, want the last verified binary", oldPath, got, readErr)
	}
}

func TestKeepUnmarkedRecoveryCopyMovesTheOpenedObject(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "zero.exe.old")
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile recovery copy: %v", err)
	}
	stubRandomStagingSuffix(t, "deadbeef")

	originalRename := renameRecoveryFileByHandle
	renameRecoveryFileByHandle = func(file *os.File, kept string) error {
		// Try to substitute oldPath after keepUnmarkedRecoveryCopy has opened and
		// verified it. The exclusive delete sharing normally blocks this. Even
		// on a filesystem that permits it, the handle-bound rename must still
		// move the verified object rather than the replacement pathname entry.
		displaced := oldPath + ".attacker-moved"
		if err := os.Rename(oldPath, displaced); err == nil {
			if err := os.WriteFile(oldPath, []byte("attacker"), 0o755); err != nil {
				return fmt.Errorf("plant substituted recovery: %w", err)
			}
		}
		return renameOpenFile(file, kept)
	}
	t.Cleanup(func() { renameRecoveryFileByHandle = originalRename })

	recovery := openRecoveryHandle(t, oldPath)
	kept, err := keepUnmarkedRecoveryCopy(recovery, oldPath)
	_ = recovery.Close()
	if err != nil {
		t.Fatalf("keepUnmarkedRecoveryCopy: %v", err)
	}
	if got, err := os.ReadFile(kept); err != nil || string(got) != "known-good" {
		t.Fatalf("kept recovery = %q err=%v, want the verified object", got, err)
	}
}

// TestRestoreOriginalBinaryNamesTheRelocationItCouldNotVerify covers the gap
// jatmn flagged on the pathname restore and that the handle restore inherited:
// when the relocation rename succeeds but the post-move verification does not,
// oldPath has already been vacated, so an error naming oldPath sends the
// operator to a path that no longer holds anything.
func TestRestoreOriginalBinaryNamesTheRelocationItCouldNotVerify(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero.exe")
	oldPath := targetPath + ".old"
	if err := os.WriteFile(targetPath, []byte("unverified"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("known-good"), 0o755); err != nil {
		t.Fatalf("WriteFile old binary: %v", err)
	}
	file := openRecoveryHandle(t, oldPath)
	stubRandomStagingSuffix(t, "deadbeef")
	originalMark := markOldBinaryPreserved
	markOldBinaryPreserved = func(string) error { return errors.New("injected marker failure") }
	t.Cleanup(func() { markOldBinaryPreserved = originalMark })

	kept := oldPath + ".deadbeef.recovery"
	elsewhere := oldPath + ".deadbeef.elsewhere"
	originalRename := renameRecoveryFileByHandle
	renameRecoveryFileByHandle = func(handle *os.File, destination string) error {
		if destination != kept {
			return errors.New("injected restore failure")
		}
		// The rename reports success but lands somewhere else, so kept is
		// vacated and verification there fails.
		return renameOpenFile(handle, elsewhere)
	}
	t.Cleanup(func() { renameRecoveryFileByHandle = originalRename })

	err := restoreOriginalBinary(file, oldPath, targetPath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("restore error = %v, want ErrTargetPossiblyTampered", err)
	}
	if !strings.Contains(err.Error(), kept) {
		t.Fatalf("error = %v, want it to name the relocation path %s", err, kept)
	}
	if _, statErr := os.Lstat(oldPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oldPath still exists after the relocation rename: %v", statErr)
	}
}

// openRecoveryHandle opens path the way promote holds the binary it moved
// aside: with delete access and no delete sharing, so the object cannot be
// substituted while the restore path works with it.
func openRecoveryHandle(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := openRecoveryCopy(path)
	if err != nil {
		t.Fatalf("openRecoveryCopy %s: %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// openWithoutSharing opens an existing file denying every share mode, so a
// rename onto it fails the way a principal squatting the executable path does.
func openWithoutSharing(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
