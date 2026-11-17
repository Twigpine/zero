package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// THE BLOCK FILTERS ARE MACHINE-GLOBAL, SO ONE WORKSPACE'S SETUP DECIDES ANOTHER
// WORKSPACE'S NETWORK.
//
// Every setup installs them by deleting and recreating one fixed set. The plan
// each home builds names the offline group only when THAT home opted in, which
// is right for the marker it fingerprints and wrong for the filters it installs:
// an ordinary opted-out setup for a second workspace replaced the filters
// without the group SID, and the first workspace's offline principal — still in
// the group, still passing the runtime membership check, still holding a valid
// marker — was no longer matched by any filter. A NetworkDeny command there
// gained egress, silently, because the second setup did exactly what it was
// asked to do.
//
// The plan a home installs and the plan it records are therefore different
// questions, and these pin both.

const testOfflineGroupSID = "S-1-5-32-9990"

func offlineGroupResolver(sid string) func() (string, error) {
	return func() (string, error) { return sid, nil }
}

// The regression itself: an opted-OUT home must still install filters covering
// the managed offline group, because an opted-IN home's principals depend on it.
func TestOptedOutSetupKeepsOfflineGroupCoverageForOtherWorkspaces(t *testing.T) {
	optedOut, err := BuildWindowsNetworkInfraPlan(WindowsSandboxCommandConfig{
		SandboxHome: t.TempDir(),
		// No opt-in in the environment: this is the ordinary workspace.
		Env: map[string]string{},
	})
	if err != nil {
		t.Fatalf("BuildWindowsNetworkInfraPlan: %v", err)
	}

	// What this home RECORDS must not change, or every opted-out marker on the
	// machine goes stale the moment another workspace opts in.
	if WindowsNetworkPlanCoversPrincipals(optedOut, testOfflineGroupSID) {
		t.Fatal("the recorded plan named the offline group; that is what invalidated other homes' markers")
	}

	// What this home INSTALLS must cover the group, since the group exists.
	applied, err := WindowsNetworkPlanForApply(optedOut, offlineGroupResolver(testOfflineGroupSID))
	if err != nil {
		t.Fatalf("WindowsNetworkPlanForApply: %v", err)
	}
	if !WindowsNetworkPlanCoversPrincipals(applied, testOfflineGroupSID) {
		t.Errorf("an opted-out setup would install filters that do not match the offline group, so another workspace's NetworkDeny commands gain egress: %v", applied.IdentitySIDs)
	}
	// The offline-marker SID has to survive alongside it: the restricted-token
	// tier is matched by that one, and it is the whole denial on machines with no
	// principals at all.
	if len(applied.IdentitySIDs) == 0 || strings.TrimSpace(applied.IdentitySIDs[0]) == "" {
		t.Fatalf("the offline-marker SID was lost from the applied plan: %v", applied.IdentitySIDs)
	}
	if applied.IdentitySIDs[0] != optedOut.IdentitySIDs[0] {
		t.Errorf("the marker SID must stay first, since the setup marker records IdentitySIDs[0]: %v", applied.IdentitySIDs)
	}
}

// Augmenting must not mutate the plan the caller fingerprints. Appending in
// place would let installing a plan change the plan that was recorded.
func TestPlanForApplyDoesNotMutateTheRecordedPlan(t *testing.T) {
	recorded, err := BuildWindowsNetworkInfraPlan(WindowsSandboxCommandConfig{
		SandboxHome: t.TempDir(),
		Env:         map[string]string{},
	})
	if err != nil {
		t.Fatalf("BuildWindowsNetworkInfraPlan: %v", err)
	}
	before := append([]string{}, recorded.IdentitySIDs...)

	if _, err := WindowsNetworkPlanForApply(recorded, offlineGroupResolver(testOfflineGroupSID)); err != nil {
		t.Fatalf("WindowsNetworkPlanForApply: %v", err)
	}

	if len(recorded.IdentitySIDs) != len(before) {
		t.Fatalf("the recorded plan grew from %v to %v", before, recorded.IdentitySIDs)
	}
	for index := range before {
		if recorded.IdentitySIDs[index] != before[index] {
			t.Errorf("the recorded plan changed at %d: %v vs %v", index, before, recorded.IdentitySIDs)
		}
	}
}

// An opted-IN home already names the group, and must not name it twice: a
// duplicate identity would change the installed filter set for no reason.
func TestPlanForApplyDoesNotDuplicateAnAlreadyCoveredGroup(t *testing.T) {
	plan := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-marker", testOfflineGroupSID}}
	applied, err := WindowsNetworkPlanForApply(plan, offlineGroupResolver(strings.ToLower(testOfflineGroupSID)))
	if err != nil {
		t.Fatalf("WindowsNetworkPlanForApply: %v", err)
	}
	if len(applied.IdentitySIDs) != 2 {
		t.Errorf("the group was added again despite already being covered: %v", applied.IdentitySIDs)
	}
}

// On an ordinary machine with no principals the group does not exist, and the
// plan must be left exactly as it was.
func TestPlanForApplyLeavesAMachineWithoutTheGroupAlone(t *testing.T) {
	plan := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-marker"}}

	for _, testCase := range []struct {
		name    string
		resolve func() (string, error)
	}{
		{name: "no resolver", resolve: nil},
		{name: "group absent", resolve: func() (string, error) { return "", nil }},
		{name: "blank SID", resolve: func() (string, error) { return "   ", nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			applied, err := WindowsNetworkPlanForApply(plan, testCase.resolve)
			if err != nil {
				t.Fatalf("a machine with no offline group is not a failure: %v", err)
			}
			if len(applied.IdentitySIDs) != 1 || applied.IdentitySIDs[0] != "S-1-5-21-marker" {
				t.Errorf("plan changed on a machine with no offline group: %v", applied.IdentitySIDs)
			}
		})
	}
}

// A LOOKUP FAILURE IS NOT AN ABSENT GROUP. The filters are machine-global and
// are replaced wholesale, so returning a marker-only plan here does not decline
// to add coverage, it removes coverage another workspace is relying on.
//
// This is reachable only from an opted-OUT home, because
// assertWindowsNetworkPlanCoversOfflineGroup is gated on provisioned and never
// runs for one. Workspace A opts in, the group SID lands in the global filters;
// workspace B is opted out and its lookup fails transiently; B installs a plan
// without the group and A's NetworkDeny commands silently regain egress while
// A's marker and its direct membership check both still pass.
func TestPlanForApplyRefusesToReplaceFiltersWhenTheGroupCannotBeRead(t *testing.T) {
	// Opted-out home B: the coverage assert is skipped for it, so nothing else
	// stands between the failure and the wholesale replace.
	if err := assertWindowsNetworkPlanCoversOfflineGroup(
		WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-B-marker"}},
		func() (string, error) { return "", errors.New("group lookup failed") },
		false,
	); err != nil {
		t.Fatalf("the coverage assert is meant to be skipped for an opted-out home: %v", err)
	}

	plan := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-5-21-B-marker"}}
	applied, err := WindowsNetworkPlanForApply(plan, func() (string, error) {
		return "", errors.New("group lookup failed")
	})
	if err == nil {
		t.Fatalf("a failed group lookup installed %v, stripping the offline group from the machine's filters", applied.IdentitySIDs)
	}
	if !strings.Contains(err.Error(), "group lookup failed") {
		t.Errorf("the underlying failure must be reported so the operator can act on it: %v", err)
	}
}
