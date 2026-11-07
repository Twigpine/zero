package sessions

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Finding 6: when several checkpoints touch the same path, only the
// closest-to-target (oldest) entry must take effect. If that entry is Skipped
// but a newer entry has a blob, the file must NOT be restored from the newer
// blob, and FilesRestored must not be double-counted.
func TestRestoreClosestToTargetSkippedWins(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})

	// Seed a blob so the newer (further-from-target) checkpoint references it.
	hash, err := store.writeBlob("s", []byte("newer-blob-content"))
	if err != nil {
		t.Fatal(err)
	}

	// Oldest checkpoint (closest to target) marks the path Skipped (not recoverable).
	store.AppendEvent("s", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "edit_file",
		Files: []CheckpointFile{{Path: "a.txt", Skipped: true}},
	}})
	// Newer checkpoint (further from target) has a real blob.
	store.AppendEvent("s", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "edit_file",
		Files: []CheckpointFile{{Path: "a.txt", Blob: hash}},
	}})

	// Current on-disk state the user wants reverted.
	path := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(path, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	// The closest-to-target entry is Skipped: the file must be left as-is.
	got, _ := os.ReadFile(path)
	if string(got) != "current" {
		t.Fatalf("Skipped closest entry must not be overwritten by a newer blob, got %q", got)
	}
	if report.FilesRestored != 0 {
		t.Fatalf("FilesRestored = %d, want 0 (closest entry Skipped)", report.FilesRestored)
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("Skipped = %v, want exactly one entry for a.txt", report.Skipped)
	}
}

// Finding 6 (LOW double-count): a path touched by several blob checkpoints must
// be restored once, not once per checkpoint.
func TestRestoreDoesNotDoubleCountFilesRestored(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})

	oldHash, err := store.writeBlob("s", []byte("closest-content"))
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := store.writeBlob("s", []byte("newer-content"))
	if err != nil {
		t.Fatal(err)
	}
	store.AppendEvent("s", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "edit_file",
		Files: []CheckpointFile{{Path: "a.txt", Blob: oldHash}},
	}})
	store.AppendEvent("s", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "edit_file",
		Files: []CheckpointFile{{Path: "a.txt", Blob: newHash}},
	}})

	path := filepath.Join(ws, "a.txt")
	_ = os.WriteFile(path, []byte("current"), 0o644)

	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	// Closest-to-target blob must win, and the count must be 1 (not 2).
	if got, _ := os.ReadFile(path); string(got) != "closest-content" {
		t.Fatalf("closest-to-target blob should win, got %q", got)
	}
	if report.FilesRestored != 1 {
		t.Fatalf("FilesRestored = %d, want 1 (no double count)", report.FilesRestored)
	}
}

// Finding 9: readBlob must verify the content matches the requested sha256. A
// corrupted blob must be reported as Skipped, not written as truth.
func TestReadBlobRejectsCorruptContent(t *testing.T) {
	store, _ := newCkStore(t)
	hash, err := store.writeBlob("s", []byte("trusted-content"))
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the blob on disk without changing its (content-addressed) name.
	if err := os.WriteFile(store.blobPath("s", hash), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readBlob("s", hash); err == nil {
		t.Fatalf("readBlob must error on sha256 mismatch, got nil")
	}
}

func TestRestoreSkipsCorruptBlob(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	hash, err := store.writeBlob("s", []byte("trusted-content"))
	if err != nil {
		t.Fatal(err)
	}
	store.AppendEvent("s", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "edit_file",
		Files: []CheckpointFile{{Path: "a.txt", Blob: hash}},
	}})
	// Corrupt the stored blob.
	_ = os.WriteFile(store.blobPath("s", hash), []byte("tampered-evil"), 0o600)

	path := filepath.Join(ws, "a.txt")
	_ = os.WriteFile(path, []byte("current"), 0o644)

	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) == "tampered-evil" {
		t.Fatalf("corrupt blob must not be written as truth, got %q", got)
	}
	if report.FilesRestored != 0 || len(report.Skipped) != 1 {
		t.Fatalf("corrupt blob must be Skipped, report = %+v", report)
	}
}

// Finding 7: an in-workspace symlink that points outside the workspace must not
// let a restore write outside the root. resolveWithinWorkspace must resolve
// symlinks, not just reject lexical "..".
func TestRestoreRejectsInWorkspaceSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})

	// A directory outside the workspace holding a file we must not clobber.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An in-workspace symlink "link" -> outsideDir. The relative path
	// "link/secret.txt" is lexically clean (no ".."), so a purely lexical check
	// would let the write escape the workspace.
	if err := os.Symlink(outsideDir, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	hash, err := store.writeBlob("s", []byte("attacker-content"))
	if err != nil {
		t.Fatal(err)
	}
	store.AppendEvent("s", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "write_file",
		Files: []CheckpointFile{{Path: "link/secret.txt", Blob: hash}},
	}})

	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outsideFile); string(got) != "keep me" {
		t.Fatalf("restore escaped the workspace via symlink, outside file = %q", got)
	}
	if report.FilesRestored != 0 {
		t.Fatalf("FilesRestored = %d, want 0 (symlink escape must be skipped)", report.FilesRestored)
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("expected symlink-escape path reported as skipped, got %+v", report.Skipped)
	}
}

// Audit finding (HIGH): Fork must copy content-addressed checkpoint blobs into
// the fork, not just the checkpoint EVENTS. Otherwise an ApplyRewind on the
// fork reads a blob that does not exist in the fork's blobs dir and silently
// skips the file instead of restoring it.
func TestForkRewindRestoresFromCopiedBlobs(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	ws := t.TempDir()
	if _, err := store.Create(CreateInput{SessionID: "parent"}); err != nil {
		t.Fatal(err)
	}
	target, err := store.AppendEvent("parent", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	// Capture "original" into a checkpoint, then mutate on disk.
	path := filepath.Join(ws, "a.txt")
	mustWriteFile(t, path, "original")
	if _, err := store.CaptureToolCheckpoint("parent", ws, "write_file", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, "changed")

	fork, err := store.Fork("parent", ForkInput{SessionID: "fork"})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The fork must carry the parent's blobs so rewind can read them.
	parentBlobs, _ := os.ReadDir(store.blobsDir("parent"))
	if len(parentBlobs) == 0 {
		t.Fatal("precondition: parent should have at least one blob")
	}
	for _, b := range parentBlobs {
		if _, err := os.Stat(store.blobPath(fork.SessionID, b.Name())); err != nil {
			t.Fatalf("fork is missing parent blob %s: %v", b.Name(), err)
		}
	}

	// Rewind on the fork (target seq matches the copied EventMessage) must
	// restore "original" from the copied blob.
	report, err := store.ApplyRewind(fork.SessionID, ws, target.Sequence)
	if err != nil {
		t.Fatalf("ApplyRewind on fork: %v", err)
	}
	if report.FilesRestored != 1 {
		t.Fatalf("FilesRestored = %d, want 1 (blob must be present in fork)", report.FilesRestored)
	}
	if got, _ := os.ReadFile(path); string(got) != "original" {
		t.Fatalf("fork rewind did not restore from copied blob, got %q", got)
	}
}

// Audit finding (LOW): restoring a checkpointed blob must preserve the original
// file permission bits (an executable script must not come back as 0o644).
func TestRestorePreservesExecutableMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes do not apply on windows")
	}
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	path := filepath.Join(ws, "run.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustCapture(t, store, ws, "write_file", "run.sh") // captures 0o755 content
	// Mutate content (and clobber mode) as a tool edit would.
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o644)

	if _, err := store.RestoreToSequence("s", ws, target.Sequence); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("restored file lost its executable bit: mode=%v", info.Mode().Perm())
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("restored mode = %v, want 0o755", info.Mode().Perm())
	}
}

// Audit finding (MED): ApplyRewind must hold the session lock once across all
// sub-steps and must NOT deadlock by re-acquiring the non-reentrant mutex.
func TestApplyRewindDoesNotDeadlock(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	path := filepath.Join(ws, "a.txt")
	mustWriteFile(t, path, "original")
	mustCapture(t, store, ws, "write_file", "a.txt")
	mustWriteFile(t, path, "changed")

	done := make(chan error, 1)
	go func() {
		_, err := store.ApplyRewind("s", ws, target.Sequence)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApplyRewind: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyRewind deadlocked (likely re-acquired the session lock)")
	}
	if got, _ := os.ReadFile(path); string(got) != "original" {
		t.Fatalf("file not restored: %q", got)
	}
}

// Audit findings (MED): a concurrent CaptureToolCheckpoint and pruneOrphanBlobs
// must not race or delete a freshly-written-but-referenced blob, and must not
// deadlock. Exercised under -race.
func TestCaptureAndPruneConcurrent(t *testing.T) {
	store, ws := newCkStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		name := filepath.Join(ws, "f"+string(rune('a'+i%10))+".txt")
		mustWriteFile(t, name, "content-"+string(rune('0'+i%10)))
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			rel := "f" + string(rune('a'+i%10)) + ".txt"
			if _, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{rel}); err != nil {
				t.Errorf("CaptureToolCheckpoint: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := store.pruneOrphanBlobs("s"); err != nil {
				t.Errorf("pruneOrphanBlobs: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Every blob referenced by a surviving checkpoint event must still exist on
	// disk: prune must not have deleted a blob that capture had referenced.
	refs, err := store.referencedBlobs("s")
	if err != nil {
		t.Fatal(err)
	}
	for hash := range refs {
		if _, err := store.readBlob("s", hash); err != nil {
			t.Fatalf("referenced blob %s missing after concurrent prune: %v", hash, err)
		}
	}
}

// Finding 8: an OS-level file lock must serialize session mutations across
// separate Store instances on the same RootDir (e.g. CLI rewind vs TUI). While
// one Store holds the session lock, another Store's AppendEvent must block.
func TestSessionFileLockSerializesAcrossStores(t *testing.T) {
	root := t.TempDir()
	storeA := NewStore(StoreOptions{RootDir: root})
	if _, err := storeA.Create(CreateInput{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	storeB := NewStore(StoreOptions{RootDir: root})

	// storeA grabs the OS lock and holds it.
	unlock, err := storeA.lockSession("s")
	if err != nil {
		t.Fatalf("lockSession: %v", err)
	}

	started := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		// This must block until storeA releases the OS lock.
		if _, err := storeB.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}}); err != nil {
			t.Errorf("storeB AppendEvent: %v", err)
		}
		close(done)
	}()

	<-started
	select {
	case <-done:
		t.Fatalf("storeB AppendEvent completed while storeA held the OS lock")
	case <-time.After(150 * time.Millisecond):
		// Expected: still blocked.
	}

	unlock()

	select {
	case <-done:
		// storeB proceeded once the lock was released.
	case <-time.After(2 * time.Second):
		t.Fatalf("storeB AppendEvent did not proceed after lock release")
	}
	wg.Wait()
}
