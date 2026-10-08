package installtxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommitDirRestoresPreviousInstallWhenPublishFails(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "demo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := StageDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "version"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	publishErr := errors.New("publish failed")
	err = CommitDir(target, staged, func() error { return publishErr })
	if !errors.Is(err, publishErr) {
		t.Fatalf("CommitDir error = %v, want publish failure", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "version"))
	if err != nil {
		t.Fatalf("read restored install: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("restored content = %q, want old", data)
	}
}

func TestCommitDirRemovesFirstInstallWhenPublishFails(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "demo")
	staged, cleanup, err := StageDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}

	err = CommitDir(target, staged, func() error { return errors.New("publish failed") })
	if err == nil {
		t.Fatal("CommitDir unexpectedly succeeded")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed first install remains at target: %v", statErr)
	}
}

func TestCleanupWorkspacePreservesRetainedPreviousInstall(t *testing.T) {
	workspace := t.TempDir()
	previous := filepath.Join(workspace, "previous")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupWorkspace(workspace)

	if _, err := os.Stat(previous); err != nil {
		t.Fatalf("cleanup removed retained previous install: %v", err)
	}
}

// A retained backup is only recoverable if something can tell which install it
// came from, so CommitDir records the target before it moves anything. publish
// runs while the workspace is still in place, which is where that is visible.
func TestCommitDirRecordsItsTargetForRecovery(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "demo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := StageDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)

	var marker string
	var markerErr error
	if err := CommitDir(target, staged, func() error {
		data, err := os.ReadFile(filepath.Join(workspace, markerFileName))
		marker, markerErr = string(data), err
		return nil
	}); err != nil {
		t.Fatalf("CommitDir: %v", err)
	}

	if markerErr != nil {
		t.Fatalf("CommitDir left no way to attribute its backup: %v", markerErr)
	}
	if want := "zero-install-txn v1\ntarget demo\n"; marker != want {
		t.Fatalf("recorded marker = %q, want %q", marker, want)
	}
}

// writeMarker plants a workspace ownership marker verbatim, so the tests pin the
// exact bytes recovery has to see rather than whatever the writer happens to
// produce.
func writeMarker(t *testing.T, workspace string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, markerFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// plantInterruptedCommit builds what a process killed between CommitDir's two
// renames leaves in dir: a workspace naming its target, the live tree moved into
// the backup beside it, and nothing at the target.
func plantInterruptedCommit(t *testing.T, dir, name, recorded, content string) string {
	t.Helper()
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "version"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, _, err := StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeMarker(t, workspace, "zero-install-txn v1\ntarget "+recorded+"\n")
	if err := os.Rename(target, filepath.Join(workspace, "previous")); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestRecoverPutsBackAnInterruptedCommit(t *testing.T) {
	dir := t.TempDir()
	workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("the interrupted install was not put back: got %q err %v", data, err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the recovered workspace should be cleared, got %v", err)
	}
}

// A caller with no metadata beside its tree passes no reconciler, and then a
// tree at the target is the committed one: the backup beside it holds what that
// install replaced and is retired rather than kept, empty target or not.
// Keeping it let a later removal of the live tree hand the stale copy to the
// next recovery. Go's os.Rename refuses an existing directory on the platforms
// tested, but POSIX allows replacing an empty one, so the empty case pins the
// decision rather than one syscall's take on it. What recovery never does is
// destroy a tree without a complete copy in hand, or touch a target the
// metadata records: replacing a live target happens only on the reconciled
// PhasePrePublish path, which restores through a move aside.
func TestRecoverTreatsALiveInstallAsCommittedWithoutAReconciler(t *testing.T) {
	for _, tc := range []struct{ name, live string }{
		{"empty install", ""},
		{"populated install", "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")
			live := filepath.Join(dir, "demo")
			if err := os.MkdirAll(live, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.live != "" {
				if err := os.WriteFile(filepath.Join(live, "version"), []byte(tc.live), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := Recover(dir, nil); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(live, "version"))
			if tc.live == "" {
				if err == nil {
					t.Fatalf("an existing install was replaced by a backup: version = %q", data)
				}
			} else if err != nil || string(data) != tc.live {
				t.Fatalf("the live install must win: got %q err %v", data, err)
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Errorf("the superseded workspace should be retired, got %v", err)
			}
		})
	}
}

// The recorded target names a directory inside the install root and nothing
// else. A name that could resolve anywhere is refused, not restored over.
func TestRecoverRefusesATargetOutsideTheInstallRoot(t *testing.T) {
	for _, recorded := range []string{"..", ".", "", "  ", "../escape", "a/b", string(filepath.Separator) + "etc"} {
		t.Run(recorded, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "installs")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(root, "escape")
			workspace := plantInterruptedCommit(t, dir, "demo", recorded, "old")

			if err := Recover(dir, nil); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Errorf("recovery wrote outside the install root: %v", err)
			}
			if _, err := os.Stat(filepath.Join(workspace, "previous", "version")); err != nil {
				t.Errorf("an unattributable backup must be left intact: %v", err)
			}
		})
	}
}

// A workspace mid-transaction has no backup yet, and one whose marker never got
// written cannot be attributed. Neither is something to act on, and neither is
// something to delete.
func TestRecoverSkipsWorkspacesItCannotActOn(t *testing.T) {
	dir := t.TempDir()
	noBackup, _, err := StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(noBackup, 0o755); err != nil {
		t.Fatal(err)
	}
	noMarker := plantInterruptedCommit(t, dir, "demo", "demo", "old")
	if err := os.Remove(filepath.Join(noMarker, markerFileName)); err != nil {
		t.Fatal(err)
	}

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := os.Stat(noBackup); err != nil {
		t.Errorf("a workspace with no backup must be left alone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(noMarker, "previous", "version")); err != nil {
		t.Errorf("a backup with no marker must be left intact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo")); !os.IsNotExist(err) {
		t.Errorf("nothing should have been restored, got %v", err)
	}
}

// The workspace prefix is a public dot prefixed name, and the skill loader
// enumerates dot prefixed directories, so a user authored skill can legitimately
// be named with it and hold the same two entries a workspace does. Ownership is
// proven by the marker's magic line, which ordinary content never carries, so
// this directory is left untouched and unreported.
func TestRecoverIgnoresAnInstallThatLooksLikeAWorkspace(t *testing.T) {
	dir := t.TempDir()
	lookalike := filepath.Join(dir, workspacePrefix+"notes")
	if err := os.MkdirAll(filepath.Join(lookalike, "previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lookalike, "previous", "version"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lookalike, "target"), []byte("elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := os.Stat(filepath.Join(lookalike, "previous", "version")); err != nil {
		t.Fatalf("recovery consumed installed content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "elsewhere")); !os.IsNotExist(err) {
		t.Errorf("recovery published from a directory that is not its workspace: %v", err)
	}
}

// plantPublishedCommit builds what a process killed after CommitDir's second
// rename leaves in dir: the replacement live at the target, and the tree it
// replaced still sitting in the workspace beside it.
func plantPublishedCommit(t *testing.T, dir, name, recorded, published, superseded string) string {
	t.Helper()
	workspace := plantInterruptedCommit(t, dir, name, recorded, superseded)
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "version"), []byte(published), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// A backup left beside a target that the publish rename already replaced is
// superseded, not pending. Keeping it made a removal reversible by accident:
// the removal deleted the live target, and the next recovery then read the
// absent target as an interrupted swap and published the stale tree again.
func TestRecoverRetiresABackupASuccessfulPublishSuperseded(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	target := filepath.Join(dir, "demo")

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(target, "version"))
	if err != nil || string(data) != "new" {
		t.Fatalf("the published install must be left alone: got %q err %v", data, err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("the superseded workspace should be retired, got %v", err)
	}

	if err := RemoveDir(target, func() error { return nil }); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a removed install was resurrected by recovery: %v", err)
	}
}

// A rollback must never leave a partial tree at the target, because recovery
// reads a present target as proof the publish committed and retires the backup
// beside it. Deleting the failed install in place opened exactly that window:
// a kill partway through the delete left a husk at the target while the backup
// was still the only complete copy. Moving the failed tree aside first closes
// it. The permission trick makes the in-place delete fail partway on demand.
func TestCommitDirRollbackNeverLeavesAPartialTargetTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block removal on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	root := t.TempDir()
	target := filepath.Join(root, "demo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := StageDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	locked := filepath.Join(staged, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "held"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})

	publishErr := errors.New("publish failed")
	if err := CommitDir(target, staged, func() error { return publishErr }); !errors.Is(err, publishErr) {
		t.Fatalf("CommitDir error = %v, want publish failure", err)
	}

	data, err := os.ReadFile(filepath.Join(target, "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("the previous install must be back at the target: got %q err %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(target, "locked")); !os.IsNotExist(err) {
		t.Fatalf("part of the failed install was left at the target: %v", err)
	}
}

// The window between rollback's two renames leaves the target absent, the
// backup intact, and the failed install set aside beside it. That is the same
// pre-publish shape recovery already restores, and the set-aside tree must not
// change its reading of it.
func TestRecoverPutsBackAnInterruptedRollback(t *testing.T) {
	dir := t.TempDir()
	workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")
	failed := filepath.Join(workspace, "failed")
	if err := os.MkdirAll(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failed, "version"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("the interrupted rollback was not put back: got %q err %v", data, err)
	}
}

// The move aside can itself fail, and then the failed install is still live at
// the target with the backup still the only copy of what the user had. Recovery
// reads a target that is there as a committed publish, so a workspace left
// attributable here would have its backup retired: the one state where the new
// retire branch would destroy a tree that was never superseded. Dropping the
// marker hands it to the guard that already leaves unattributable workspaces
// alone. The parent permissions make the move aside fail on demand.
func TestRollbackKeepsABackupItCouldNotRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block renames on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	root := t.TempDir()
	target := filepath.Join(root, "demo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := StageDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "version"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	publishErr := errors.New("publish failed")
	// The install root goes read only after the publish rename, so rollback
	// cannot move the failed install off the target.
	err = CommitDir(target, staged, func() error {
		if err := os.Chmod(root, 0o555); err != nil {
			t.Fatal(err)
		}
		return publishErr
	})
	if !errors.Is(err, publishErr) {
		t.Fatalf("CommitDir error = %v, want publish failure", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Recover(root, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, "previous", "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("a backup rollback could not restore must be kept: got %q err %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(target, "version"))
	if err != nil || string(data) != "new" {
		t.Fatalf("recovery must leave the tree at the target alone: got %q err %v", data, err)
	}
}

// Everything short of the exact magic and version line, and a marker that is not
// a regular file, leaves the workspace alone and unreported: a workspace we
// cannot prove is ours may be somebody's content.
func TestRecoverSkipsAWorkspaceWithoutAValidMarker(t *testing.T) {
	for _, tc := range []struct{ name, marker string }{
		{"wrong magic", "install-txn v1\ntarget demo\n"},
		{"wrong version", "zero-install-txn v2\ntarget demo\n"},
		{"magic not first", "target demo\nzero-install-txn v1\n"},
		{"no target line", "zero-install-txn v1\n"},
		{"legacy plain name", "demo"},
		{"multi element name", "zero-install-txn v1\ntarget nested/demo\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")
			writeMarker(t, workspace, tc.marker)

			if err := Recover(dir, nil); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			if _, err := os.Stat(filepath.Join(workspace, "previous", "version")); err != nil {
				t.Errorf("an unattributable backup must be left intact: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "demo")); !os.IsNotExist(err) {
				t.Errorf("nothing should have been restored, got %v", err)
			}
		})
	}
}

// A directory in the marker's place is not a marker. os.ReadFile fails on one
// anyway, but the check is on the file mode so that stays true of any future
// reader.
func TestRecoverSkipsAWorkspaceWhoseMarkerIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")
	marker := filepath.Join(workspace, markerFileName)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, "previous", "version")); err != nil {
		t.Errorf("an unattributable backup must be left intact: %v", err)
	}
}

// A recorded name is a target, never another transaction. A marker naming a
// second in-flight workspace would have recovery rename a backup over it or
// publish into it, destroying a transaction that is still running.
func TestRecoverRefusesATargetThatNamesAnotherWorkspace(t *testing.T) {
	dir := t.TempDir()
	inflight, _, err := StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inflight, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inflight, "version"), []byte("staging"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Dir(inflight)
	workspace := plantInterruptedCommit(t, dir, "demo", filepath.Base(other), "old")

	if err := Recover(dir, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(inflight, "version"))
	if err != nil || string(data) != "staging" {
		t.Fatalf("recovery wrote over an in-flight transaction: got %q err %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "previous", "version")); err != nil {
		t.Errorf("an unattributable backup must be left intact: %v", err)
	}
}

// recordingReconciler answers with a fixed phase and remembers what it was
// asked, so the tests can pin both the decision and the question.
type recordingReconciler struct {
	phase  Phase
	err    error
	calls  int
	name   string
	target string
	backup string
}

func (r *recordingReconciler) reconcile(name string, target string, backup string) (Phase, error) {
	r.calls++
	r.name, r.target, r.backup = name, target, backup
	return r.phase, r.err
}

// Which tree the publish recorded cannot be read off the filesystem: a kill
// between the tree swap and the lockfile write leaves exactly what a kill after
// both leaves. Only the caller knows what its metadata says, so recovery asks,
// and a metadata record still naming the backup means the backup is the
// truthful tree and goes back over the live one.
func TestRecoverRestoresTheBackupTheMetadataStillRecords(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	target := filepath.Join(dir, "demo")
	reconciler := &recordingReconciler{phase: PhasePrePublish}

	if err := Recover(dir, reconciler.reconcile); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(target, "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("the recorded tree must be back at the target: got %q err %v", data, err)
	}
	if reconciler.calls != 1 {
		t.Fatalf("reconciler calls = %d, want 1", reconciler.calls)
	}
	if reconciler.name != "demo" || reconciler.target != target || reconciler.backup != filepath.Join(workspace, "previous") {
		t.Fatalf("reconciler asked about (%q, %q, %q)", reconciler.name, reconciler.target, reconciler.backup)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the resolved workspace should be cleared, got %v", err)
	}
}

// The mirror case: the metadata records the live tree, so the publish committed
// and the backup beside it is superseded rather than owed a restore.
func TestRecoverRetiresTheBackupWhenTheMetadataRecordsTheTarget(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	reconciler := &recordingReconciler{phase: PhaseCommitted}

	if err := Recover(dir, reconciler.reconcile); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if err != nil || string(data) != "new" {
		t.Fatalf("the committed install must be left alone: got %q err %v", data, err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the superseded workspace should be retired, got %v", err)
	}
}

// Metadata matching neither tree means recovery has no idea which one the user
// is owed. Guessing either way risks destroying the other, so it moves nothing
// and reports the workspace as unresolved.
func TestRecoverReportsAWorkspaceItCannotClassify(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	reconciler := &recordingReconciler{phase: PhaseUnknown}

	err := Recover(dir, reconciler.reconcile)

	if err == nil {
		t.Fatal("an unclassifiable workspace must be reported, not skipped silently")
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if readErr != nil || string(data) != "new" {
		t.Fatalf("nothing should have moved: got %q err %v", data, readErr)
	}
	data, readErr = os.ReadFile(filepath.Join(workspace, "previous", "version"))
	if readErr != nil || string(data) != "old" {
		t.Fatalf("the backup must be left intact: got %q err %v", data, readErr)
	}
}

// A reconciler that cannot answer (its lockfile is unreadable, a hash fails) is
// not permission to fall back on filesystem shape.
func TestRecoverReportsAReconcilerFailure(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	classifyErr := errors.New("lockfile unreadable")
	reconciler := &recordingReconciler{err: classifyErr}

	err := Recover(dir, reconciler.reconcile)

	if !errors.Is(err, classifyErr) {
		t.Fatalf("Recover error = %v, want the reconciler failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "previous", "version")); statErr != nil {
		t.Errorf("the backup must be left intact: %v", statErr)
	}
}

// With nothing at the target there is no second tree to choose between, and the
// backup is the only copy in existence. Asking the caller could only produce an
// answer that throws it away.
func TestRecoverDoesNotConsultTheReconcilerWithNothingAtTheTarget(t *testing.T) {
	dir := t.TempDir()
	plantInterruptedCommit(t, dir, "demo", "demo", "old")
	reconciler := &recordingReconciler{phase: PhaseCommitted}

	if err := Recover(dir, reconciler.reconcile); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if reconciler.calls != 0 {
		t.Errorf("reconciler calls = %d, want 0", reconciler.calls)
	}
	data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("the only copy of the install was not put back: got %q err %v", data, err)
	}
}

// A recovery set that was never enumerated is not an empty recovery set. Callers
// abort on this rather than installing over, or reporting the removal of, a tree
// that may still be owed a restore.
func TestRecoverReportsAnInstallRootItCannotRead(t *testing.T) {
	if err := Recover(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("an unreadable install root must be reported, not read as nothing to do")
	}
}

// The restore is the whole point of the transaction, so a rename it cannot
// complete is the loudest failure recovery has.
func TestRecoverReportsAFailedRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block renames on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	reconciler := &recordingReconciler{phase: PhasePrePublish}

	err := Recover(dir, reconciler.reconcile)

	if err == nil {
		t.Fatal("a restore that could not be made must be reported")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, readErr := os.ReadFile(filepath.Join(workspace, "previous", "version"))
	if readErr != nil || string(data) != "old" {
		t.Fatalf("a backup that could not be restored must be kept: got %q err %v", data, readErr)
	}
}

// Retiring a superseded workspace is bookkeeping, but a failure still leaves a
// backup on disk that the next pass will read again, so it is reported too.
func TestRecoverReportsAFailedRetirement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block removal on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o755) })
	if err := os.Chmod(workspace, 0o555); err != nil {
		t.Fatal(err)
	}
	reconciler := &recordingReconciler{phase: PhaseCommitted}

	err := Recover(dir, reconciler.reconcile)

	if err == nil {
		t.Fatal("a retirement that could not be made must be reported")
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if readErr != nil || string(data) != "new" {
		t.Fatalf("the committed install must be left alone: got %q err %v", data, readErr)
	}
}

// A backup we cannot even stat is not a missing backup. Reading it as one would
// have the workspace skipped as mid-transaction while a tree sits in it.
func TestRecoverReportsABackupItCannotStat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	dir := t.TempDir()
	staged, _, err := StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeMarker(t, workspace, "zero-install-txn v1\ntarget demo\n")
	if err := os.Symlink("previous", filepath.Join(workspace, "previous")); err != nil {
		t.Fatal(err)
	}

	if err := Recover(dir, nil); err == nil {
		t.Fatal("a backup that could not be inspected must be reported")
	}
}

// One unresolved workspace must not strand the others: the recovery set is
// processed to the end and every failure is reported together.
func TestRecoverProcessesEveryWorkspaceAndReportsThemTogether(t *testing.T) {
	dir := t.TempDir()
	unresolved := plantPublishedCommit(t, dir, "unknown", "unknown", "new", "old")
	plantInterruptedCommit(t, dir, "demo", "demo", "old")
	reconciler := &recordingReconciler{phase: PhaseUnknown}

	err := Recover(dir, reconciler.reconcile)

	if err == nil {
		t.Fatal("the unresolved workspace must be reported")
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if readErr != nil || string(data) != "old" {
		t.Fatalf("the resolvable workspace must still be recovered: got %q err %v", data, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(unresolved, "previous")); statErr != nil {
		t.Errorf("the unresolved backup must be left intact: %v", statErr)
	}
}

// A workspace killed before the first rename holds a staged tree and no backup.
// There is nothing to put back, the live install never moved, and the workspace
// is somebody else's to clean up.
func TestRecoverSkipsAWorkspaceInterruptedBeforeTheFirstRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "demo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, _, err := StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeMarker(t, workspace, "zero-install-txn v1\ntarget demo\n")
	reconciler := &recordingReconciler{phase: PhaseUnknown}

	if err := Recover(dir, reconciler.reconcile); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if reconciler.calls != 0 {
		t.Errorf("reconciler calls = %d, want 0", reconciler.calls)
	}
	data, err := os.ReadFile(filepath.Join(target, "version"))
	if err != nil || string(data) != "old" {
		t.Fatalf("the live install must be left alone: got %q err %v", data, err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("a workspace with no backup must be left alone: %v", err)
	}
}

// Recovery runs on every lock acquisition, so it has to be a no-op on a state it
// already resolved. Nothing it leaves behind may read as another interrupted
// transaction.
func TestRecoverIsANoOpOnAResolvedState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, dir string)
		phase Phase
		want  string
	}{
		{"restored backup", func(t *testing.T, dir string) { plantInterruptedCommit(t, dir, "demo", "demo", "old") }, PhaseCommitted, "old"},
		{"restored over the target", func(t *testing.T, dir string) {
			plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
		}, PhasePrePublish, "old"},
		{"retired backup", func(t *testing.T, dir string) {
			plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
		}, PhaseCommitted, "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.plant(t, dir)
			reconciler := &recordingReconciler{phase: tc.phase}
			if err := Recover(dir, reconciler.reconcile); err != nil {
				t.Fatalf("first Recover: %v", err)
			}
			before := reconciler.calls

			if err := Recover(dir, reconciler.reconcile); err != nil {
				t.Fatalf("second Recover: %v", err)
			}

			if reconciler.calls != before {
				t.Errorf("the second pass found another transaction to classify: calls %d then %d", before, reconciler.calls)
			}
			data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
			if err != nil || string(data) != tc.want {
				t.Fatalf("the second pass changed the install: got %q err %v", data, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), workspacePrefix) {
					t.Errorf("a resolved workspace was left behind: %s", entry.Name())
				}
			}
		})
	}
}

// A retirement that fails partway must not cost the workspace its attribution.
// os.RemoveAll unlinks the marker before it reaches the backup, so a retirement
// that dies inside the backup used to leave a workspace holding a tree that
// nothing could attribute: the next pass read it as somebody else's content,
// stayed silent, and every later caller went on as though the transaction had
// resolved. The backup goes first, so anything left behind is still ours and is
// still reported.
func TestRecoverKeepsAttributionWhenRetirementFailsPartway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block removal on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	backup := filepath.Join(workspace, "previous")
	t.Cleanup(func() { _ = os.Chmod(backup, 0o755) })
	if err := os.Chmod(backup, 0o555); err != nil {
		t.Fatal(err)
	}
	reconciler := &recordingReconciler{phase: PhaseCommitted}

	if err := Recover(dir, reconciler.reconcile); err == nil {
		t.Fatal("a retirement that could not be made must be reported")
	}

	if _, err := os.Stat(filepath.Join(workspace, markerFileName)); err != nil {
		t.Fatalf("a failed retirement dropped the marker that attributes the workspace: %v", err)
	}
	if err := Recover(dir, reconciler.reconcile); err == nil {
		t.Fatal("the second pass went silent on a workspace still holding a backup")
	}
}

// The recorded name resolves the path after normalization, so it must be the
// same name the reconciler is asked about. Handing the reconciler the raw line
// while resolving the path from the trimmed one splits the decision across two
// values: the lookup misses, the miss reads as a publish that never ran, and the
// committed target is replaced with the tree it superseded. The reconciler here
// answers the way a lockfile does, by name.
func TestRecoverAsksTheReconcilerAboutTheNormalizedName(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	writeMarker(t, workspace, markerMagic+"\ntarget demo \n")
	var asked string
	reconcile := func(name string, target string, backup string) (Phase, error) {
		asked = name
		if name != "demo" {
			return PhasePrePublish, nil
		}
		return PhaseCommitted, nil
	}

	if err := Recover(dir, reconcile); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if asked != "demo" {
		t.Errorf("reconciler asked about %q, want the normalized %q", asked, "demo")
	}
	data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if err != nil {
		t.Fatalf("read the live install: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("the committed install was replaced with %q", data)
	}
}

// A marker we could not read is not a marker that is absent. Skipping silently
// on a read error tells the caller there was nothing to do while the only copy
// of the install stays stranded in the workspace, which is the fail-open half of
// the distinction the target probe already draws.
func TestRecoverReportsAMarkerItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permissions do not block reads on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file permissions this test relies on")
	}
	dir := t.TempDir()
	workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")
	marker := filepath.Join(workspace, markerFileName)
	t.Cleanup(func() { _ = os.Chmod(marker, 0o600) })
	if err := os.Chmod(marker, 0o000); err != nil {
		t.Fatal(err)
	}

	err := Recover(dir, nil)

	if err == nil {
		t.Fatal("a marker that could not be read must be reported, not skipped")
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "previous")); statErr != nil {
		t.Fatalf("the stranded copy must be left alone: %v", statErr)
	}
}

// The retained backup is a directory this package created. A symlink or a file
// standing in its place is not something recovery may publish: renaming a
// symlink into the install root installs whatever it points at, from anywhere on
// the filesystem.
func TestRecoverRefusesABackupThatIsNotADirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "payload"), []byte("elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, plant := range []struct {
		name string
		make func(t *testing.T, backup string)
	}{
		{"symlink to a directory outside the root", func(t *testing.T, backup string) {
			if err := os.Symlink(outside, backup); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular file", func(t *testing.T, backup string) {
			if err := os.WriteFile(backup, []byte("not a tree"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(plant.name, func(t *testing.T) {
			dir := t.TempDir()
			staged, _, err := StageDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Dir(staged)
			writeMarker(t, workspace, markerMagic+"\ntarget demo\n")
			plant.make(t, filepath.Join(workspace, "previous"))

			if err := Recover(dir, nil); err == nil {
				t.Fatal("a backup that is not a directory must be reported, not published")
			}
			if _, err := os.Lstat(filepath.Join(dir, "demo")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("nothing may be published at the target: %v", err)
			}
		})
	}
}

// An entry at the target is not the same as an install at the target. A dangling
// symlink satisfies Lstat, and reading it as a live install retires the backup
// beside it, which is the only real copy there is.
func TestRecoverRefusesATargetThatIsNotADirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	dir := t.TempDir()
	workspace := plantInterruptedCommit(t, dir, "demo", "demo", "old")
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "demo")); err != nil {
		t.Fatal(err)
	}

	err := Recover(dir, nil)

	if err == nil {
		t.Fatal("a target that is not a directory must be reported")
	}
	data, readErr := os.ReadFile(filepath.Join(workspace, "previous", "version"))
	if readErr != nil {
		t.Fatalf("the retained copy must survive: %v", readErr)
	}
	if string(data) != "old" {
		t.Fatalf("retained copy = %q, want old", data)
	}
}

// A workspace of ours can hold a failed tree from an earlier rollback. Renaming
// the live target onto it fails for as long as it is there, and because every
// caller aborts on a recovery error, that wedges every install and removal in
// the root. The workspace is proven ours, so the stale tree is cleared.
func TestRecoverClearsAStaleFailedTreeBeforeRestoring(t *testing.T) {
	dir := t.TempDir()
	workspace := plantPublishedCommit(t, dir, "demo", "demo", "new", "old")
	failed := filepath.Join(workspace, "failed")
	if err := os.MkdirAll(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failed, "leftover"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	reconciler := &recordingReconciler{phase: PhasePrePublish}

	if err := Recover(dir, reconciler.reconcile); err != nil {
		t.Fatalf("a stale failed tree must not wedge recovery: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "demo", "version"))
	if err != nil {
		t.Fatalf("read the restored install: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("restored content = %q, want old", data)
	}
}

// A recorded name that cannot be a sane path element is not ours to act on. It
// used to reach Lstat, which answers with an error that is not not-exist, so the
// workspace reported an error no caller could ever clear.
func TestRecoverSkipsAMarkerNamingAnImpossibleTarget(t *testing.T) {
	for _, name := range []string{"de\x00mo", strings.Repeat("a", 300)} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			dir := t.TempDir()
			workspace := plantInterruptedCommit(t, dir, "demo", name, "old")

			if err := Recover(dir, nil); err != nil {
				t.Fatalf("an unusable recorded name must be skipped, not reported forever: %v", err)
			}
			if _, err := os.Stat(filepath.Join(workspace, "previous")); err != nil {
				t.Fatalf("the workspace must be left intact: %v", err)
			}
		})
	}
}

// Two workspaces claiming one install cannot both be right, and acting on them
// in turn publishes one over the other and deletes the rest. Recovery has no way
// to rank them, so it reports and touches nothing.
func TestRecoverRefusesTwoWorkspacesNamingOneTarget(t *testing.T) {
	dir := t.TempDir()
	first := plantInterruptedCommit(t, dir, "demo", "demo", "first")
	second := plantInterruptedCommit(t, dir, "demo", "demo", "second")

	err := Recover(dir, nil)

	if err == nil {
		t.Fatal("two workspaces naming one target must be reported")
	}
	for _, workspace := range []string{first, second} {
		if _, statErr := os.Stat(filepath.Join(workspace, "previous")); statErr != nil {
			t.Errorf("every retained copy must survive: %v", statErr)
		}
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "demo")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("nothing may be published while the claim is ambiguous: %v", statErr)
	}
}
