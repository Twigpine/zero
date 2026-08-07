package planmode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	configDir := filepath.Join(root, "config")
	tempDir := filepath.Join(root, "tmp")
	_ = os.MkdirAll(configDir, 0o700)
	_ = os.MkdirAll(tempDir, 0o700)
	setUserConfigHomeEnv(t, configDir)
	SetTempDirForTest(t, tempDir)
	return configDir
}

func TestPlanFilePathSeparatesSlugCollisions(t *testing.T) {
	// slugify alone maps '_' and '-' to the same dash form, so plan_a and
	// plan-a (and workspaces foo_bar / foo-bar) must not share a path.
	isolatePlanStorage(t)
	root := t.TempDir()
	a, err := PlanFilePath(root, "plan_a")
	if err != nil {
		t.Fatalf("PlanFilePath plan_a: %v", err)
	}
	b, err := PlanFilePath(root, "plan-a")
	if err != nil {
		t.Fatalf("PlanFilePath plan-a: %v", err)
	}
	if a == b {
		t.Fatalf("slug-colliding session IDs must not share a plan path, both %q", a)
	}

	wsA := filepath.Join(root, "foo_bar")
	wsB := filepath.Join(root, "foo-bar")
	if err := os.MkdirAll(wsA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wsB, 0o700); err != nil {
		t.Fatal(err)
	}
	pathA, err := PlanFilePath(wsA, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath wsA: %v", err)
	}
	pathB, err := PlanFilePath(wsB, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath wsB: %v", err)
	}
	if pathA == pathB {
		t.Fatalf("slug-colliding workspaces must not share a plan path, both %q", pathA)
	}
	if filepath.Dir(pathA) == filepath.Dir(pathB) {
		t.Fatalf("workspace path keys collided: %q and %q share dir", pathA, pathB)
	}
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

// TestReadPlanMissingSessionWithBasePresent covers the NtCreateFile /
// openat walk when the plan storage base exists (another session already
// wrote a plan) but this session's path is absent. Missing-file NTSTATUS
// values must map to os.ErrNotExist so ReadPlan returns ("", false, nil).
func TestReadPlanMissingSessionWithBasePresent(t *testing.T) {
	isolatePlanStorage(t)
	root := t.TempDir()
	if _, err := WritePlan(root, "other-session", "notes"); err != nil {
		t.Fatalf("WritePlan other: %v", err)
	}
	content, ok, err := ReadPlan(root, "no-such-session")
	if err != nil {
		t.Fatalf("ReadPlan missing session: %v", err)
	}
	if ok {
		t.Fatal("expected missing session plan to report ok=false")
	}
	if content != "" {
		t.Fatalf("expected empty content for missing plan, got %q", content)
	}
}

func TestPathKeyLongWorkspaceWithinNameMax(t *testing.T) {
	// slugify keeps one output character per path character, so a workspace
	// path longer than NAME_MAX would produce an oversized directory component
	// without the pathKey slug cap. The hash suffix keeps injectivity.
	isolatePlanStorage(t)
	longSeg := strings.Repeat("deepseg", 40) // 280 chars
	root := filepath.Join(t.TempDir(), longSeg, longSeg)
	if len(root) <= 255 {
		t.Fatalf("setup: expected workspace path >255 chars, got %d", len(root))
	}
	path, err := PlanFilePath(root, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	for _, part := range strings.Split(path, string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		if len(part) > 255 {
			t.Fatalf("path component %q exceeds NAME_MAX (255), len=%d", part, len(part))
		}
	}
	// Write/read must succeed: MkdirAll would ENAMETOOLONG without the cap.
	if _, err := WritePlan(root, "session-1", "long path plan"); err != nil {
		t.Fatalf("WritePlan long workspace: %v", err)
	}
	content, ok, err := ReadPlan(root, "session-1")
	if err != nil {
		t.Fatalf("ReadPlan long workspace: %v", err)
	}
	if !ok || content != "long path plan\n" {
		t.Fatalf("unexpected plan after long-workspace write: ok=%v content=%q", ok, content)
	}
	// Distinct long workspaces still get distinct keys (hash injectivity).
	other := root + "-other"
	pathOther, err := PlanFilePath(other, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath other: %v", err)
	}
	if path == pathOther {
		t.Fatalf("long workspaces must not share a plan path: %q", path)
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

// TestReadPlanFileRejectsIntermediateSymlink covers the bind-at-open property
// that final-component-only O_NOFOLLOW does not provide: a parent component
// replaced with a symlink (or Windows reparse point) to a directory outside
// the plan storage base must not yield the outside file's contents.
//
// Called against readPlanFile directly so the pre-open EvalSymlinks check in
// ensurePlanPathContained cannot mask a weak open. On Windows, os.Symlink for
// a directory creates a reparse point when the privilege is available.
func TestReadPlanFileRejectsIntermediateSymlink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	secret := []byte("outside-secret\n")
	if err := os.WriteFile(filepath.Join(outside, "plan.md"), secret, 0o600); err != nil {
		t.Fatalf("write outside plan: %v", err)
	}
	parentLink := filepath.Join(base, "ws-key")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Skipf("directory symlinks/reparse points unavailable: %v", err)
	}
	path := filepath.Join(parentLink, "plan.md")

	data, err := readPlanFile(base, path)
	if err == nil {
		t.Fatalf("expected intermediate symlink to be refused, got content %q", data)
	}
	if len(data) > 0 {
		t.Fatalf("refused read must not return bytes, got %q", data)
	}
	// Victim outside the base must be untouched and must not have been
	// returned as a successful plan read.
	got, err := os.ReadFile(filepath.Join(outside, "plan.md"))
	if err != nil {
		t.Fatalf("read outside plan: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("outside plan was modified: %q", got)
	}
}

func TestReadPlanFileRejectsFinalSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ws-key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outsideFile := filepath.Join(t.TempDir(), "exfil.md")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	path := filepath.Join(dir, "session.md")
	if err := os.Symlink(outsideFile, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	data, err := readPlanFile(base, path)
	if err == nil {
		t.Fatalf("expected final symlink to be refused, got %q", data)
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("expected symlink refusal, got: %v", err)
	}
}

// TestReadPlanFileRejectsInRootFinalSymlink covers the os.Root.Open race that
// root.Lstat-then-root.Open cannot close: when the final name is replaced with
// a symlink whose target remains inside the storage base, os.Root.Open follows
// it via checkSymlink after O_NOFOLLOW fails. The no-follow walker must refuse
// without returning the in-root target's contents.
//
// This is the sequential stand-in for the Lstat/Open TOCTOU: plant the final
// symlink before open and prove we never follow it, even in-root.
func TestReadPlanFileRejectsInRootFinalSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ws-key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	other := filepath.Join(dir, "other-plan.md")
	if err := os.WriteFile(other, []byte("other-plan\n"), 0o600); err != nil {
		t.Fatalf("write other plan: %v", err)
	}
	path := filepath.Join(dir, "requested.md")
	// Relative target stays inside the base, which is exactly the case
	// os.Root.Open would follow.
	if err := os.Symlink("other-plan.md", path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	data, err := readPlanFile(base, path)
	if err == nil {
		t.Fatalf("expected in-root final symlink to be refused, got %q", data)
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("expected symlink refusal, got: %v", err)
	}
	if len(data) > 0 {
		t.Fatalf("refused read must not return bytes, got %q", data)
	}
	// Victim in-root target must be untouched and must not have been returned.
	got, err := os.ReadFile(other)
	if err != nil {
		t.Fatalf("read other plan: %v", err)
	}
	if string(got) != "other-plan\n" {
		t.Fatalf("other plan was modified: %q", got)
	}
}

// TestReadPlanFileRefusesAfterReplaceWithSymlink simulates the Lstat/Open
// replace-with-symlink race: a regular plan is swapped for an in-root symlink
// before readPlanFile runs. The open must refuse rather than follow.
func TestReadPlanFileRefusesAfterReplaceWithSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ws-key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "session.md")
	if err := os.WriteFile(path, []byte("requested-plan\n"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	other := filepath.Join(dir, "other.md")
	if err := os.WriteFile(other, []byte("other-plan\n"), 0o600); err != nil {
		t.Fatalf("write other: %v", err)
	}

	// Sequential stand-in for the race window: remove the regular file and
	// plant an in-root symlink at the same name before the open.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove plan: %v", err)
	}
	if err := os.Symlink("other.md", path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	data, err := readPlanFile(base, path)
	if err == nil {
		t.Fatalf("expected replaced-with-symlink plan to be refused, got %q", data)
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("expected symlink refusal, got: %v", err)
	}
	if string(data) == "other-plan\n" {
		t.Fatal("open followed the in-root symlink planted after the regular file existed")
	}
}

func TestReadPlanFileRoundtripPlainFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ws-key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "session.md")
	want := "# plan\n\nstep one\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	got, err := readPlanFile(base, path)
	if err != nil {
		t.Fatalf("readPlanFile: %v", err)
	}
	if string(got) != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestReadPlanFileRejectsNonRegularFile pins the non-regular refusal:
// Unix: a planted FIFO must not hang open (O_NONBLOCK) and must be refused
// as "not a regular file". Windows: a directory at the plan path is refused
// the same way (no FIFO create API).
func TestReadPlanFileRejectsNonRegularFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ws-key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "session.md")

	if runtime.GOOS == "windows" {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("mkdir plan path: %v", err)
		}
	} else {
		if err := mkfifoForTest(path); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := readPlanFile(base, path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a non-regular plan path to be refused")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("expected the regular-file refusal, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readPlanFile blocked on a non-regular target; O_NONBLOCK (or equivalent) is missing from the final open")
	}
}

// TestWritePlanRefusesIntermediateSymlink pins that WritePlan's handle-bound
// walk refuses an intermediate directory that is a symlink rather than
// following it with pathname MkdirAll/OpenFile/Rename.
func TestWritePlanRefusesIntermediateSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Creating directory symlinks requires elevated privileges on many
		// Windows runners; skip rather than flake.
		t.Skip("directory symlink creation is privileged on Windows CI")
	}
	cfg := isolatePlanStorage(t)
	workspace := t.TempDir()
	path, err := PlanFilePath(workspace, "session-1")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	// Create the plans root, then plant the workspace-key component as a
	// symlink that points outside the storage tree.
	plansRoot := filepath.Join(cfg, filepath.FromSlash(PlanDirName))
	if err := os.MkdirAll(plansRoot, 0o700); err != nil {
		t.Fatalf("mkdir plans root: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	wsKeyDir := filepath.Dir(path)
	if err := os.Symlink(outside, wsKeyDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err = WritePlan(workspace, "session-1", "notes")
	if err == nil {
		t.Fatal("expected WritePlan to refuse intermediate symlink")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("expected symlink refusal, got: %v", err)
	}
	// Nothing should have been written through the link.
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("write escaped through intermediate symlink into %s: %v", outside, entries)
	}
}

func TestWritePlanRejectsStorageInsideWorkspace(t *testing.T) {
	// If the user config root is pointed at the workspace, plan storage would
	// become a silent workspace write. Refuse rather than undermine the
	// read-only / no-write-grant contract.
	workspace := t.TempDir()
	// Point the temp root elsewhere so the temp-directory rule cannot mask a
	// missing workspace check (t.TempDir() lives under the real os.TempDir()).
	SetTempDirForTest(t, filepath.Join(t.TempDir(), "unrelated-temp"))
	setUserConfigHomeEnv(t, workspace)
	_, err := WritePlan(workspace, "session-1", "notes")
	if err == nil {
		t.Fatal("expected WritePlan to reject plan storage inside the workspace")
	}
	if !strings.Contains(err.Error(), "resolves into the workspace") {
		t.Fatalf("expected the workspace containment error, got: %v", err)
	}
}

func TestPlanFilePathBlankIDDiffersFromLiteralPlan(t *testing.T) {
	// pathKey must stay injective: the no-session fallback must not collide
	// with a session whose ID is literally "plan".
	isolatePlanStorage(t)
	root := t.TempDir()
	blank, err := PlanFilePath(root, "")
	if err != nil {
		t.Fatalf("PlanFilePath blank: %v", err)
	}
	named, err := PlanFilePath(root, "plan")
	if err != nil {
		t.Fatalf("PlanFilePath plan: %v", err)
	}
	if blank == named {
		t.Fatalf("blank session ID and \"plan\" must not share a plan path: %q", blank)
	}
}

func TestStageForEditorRejectsStagingInsideWorkspace(t *testing.T) {
	// StageForEditor must refuse a config root inside the workspace: that is
	// the same silent sandbox-writable staging boundary as WritePlan.
	workspace := t.TempDir()
	SetTempDirForTest(t, filepath.Join(t.TempDir(), "unrelated-temp"))
	setUserConfigHomeEnv(t, workspace)
	_, cleanup, err := StageForEditor(workspace, "session-1")
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("expected StageForEditor to reject staging inside the workspace")
	}
	if !strings.Contains(err.Error(), "sandbox-writable") && !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("expected workspace/staging containment error, got: %v", err)
	}
}

func TestStageForEditorWritesUnderConfigStagingDir(t *testing.T) {
	// Config and the privacy-check temp root must both be redirectable. Build
	// them under t.TempDir() and point SetTempDirForTest at a sibling so
	// StageForEditor's effectiveTempDir() seam (and WritePlan containment)
	// agree without planting a config root beside the real OS temp dir
	// (which fails with permission denied on Linux CI and on Windows drive roots).
	configDir := isolatePlanStorage(t)

	workspace := t.TempDir()
	if _, err := WritePlan(workspace, "session-1", "1. [pending] draft step\n"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	staged, cleanup, err := StageForEditor(workspace, "session-1")
	if err != nil {
		t.Fatalf("StageForEditor: %v", err)
	}
	t.Cleanup(cleanup)
	wantRoot := filepath.Join(configDir, "zero", "plan-edit")
	physStaged := staged
	if resolved, err := filepath.EvalSymlinks(staged); err == nil {
		physStaged = resolved
	}
	physWant, err := filepath.EvalSymlinks(wantRoot)
	if err != nil {
		physWant = wantRoot
	}
	rel, err := filepath.Rel(physWant, physStaged)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("staged path %q not under config staging dir %q", staged, wantRoot)
	}
	data, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !strings.Contains(string(data), "draft step") {
		t.Fatalf("staged content missing plan body: %q", data)
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
