//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A SNAPSHOT OF ONE OBJECT DOES NOT AUTHORIZE MUTATING ANOTHER.
//
// snapshotWindowsSandboxRuntimeStamp reads the root's identity and prior stamp
// through one handle and closes it. applyWindowsACLPlanWithStamp then resolved
// the same pathname again and mutated whatever answered. Neither ever proved the
// two were the same object, so the transaction established "these prior bytes
// belong to B" and then wrote to A.
//
// The root's owner can do that with ordinary directories and no privilege:
// rename the real root aside, put a plain directory at the predictable name for
// the snapshot to read, and restore the original before the apply. Nothing is a
// reparse point, so a no-follow open does not notice, and the setup lease is a
// sibling of the root rather than the root entry itself.
//
// The damage is not only a misplaced ACL. On a later failure, stamp compensation
// compares what it finds against the snapshot's identity, refuses to restore, and
// leaves this run's stamp on a directory whose published marker still describes
// the previous successful setup. A failed setup invalidates a good one.
func TestTheApplyRefusesARuntimeRootTheSnapshotNeverRead(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	// A prior stamp, which is the thing a failed setup must not destroy.
	priorPath := filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash))
	prior := []byte("previous-successful-setup")
	if err := os.WriteFile(priorPath, prior, 0o600); err != nil {
		t.Fatalf("write the prior stamp: %v", err)
	}

	// The snapshot reads the real root.
	request := stampRequestFor(t, root, "planhash")
	if !request.RootIdentified {
		t.Fatalf("SETUP INVALID: the snapshot could not identify %s, so the check below would refuse for the wrong reason", root)
	}

	// Between the apply's open and its first mutation, the owner substitutes an
	// ordinary directory under the same name. This is the interval the two
	// separate opens created.
	moved := root + "-original"
	previous := windowsACLStampIdentitySwapHook
	t.Cleanup(func() { windowsACLStampIdentitySwapHook = previous })
	swapped := false
	windowsACLStampIdentitySwapHook = func(path string) {
		if swapped || !strings.EqualFold(filepath.Clean(path), filepath.Clean(root)) {
			return
		}
		swapped = true
		if err := os.Rename(path, moved); err != nil {
			t.Skipf("cannot rename the runtime root here: %v", err)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("plant the replacement: %v", err)
		}
	}

	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	rollback, err := applyWindowsACLPlanWithStamp(plan, request)
	if rollback != nil {
		t.Cleanup(func() { _ = rollback() })
	}
	if !swapped {
		t.Skip("the swap hook never fired, so the interval was not reproduced")
	}
	if err == nil {
		t.Fatal("the apply mutated a runtime root the snapshot never read, so its ACL and stamp attest to an object nobody inspected")
	}
	if !strings.Contains(err.Error(), "no longer the directory this run recorded") {
		t.Errorf("the refusal does not say what went wrong: %v", err)
	}

	// And the substitute is untouched: no stamp, so it cannot later validate as
	// set up while carrying no capability ACE.
	if _, statErr := os.Stat(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash))); statErr == nil {
		t.Error("the substituted directory collected a stamp")
	}
	// The real root's prior stamp survives byte for byte.
	got, readErr := os.ReadFile(filepath.Join(moved, windowsSandboxRuntimeStampName(testStampPlanHash)))
	if readErr != nil {
		t.Fatalf("read the original stamp back: %v", readErr)
	}
	if string(got) != string(prior) {
		t.Errorf("the previous setup's stamp was changed to %q", got)
	}
}

// And an unswapped root still applies, or the refusal above would be satisfied by
// an apply that refuses everything.
func TestTheApplyStillStampsTheRootTheSnapshotRead(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	rollback, err := applyWindowsACLPlanWithStamp(plan, stampRequestFor(t, root, "planhash"))
	if err != nil {
		t.Fatalf("an unswapped runtime root was refused: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })
	if _, statErr := os.Stat(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash))); statErr != nil {
		t.Fatalf("the stamp was not written: %v", statErr)
	}
}

// An identity the snapshot could not establish refuses rather than passing. The
// guard exists for the case where nobody can tell, so treating "unknown" as
// permission would make it a no-op exactly there.
func TestTheApplyRefusesAnUnidentifiedRuntimeRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	rollback, err := applyWindowsACLPlanWithStamp(plan, &windowsACLStampRequest{Root: root, PlanHash: "planhash"})
	if rollback != nil {
		t.Cleanup(func() { _ = rollback() })
	}
	if err == nil {
		t.Fatal("an apply whose snapshot never identified the root was allowed to mutate and stamp it")
	}
	if !strings.Contains(err.Error(), "could not be identified") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}
