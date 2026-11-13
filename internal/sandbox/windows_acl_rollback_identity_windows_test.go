//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// ROLLBACK DELETES THE OBJECT IT CREATED, NOT WHATEVER NOW WEARS ITS NAME.
//
// The anchor's identity was verified, but the created children were recorded
// by name only, so after setup materialized .git/config another process could
// move it aside and put an ordinary file at that name beneath the unchanged
// anchor, and a later setup failure deleted the replacement. Each created
// component now carries the identity read from the handle that created it,
// and the delete disposition is set on a handle whose identity was checked.
func TestRollbackRefusesAReplacedMaterializedChild(t *testing.T) {
	for name, asFile := range map[string]bool{"file target": true, "directory target": false} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			target := filepath.Join(workspace, ".git", "hooks")
			if asFile {
				target = filepath.Join(workspace, ".git", "config")
			}
			created, err := materializeWindowsACLTarget(target, asFile)
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if !created.createdAnything() {
				t.Fatal("SETUP INVALID: nothing was materialized")
			}

			// Another process moves the created object aside and puts an
			// ordinary one of the same shape at its name. The anchor above is
			// untouched, so the anchor identity check cannot see this.
			aside := target + ".aside"
			if err := os.Rename(target, aside); err != nil {
				t.Fatalf("rename aside: %v", err)
			}
			if asFile {
				if err := os.WriteFile(target, []byte("theirs"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "keep"), []byte("theirs"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			removed, err := rollbackWindowsACLMaterialization(created)
			// The replacement is the whole point: it has to survive, whatever the
			// rollback reported about itself.
			if _, statErr := os.Lstat(target); statErr != nil {
				t.Fatalf("rollback deleted the replacement at %s, which it never created: %v", target, statErr)
			}
			if !asFile {
				if _, statErr := os.Lstat(filepath.Join(target, "keep")); statErr != nil {
					t.Fatalf("the replacement directory's contents are gone: %v", statErr)
				}
			}
			if err == nil {
				t.Fatal("rollback left a replacement it never created without saying why")
			}
			if removed {
				t.Fatal("rollback reported the target removed after refusing the replacement")
			}
		})
	}
}

// CONTROL: the unreplaced case still unwinds completely, so the identity check
// cannot pass by refusing everything.
func TestRollbackRemovesTheChildrenItActuallyCreated(t *testing.T) {
	for name, asFile := range map[string]bool{"file target": true, "directory target": false} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			target := filepath.Join(workspace, ".git", "hooks")
			if asFile {
				target = filepath.Join(workspace, ".git", "config")
			}
			created, err := materializeWindowsACLTarget(target, asFile)
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			removed, err := rollbackWindowsACLMaterialization(created)
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if !removed {
				t.Fatal("rollback did not report the target removed")
			}
			if _, statErr := os.Lstat(filepath.Join(workspace, ".git")); !os.IsNotExist(statErr) {
				t.Fatalf(".git survived a rollback of what created it: %v", statErr)
			}
		})
	}
}
