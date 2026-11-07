package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A FAILED SETUP MUST LEAVE NOTHING IT CREATED.
//
// The stamp goes inside the runtime root and is written before the marker file
// is renamed into place, so any failure after that point left the root holding a
// file this run had written. The directory removal refuses a non-empty directory
// on purpose, to avoid turning a failed setup into data loss, and the two
// combined meant the residue could never be cleaned up: a failed run kept the
// persistent runtime tree it had just created, permanently.
func TestRollbackRemovesTheStampItWroteAndThenTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "zero", "runtime", "v1", "abcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}

	// Snapshot BEFORE the stamp exists, which is the fresh-setup case.
	snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, testStampPlanHash)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := writeWindowsSandboxRuntimeStamp(root, "planhash"); err != nil {
		t.Fatalf("writeWindowsSandboxRuntimeStamp: %v", err)
	}

	rollback := windowsRuntimeRootRollback{
		created: createdRuntimeDirsForTest(
			filepath.Join(parent, "zero"),
			filepath.Join(parent, "zero", "runtime"),
			filepath.Join(parent, "zero", "runtime", "v1"),
			root,
		),
		stamp: snapshot,
	}
	if err := rollback.run(); err != nil {
		t.Fatalf("rollback.run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "zero")); !os.IsNotExist(err) {
		t.Errorf("the failed setup kept the runtime tree it created (stat err %v)", err)
	}
}

// And a stamp that was already there is RESTORED, not deleted.
//
// Deleting it would leave a machine whose previous setup succeeded with a valid
// marker pointing at a tree with no stamp, which reads as "the runtime directory
// was removed since setup ran". A healthy machine would start reporting itself
// broken because an unrelated later setup failed.
//
// The stamp is named after the plan it attests, so there are two shapes of
// "already there", and a failed setup has to leave the previous one standing in
// both.
func TestRollbackRestoresAPreviousSetupsStamp(t *testing.T) {
	// A DIFFERENT PLAN. The previous setup's attestation is a different file, so
	// this run never reaches it: only the stamp this run wrote is taken back.
	t.Run("a failed setup for another plan", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		if err := writeWindowsSandboxRuntimeStamp(root, "the-previous-setup"); err != nil {
			t.Fatalf("writeWindowsSandboxRuntimeStamp: %v", err)
		}
		snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, "this-run")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if snapshot.priorState != runtimeStampAbsent {
			t.Fatalf("SETUP INVALID: this run's plan already had a stamp (state %v)", snapshot.priorState)
		}
		if err := writeWindowsSandboxRuntimeStamp(root, "this-run"); err != nil {
			t.Fatalf("writeWindowsSandboxRuntimeStamp: %v", err)
		}

		// created is empty: this run found the root already there and owns none of it.
		if err := (windowsRuntimeRootRollback{stamp: snapshot}).run(); err != nil {
			t.Fatalf("rollback.run: %v", err)
		}

		kept, err := os.ReadFile(windowsSandboxRuntimeStampPath(root, "the-previous-setup"))
		if err != nil {
			t.Fatalf("the previous setup's stamp is gone: %v", err)
		}
		if string(kept) != "the-previous-setup" {
			t.Errorf("the previous stamp is %q, want %q", kept, "the-previous-setup")
		}
		if _, err := os.Stat(windowsSandboxRuntimeStampPath(root, "this-run")); !os.IsNotExist(err) {
			t.Errorf("the failed run's own stamp was left behind (stat err %v)", err)
		}
	})

	// THE SAME PLAN, re-applied. Now the file is shared with the previous
	// success, so what this run overwrote has to be put back rather than removed.
	t.Run("a failed re-run of the same plan", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		if err := writeWindowsSandboxRuntimeStamp(root, testStampPlanHash); err != nil {
			t.Fatalf("writeWindowsSandboxRuntimeStamp: %v", err)
		}
		snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, testStampPlanHash)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if snapshot.priorState != runtimeStampPresent {
			t.Fatalf("SETUP INVALID: the previous stamp was not recorded (state %v)", snapshot.priorState)
		}
		if err := writeWindowsSandboxRuntimeStamp(root, testStampPlanHash); err != nil {
			t.Fatalf("writeWindowsSandboxRuntimeStamp: %v", err)
		}

		if err := (windowsRuntimeRootRollback{stamp: snapshot}).run(); err != nil {
			t.Fatalf("rollback.run: %v", err)
		}

		restored, err := os.ReadFile(windowsSandboxRuntimeStampPath(root, testStampPlanHash))
		if err != nil {
			t.Fatalf("the previous setup's stamp is gone: %v", err)
		}
		if string(restored) != testStampPlanHash {
			t.Errorf("the stamp is %q, want the previous setup's %q", restored, testStampPlanHash)
		}
	})
}

// Pre-existing content is never removed, whatever else the rollback does.
func TestRollbackRefusesToRemoveWhatItDidNotCreate(t *testing.T) {
	root := t.TempDir()
	theirs := filepath.Join(root, "somebody-elses-file")
	if err := os.WriteFile(theirs, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("seed the pre-existing file: %v", err)
	}

	if err := (windowsRuntimeRootRollback{created: createdRuntimeDirsForTest(root)}).run(); err == nil {
		t.Error("a non-empty directory was removed without complaint")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("pre-existing content was destroyed by rollback: %v", err)
	}
}

// One broken compensation must not strand the others. The stamp restore is
// attempted first, and a failure there has to be reported without stopping the
// directory removal.
func TestRollbackContinuesAfterACompensationFails(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "zero", "runtime")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}

	// The failure is a root that is no longer the directory this run stamped,
	// which is a shape production actually produces.
	//
	// It used to be a snapshot pointing at an unwritable stamp path. That stopped
	// meaning anything once compensation began deriving the stamp from the verified
	// root handle rather than the recorded path: the restore then wrote a stamp INTO
	// the directory the rollback goes on to remove, so the removal failed for a
	// reason this test is not about. It passed on an unelevated box and failed on
	// every CI runner, because whether that recreate succeeds depends on the token.
	stamp := windowsSandboxStampSnapshot{
		path:           windowsSandboxRuntimeStampPath(root, testStampPlanHash),
		prior:          []byte("x"),
		priorState:     runtimeStampPresent,
		root:           root,
		rootIdentity:   "0:0:0",
		rootIdentified: true,
	}
	rollback := windowsRuntimeRootRollback{
		created: createdRuntimeDirsForTest(filepath.Join(parent, "zero"), root),
		stamp:   stamp,
	}

	err := rollback.run()
	if err == nil {
		t.Fatal("the failed stamp restore was not reported")
	}
	if _, statErr := os.Stat(filepath.Join(parent, "zero")); !os.IsNotExist(statErr) {
		t.Errorf("the directory removal was skipped because the stamp restore failed (stat err %v); every compensation has to run", statErr)
	}
}

// A FAILING ACL ROLLBACK MUST NOT STRAND THE RUNTIME ROLLBACK.
//
// The two undos used to compose by calling each other, and the outer one
// returned as soon as the ACL rollback reported an error. The runtime rollback
// then never ran, so the failure most likely to leave a machine in a strange
// state was the one failure that skipped half the cleanup.
//
// Composed through a function with no build tag on purpose: the setup entry
// point is Windows-only and needs Administrator plus WFP to reach, so a test
// there would run on nobody's machine and prove nothing on CI.
func TestEveryCompensationRunsWhenTheACLRollbackFails(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "zero", "runtime")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}

	aclCalled := false
	err := runWindowsSandboxSetupCompensations(
		errors.New("the setup failure"),
		func() error { aclCalled = true; return errors.New("acl restore exploded") },
		windowsRuntimeRootRollback{created: createdRuntimeDirsForTest(filepath.Join(parent, "zero"), root)},
	)
	if !aclCalled {
		t.Fatal("the ACL rollback was never attempted")
	}
	if err == nil {
		t.Fatal("the failures were not reported")
	}
	for _, want := range []string{"the setup failure", "acl restore exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report drops %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(parent, "zero")); !os.IsNotExist(statErr) {
		t.Errorf("the runtime rollback was skipped because the ACL rollback failed (stat err %v)", statErr)
	}
}
