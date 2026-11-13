//go:build windows

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// runtimeRootGrants reports whether path's DACL carries an allow ACE for sid.
func runtimeRootGrants(t *testing.T, path string, sid string) bool {
	t.Helper()
	want, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo %s: %v", path, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		return false
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			t.Fatalf("GetAce %d: %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if got, ok := windowsAceSID(ace); ok && got.Equals(want) {
			return true
		}
	}
	return false
}

// A FAILED SETUP RETRY LEAVES THE EARLIER SETUP'S GRANT IN PLACE.
//
// Preparing the runtime candidates rewrote an existing root's DACL before the
// ACL transaction had a baseline, so a retry that then failed on the second
// candidate, or in the apply, returned with the working grant gone and the old
// marker still claiming success. The DACLs are snapshotted before preparation
// and restored on every later failure.
func TestSetupRetryFailurePreservesAnExistingRuntimeGrant(t *testing.T) {
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true})
	isolateSandboxRuntimeRoots(t)
	config := windowsSetupTestConfig(t, false)
	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) != 2 {
		t.Fatalf("SETUP INVALID: want a cache-derived and a fallback candidate, got %v", candidates)
	}
	granted, fallback := candidates[0], candidates[1]

	// A previous, successful setup: the first candidate exists and grants the
	// capability SID. Guests stands in for it here.
	if err := ensureRuntimeCandidateDir(granted); err != nil {
		t.Fatalf("prepare the first candidate: %v", err)
	}
	rollback, err := applyWindowsACLPlan(WindowsACLPlan{Entries: []WindowsACLEntry{{
		Action: WindowsACLAllowWrite, Path: granted, Capability: windowsACLTestDenySID,
	}}})
	if err != nil {
		t.Fatalf("grant the first candidate: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })
	if !runtimeRootGrants(t, granted, windowsACLTestDenySID) {
		t.Fatal("SETUP INVALID: the grant did not land on the first candidate")
	}

	t.Run("second candidate fails validation", func(t *testing.T) {
		// A file where the fallback candidate directory should be: EnsurePrivateDir
		// refuses it, after it has already rewritten the first candidate.
		if err := os.MkdirAll(filepath.Dir(fallback), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fallback, []byte("in the way"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(fallback) })

		var stderr bytes.Buffer
		if code := runWindowsSandboxSetup(config, &stderr); code == 0 {
			t.Fatalf("setup succeeded with a file at the fallback candidate: %s", stderr.String())
		}
		if !runtimeRootGrants(t, granted, windowsACLTestDenySID) {
			t.Fatalf("the failed retry stripped the earlier grant from %s: %s", granted, stderr.String())
		}
	})

	t.Run("apply fails", func(t *testing.T) {
		if err := os.Remove(fallback); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		previous := applyWindowsACLPlanFn
		applyWindowsACLPlanFn = func(WindowsACLPlan) (func() error, error) {
			return nil, errors.New("injected apply failure")
		}
		t.Cleanup(func() { applyWindowsACLPlanFn = previous })

		var stderr bytes.Buffer
		if code := runWindowsSandboxSetup(config, &stderr); code == 0 {
			t.Fatalf("setup succeeded through an injected apply failure: %s", stderr.String())
		}
		if !runtimeRootGrants(t, granted, windowsACLTestDenySID) {
			t.Fatalf("the failed apply stripped the earlier grant from %s: %s", granted, stderr.String())
		}
	})
}

// CONTROL: fresh roots still get created and setup still succeeds.
func TestSetupCreatesFreshRuntimeCandidates(t *testing.T) {
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true})
	isolateSandboxRuntimeRoots(t)
	config := windowsSetupTestConfig(t, false)
	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(config, &stderr); code != 0 {
		t.Fatalf("setup on fresh roots failed: %s", stderr.String())
	}
	for _, root := range windowsSandboxRuntimeCandidates(config.WorkspaceRoots) {
		if info, err := os.Lstat(root); err != nil || !info.IsDir() {
			t.Fatalf("candidate %s was not created: %v", root, err)
		}
	}
}
