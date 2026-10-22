package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// The post-provisioning coverage assertion is the last thing standing between a
// mis-ordered plan and a machine that reports a successful setup while every
// offline principal has an open network. It has to fail closed on every way of
// not knowing the answer, not just on a definite negative.
func TestAssertWindowsNetworkPlanCoversOfflineGroupFailsClosed(t *testing.T) {
	const groupSID = "S-1-5-32-9999"
	covering := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-marker", groupSID}}
	notCovering := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-marker"}}
	lookupFailed := errors.New("look up zero-sandbox-offline: access is denied")

	tests := []struct {
		name     string
		plan     WindowsNetworkPlan
		resolve  func() (string, error)
		wantErr  bool
		wantText string
	}{
		{
			name:    "plan names the group",
			plan:    covering,
			resolve: func() (string, error) { return groupSID, nil },
		},
		{
			// The finding anandh8x raised: a failed lookup skipped the assertion
			// entirely and setup carried on to write a success marker.
			name:     "lookup fails",
			plan:     covering,
			resolve:  func() (string, error) { return "", lookupFailed },
			wantErr:  true,
			wantText: "access is denied",
		},
		{
			// Empty-and-no-error means "the group does not exist", which is a
			// legitimate answer BEFORE provisioning and an impossible one after it.
			// WindowsNetworkPlanCoversPrincipals answers true for an empty SID, so
			// without an explicit check the assertion passes vacuously.
			name:     "group missing after provisioning",
			plan:     covering,
			resolve:  func() (string, error) { return "", nil },
			wantErr:  true,
			wantText: "does not exist",
		},
		{
			name:     "whitespace-only SID",
			plan:     covering,
			resolve:  func() (string, error) { return "   ", nil },
			wantErr:  true,
			wantText: "does not exist",
		},
		{
			name:     "plan omits the group",
			plan:     notCovering,
			resolve:  func() (string, error) { return groupSID, nil },
			wantErr:  true,
			wantText: "do not name the sandbox offline group",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := assertWindowsNetworkPlanCoversOfflineGroup(test.plan, test.resolve, true)
			if test.wantErr && err == nil {
				t.Fatalf("assertWindowsNetworkPlanCoversOfflineGroup() = nil, want an error so setup rolls back")
			}
			if !test.wantErr {
				if err != nil {
					t.Fatalf("assertWindowsNetworkPlanCoversOfflineGroup() = %v, want nil", err)
				}
				return
			}
			if !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error = %q, want it to mention %q so the operator can tell the cases apart", err, test.wantText)
			}
		})
	}

	// The lookup failure has to stay unwrapped-comparable: setup logs it and an
	// operator needs the underlying Win32 reason, not a flattened string.
	err := assertWindowsNetworkPlanCoversOfflineGroup(covering, func() (string, error) { return "", lookupFailed }, true)
	if !errors.Is(err, lookupFailed) {
		t.Fatalf("errors.Is(err, lookupFailed) = false; the cause was dropped: %v", err)
	}
}

// The assert only means something AFTER principals were provisioned. The offline
// group is created inside provisionWindowsSandboxIdentity, which runs only under
// the ZERO_WINDOWS_SANDBOX_IDENTITY opt-in, so on a default machine there is no
// group, no principal, and nothing for the filters to miss.
//
// Rejecting an empty SID is right once provisioning ran (that is the vacuous
// pass anandh8x found) and wrong before it: it turned the ordinary opt-out setup
// into a hard failure, which is the regression jatmn caught on #812.
func TestAssertWindowsNetworkPlanCoversOfflineGroupSkipsWhenNothingWasProvisioned(t *testing.T) {
	plan := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-marker"}}

	// The default machine: no group exists, so the resolver reports ("", nil).
	if err := assertWindowsNetworkPlanCoversOfflineGroup(plan, func() (string, error) { return "", nil }, false); err != nil {
		t.Fatalf("opt-out setup was refused: %v", err)
	}
	// Same state WITH provisioning claimed: now the missing group is a real fault.
	if err := assertWindowsNetworkPlanCoversOfflineGroup(plan, func() (string, error) { return "", nil }, true); err == nil {
		t.Fatal("a missing group after provisioning must still fail closed")
	}
	// Not provisioned must not become a licence to skip a lookup failure either:
	// the resolver is never consulted, so a broken lookup cannot matter here.
	if err := assertWindowsNetworkPlanCoversOfflineGroup(plan, func() (string, error) {
		t.Fatal("the resolver must not be consulted when nothing was provisioned")
		return "", nil
	}, false); err != nil {
		t.Fatalf("opt-out setup was refused: %v", err)
	}
}

// A nil resolver is a wiring mistake, and guessing that coverage is fine is the
// one thing it must not do.
func TestAssertWindowsNetworkPlanCoversOfflineGroupRejectsNilResolver(t *testing.T) {
	plan := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-32-9999"}}
	if err := assertWindowsNetworkPlanCoversOfflineGroup(plan, nil, true); err == nil {
		t.Fatal("assertWindowsNetworkPlanCoversOfflineGroup(nil resolver) = nil, want an error")
	}
}
