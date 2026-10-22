package sandbox

import (
	"strings"
	"testing"
)

// TWO SANDBOX HOMES, ONE WORKSPACE, ONE RUNTIME TREE.
//
// The runtime root is chosen per workspace, so two homes working on the same
// checkout select the same tree. Their plans cannot match: the capability SIDs
// in a plan come from the home's own store. The tree used to hold one stamp file
// with one plan hash in it, so a successful setup for the second home overwrote
// the attestation of the first, whose marker and configuration had not changed,
// and that home was rejected as "provisioned for a different configuration".
// Re-running its setup only moved the breakage to the other home.
//
// Asked of a LATER COMMAND, prepared the way the planner prepares one, because
// the marker and the home selection were both correct throughout: the only
// thing wrong was what the shared tree said when the command went to read it.
func twoSandboxHomesScenario(t *testing.T, publish publishWindowsSetup) {
	for _, order := range []struct{ name, first, second string }{
		{"A then B", "A", "B"},
		{"B then A", "B", "A"},
	} {
		t.Run(order.name, func(t *testing.T) {
			workspace, _ := windowsRuntimeTestRoots(t)
			homes := map[string]string{"A": t.TempDir(), "B": t.TempDir()}
			profile := bareWindowsProfile(workspace)

			setups := map[string]WindowsSandboxSetupConfig{}
			setup := func(name string) {
				t.Helper()
				setups[name] = preparedWindowsSetupConfig(t, workspace, homes[name], profile)
				publish(t, setups[name])
			}
			valid := func(name string) error {
				t.Helper()
				return validateAsALaterCommand(t, workspace, homes[name], profile)
			}

			setup(order.first)
			if err := valid(order.first); err != nil {
				t.Fatalf("SETUP INVALID: %s is rejected straight after its own setup: %v", order.first, err)
			}

			setup(order.second)

			// The premise, checked rather than assumed: one tree, two plans.
			rootFirst := windowsSandboxSelectedRuntimeRoot(setups[order.first].PermissionProfile)
			rootSecond := windowsSandboxSelectedRuntimeRoot(setups[order.second].PermissionProfile)
			if rootFirst == "" || !sameWindowsRuntimeRootPath(rootFirst, rootSecond) {
				t.Fatalf("SETUP INVALID: the two homes did not select one runtime root (%q, %q)", rootFirst, rootSecond)
			}
			markerFirst, err := BuildWindowsSandboxSetupMarker(setups[order.first])
			if err != nil {
				t.Fatal(err)
			}
			markerSecond, err := BuildWindowsSandboxSetupMarker(setups[order.second])
			if err != nil {
				t.Fatal(err)
			}
			if markerFirst.ACLPlanHash == markerSecond.ACLPlanHash {
				t.Fatal("SETUP INVALID: the two homes produced one plan hash, so neither could displace the other")
			}

			if err := valid(order.second); err != nil {
				t.Errorf("%s is rejected after its own setup: %v", order.second, err)
			}
			if err := valid(order.first); err != nil {
				t.Errorf("a successful setup for %s invalidated %s, whose marker and configuration did not change: %v", order.second, order.first, err)
			}

			// AND THE STAMP STILL DOES ITS JOB. The same pathname with a new object
			// behind it carries no capability ACL, and both homes have to notice,
			// each through its own attestation.
			recreateRuntimeTree(t, rootFirst)
			for _, name := range []string{order.first, order.second} {
				err := valid(name)
				if err == nil {
					t.Errorf("%s still validates against a runtime tree that was removed and recreated", name)
					continue
				}
				if !strings.Contains(err.Error(), "removed since setup ran") {
					t.Errorf("%s is rejected for the wrong reason: %v", name, err)
				}
			}
			// Repairing one home repairs that home only.
			setup(order.first)
			if err := valid(order.first); err != nil {
				t.Errorf("%s is still rejected after re-running its setup: %v", order.first, err)
			}
			if err := valid(order.second); err == nil {
				t.Errorf("%s validates on the strength of a setup that was run for %s", order.second, order.first)
			}
		})
	}
}

func TestTwoSandboxHomesKeepTheirOwnAttestation(t *testing.T) {
	twoSandboxHomesScenario(t, publishThroughTheMarkerWriter)
}

// The name is a function of the plan and of nothing else, and it is one safe
// path component whatever it is handed.
func TestRuntimeStampNameIsScopedToThePlan(t *testing.T) {
	first := windowsSandboxRuntimeStampName("plan-one")
	if first != windowsSandboxRuntimeStampName("  plan-one\n") {
		t.Error("surrounding whitespace changed the stamp name, so a writer and a reader can disagree about one plan")
	}
	if first == windowsSandboxRuntimeStampName("plan-two") {
		t.Error("two plans share a stamp name")
	}
	for _, hostile := range []string{"../../outside", `..\..\outside`, "a/b", "C:\\x", strings.Repeat("h", 4096), ""} {
		name := windowsSandboxRuntimeStampName(hostile)
		if !strings.HasPrefix(name, windowsSandboxRuntimeStampPrefix) || strings.ContainsAny(name, `/\:`) || len(name) > 64 {
			t.Errorf("stamp name for %.20q is %q, want one short path component under the stamp prefix", hostile, name)
		}
	}
}
