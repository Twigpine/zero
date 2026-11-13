package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// PROVISIONING STAYS CLOSED WHILE NO LAUNCH MECHANISM EXISTS.
//
// Launching a command as a separate account needs SeAssignPrimaryTokenPrivilege
// and SeIncreaseQuotaPrivilege. The process that needs them is the LATER,
// ordinary command process, not setup, and nothing carries launch authority
// across that boundary.
//
// The previous gate asked whether the CALLING process held the privileges,
// which is the wrong lifetime and got the answer backwards where it mattered:
// it refused unelevated setup, which was never dangerous, and it PASSED
// elevated setup, which is precisely the case that lands a local account, its
// password, its logon rights, workspace ACEs, the recovery ledger and network
// filter state for a backend no later command can use. The gate existed to
// prevent durable state serving something that cannot run, and it produced it.
//
// So the refusal must not be satisfiable by holding the privileges right now.
func TestSetupRefusesToProvisionAPrincipalNothingCanLaunch(t *testing.T) {
	optIn := true
	options := WindowsSandboxSetupArgsOptions{
		CommandCWD:     t.TempDir(),
		SandboxHome:    t.TempDir(),
		PrincipalOptIn: &optIn,
	}

	args, err := BuildWindowsSandboxSetupArgs(options)
	if err == nil {
		t.Fatalf("setup args were built for a principal nothing can launch: %v", args)
	}
	if !errors.Is(err, errWindowsPrincipalLaunchUnavailable) {
		t.Errorf("the refusal is not the launch-unavailable one: %v", err)
	}
	for _, want := range []string{"SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege", "provision"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// The way forward has to be the one that works. Telling the operator to
	// re-run elevated is the advice that produced the unusable state.
	if strings.Contains(strings.ToLower(err.Error()), "elevated terminal") {
		t.Errorf("the refusal still points at an elevated terminal, which does not help: %v", err)
	}
	if !strings.Contains(err.Error(), windowsSandboxIdentityEnv) {
		t.Errorf("the refusal does not name the opt-out: %v", err)
	}
}

// THE REGRESSION, STATED DIRECTLY: an elevated caller must be refused too.
//
// This is the case the old check let through, and it is the only one that
// creates durable machine state. A gate that consults the current process
// cannot express it, which is why the seam takes no token and no argument.
func TestSetupRefusalDoesNotDependOnTheCallersPrivileges(t *testing.T) {
	optIn := true
	options := WindowsSandboxSetupArgsOptions{
		CommandCWD:     t.TempDir(),
		SandboxHome:    t.TempDir(),
		PrincipalOptIn: &optIn,
	}

	// Whatever this process holds, twice, must give the same answer.
	first, firstErr := BuildWindowsSandboxSetupArgs(options)
	second, secondErr := BuildWindowsSandboxSetupArgs(options)
	if firstErr == nil || secondErr == nil {
		t.Fatalf("provisioning succeeded: %v / %v", first, second)
	}
	if firstErr.Error() != secondErr.Error() {
		t.Errorf("the refusal is not stable across calls:\n  %v\n  %v", firstErr, secondErr)
	}
}

// AND THE GATE IS ONLY FOR THE OPTED-IN PATH. Without the opt-in there is no
// principal to provision, so the ordinary restricted-token sandbox, which needs
// none of those privileges, must still set up.
func TestSetupWithoutTheOptInIsUnaffected(t *testing.T) {
	optOut := false
	if _, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		CommandCWD:     t.TempDir(),
		SandboxHome:    t.TempDir(),
		PrincipalOptIn: &optOut,
	}); err != nil {
		t.Errorf("the restricted-token sandbox was refused for a privilege it does not need: %v", err)
	}
}

// allowPrincipalLaunchForTest opens the gate for tests that are about argument
// PLUMBING rather than about whether a principal can be launched. The opt-in
// still has to round-trip correctly for `zero doctor` and for the day a broker
// lands, and those assertions should not be deleted just because the entry
// point is closed today.
func allowPrincipalLaunchForTest(t *testing.T) {
	t.Helper()
	previous := windowsPrincipalLaunchAvailable
	windowsPrincipalLaunchAvailable = func() error { return nil }
	t.Cleanup(func() { windowsPrincipalLaunchAvailable = previous })
}
