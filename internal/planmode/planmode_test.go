package planmode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setUserConfigHomeEnv points config.UserConfigDir at dir. os.UserConfigDir
// (which UserConfigDir defers to outside darwin) reads %AppData% on Windows
// and ignores XDG_CONFIG_HOME there, so a test that only sets XDG_CONFIG_HOME
// silently fails to isolate storage on Windows and falls through to the
// runner's real profile directory.
func setUserConfigHomeEnv(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", dir)
		return
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
}

// isolatePlanStorage redirects the user config root so plan files land under a
// throwaway directory rather than the real ~/.config. Durable plans live under
// UserConfigDir (not the workspace), so every planmode test must isolate it.
func isolatePlanStorage(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	setUserConfigHomeEnv(t, root)
	return root
}

func TestPlanFilePathIsStableAcrossCalls(t *testing.T) {
	isolatePlanStorage(t)
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
	isolatePlanStorage(t)
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

func TestPlanFilePathLivesOutsideWorkspace(t *testing.T) {
	// Regression for the update_plan auto-persist write: durable plan state
	// must not land under the workspace, or a read-only auto-allowed tool
	// would create/overwrite workspace files without a write grant.
	cfg := isolatePlanStorage(t)
	workspace := t.TempDir()
	path, err := PlanFilePath(workspace, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if isUnderOrEqual(path, workspace) {
		t.Fatalf("plan path %q must not live under the workspace %q", path, workspace)
	}
	if !isUnderOrEqual(path, cfg) {
		t.Fatalf("plan path %q must live under the user config root %q", path, cfg)
	}
	if !strings.Contains(path, filepath.FromSlash(PlanDirName)) {
		t.Fatalf("plan path %q must include %q", path, PlanDirName)
	}
}

func TestWritePlanUsesRestrictivePermissions(t *testing.T) {
	// Windows reports 0666 for a plan file regardless of the mode passed to
	// OpenFile - NTFS permissions are governed by ACLs, not the POSIX mode
	// bits Go maps them to. Assert the mode bits only where they mean
	// something; Windows containment relies on path isolation instead.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	isolatePlanStorage(t)
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
	isolatePlanStorage(t)
	root := t.TempDir()
	path, err := PlanFilePath(root, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	planDir := filepath.Dir(path)
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatalf("pre-create loose plan dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("pre-create loose plan file: %v", err)
	}

	written, err := WritePlan(root, "session-1", "notes")
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	info, err := os.Stat(written)
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

func TestWritePlanDoesNotTouchWorkspace(t *testing.T) {
	// Core P1 regression: persisting a plan must not create anything under
	// the workspace, even via .zero/plans (the previous location).
	isolatePlanStorage(t)
	workspace := t.TempDir()
	if _, err := WritePlan(workspace, "session-1", "notes"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".zero")); !os.IsNotExist(err) {
		t.Fatalf("WritePlan must not create .zero under the workspace, stat err=%v", err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatalf("ReadDir workspace: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty workspace after WritePlan, got %v", entries)
	}
}

func TestReadWritePlanRoundtrip(t *testing.T) {
	isolatePlanStorage(t)
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
	isolatePlanStorage(t)
	root := t.TempDir()
	_, ok, err := ReadPlan(root, "no-such-session")
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if ok {
		t.Fatal("expected ReadPlan to report no file for a session that never opened one")
	}
}

func TestWritePlanRejectsSymlinkedPlanFile(t *testing.T) {
	isolatePlanStorage(t)
	root := t.TempDir()
	path, err := PlanFilePath(root, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir plan dir: %v", err)
	}
	outsideFile := filepath.Join(t.TempDir(), "exfil.md")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsideFile, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := WritePlan(root, "session-1", "notes"); err == nil {
		t.Fatal("expected WritePlan to reject a symlinked plan file")
	}
	if _, _, err := ReadPlan(root, "session-1"); err == nil {
		t.Fatal("expected ReadPlan to reject a symlinked plan file")
	}
	// Victim content must be untouched.
	data, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(data) != "secret" {
		t.Fatalf("symlinked victim was modified: %q", data)
	}
}

func TestWritePlanRejectsStorageInsideWorkspace(t *testing.T) {
	// If the user config root is pointed at the workspace, plan storage would
	// become a silent workspace write. Refuse rather than undermine the
	// read-only / no-write-grant contract.
	workspace := t.TempDir()
	setUserConfigHomeEnv(t, workspace)
	if _, err := WritePlan(workspace, "session-1", "notes"); err == nil {
		t.Fatal("expected WritePlan to reject plan storage inside the workspace")
	}
}

func TestEditorStagingDirIsPrivateRejectsOSTempDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	// t.TempDir() itself lives under os.TempDir(), so it doubles as a stand-in
	// for what config.UserConfigDir() would resolve to if XDG_CONFIG_HOME were
	// pointed at the OS temp directory.
	dir := t.TempDir()
	if editorStagingDirIsPrivate(dir, workspaceRoot, os.TempDir()) {
		t.Fatalf("expected %q (under the OS temp dir) to be rejected", dir)
	}
}

func TestEditorStagingDirIsPrivateRejectsWorkspaceDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	dir := filepath.Join(workspaceRoot, ".config", "zero", "plan-edit")
	if editorStagingDirIsPrivate(dir, workspaceRoot, os.TempDir()) {
		t.Fatalf("expected %q (inside the workspace) to be rejected", dir)
	}
	// The workspace root itself, not just a descendant, must also be rejected.
	if editorStagingDirIsPrivate(workspaceRoot, workspaceRoot, os.TempDir()) {
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
	if !editorStagingDirIsPrivate(dir, workspaceRoot, os.TempDir()) {
		t.Fatalf("expected %q to be accepted as private", dir)
	}
}

func TestEditorStagingDirIsPrivateResolvesSymlinkedDir(t *testing.T) {
	// An XDG config path that is lexically outside both roots but is a
	// symlink INTO the workspace (or temp) must be rejected: MkdirAll and
	// CreateTemp follow the link, so judging the spelled path would stage
	// the file somewhere sandbox-writable. The fake temp root keeps the
	// scenario constructible portably (everything a test may create lives
	// under the real temp dir, which would otherwise mask the workspace case).
	base := t.TempDir()
	fakeTemp := filepath.Join(base, "faketemp")
	workspaceRoot := filepath.Join(base, "workspace")
	target := filepath.Join(workspaceRoot, "hidden-staging")
	if err := os.MkdirAll(fakeTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "looks-private")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if editorStagingDirIsPrivate(link, workspaceRoot, fakeTemp) {
		t.Fatal("expected a staging dir symlinked into the workspace to be rejected")
	}

	// Same for a link into the temp root.
	tempTarget := filepath.Join(fakeTemp, "hidden-staging")
	if err := os.MkdirAll(tempTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	tempLink := filepath.Join(base, "looks-private-too")
	if err := os.Symlink(tempTarget, tempLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if editorStagingDirIsPrivate(tempLink, workspaceRoot, fakeTemp) {
		t.Fatal("expected a staging dir symlinked into the temp root to be rejected")
	}
}

func TestEditorStagingDirIsPrivateResolvesSymlinkedRoots(t *testing.T) {
	// The inverse direction: the WORKSPACE itself is reached through a
	// symlink, so a staging dir spelled via the physical workspace path does
	// not lexically sit under the symlinked spelling. Physical comparison
	// must still reject it.
	base := t.TempDir()
	fakeTemp := filepath.Join(base, "faketemp")
	realWorkspace := filepath.Join(base, "real-workspace")
	if err := os.MkdirAll(fakeTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realWorkspace, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceLink := filepath.Join(base, "workspace-link")
	if err := os.Symlink(realWorkspace, workspaceLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if editorStagingDirIsPrivate(filepath.Join(realWorkspace, "cfg"), workspaceLink, fakeTemp) {
		t.Fatal("expected a staging dir inside the physical workspace to be rejected when the workspace is addressed through a symlink")
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

func TestStageContentForEditorTightensPreExistingLoosePermissions(t *testing.T) {
	// Regression: MkdirAll(0700) does not change an existing group/world-
	// writable plan-edit directory. stageContentForEditor must chmod before
	// CreateTemp so a closed staged file is not writable by another local
	// user before $EDITOR reopens it.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod loose staging dir: %v", err)
	}
	path, cleanup, err := stageContentForEditor(dir, "session-1", "draft")
	if err != nil {
		t.Fatalf("stageContentForEditor: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat staging dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		t.Fatalf("expected staging dir tightened away from group/world write, got %o", perm)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
}

func TestVerifyPrivateDirectoryRejectsGroupWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := verifyPrivateDirectory(dir); err == nil {
		t.Fatal("expected verifyPrivateDirectory to reject a group-writable directory")
	}
}

func TestVerifyPrivateDirectoryAcceptsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := verifyPrivateDirectory(dir); err != nil {
		t.Fatalf("verifyPrivateDirectory: %v", err)
	}
}
