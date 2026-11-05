//go:build windows

package planmode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestWritePlanRefusesStorageRootReparsePoint is the Windows counterpart of
// TestPlanStorageBaseSymlinkRefused. Directory-symlink creation is privileged
// on many runners, so that test skips here. A junction is an unprivileged
// directory reparse point and is exactly what openWindowsBaseDir's
// OBJ_DONT_REPARSE must refuse.
func TestWritePlanRefusesStorageRootReparsePoint(t *testing.T) {
	cfg := isolatePlanStorage(t)
	workspace := t.TempDir()

	if _, err := WritePlan(workspace, "session-1", "1. [pending] real step\n"); err != nil {
		t.Fatalf("WritePlan (seed): %v", err)
	}
	plansRoot := filepath.Join(cfg, filepath.FromSlash(PlanDirName))
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("mkdir elsewhere: %v", err)
	}
	if err := os.RemoveAll(plansRoot); err != nil {
		t.Fatalf("remove plans root: %v", err)
	}
	createWindowsDirReparse(t, plansRoot, elsewhere)

	absBase, err := filepath.Abs(plansRoot)
	if err != nil {
		t.Fatalf("Abs plans root: %v", err)
	}
	handle, err := openWindowsBaseDir(absBase)
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("openWindowsBaseDir accepted a reparse-point storage root")
	}
	if !errors.Is(err, errPlanSymlinkRefusal) {
		t.Fatalf("openWindowsBaseDir err = %v, want errPlanSymlinkRefusal", err)
	}

	if _, err := WritePlan(workspace, "session-1", "1. [pending] redirected\n"); err == nil {
		t.Fatal("expected WritePlan to refuse a reparse-point plan storage root")
	} else if !errors.Is(err, errPlanSymlinkRefusal) || !strings.Contains(err.Error(), "plan storage root") {
		t.Fatalf("expected WritePlan to propagate errPlanSymlinkRefusal, got: %v", err)
	}

	if content, ok, err := ReadPlan(workspace, "session-1"); err == nil {
		t.Fatalf("expected ReadPlan to refuse a reparse-point plan storage root, got ok=%t content=%q", ok, content)
	} else if !errors.Is(err, errPlanSymlinkRefusal) {
		t.Fatalf("expected ReadPlan to propagate errPlanSymlinkRefusal, got: %v", err)
	}

	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("write escaped through the storage-root reparse point into %s: %v", elsewhere, entries)
	}
}

func TestWritePlanAllowsBenignAncestorReparsePoint(t *testing.T) {
	realCfg := t.TempDir()
	junctionParent := t.TempDir()
	junctionCfg := filepath.Join(junctionParent, "junction_cfg")
	createWindowsDirReparse(t, junctionCfg, realCfg)

	isolatePlanStorage(t)
	t.Setenv("AppData", junctionCfg)
	t.Setenv("XDG_CONFIG_HOME", junctionCfg)

	workspace := t.TempDir()
	path, err := WritePlan(workspace, "session-ancestor", "1. [pending] step\n")
	if err != nil {
		t.Fatalf("WritePlan failed through benign ancestor junction: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty plan path")
	}

	content, ok, err := ReadPlan(workspace, "session-ancestor")
	if err != nil {
		t.Fatalf("ReadPlan failed through benign ancestor junction: %v", err)
	}
	if !ok || !strings.Contains(content, "step") {
		t.Fatalf("ReadPlan content mismatch: ok=%v, content=%q", ok, content)
	}
}

func createWindowsDirReparse(t *testing.T, link, target string) {
	t.Helper()
	// Prefer a junction: unlike a directory symlink it needs no
	// SeCreateSymbolicLinkPrivilege / Developer Mode.
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		if serr := os.Symlink(target, link); serr != nil {
			t.Skipf("cannot create a reparse point (junction: %v %q; symlink: %v)", err, strings.TrimSpace(string(out)), serr)
		}
	}
}
