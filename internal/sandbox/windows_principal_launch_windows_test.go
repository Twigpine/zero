//go:build windows

package sandbox

import (
	"strings"
	"testing"
)

// The launch preflight must name what is missing and what to do about it.
//
// Without it the failure surfaces as a bare "Access is denied" from inside
// CreateProcessAsUser, which is indistinguishable from the sandboxed command
// itself being rejected. It happens before the command's executable is opened,
// so there is nothing else in the output to tell the two apart.
//
// WHICH privilege is missing is a property of the account running the test, not
// of the code. An unelevated user often holds neither, but not always: the box
// this was written on holds SeAssignPrimaryTokenPrivilege and not
// SeIncreaseQuotaPrivilege. The previous version of this test ran the real
// preflight and then asserted SeAssignPrimaryTokenPrivilege appeared in the
// message, so it passed or failed on the tester's token rather than on
// anything in the source, and it failed here for a reason that was never a
// defect. Rendering is tested directly instead, once per combination.
func TestPrincipalLaunchPrivilegeErrorNamesWhatIsMissing(t *testing.T) {
	cases := []struct {
		name    string
		missing []string
	}{
		{"neither held", []string{seAssignPrimaryTokenPrivilege, seIncreaseQuotaPrivilege}},
		{"only assign-primary-token missing", []string{seAssignPrimaryTokenPrivilege}},
		{"only increase-quota missing", []string{seIncreaseQuotaPrivilege}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := principalLaunchPrivilegeError(tc.missing)
			if err == nil {
				t.Fatal("a missing privilege produced no error")
			}
			for _, want := range tc.missing {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message does not name %s, so an operator cannot act on it: %v", want, err)
				}
			}
			// And it must not name one that IS held, or the operator goes looking
			// for a privilege that was never the problem.
			for _, other := range []string{seAssignPrimaryTokenPrivilege, seIncreaseQuotaPrivilege} {
				if contains(tc.missing, other) {
					continue
				}
				if strings.Contains(err.Error(), other) {
					t.Errorf("the message names %s, which this case holds: %v", other, err)
				}
			}
			if !strings.Contains(err.Error(), windowsSandboxIdentityEnv) {
				t.Errorf("the message does not name the opt-out: %v", err)
			}
			// The point of the message is the way out. Without it the operator is
			// told only that something is denied.
			if !strings.Contains(err.Error(), "restricted-token sandbox") {
				t.Errorf("the message does not offer the fallback, so it reads as a dead end: %v", err)
			}
		})
	}
}

// Holding both is not an error, or the sandbox would refuse on exactly the
// machines it is meant to run on.
func TestPrincipalLaunchPrivilegeErrorIsNilWhenNothingIsMissing(t *testing.T) {
	if err := principalLaunchPrivilegeError(nil); err != nil {
		t.Errorf("no missing privileges produced an error: %v", err)
	}
	if err := principalLaunchPrivilegeError([]string{}); err != nil {
		t.Errorf("an empty missing set produced an error: %v", err)
	}
}

// The real preflight against this machine's token. The verdict legitimately
// differs by account, so the only thing asserted is that a refusal is
// actionable: it names at least one of the two privileges rather than failing
// with something the operator cannot use.
func TestPrincipalLaunchPreflightRefusalIsActionable(t *testing.T) {
	err := enableWindowsPrincipalLaunchPrivileges()
	if err == nil {
		t.Log("this process holds the principal launch privileges; nothing to explain")
		return
	}
	if !strings.Contains(err.Error(), seAssignPrimaryTokenPrivilege) &&
		!strings.Contains(err.Error(), seIncreaseQuotaPrivilege) {
		t.Errorf("the preflight refused without naming either privilege: %v", err)
	}
}

// Repeated calls must agree. This runs once per sandboxed command, and a
// preflight that answered differently on the second call would let a command
// through that the first call refused.
func TestPrincipalLaunchPreflightIsStable(t *testing.T) {
	first := enableWindowsPrincipalLaunchPrivileges()
	second := enableWindowsPrincipalLaunchPrivileges()
	if (first == nil) != (second == nil) {
		t.Fatalf("preflight verdict changed between calls: first=%v second=%v", first, second)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
