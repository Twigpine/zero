package planmode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPlanFilePathIsStableAcrossCalls(t *testing.T) {
	root := t.TempDir()
	first, err := PlanFilePath(root, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	second, err := PlanFilePath(root, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if first != second {
		t.Fatalf("expected stable path for the same session, got %q then %q", first, second)
	}
}

func TestPlanFilePathEmptySessionIsStable(t *testing.T) {
	// PlanFilePath(root, "") is called independently from several TUI call
	// sites before a session ID may exist (planEnterText, planText,
	// openPlanInEditor); they must all resolve to the same file rather than a
	// fresh one each call (regression for the old time.Now().UnixNano() slug).
	root := t.TempDir()
	first, err := PlanFilePath(root, "")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	second, err := PlanFilePath(root, "")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if first != second {
		t.Fatalf("expected stable path for an empty session id, got %q then %q", first, second)
	}
}

func TestWritePlanUsesRestrictivePermissions(t *testing.T) {
	// Windows reports 0666 for a plan file regardless of the mode passed to
	// OpenFile - NTFS permissions are governed by ACLs, not the POSIX mode
	// bits Go maps them to. Assert the mode bits only where they mean
	// something; Windows containment relies on the workspace-scoped os.Root
	// resolution in WritePlan/ReadPlan instead, not on file permissions.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	root := t.TempDir()
	path, err := WritePlan(root, "session-1", "notes")
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat plan file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected plan file mode 0600, got %o", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat plan dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("expected plan dir mode 0700, got %o", perm)
	}
}

func TestWritePlanTightensPreExistingLoosePermissions(t *testing.T) {
	// Regression: MkdirAll/OpenFile's mode argument only applies at creation
	// time, so a pre-existing 0755 plan directory or 0644 plan file (e.g.
	// predating this restriction, or created some other way) stayed
	// group/other-readable forever after, contrary to the owner-only
	// storage contract WritePlan is supposed to enforce on every write.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	root := t.TempDir()
	planDir := filepath.Join(root, PlanDirName)
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatalf("pre-create loose plan dir: %v", err)
	}
	planFile := filepath.Join(planDir, "session-1.md")
	if err := os.WriteFile(planFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("pre-create loose plan file: %v", err)
	}

	path, err := WritePlan(root, "session-1", "notes")
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat plan file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected pre-existing plan file tightened to mode 0600, got %o", perm)
	}
	dirInfo, err := os.Stat(planDir)
	if err != nil {
		t.Fatalf("stat plan dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("expected pre-existing plan dir tightened to mode 0700, got %o", perm)
	}
}

func TestReadWritePlanRoundtrip(t *testing.T) {
	root := t.TempDir()
	if _, err := WritePlan(root, "session-1", "# Draft\n\nStep one."); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	content, ok, err := ReadPlan(root, "session-1")
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if !ok {
		t.Fatal("expected ReadPlan to report the file exists")
	}
	if content != "# Draft\n\nStep one.\n" {
		t.Fatalf("unexpected plan content: %q", content)
	}
}

func TestReadPlanMissingFileIsNotAnError(t *testing.T) {
	root := t.TempDir()
	_, ok, err := ReadPlan(root, "no-such-session")
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if ok {
		t.Fatal("expected ReadPlan to report no file for a session that never opened one")
	}
}

func TestWritePlanRejectsSymlinkedPlansDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".zero"), 0o700); err != nil {
		t.Fatalf("mkdir .zero: %v", err)
	}
	// Plant a symlink at .zero/plans pointing outside the workspace, as if an
	// attacker (or stale state) had redirected it before WritePlan ran. Unlike
	// a preflight Lstat check, os.Root re-resolves this on every call, so
	// planting the symlink right before the call still gets caught.
	if err := os.Symlink(outside, filepath.Join(root, ".zero", "plans")); err != nil {
		t.Fatalf("symlink .zero/plans: %v", err)
	}

	if _, err := WritePlan(root, "session-1", "notes"); err == nil {
		t.Fatal("expected WritePlan to reject a symlinked plans directory")
	}
	if _, _, err := ReadPlan(root, "session-1"); err == nil {
		t.Fatal("expected ReadPlan to reject a symlinked plans directory")
	}
}

func TestWritePlanRejectsSymlinkedPlanFile(t *testing.T) {
	root := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "exfil.md")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	plansDir := filepath.Join(root, ".zero", "plans")
	if err := os.MkdirAll(plansDir, 0o700); err != nil {
		t.Fatalf("mkdir plans: %v", err)
	}
	id := slugify("session-1")
	if err := os.Symlink(outsideFile, filepath.Join(plansDir, id+".md")); err != nil {
		t.Fatalf("symlink plan file: %v", err)
	}

	if _, err := WritePlan(root, "session-1", "notes"); err == nil {
		t.Fatal("expected WritePlan to reject a symlinked plan file")
	}
	if _, _, err := ReadPlan(root, "session-1"); err == nil {
		t.Fatal("expected ReadPlan to reject a symlinked plan file")
	}
}

func TestEditorStagingDirIsPrivateRejectsOSTempDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	// t.TempDir() itself lives under os.TempDir(), so it doubles as a stand-in
	// for what config.UserConfigDir() would resolve to if XDG_CONFIG_HOME were
	// pointed at the OS temp directory.
	dir := t.TempDir()
	if editorStagingDirIsPrivate(dir, workspaceRoot) {
		t.Fatalf("expected %q (under the OS temp dir) to be rejected", dir)
	}
}

func TestEditorStagingDirIsPrivateRejectsWorkspaceDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	dir := filepath.Join(workspaceRoot, ".config", "zero", "plan-edit")
	if editorStagingDirIsPrivate(dir, workspaceRoot) {
		t.Fatalf("expected %q (inside the workspace) to be rejected", dir)
	}
	// The workspace root itself, not just a descendant, must also be rejected.
	if editorStagingDirIsPrivate(workspaceRoot, workspaceRoot) {
		t.Fatal("expected the workspace root itself to be rejected")
	}
}

func TestEditorStagingDirIsPrivateAcceptsElsewhere(t *testing.T) {
	// workspaceRoot (via t.TempDir()) and a naive "sibling of workspaceRoot"
	// both live under os.TempDir(), so the stand-in for a real XDG config
	// directory has to be built as a sibling of the OS temp dir itself,
	// not of the workspace, to land genuinely outside both.
	workspaceRoot := t.TempDir()
	tempDir := filepath.Clean(os.TempDir())
	dir := filepath.Join(filepath.Dir(tempDir), "not-temp-not-workspace", "zero", "plan-edit")
	if !editorStagingDirIsPrivate(dir, workspaceRoot) {
		t.Fatalf("expected %q to be accepted as private", dir)
	}
}

func TestStageContentForEditorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := stageContentForEditor(dir, "session-1", "# Draft\n\nStep one.")
	if err != nil {
		t.Fatalf("stageContentForEditor: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(data) != "# Draft\n\nStep one.\n" {
		t.Fatalf("staged content = %q", string(data))
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup to remove the staged file, stat err=%v", err)
	}
}

func TestStageContentForEditorGeneratesUniquePathsPerCall(t *testing.T) {
	// Two concurrent invocations for the same session (e.g. two Zero
	// instances editing a resumed session) must not collide on one shared
	// deterministic path.
	dir := t.TempDir()
	pathA, cleanupA, err := stageContentForEditor(dir, "session-1", "draft A")
	if err != nil {
		t.Fatalf("stageContentForEditor (A): %v", err)
	}
	defer cleanupA()
	pathB, cleanupB, err := stageContentForEditor(dir, "session-1", "draft B")
	if err != nil {
		t.Fatalf("stageContentForEditor (B): %v", err)
	}
	defer cleanupB()

	if pathA == pathB {
		t.Fatalf("expected distinct staged paths, both were %q", pathA)
	}
	dataA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	dataB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if string(dataA) != "draft A\n" || string(dataB) != "draft B\n" {
		t.Fatalf("cross-contaminated staged files: A=%q B=%q", dataA, dataB)
	}

	// cleanupA must not touch B's file, and vice versa.
	cleanupA()
	if _, err := os.Stat(pathB); err != nil {
		t.Fatalf("cleanupA should not have removed B's staged file: %v", err)
	}
}
