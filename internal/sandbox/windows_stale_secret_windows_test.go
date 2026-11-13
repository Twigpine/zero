//go:build windows

package sandbox

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// "Absent beats stale" is the invariant the rollback's secret removal exists to
// keep: the command path treats a missing secret as not-provisioned and falls
// back, while a stale one fails the logon and reports a broken sandbox.
//
// So when the removal itself fails after a password rotation, the invariant was
// NOT restored — a credential for a password that no longer works is still on
// disk. Swallowing that error claims otherwise, and the operator finds out on
// the next command instead of from the setup that broke it.
func TestProvisionSurfacesFailedStaleSecretCleanup(t *testing.T) {
	for name, testCase := range map[string]struct {
		rotate     bool
		removeErr  error
		wantInErr  string
		wantRemove bool
	}{
		"rotation happened and the stale secret cannot be removed": {
			rotate: true, removeErr: errors.New("access is denied"),
			wantRemove: true, wantInErr: "stale and could not be removed",
		},
		"rotation happened and cleanup succeeds": {
			rotate: true, wantRemove: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			prevProvision := provisionWindowsSandboxIdentityFn
			prevRemove := removeWindowsSandboxSecretFn
			prevReset := resetWindowsSandboxUserPasswordFn
			prevGrant := grantWindowsSandboxLogonRightsFn
			prevWrite := writeWindowsSandboxSecretFn
			t.Cleanup(func() {
				provisionWindowsSandboxIdentityFn = prevProvision
				removeWindowsSandboxSecretFn = prevRemove
				resetWindowsSandboxUserPasswordFn = prevReset
				grantWindowsSandboxLogonRightsFn = prevGrant
				writeWindowsSandboxSecretFn = prevWrite
			})

			sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
			if err != nil {
				t.Fatal(err)
			}
			// created=false so the run ADOPTS an account and rotation applies.
			provisionWindowsSandboxIdentityFn = func(string, windowsSandboxRole) (windowsSandboxIdentity, string, bool, error) {
				return windowsSandboxIdentity{Username: "zero-sbx-test", SID: sid}, "pw", false, nil
			}
			grantWindowsSandboxLogonRightsFn = func(*windows.SID) error { return nil }
			removed := false
			removeWindowsSandboxSecretFn = func(string) error {
				removed = true
				return testCase.removeErr
			}
			// Rotate, then fail immediately after so undo runs with rotated=true.
			resetWindowsSandboxUserPasswordFn = func(string, string) error {
				if testCase.rotate {
					return nil
				}
				return errors.New("no rotation")
			}

			// The only step after rotation; failing it is what drives undo with
			// rotated=true, which is the state the invariant is about.
			writeWindowsSandboxSecretFn = func(string, string) error {
				return errors.New("secret store refused")
			}

			config := WindowsSandboxCommandConfig{
				SandboxHome:    t.TempDir(),
				CommandCWD:     `C:\ws`,
				WorkspaceRoots: []string{`C:\ws`},
			}
			_, _, err = provisionWindowsSandboxPrincipalForSetup(config, windowsSandboxRoleOffline)

			if removed != testCase.wantRemove {
				t.Fatalf("stale secret removal attempted = %v, want %v", removed, testCase.wantRemove)
			}
			if testCase.wantInErr == "" {
				return
			}
			if err == nil {
				t.Fatal("a failed stale-secret cleanup was swallowed")
			}
			if !strings.Contains(err.Error(), testCase.wantInErr) {
				t.Fatalf("error = %q, want it to mention %q", err, testCase.wantInErr)
			}
		})
	}
}
