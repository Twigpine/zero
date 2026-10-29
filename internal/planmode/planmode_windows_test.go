//go:build windows

package planmode

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEditorStagingDirIsPrivateRejectsJunctionIntoWorkspace is the Windows
// counterpart of TestEditorStagingDirIsPrivateResolvesSymlinkedDir. That test
// skips here whenever directory-symlink creation is privileged, which left the
// staging containment check with no Windows coverage at all — and it was inert
// on exactly this platform, because filepath.EvalSymlinks hands a junction
// back unresolved while MkdirAll and CreateTemp follow it. A junction needs no
// privilege to create, so it is the reparse point that actually matters here.
func TestEditorStagingDirIsPrivateRejectsJunctionIntoWorkspace(t *testing.T) {
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
	createWindowsDirReparse(t, link, target)
	if editorStagingDirIsPrivate(link, workspaceRoot, fakeTemp) {
		t.Error("a staging dir junctioned into the workspace was accepted as private")
	}

	tempTarget := filepath.Join(fakeTemp, "hidden-staging")
	if err := os.MkdirAll(tempTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	tempLink := filepath.Join(base, "looks-private-too")
	createWindowsDirReparse(t, tempLink, tempTarget)
	if editorStagingDirIsPrivate(tempLink, workspaceRoot, fakeTemp) {
		t.Error("a staging dir junctioned into the temp root was accepted as private")
	}
}

// TestEditorStagingDirIsPrivateResolvesJunctionedRoots is the inverse
// direction: the workspace is reached through a junction, so a staging dir
// spelled with the physical workspace path does not lexically sit under the
// junctioned spelling. Physical comparison must still reject it.
func TestEditorStagingDirIsPrivateResolvesJunctionedRoots(t *testing.T) {
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
	createWindowsDirReparse(t, workspaceLink, realWorkspace)

	if editorStagingDirIsPrivate(filepath.Join(realWorkspace, "cfg"), workspaceLink, fakeTemp) {
		t.Error("a staging dir inside the physical workspace was accepted when the workspace is addressed through a junction")
	}
}

// TestResolvePhysicalTraversesJunction pins the primitive the containment check
// rests on, so a future change back to filepath.EvalSymlinks fails here with a
// clear cause rather than only as a containment miss two layers up.
func TestResolvePhysicalTraversesJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	createWindowsDirReparse(t, link, target)

	resolved, err := resolvePhysical(link)
	if err != nil {
		t.Fatalf("resolvePhysical(junction): %v", err)
	}
	wantPhysical, err := resolvePhysical(target)
	if err != nil {
		t.Fatalf("resolvePhysical(target): %v", err)
	}
	if resolved != wantPhysical {
		t.Errorf("resolvePhysical(junction) = %q, want the junction target %q", resolved, wantPhysical)
	}
	if resolved == filepath.Clean(link) {
		t.Error("resolvePhysical returned the junction itself, so the reparse point was not traversed")
	}
}

// TestVerifyPrivateDirectoryRejectsJunction covers the backstop. os.Lstat maps
// a junction to os.ModeIrregular, so the os.ModeSymlink test cannot see one and
// verifyPrivateDirectory used to accept it.
func TestVerifyPrivateDirectoryRejectsJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	createWindowsDirReparse(t, link, target)

	if err := verifyPrivateDirectory(link); err == nil {
		t.Error("verifyPrivateDirectory accepted a junction")
	}
	if err := verifyPrivateDirectory(target); err != nil {
		t.Errorf("verifyPrivateDirectory(plain directory) = %v, want nil", err)
	}
}

// TestPlanFlowRefusesJunctionedConfigRootIntoWorkspace pins the end-to-end
// outcome: a junction at %AppData% puts both the plan storage root and the
// editor staging directory physically inside the workspace while every
// component of their spelling looks ordinary.
//
// This one passes before the fix as well, and that is worth recording rather
// than hiding. Two other gates already refuse this route: the no-follow
// storage walk (OBJ_DONT_REPARSE) rejects a junctioned plans root, and
// verifyPrivateDirectory rejects a junctioned staging directory through its
// !IsDir test, because os.Lstat maps a junction to os.ModeIrregular. So the
// inert containment check was a boundary that did not hold, not a reachable
// path to a staged file. The test guards the outcome against a future change
// to either of those gates.
func TestPlanFlowRefusesJunctionedConfigRootIntoWorkspace(t *testing.T) {
	base := t.TempDir()
	// The scenario is about the workspace root, so point the temp check at an
	// unrelated directory: base itself lives under the real temp dir, which
	// would otherwise reject the paths for the wrong reason.
	fakeTemp := filepath.Join(base, "faketemp")
	workspace := filepath.Join(base, "workspace")
	insideWorkspace := filepath.Join(workspace, "sandbox-writable")
	for _, dir := range []string{fakeTemp, insideWorkspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	SetTempDirForTest(t, fakeTemp)

	appData := filepath.Join(base, "appdata")
	createWindowsDirReparse(t, appData, insideWorkspace)
	t.Setenv("AppData", appData)

	const sessionID = "sess-junction"
	refused := false
	if _, err := WritePlan(workspace, sessionID, "# plan\n"); err != nil {
		refused = true
	}
	if !refused {
		staged, cleanup, err := StageForEditor(workspace, sessionID)
		if cleanup != nil {
			cleanup()
		}
		if err != nil {
			refused = true
		} else if physical, perr := resolvePhysical(staged); perr == nil && !isUnderOrEqual(physical, physicalPath(workspace)) {
			// Staged outside the workspace after all: not the failure case.
			refused = true
		}
	}
	if !refused {
		t.Fatal("a junctioned config root let the plan flow write and stage inside the workspace")
	}

	var strays []string
	_ = filepath.WalkDir(insideWorkspace, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			strays = append(strays, path)
		}
		return nil
	})
	if len(strays) != 0 {
		t.Errorf("plan content landed inside the workspace: %v", strays)
	}
}
