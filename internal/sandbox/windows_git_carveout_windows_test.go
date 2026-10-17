//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The git control-plane carveouts are two different shapes: .git/hooks is a
// directory, .git/config is a FILE. Materialization has to respect that.
//
// On a fresh workspace neither exists yet, which is precisely the case
// Materialize was added for — so this is the common path, not a corner. Creating
// .git/config as a directory does not just mis-ACL it: it makes the workspace
// permanently unusable, because git refuses to initialise over a directory
// where its config file belongs.
func TestPrincipalACLPlanMaterializesGitConfigAsFile(t *testing.T) {
	workspace := t.TempDir()

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		// Guests, deliberately: the deny-write ACE must land on the sandbox
		// principal, not on whoever runs the test. With Everyone (S-1-1-0) the
		// ACE denies the test process too and `git init` fails with "Permission
		// denied" for a reason that has nothing to do with the shape bug.
		PrincipalSID: "S-1-5-32-546",
		WriteRoots: []WritableRoot{{
			Root:             workspace,
			ReadOnlySubpaths: gitMetadataWriteCarveouts(workspace),
		}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}

	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() {
		if rollback != nil {
			_ = rollback()
		}
	})

	configPath := filepath.Join(workspace, ".git", "config")
	if info, err := os.Stat(configPath); err == nil && info.IsDir() {
		t.Errorf(".git/config was materialized as a directory; git requires a file")
	}
	hooksPath := filepath.Join(workspace, ".git", "hooks")
	if info, err := os.Stat(hooksPath); err == nil && !info.IsDir() {
		t.Errorf(".git/hooks was materialized as a file; git requires a directory")
	}

	// The failure users would actually hit.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; shape assertions above still ran")
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = workspace
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("git init failed on a workspace after sandbox setup: %v\n%s", err, out)
	}
}
