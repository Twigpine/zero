//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// leaseRootUnder builds a production-shaped runtime root beneath a test-owned
// cache directory, so the owned tail is real rather than assumed.
func leaseRootUnder(t *testing.T, cacheRoot string) string {
	t.Helper()
	workspace := t.TempDir()
	root, ok := deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic runtime root under %s", cacheRoot)
	}
	return root
}

// THE LEASE IS THE FIRST WRITE SETUP MAKES, SO IT IS THE ONE THAT MATTERS MOST.
//
// prepareSandboxRuntimeLease checked the owned components for aliases and then
// created the parent with os.MkdirAll and opened "<root>.lease" by pathname.
// Both follow. An ordinary same-account process could replace "zero", "runtime"
// or "v1" with a junction after the check and before either call, and elevated
// setup would then build the tree and the lease file inside somebody else's
// target. Putting the component back afterwards left the later handle-relative
// provisioning working on the legitimate tree, so no post-check ever saw it, and
// the rollback could not compensate what it had no record of.
//
// A junction needs no privilege on Windows, so the caller's half of this runs on
// an ordinary unelevated box.
func TestRuntimeLeaseRefusesAJunctionPlantedAfterTheCheck(t *testing.T) {
	cacheRoot := t.TempDir()
	target := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)

	owned := filepath.Join(canonicalSandboxWorkspaceRoot(cacheRoot), "zero")
	if !strings.HasPrefix(strings.ToLower(root), strings.ToLower(owned)) {
		t.Fatalf("SETUP INVALID: %s does not sit under the owned component %s", root, owned)
	}

	// THE JUNCTION ARRIVES IN THE WINDOW, WHICH IS THE WHOLE POINT.
	//
	// One that is already there when acquisition starts was refused by the old
	// alias pre-check too, so a test that plants it up front passes against the
	// defective implementation and proves nothing. This one lands after the
	// decision and before the first create, which is exactly where os.MkdirAll and
	// the pathname lease open used to follow it.
	previous := runtimeLeasePreCreateBarrier
	t.Cleanup(func() { runtimeLeasePreCreateBarrier = previous })
	attempted, planted := false, false
	runtimeLeasePreCreateBarrier = func() {
		if attempted {
			return
		}
		attempted = true
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", owned, target).CombinedOutput(); err != nil {
			t.Logf("mklink /J unavailable: %v: %s", err, out)
			return
		}
		planted = true
	}

	lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if lease != nil {
		lease.release()
	}
	if !attempted {
		t.Skip("the pre-create barrier never ran, so the race was not reproduced")
	}
	// Bound to mklink's result, not to the attempt. When it fails, acquisition
	// goes on to create an ordinary directory at the same name, the check below
	// finds it, and a correct acquisition would be failed as a junction bypass.
	if !planted {
		t.Skip("mklink /J could not create the junction here, so the race was not reproduced")
	}
	if _, statErr := os.Lstat(owned); statErr != nil {
		t.Skipf("the junction could not be created here: %v", statErr)
	}

	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("lease acquisition wrote %v beneath a junction planted after its own check; err=%v", names, err)
	}
	if err == nil {
		t.Fatal("lease acquisition reported success through a junction planted after its check")
	}
}

// And an ordinary cache tree still gets its lease, or the refusal above would be
// satisfied by an acquirer that refuses everything.
func TestRuntimeLeaseStillAcquiresOnAnOrdinaryTree(t *testing.T) {
	cacheRoot := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)

	lease, created, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("an ordinary cache tree was refused: %v", err)
	}
	defer lease.release()

	if _, statErr := os.Stat(sandboxRuntimeLeasePath(root)); statErr != nil {
		t.Fatalf("the lease file was not created: %v", statErr)
	}
	// The components above the leaf are recorded, so a later failure can undo
	// them. The leaf itself belongs to provisioning and must NOT appear here.
	for _, entry := range created {
		if strings.EqualFold(filepath.Clean(entry.path), filepath.Clean(root)) {
			t.Fatalf("lease acquisition claimed the runtime leaf %s, which provisioning owns", root)
		}
	}
	if len(created) == 0 {
		t.Fatal("SETUP INVALID: nothing was recorded as created, so the accounting assertion above is vacuous")
	}
}

// A second acquisition over an existing tree records nothing: it created nothing,
// so it owns nothing, and rollback must not remove a tree it found.
func TestRuntimeLeaseRecordsOnlyWhatItCreated(t *testing.T) {
	cacheRoot := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)

	first, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}
	first.release()

	second, created, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("second acquisition: %v", err)
	}
	defer second.release()
	if len(created) != 0 {
		t.Fatalf("the second acquisition claimed %d directories it did not create; rollback would delete a tree it found", len(created))
	}
}
