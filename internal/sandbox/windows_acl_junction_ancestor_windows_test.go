//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeJunction points link at target, skipping the test when the environment
// refuses to create one. A junction needs no privilege, which is exactly why
// this attack is reachable by an ordinary workspace owner.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v: %s", err, out)
	}
	// Assert it actually redirects. A junction that silently did nothing would
	// green this test while proving nothing.
	probe := filepath.Join(link, "redirect-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatalf("write through junction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "redirect-probe")); err != nil {
		t.Fatalf("junction does not redirect, so this test would prove nothing: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("clean probe: %v", err)
	}
}

// Elevated setup must not create anything through a reparse-point ancestor.
//
// FILE_FLAG_OPEN_REPARSE_POINT only stops the FINAL component being followed.
// materializeWindowsACLTarget used os.MkdirAll/os.OpenFile on the pathname, both
// of which resolve ancestors, so a workspace owner who turned .git into a
// junction before setup ran got objects created at a location of their choosing
// — as Administrator — and the no-follow check only rejected it afterwards, far
// too late to un-create them.
func TestMaterializeRefusesAncestorJunctionBeforeCreating(t *testing.T) {
	for name, asFile := range map[string]bool{"file target": true, "directory target": false} {
		t.Run(name, func(t *testing.T) {
			external := t.TempDir()
			workspace := t.TempDir()
			gitDir := filepath.Join(workspace, ".git")
			makeJunction(t, gitDir, external)

			target := filepath.Join(gitDir, "hooks", "config")
			_, err := materializeWindowsACLTarget(target, asFile)
			if err == nil {
				t.Fatalf("materialized %s through a junction ancestor instead of refusing", target)
			}
			if !strings.Contains(err.Error(), "reparse") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
			// Nothing may survive on the other side of the junction. The old code
			// left every intermediate directory MkdirAll had created.
			leaked, lerr := os.ReadDir(external)
			if lerr != nil {
				t.Fatalf("read external dir: %v", lerr)
			}
			if len(leaked) != 0 {
				names := make([]string, 0, len(leaked))
				for _, entry := range leaked {
					names = append(names, entry.Name())
				}
				t.Fatalf("created %v outside the workspace through the junction", names)
			}
		})
	}
}

// The ordinary path still works: no reparse point anywhere, target gets made.
func TestMaterializeStillCreatesOrdinaryTargets(t *testing.T) {
	root := t.TempDir()
	for name, asFile := range map[string]bool{"file target": true, "directory target": false} {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(root, name, "nested", "deeper", "target")
			if _, err := materializeWindowsACLTarget(target, asFile); err != nil {
				t.Fatalf("materializeWindowsACLTarget: %v", err)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatalf("target was not created: %v", err)
			}
			if info.IsDir() == asFile {
				t.Fatalf("target isDir=%v, wanted file=%v", info.IsDir(), asFile)
			}
		})
	}
}
