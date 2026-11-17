//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// THROUGH THE REAL APPLY, with the swap in the window that matters.
//
// The direct-handle test cannot distinguish handle from pathname, because any
// name derived from the handle resolves back to the same object. What the old
// code did was different: it re-opened the ORIGINAL root string after the ACL
// step, so a directory swapped in under that name collected the stamp. This
// drives applyWindowsACLPlanWithStamp and performs the swap between the ACE
// landing and the stamp write.
func TestTheStampSkipsADirectorySwappedInAfterTheACE(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}

	moved := root + "-original"
	previous := windowsACLStampSwapHook
	windowsACLStampSwapHook = func(path string) {
		// Ordinary directories throughout. Nothing here is a reparse point, which
		// is why a no-follow re-open does not catch it.
		if err := os.Rename(path, moved); err != nil {
			t.Skipf("cannot rename the runtime root here: %v", err)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("plant the replacement: %v", err)
		}
	}
	t.Cleanup(func() { windowsACLStampSwapHook = previous })

	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	rollback, err := applyWindowsACLPlanWithStamp(plan, stampRequestFor(t, root, "planhash"))
	if err != nil {
		t.Fatalf("applyWindowsACLPlanWithStamp: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })

	if _, err := os.Stat(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash))); err == nil {
		t.Error("the swapped-in directory collected the stamp; it carries no capability ACE and would still validate as set up")
	}
	recorded, err := os.ReadFile(filepath.Join(moved, windowsSandboxRuntimeStampName(testStampPlanHash)))
	if err != nil {
		t.Fatalf("the stamp did not land on the object the ACE was applied to: %v", err)
	}
	if string(recorded) != "planhash" {
		t.Errorf("stamp contents = %q, want the plan hash", recorded)
	}
}

// stampRequestFor builds the request the way runWindowsSandboxSetup does, with
// the snapshot's identity carried on it. Building one by hand without that
// identity is refused now, and rightly: the apply cannot prove it holds the
// object the snapshot read.
func stampRequestFor(t *testing.T, root string, planHash string) *windowsACLStampRequest {
	t.Helper()
	snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, testStampPlanHash)
	if err != nil {
		t.Fatalf("snapshot the runtime stamp for %s: %v", root, err)
	}
	return &windowsACLStampRequest{
		Root:           root,
		PlanHash:       planHash,
		RootIdentity:   snapshot.rootIdentity,
		RootIdentified: snapshot.rootIdentified,
	}
}
