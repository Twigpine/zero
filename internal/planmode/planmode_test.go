package planmode

import (
	"os"
	"path/filepath"
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

func TestPlanFilePathRejectsSymlinkedPlansDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".zero"), 0o700); err != nil {
		t.Fatalf("mkdir .zero: %v", err)
	}
	// Plant a symlink at .zero/plans pointing outside the workspace, as if an
	// attacker (or a stale state) had redirected it before /plan open ran.
	if err := os.Symlink(outside, filepath.Join(root, ".zero", "plans")); err != nil {
		t.Fatalf("symlink .zero/plans: %v", err)
	}

	if _, err := PlanFilePath(root, "session-1"); err == nil {
		t.Fatal("expected PlanFilePath to reject a symlinked plans directory")
	}
}

func TestPlanFilePathRejectsSymlinkedPlanFile(t *testing.T) {
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

	if _, err := PlanFilePath(root, "session-1"); err == nil {
		t.Fatal("expected PlanFilePath to reject a symlinked plan file")
	}
}
