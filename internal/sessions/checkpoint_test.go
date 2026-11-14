package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCkStore(t *testing.T) (*Store, string) {
	t.Helper()
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	if _, err := store.Create(CreateInput{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	return store, t.TempDir() // store, workspaceRoot
}

func decodeCk(t *testing.T, ev Event) CheckpointPayload {
	t.Helper()
	var p CheckpointPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode checkpoint payload: %v", err)
	}
	return p
}

func TestCaptureToolCheckpointWritesBlobAndEvent(t *testing.T) {
	store, ws := newCkStore(t)
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := store.CaptureToolCheckpoint("s", ws, "edit_file", []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != EventSessionCheckpoint {
		t.Fatalf("event type = %s", ev.Type)
	}
	p := decodeCk(t, ev)
	verifiedWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if p.WorkspaceRoot != verifiedWS {
		t.Fatalf("workspace binding = %q, want verified local root %q", p.WorkspaceRoot, verifiedWS)
	}
	if len(p.Files) != 1 || p.Files[0].Path != "a.txt" || p.Files[0].Blob == "" || p.Files[0].Bytes != 2 {
		t.Fatalf("unexpected payload: %+v", p)
	}
	if _, err := store.readBlob("s", p.Files[0].Blob); err != nil {
		t.Fatalf("blob not stored: %v", err)
	}
}

func TestCaptureDedupsIdenticalContent(t *testing.T) {
	store, ws := newCkStore(t)
	mustWriteFile(t, filepath.Join(ws, "a.txt"), "same")
	mustWriteFile(t, filepath.Join(ws, "b.txt"), "same")
	if _, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{"b.txt"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.blobsDir("s"))
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 deduped blob, got %d", len(entries))
	}
}

func TestCaptureRecordsAbsentForNewFile(t *testing.T) {
	store, ws := newCkStore(t)
	ev, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{"new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	p := decodeCk(t, ev)
	if len(p.Files) != 1 {
		t.Fatalf("expected 1 file entry, got %+v", p.Files)
	}
	if !p.Files[0].Absent || p.Files[0].Blob != "" {
		t.Fatalf("expected absent marker, got %+v", p.Files[0])
	}
}

func TestCaptureRejectsTraversalTargets(t *testing.T) {
	store, ws := newCkStore(t)
	// A file outside the workspace must never be read into a blob or marked
	// Absent (which would delete it on rewind). Regression for capture-side
	// path-traversal confinement.
	outside := filepath.Join(filepath.Dir(ws), "secret.txt")
	mustWriteFile(t, outside, "top secret")
	ev, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{"../secret.txt"})
	if err != nil {
		t.Fatal(err)
	}
	p := decodeCk(t, ev)
	if len(p.Files) != 1 {
		t.Fatalf("expected 1 file entry, got %+v", p.Files)
	}
	if f := p.Files[0]; !f.Skipped || f.Absent || f.Blob != "" {
		t.Fatalf("traversal target must be skipped (not captured/absent), got %+v", f)
	}
	if got, rerr := os.ReadFile(outside); rerr != nil || string(got) != "top secret" {
		t.Fatalf("outside file must remain untouched: got %q err %v", got, rerr)
	}
}

func TestCaptureSkipsOversizeFiles(t *testing.T) {
	t.Setenv("ZERO_CHECKPOINT_MAX_BYTES", "4")
	store, ws := newCkStore(t)
	mustWriteFile(t, filepath.Join(ws, "big.txt"), "123456")
	ev, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{"big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	p := decodeCk(t, ev)
	if len(p.Files) != 1 {
		t.Fatalf("expected 1 file entry, got %+v", p.Files)
	}
	if !p.Files[0].Skipped || p.Files[0].Blob != "" {
		t.Fatalf("expected skipped, got %+v", p.Files[0])
	}
}

func TestCaptureDisabled(t *testing.T) {
	t.Setenv("ZERO_CHECKPOINTS", "off")
	store, ws := newCkStore(t)
	mustWriteFile(t, filepath.Join(ws, "a.txt"), "v1")
	ev, err := store.CaptureToolCheckpoint("s", ws, "write_file", []string{"a.txt"})
	if err != nil || ev.Type != "" {
		t.Fatalf("expected no-op when disabled, got ev=%+v err=%v", ev, err)
	}
}

func TestTruncateEvents(t *testing.T) {
	store, _ := newCkStore(t)
	var seqs []int
	for i := 0; i < 3; i++ {
		ev, err := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{"i": i}})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ev.Sequence)
	}
	if err := store.TruncateEvents("s", seqs[1]); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents("s")
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 2 || events[len(events)-1].Sequence != seqs[1] {
		t.Fatalf("expected 2 events through seq %d, got %d", seqs[1], len(events))
	}
}

func TestRestoreToSequenceRevertsFileContent(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	path := filepath.Join(ws, "a.txt")
	mustWriteFile(t, path, "original")
	mustCapture(t, store, ws, "write_file", "a.txt") // captures "original"
	mustWriteFile(t, path, "edited1")
	mustCapture(t, store, ws, "edit_file", "a.txt") // captures "edited1"
	mustWriteFile(t, path, "edited2")

	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("restore should yield 'original', got %q", got)
	}
	if report.FilesRestored < 1 {
		t.Fatalf("expected FilesRestored>=1, got %+v", report)
	}
}

func TestRestoreDeletesFileThatWasAbsent(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	path := filepath.Join(ws, "new.txt")
	mustCapture(t, store, ws, "write_file", "new.txt") // absent before
	mustWriteFile(t, path, "created")

	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file deleted on restore, stat err=%v", err)
	}
	if report.FilesDeleted < 1 {
		t.Fatalf("expected FilesDeleted>=1, got %+v", report)
	}
}

func TestApplyRewindRestoresAndTruncates(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	path := filepath.Join(ws, "a.txt")
	mustWriteFile(t, path, "original")
	mustCapture(t, store, ws, "write_file", "a.txt")
	mustWriteFile(t, path, "changed")

	report, err := store.ApplyRewind("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read restored file: %v", rerr)
	}
	if string(got) != "original" {
		t.Fatalf("file not restored: %q", got)
	}
	events, err := store.ReadEvents("s")
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("expected >=2 events, got %d", len(events))
	}
	last := events[len(events)-1]
	if last.Type != EventSessionRewind {
		t.Fatalf("expected trailing rewind marker, got %s", last.Type)
	}
	// kept events through target + the appended rewind marker
	if events[len(events)-2].Sequence != target.Sequence {
		t.Fatalf("expected truncation to target seq %d", target.Sequence)
	}
	_ = report
}

func TestImportedSessionRewindUsesCapturedLocalWorkspaceBinding(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	localWorkspace := t.TempDir()
	foreignWorkspace := t.TempDir()
	if _, err := store.Create(CreateInput{
		SessionID:    "imported-session",
		Cwd:          foreignWorkspace,
		WorkspaceKey: foreignWorkspace,
		Tag:          ImportedSessionTag("claude-code", "foreign-id"),
	}); err != nil {
		t.Fatal(err)
	}

	localPath := filepath.Join(localWorkspace, "config.yaml")
	foreignPath := filepath.Join(foreignWorkspace, "config.yaml")
	mustWriteFile(t, localPath, "local-before")
	mustWriteFile(t, foreignPath, "foreign-must-not-change")
	if _, err := store.CaptureToolCheckpoint("imported-session", localWorkspace, "write_file", []string{"config.yaml"}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, localPath, "local-after")

	// The caller supplies the imported session's foreign WorkspaceKey, matching
	// the old CLI failure path. Restore must ignore it in favour of the verified
	// local root bound at capture time.
	if _, err := store.ApplyRewind("imported-session", foreignWorkspace, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(localPath); err != nil || string(got) != "local-before" {
		t.Fatalf("local checkpoint was not restored: got %q err=%v", got, err)
	}
	if got, err := os.ReadFile(foreignPath); err != nil || string(got) != "foreign-must-not-change" {
		t.Fatalf("foreign workspace was mutated: got %q err=%v", got, err)
	}
}

func TestImportedSessionRefusesLegacyCheckpointWithoutLocalBinding(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	if _, err := store.Create(CreateInput{
		SessionID: "imported-session",
		Tag:       ImportedSessionTag("codex", "foreign-id"),
	}); err != nil {
		t.Fatal(err)
	}
	target, err := store.AppendEvent("imported-session", AppendEventInput{Type: EventMessage, Payload: map[string]any{"content": "before"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent("imported-session", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "write_file",
		Files: []CheckpointFile{{Path: "new.txt", Absent: true}},
	}}); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyRewind("imported-session", t.TempDir(), target.Sequence)
	if err == nil || !strings.Contains(err.Error(), "no verified local workspace binding") {
		t.Fatalf("unbound imported checkpoint error = %v", err)
	}
	events, readErr := store.ReadEvents("imported-session")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(events) != 2 {
		t.Fatalf("failed rewind modified the event log: got %d events, want 2", len(events))
	}
}

func TestImportedSessionRejectsTamperedAbsoluteCheckpointRoot(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	localWorkspace := t.TempDir()
	outsideWorkspace := t.TempDir()
	if _, err := store.Create(CreateInput{
		SessionID: "imported-session",
		Tag:       ImportedSessionTag("codex", "foreign-id"),
	}); err != nil {
		t.Fatal(err)
	}
	target, err := store.AppendEvent("imported-session", AppendEventInput{Type: EventMessage, Payload: map[string]any{"content": "before"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := store.SnapshotForCheckpoint("imported-session", localWorkspace, "write_file", []string{"new.txt"})
	if !ok || len(payload.WorkspaceBinding) == 0 {
		t.Fatal("checkpoint snapshot did not carry a verified local workspace binding")
	}

	victim := filepath.Join(outsideWorkspace, "victim.txt")
	mustWriteFile(t, victim, "keep me")
	payload.WorkspaceRoot = outsideWorkspace
	payload.Files = []CheckpointFile{{Path: "victim.txt", Absent: true}}
	if _, err := store.AppendEvent("imported-session", AppendEventInput{Type: EventSessionCheckpoint, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ApplyRewind("imported-session", localWorkspace, target.Sequence); err == nil || !strings.Contains(err.Error(), "does not match its verified local binding") {
		t.Fatalf("tampered checkpoint root error = %v", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep me" {
		t.Fatalf("tampered checkpoint mutated outside file: got %q err=%v", got, err)
	}
}

func TestNativeImportedPrefixTagCanApplyLegacyCheckpoint(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	workspace := t.TempDir()
	path := filepath.Join(workspace, "config.yaml")
	mustWriteFile(t, path, "before")
	if _, err := store.Create(CreateInput{SessionID: "native-tagged", Tag: "imported:archive"}); err != nil {
		t.Fatal(err)
	}
	target, err := store.AppendEvent("native-tagged", AppendEventInput{Type: EventMessage, Payload: map[string]any{"content": "before"}})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.writeBlob("native-tagged", []byte("before"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent("native-tagged", AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool: "write_file",
		Files: []CheckpointFile{{
			Path:  "config.yaml",
			Blob:  blob,
			Bytes: len("before"),
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, "after")

	if _, err := store.ApplyRewind("native-tagged", workspace, target.Sequence); err != nil {
		t.Fatalf("native tagged rewind: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "before" {
		t.Fatalf("legacy checkpoint was not restored: got %q err=%v", got, err)
	}
}

func TestRestoreRejectsPathTraversal(t *testing.T) {
	store, ws := newCkStore(t)
	target, _ := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	// Hand-craft a tampered checkpoint event with a traversal path.
	outside := filepath.Join(filepath.Dir(ws), "evil.txt")
	mustWriteFile(t, outside, "keep me")
	mustAppend(t, store, AppendEventInput{Type: EventSessionCheckpoint, Payload: CheckpointPayload{
		Tool:  "write_file",
		Files: []CheckpointFile{{Path: "../evil.txt", Absent: true}},
	}})
	report, err := store.RestoreToSequence("s", ws, target.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("restore must NOT delete files outside the workspace: %v", err)
	}
	if len(report.Skipped) == 0 {
		t.Errorf("expected traversal path reported as skipped, got %+v", report)
	}
}

func TestRestoreDedupesEquivalentRawPaths(t *testing.T) {
	store, ws := newCkStore(t)
	target := mustAppend(t, store, AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	path := filepath.Join(ws, "a.txt")

	// Closest-to-target checkpoint captures "closest" via raw path "a.txt".
	mustWriteFile(t, path, "closest")
	mustCapture(t, store, ws, "write_file", "a.txt")
	// A NEWER checkpoint references the SAME file via an equivalent raw path.
	mustWriteFile(t, path, "newer")
	mustCapture(t, store, ws, "edit_file", "./a.txt")
	mustWriteFile(t, path, "current")

	if _, err := store.RestoreToSequence("s", ws, target.Sequence); err != nil {
		t.Fatal(err)
	}
	// The closest-to-target snapshot must win; the newer entry under the
	// equivalent raw path must not overwrite it.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "closest" {
		t.Fatalf("equivalent raw path bypassed dedupe: got %q, want \"closest\"", got)
	}
}

func TestRestoreFailsOnCorruptCheckpointPayload(t *testing.T) {
	store, ws := newCkStore(t)
	target := mustAppend(t, store, AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	// A checkpoint event whose payload does not decode into CheckpointPayload must
	// abort the rewind, not be silently skipped (which could restore a later state).
	mustAppend(t, store, AppendEventInput{Type: EventSessionCheckpoint, Payload: "corrupt-not-a-checkpoint"})
	if _, err := store.RestoreToSequence("s", ws, target.Sequence); err == nil {
		t.Fatal("expected rewind to fail on undecodable checkpoint payload")
	}
}

func TestTruncateToZeroProducesEmptyFile(t *testing.T) {
	store, _ := newCkStore(t)
	mustAppend(t, store, AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
	if err := store.TruncateEvents("s", 0); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents("s")
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events after truncate-to-0, got %d", len(events))
	}
}

// mustWriteFile / mustCapture / mustAppend are setup helpers that fail the test
// immediately on error, so a setup failure is diagnosed at its source instead of
// surfacing as a confusing downstream assertion.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustCapture(t *testing.T, store *Store, ws, tool, path string) {
	t.Helper()
	if _, err := store.CaptureToolCheckpoint("s", ws, tool, []string{path}); err != nil {
		t.Fatalf("CaptureToolCheckpoint(%s, %s): %v", tool, path, err)
	}
}

func mustAppend(t *testing.T, store *Store, input AppendEventInput) Event {
	t.Helper()
	ev, err := store.AppendEvent("s", input)
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	return ev
}
