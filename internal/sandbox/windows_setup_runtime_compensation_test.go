package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupCompensationFixture points the cache root at a test-owned directory and
// returns it with the runtime root the selector will choose.
func setupCompensationFixture(t *testing.T) (cacheRoot string, workspace string, root string) {
	t.Helper()
	cacheRoot = t.TempDir()
	workspace = t.TempDir()
	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }

	var ok bool
	root, ok = deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic runtime root under %s", cacheRoot)
	}
	return cacheRoot, workspace, root
}

func buildSetupPlan(t *testing.T, workspace string) (WindowsSandboxSetupPlan, error) {
	t.Helper()
	return BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		CommandCWD:  workspace,
		SandboxHome: t.TempDir(),
	})
}

// ownedRuntimeTreeExists reports whether the first component Zero owns is still
// there, which is the thing a rollback has to have removed.
func ownedRuntimeTreeExists(cacheRoot string) bool {
	_, err := os.Lstat(filepath.Join(canonicalSandboxWorkspaceRoot(cacheRoot), "zero"))
	return err == nil
}

// BUILDING THE ARGS IS ALREADY A TRANSACTION.
//
// Selecting the runtime root takes a lease, and taking a lease creates
// zero/runtime/v1 and the lease file when they are not there. Those writes happen
// in this process before the helper exists, and the wrapper that produced them
// dropped the record: the helper reacquires an already-existing tree and records
// no creation, provisioning records only the leaf, so a failure before the marker
// left the parents behind with nothing that knew it had made them.
func TestSetupArgsRollbackRemovesWhatTheSelectionCreated(t *testing.T) {
	cacheRoot, workspace, root := setupCompensationFixture(t)

	// SETUP: nothing of ours is there yet, or the rollback would be removing
	// somebody else's tree and the assertion below would be about the wrong thing.
	if ownedRuntimeTreeExists(cacheRoot) {
		t.Fatal("SETUP INVALID: the owned tree already exists before the selection")
	}

	plan, err := buildSetupPlan(t, workspace)
	if err != nil {
		t.Skipf("cannot build setup args here: %v", err)
	}
	if !ownedRuntimeTreeExists(cacheRoot) {
		t.Fatal("SETUP INVALID: building the args created no owned tree, so there is nothing for rollback to undo")
	}
	if _, err := os.Lstat(sandboxRuntimeLeasePath(root)); err != nil {
		t.Fatalf("SETUP INVALID: no lease file was created, so the artifact under test is missing: %v", err)
	}

	if err := plan.Rollback(); err != nil {
		t.Fatalf("rollback reported residue on a tree it had just created alone: %v", err)
	}
	if ownedRuntimeTreeExists(cacheRoot) {
		t.Error("the owned runtime tree survived a rollback of the invocation that created it")
	}
	if _, err := os.Lstat(sandboxRuntimeLeasePath(root)); err == nil {
		t.Error("the lease file survived; it keeps v1 non-empty, so the tree can never be removed")
	}
}

// A FAILURE AFTER THE TREE EXISTS COMPENSATES BEFORE IT RETURNS.
//
// This is the interval the whole finding is about. The selection has already
// created zero/runtime/v1 and the lease file, the helper does not exist yet, and
// the builder can still fail. The old shape returned that error and left the
// tree, with nothing downstream holding a record that this invocation had made
// it: the helper reacquires an existing tree and records no creation, and
// provisioning records only the leaf.
func TestSetupArgsCompensateWhenTheyFailAfterCreatingTheTree(t *testing.T) {
	cacheRoot, workspace, root := setupCompensationFixture(t)

	previousSID := setupConsumerSID
	t.Cleanup(func() { setupConsumerSID = previousSID })
	asked := false
	setupConsumerSID = func() (string, error) {
		asked = true
		// Anything that fails after the selection. The identity lookup is the only
		// step in this interval that can.
		return "", errors.New("cannot read this process's identity")
	}

	_, err := buildSetupPlan(t, workspace)

	// SETUP: the failure really landed after the selection, or this is measuring
	// an error path that never reached the state under test.
	if !asked {
		t.Skip("the builder failed before the identity lookup, so the tree was never created here")
	}
	if err == nil {
		t.Fatal("the builder returned args although the step after the selection failed")
	}

	if ownedRuntimeTreeExists(cacheRoot) {
		t.Errorf("a failed build left its owned runtime tree behind at %s: %v", cacheRoot, err)
	}
	if _, statErr := os.Lstat(sandboxRuntimeLeasePath(root)); statErr == nil {
		t.Error("a failed build left its lease file behind, which keeps v1 non-empty forever")
	}
}

// AND A FAILURE DURING THE LEASE CREATION ITSELF LEAVES NOTHING EITHER.
//
// The interval above opens once the selection has succeeded. This one is inside
// the selection: the lease file has been created and the step after it fails, so
// no lease object comes back and the builder takes the early return that
// compensates only what the selection managed to report. The created lease file
// was not part of that report, and it keeps its directory non-empty, so every
// component the same invocation created stayed behind with it.
//
// Both candidate roots are failed, because failing only the preferred one
// relocates to the fallback and the build goes on to succeed.
func TestSetupArgsCompensateWhenTheLeaseCreationFails(t *testing.T) {
	cacheRoot, workspace, root := setupCompensationFixture(t)

	runtimeCreationFailure = func(created string) error {
		if strings.HasSuffix(created, sandboxRuntimeLeaseSuffix) {
			return errors.New("injected failure after the lease file was created")
		}
		return nil
	}
	t.Cleanup(func() { runtimeCreationFailure = nil })

	if _, err := buildSetupPlan(t, workspace); err == nil {
		t.Fatal("SETUP INVALID: the builder succeeded, so the injected failure never fired and nothing here is under test")
	}
	if _, err := os.Lstat(sandboxRuntimeLeasePath(root)); err == nil {
		t.Error("the lease file this invocation created survived the failure that happened right after creating it")
	}
	if ownedRuntimeTreeExists(cacheRoot) {
		t.Error("the owned runtime tree survived a failure inside the invocation that created it")
	}
}

// A PRE-EXISTING TREE IS NOT THIS INVOCATION'S TO REMOVE.
//
// Without this, a rollback that deleted the runtime root unconditionally would
// satisfy every test above, and would destroy a tree another setup had already
// provisioned and published a marker for.
func TestSetupArgsRollbackLeavesAPreExistingTreeAlone(t *testing.T) {
	cacheRoot, workspace, root := setupCompensationFixture(t)

	// Somebody else's tree, complete with a stamp-shaped file, made before this
	// invocation runs.
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(filepath.Dir(root), "someone-elses.txt")
	if err := os.WriteFile(keep, []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// AND A LEASE FILE THAT WAS ALREADY THERE. Removing one this invocation did
	// not create takes the coordination object out from under whoever made it,
	// which is why createdness is recorded by the create rather than guessed at
	// afterwards.
	preExistingLease := sandboxRuntimeLeasePath(root)
	if err := os.WriteFile(preExistingLease, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := buildSetupPlan(t, workspace)
	if err != nil {
		t.Skipf("cannot build setup args here: %v", err)
	}
	// Rollback may report residue, because it refuses a non-empty directory on
	// purpose. What it must not do is remove any of it.
	_ = plan.Rollback()

	if !ownedRuntimeTreeExists(cacheRoot) {
		t.Fatal("rollback removed an owned tree this invocation did not create")
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatalf("rollback removed a file that was there before this invocation: %v", err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("rollback removed a pre-existing runtime root: %v", err)
	}
	if _, err := os.Lstat(preExistingLease); err != nil {
		t.Fatalf("rollback removed a lease file that was there before this invocation: %v", err)
	}
}

// AND A ROLLBACK THAT CANNOT FINISH SAYS SO.
//
// Reporting a clean undo while a tree is still on disk is worse than reporting
// the residue: the operator retries setup against state they were told was gone.
// A holder on the lease is the case that matters, because it also means the tree
// must NOT be removed while somebody is using it.
func TestSetupArgsRollbackReportsResidueWhileTheLeaseIsHeld(t *testing.T) {
	cacheRoot, workspace, root := setupCompensationFixture(t)

	plan, err := buildSetupPlan(t, workspace)
	if err != nil {
		t.Skipf("cannot build setup args here: %v", err)
	}
	if !ownedRuntimeTreeExists(cacheRoot) {
		t.Fatal("SETUP INVALID: building the args created no owned tree")
	}

	// Another process's grip on the same runtime root.
	holder, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Skipf("cannot take a second lease here: %v", err)
	}
	defer holder.release()

	undo := plan.Rollback()
	if undo == nil {
		t.Fatal("rollback claimed a clean undo while another holder had the lease; it either removed a tree in use or reported success for work it did not do")
	}
	if !strings.Contains(undo.Error(), "in use") {
		t.Errorf("the residue report does not say why it stopped: %v", undo)
	}
	if !ownedRuntimeTreeExists(cacheRoot) {
		t.Error("rollback removed the runtime tree while another process held its lease")
	}
	if _, err := os.Lstat(sandboxRuntimeLeasePath(root)); err != nil {
		t.Errorf("rollback removed a lease another process was holding: %v", err)
	}
}
