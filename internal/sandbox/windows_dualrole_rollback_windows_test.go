//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The outer rollback must only delete principals this run created.
//
// Dual-role setup provisions offline then online. If the offline role adopts an
// account that already existed and anything afterwards fails, the outer rollback
// used to delete it anyway, turning a partial re-setup failure into the loss of
// a principal that was working before the run started. Two roles make that the
// common case rather than an unlucky one: the first role usually succeeds, so
// there is nearly always something for a later failure to destroy.
func TestDualRoleSetupRollbackSparesAdoptedPrincipals(t *testing.T) {
	for name, testCase := range map[string]struct {
		createdByRole map[windowsSandboxRole]bool
		wantRemoved   []windowsSandboxRole
	}{
		"offline adopted, online created": {
			createdByRole: map[windowsSandboxRole]bool{
				windowsSandboxRoleOffline: false,
				windowsSandboxRoleOnline:  true,
			},
			// Only the one this run made. Deleting the adopted offline principal
			// is the data loss this guards against.
			wantRemoved: []windowsSandboxRole{windowsSandboxRoleOnline},
		},
		"both adopted": {
			createdByRole: map[windowsSandboxRole]bool{
				windowsSandboxRoleOffline: false,
				windowsSandboxRoleOnline:  false,
			},
			wantRemoved: nil,
		},
		"both created": {
			createdByRole: map[windowsSandboxRole]bool{
				windowsSandboxRoleOffline: true,
				windowsSandboxRoleOnline:  true,
			},
			wantRemoved: []windowsSandboxRole{windowsSandboxRoleOffline, windowsSandboxRoleOnline},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var removed []windowsSandboxRole
			restoreDualRoleSeams(t, testCase.createdByRole, &removed)

			// Fail on the SECOND role, not the first. ACL application happens per
			// role inside the loop, so failing the first aborts before the second
			// is provisioned at all and the rollback never has more than one
			// principal to consider. The case worth testing is the one review
			// described: the first role succeeds, the second fails, and the
			// question is whether the first gets destroyed on the way out.
			// Count GRANT plans, not ACL calls. applyWindowsPrincipalACLs revokes
			// this trustee's existing ACEs before applying the new plan, so there
			// are two calls per role now. The contract under test is "the second
			// ROLE fails"; counting raw calls would fail the first role's grant
			// instead and prove something else entirely.
			grants := 0
			applyWindowsACLPlanFn = func(plan WindowsACLPlan) (func() error, error) {
				if len(plan.Entries) > 0 && plan.Entries[0].Action == windowsACLRevoke {
					return func() error { return nil }, nil
				}
				grants++
				if grants < 2 {
					return func() error { return nil }, nil
				}
				return nil, errors.New("ACL apply refused")
			}

			if _, err := setupWindowsSandboxPrincipal(windowsSandboxTestConfig(t)); err == nil {
				t.Fatal("setup reported success despite an injected ACL failure")
			}
			assertRolesEqual(t, removed, testCase.wantRemoved)
		})
	}
}

// The same contract on the success path: the rollback handed back to the caller
// for a LATER setup step to invoke must be just as reluctant.
func TestDualRoleSetupReturnedRollbackSparesAdoptedPrincipals(t *testing.T) {
	var removed []windowsSandboxRole
	restoreDualRoleSeams(t, map[windowsSandboxRole]bool{
		windowsSandboxRoleOffline: false,
		windowsSandboxRoleOnline:  true,
	}, &removed)

	rollback, err := setupWindowsSandboxPrincipal(windowsSandboxTestConfig(t))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("setup removed principals before anything failed: %v", removed)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	assertRolesEqual(t, removed, []windowsSandboxRole{windowsSandboxRoleOnline})
}

func restoreDualRoleSeams(t *testing.T, createdByRole map[windowsSandboxRole]bool, removed *[]windowsSandboxRole) {
	t.Helper()
	prevProvision := provisionWindowsSandboxPrincipalForSetupFn
	prevApply := applyWindowsACLPlanFn
	prevRemove := removeWindowsSandboxPrincipalForSetupFn
	t.Cleanup(func() {
		provisionWindowsSandboxPrincipalForSetupFn = prevProvision
		applyWindowsACLPlanFn = prevApply
		removeWindowsSandboxPrincipalForSetupFn = prevRemove
	})

	provisionWindowsSandboxPrincipalForSetupFn = func(_ WindowsSandboxCommandConfig, role windowsSandboxRole) (windowsSandboxIdentity, bool, error) {
		sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
		if err != nil {
			return windowsSandboxIdentity{}, false, err
		}
		return windowsSandboxIdentity{Username: "zero-sbx-test", SID: sid}, createdByRole[role], nil
	}
	applyWindowsACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		return func() error { return nil }, nil
	}
	removeWindowsSandboxPrincipalForSetupFn = func(_ WindowsSandboxCommandConfig, role windowsSandboxRole) error {
		*removed = append(*removed, role)
		return nil
	}
}

// windowsSandboxTestConfig hands the production setup path test-owned roots and
// nothing else. The principal and ACL seams intercept the identity work, not the
// filesystem work: setupWindowsSandboxPrincipal materializes runtime candidates
// under the user cache and TEMP, and applyWindowsPrincipalACLs writes ledgers
// below the sandbox home, so the fixed C:\sandboxhome and C:\ws these tests used
// to name meant an ordinary run wrote to the live cache, failed on drive-root
// permissions, or collided with host state. Everything setup touches is
// redirected to t.TempDir, and windowsSandboxTestRootsUntouched proves the
// redirect took. Reported by @gnanam1990.
func windowsSandboxTestConfig(t *testing.T) WindowsSandboxCommandConfig {
	t.Helper()
	home := filepath.Join(t.TempDir(), "sandboxhome")
	workspace := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	prevCache := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cache, nil }
	t.Cleanup(func() { sandboxUserCacheDir = prevCache })
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	before := windowsFixedTestRootsState()
	t.Cleanup(func() { windowsSandboxTestRootsUntouched(t, cache, before) })
	return WindowsSandboxCommandConfig{
		SandboxHome:    home,
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
		},
	}
}

// windowsFixedRootState is what a run may not change about a path outside the
// test-owned roots: whether it exists, and if it does, when its entries last
// changed. A directory's modification time moves whenever something is created
// or removed inside it, which is exactly what setup would do to a home it was
// still writing ledgers into.
type windowsFixedRootState struct {
	exists  bool
	modTime time.Time
}

var windowsFixedTestRoots = []string{`C:\sandboxhome`, `C:\ws`}

func windowsFixedTestRootsState() map[string]windowsFixedRootState {
	states := make(map[string]windowsFixedRootState, len(windowsFixedTestRoots))
	for _, fixed := range windowsFixedTestRoots {
		info, err := os.Stat(fixed)
		if err != nil {
			states[fixed] = windowsFixedRootState{}
			continue
		}
		states[fixed] = windowsFixedRootState{exists: true, modTime: info.ModTime()}
	}
	return states
}

// windowsSandboxTestRootsUntouched asserts the production setup path stayed
// inside the test-owned roots. Not "the fixed paths are absent": a box that ran
// the old form of these tests still has C:\sandboxhome from them, which is the
// host state the finding was about, so the assertion is that THIS run neither
// created nor changed them. And the cache the run was redirected to must not be
// the live one, or the redirect proved nothing.
func windowsSandboxTestRootsUntouched(t *testing.T, cache string, before map[string]windowsFixedRootState) {
	t.Helper()
	after := windowsFixedTestRootsState()
	for _, fixed := range windowsFixedTestRoots {
		was, now := before[fixed], after[fixed]
		switch {
		case !was.exists && now.exists:
			t.Errorf("setup under test created %s, a path outside the test-owned roots", fixed)
		case was.exists && now.exists && !now.modTime.Equal(was.modTime):
			t.Errorf("setup under test wrote into %s, a path outside the test-owned roots", fixed)
		}
	}
	if live, err := os.UserCacheDir(); err == nil && live == cache {
		t.Fatalf("SETUP INVALID: the redirected cache %s is the live one", cache)
	}
}

func assertRolesEqual(t *testing.T, got []windowsSandboxRole, want []windowsSandboxRole) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
	seen := map[windowsSandboxRole]bool{}
	for _, role := range got {
		seen[role] = true
	}
	for _, role := range want {
		if !seen[role] {
			t.Fatalf("removed %v, want %v", got, want)
		}
	}
}
