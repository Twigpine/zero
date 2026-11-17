//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyWindowsACLPlan merges into the existing DACL, so narrowing a policy and
// re-running setup used to leave the wider ACEs in place next to the new ones.
// The principal kept access the current policy no longer grants — a silent
// widening of the sandbox produced by tightening it.
func TestRevokeDropsStalePrincipalACEsBeforeReapply(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, "kept")
	dropped := filepath.Join(root, "dropped")
	for _, dir := range []string{kept, dropped} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Guests: a real, resolvable trustee that the test process is not a member
	// of, so the ACEs below are observable without affecting this process.
	principal := "S-1-5-32-546"

	// First setup: both roots writable.
	wide, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: principal,
		WriteRoots:   []WritableRoot{{Root: kept}, {Root: dropped}},
	})
	if err != nil {
		t.Fatalf("wide plan: %v", err)
	}
	if _, err := applyWindowsACLPlan(wide); err != nil {
		t.Fatalf("apply wide plan: %v", err)
	}
	if !hasACEForTrustee(t, dropped, principal) {
		t.Fatal("precondition: the wide plan should have granted the root it is about to lose")
	}

	// Policy narrows: "dropped" is no longer a write root.
	narrow, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: principal,
		WriteRoots:   []WritableRoot{{Root: kept}},
	})
	if err != nil {
		t.Fatalf("narrow plan: %v", err)
	}
	// Revocation has to cover the paths the OLD plan touched, not just the new
	// one — the whole point is the path that left the policy.
	if _, err := revokeWindowsPrincipalACEs(principal, windowsACLPlanPaths(wide)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := applyWindowsACLPlan(narrow); err != nil {
		t.Fatalf("apply narrow plan: %v", err)
	}

	if hasACEForTrustee(t, dropped, principal) {
		t.Error("principal kept its grant on a root the narrowed policy removed")
	}
	if !hasACEForTrustee(t, kept, principal) {
		t.Error("revocation also dropped the grant the narrowed policy still wants")
	}
}

// Revoking a path that was never created is cleanup with nothing to clean, not
// an error — setup would otherwise fail on any carveout git has not made yet.
func TestRevokeIgnoresPathsThatDoNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	if _, err := revokeWindowsPrincipalACEs("S-1-5-32-546", []string{missing}); err != nil {
		t.Fatalf("revoke over a missing path: %v", err)
	}
}

func hasACEForTrustee(t *testing.T, path string, trustee string) bool {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("DACL(%s): %v", path, err)
	}
	if dacl == nil {
		return false
	}
	want, err := windows.StringToSid(trustee)
	if err != nil {
		t.Fatalf("StringToSid(%s): %v", trustee, err)
	}
	// Deny ACEs count here as much as allow ACEs: revocation is by trustee and
	// drops both, so an assertion that only saw allows would call a leftover
	// deny "revoked".
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var header *windows.ACE_HEADER
		if err := windows.GetAce(dacl, index, (**windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(&header))); err != nil {
			continue
		}
		ace := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(header))
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE:
		default:
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(want) {
			return true
		}
	}
	return false
}

// The mechanism working is not the same as the production path using it. This
// pins the call site and its ORDER: revocation is only worth anything if it
// runs before the plan that re-adds the current grants.
func TestApplyPrincipalACLsRevokesBeforeApplying(t *testing.T) {
	prevApply := applyWindowsACLPlanFn
	t.Cleanup(func() { applyWindowsACLPlanFn = prevApply })

	var actions []WindowsACLAction
	applyWindowsACLPlanFn = func(plan WindowsACLPlan) (func() error, error) {
		if len(plan.Entries) > 0 {
			actions = append(actions, plan.Entries[0].Action)
		}
		return func() error { return nil }, nil
	}

	workspace := t.TempDir()
	filesystem := FileSystemPolicy{
		Kind:       FileSystemRestricted,
		WriteRoots: []WritableRoot{{Root: workspace}},
	}
	if _, err := applyWindowsPrincipalACLs(t.TempDir(), "zero-sbx-test", "S-1-5-32-546", filesystem, filesystem.WriteRoots); err != nil {
		t.Fatalf("applyWindowsPrincipalACLs: %v", err)
	}

	if len(actions) != 2 {
		t.Fatalf("saw %d ACL plans (%v), want a revocation then the grants", len(actions), actions)
	}
	if actions[0] != windowsACLRevoke {
		t.Errorf("first plan was %q, want the trustee revocation to go first", actions[0])
	}
	if actions[1] == windowsACLRevoke {
		t.Error("second plan was another revocation; the current grants were never applied")
	}
}

// The revocation's rollback has to be returned, not discarded.
//
// Discarding it was justified on the grounds that the only failure path from
// applyWindowsPrincipalACLs removes the principal outright, so restoring ACEs
// for a doomed account would be pointless. That holds only for a principal the
// run CREATED. #812 keeps an ADOPTED principal alive on failure rather than
// destroying a working account someone else provisioned — and then the discarded
// snapshot left it logged-on and unable to reach its own workspace, with its
// previous ACEs revoked and the new ones rolled back.
func TestApplyPrincipalACLsRollbackRestoresTheRevokedACEs(t *testing.T) {
	prevApply := applyWindowsACLPlanFn
	t.Cleanup(func() { applyWindowsACLPlanFn = prevApply })

	var applied []WindowsACLAction
	var reverted []WindowsACLAction
	applyWindowsACLPlanFn = func(plan WindowsACLPlan) (func() error, error) {
		action := WindowsACLAction("")
		if len(plan.Entries) > 0 {
			action = plan.Entries[0].Action
		}
		applied = append(applied, action)
		return func() error {
			reverted = append(reverted, action)
			return nil
		}, nil
	}

	workspace := t.TempDir()
	filesystem := FileSystemPolicy{
		Kind:       FileSystemRestricted,
		WriteRoots: []WritableRoot{{Root: workspace}},
	}
	rollback, err := applyWindowsPrincipalACLs(t.TempDir(), "zero-sbx-test", "S-1-5-32-546", filesystem, filesystem.WriteRoots)
	if err != nil {
		t.Fatalf("applyWindowsPrincipalACLs: %v", err)
	}
	if len(applied) != 2 || applied[0] != windowsACLRevoke {
		t.Fatalf("applied %v, want a revocation then the grants", applied)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Both halves must unwind, and in reverse order: the grant comes off first,
	// then the ACEs the revocation removed go back.
	if len(reverted) != 2 {
		t.Fatalf("rollback reverted %v, want both the grant and the revocation", reverted)
	}
	if reverted[0] == windowsACLRevoke {
		t.Error("rollback undid the revocation before the grant; the grant would survive")
	}
	if reverted[1] != windowsACLRevoke {
		t.Errorf("rollback never restored the revoked ACEs, got %v", reverted)
	}
}

// THE LEDGER IS PART OF THE ROLLBACK, and nothing pinned it.
//
// applyWindowsPrincipalACLs narrows the ledger before the whole setup
// transaction has succeeded. If network setup or the marker write then fails,
// rollback restores the DACLs, and the ledger has to come back with them: a
// ledger that still records only the narrowed path set leaves the restored
// grants unnamed, so later cleanup cannot find them.
//
// The neighbouring test asserts only the ORDER of the two ACL reverts and never
// reads the ledger afterwards, so the restore could be deleted and the suite
// would stay green.
func TestApplyPrincipalACLsRollbackRestoresTheLedger(t *testing.T) {
	prevApply := applyWindowsACLPlanFn
	t.Cleanup(func() { applyWindowsACLPlanFn = prevApply })
	applyWindowsACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		return func() error { return nil }, nil
	}

	sandboxHome := t.TempDir()
	const username = "zero-sbx-test"

	// A wider path set than the new plan will record, standing in for a previous
	// policy's grants.
	stale := []string{filepath.Join(t.TempDir(), "previously-granted")}
	if err := writeWindowsPrincipalACLLedger(sandboxHome, username, stale); err != nil {
		t.Fatalf("seed the ledger: %v", err)
	}

	workspace := t.TempDir()
	filesystem := FileSystemPolicy{
		Kind:       FileSystemRestricted,
		WriteRoots: []WritableRoot{{Root: workspace}},
	}
	rollback, err := applyWindowsPrincipalACLs(sandboxHome, username, "S-1-5-32-546", filesystem, filesystem.WriteRoots)
	if err != nil {
		t.Fatalf("applyWindowsPrincipalACLs: %v", err)
	}

	narrowed, ok := readWindowsPrincipalACLLedger(sandboxHome, username)
	if !ok {
		t.Fatal("no ledger after apply")
	}
	if slicesContainsString(narrowed, stale[0]) {
		t.Fatalf("the ledger still names the old path after apply, so this test is not exercising the restore: %v", narrowed)
	}

	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	restored, ok := readWindowsPrincipalACLLedger(sandboxHome, username)
	if !ok {
		t.Fatal("no ledger after rollback")
	}
	if !slicesContainsString(restored, stale[0]) {
		t.Errorf("rollback restored the DACLs but left the narrowed ledger %v; the path %q it put back is now unnamed, so cleanup cannot find it", restored, stale[0])
	}
}

func slicesContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
