package execprofile

import (
	"reflect"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

// The no-regression invariant of the whole feature: balanced must be the empty
// profile (nothing but a name), so selecting it cannot change a run.
func TestBalancedProfileIsEmpty(t *testing.T) {
	profile, ok := Lookup("balanced")
	if !ok {
		t.Fatal("balanced must exist in the catalog")
	}
	if profile.Name != "balanced" {
		t.Fatalf("Name = %q, want %q", profile.Name, "balanced")
	}
	profile.Name = ""
	if !reflect.DeepEqual(profile, Profile{}) {
		t.Fatalf("balanced must be zero-valued apart from its name, got %+v", profile)
	}
	if policy := Balanced.Policy(80, false); policy != nil {
		t.Fatalf("balanced Policy must be nil (byte-identical loop), got %+v", policy)
	}
}

func TestLookupIsCaseAndSpaceInsensitive(t *testing.T) {
	for _, name := range []string{"fast", "Fast", "  FAST  "} {
		if _, ok := Lookup(name); !ok {
			t.Fatalf("Lookup(%q) should resolve", name)
		}
	}
	if _, ok := Lookup("turbo"); ok {
		t.Fatal("Lookup(turbo) should not resolve")
	}
}

func TestNamesAreSorted(t *testing.T) {
	want := []string{"balanced", "fast", "thorough"}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

// Escalation targets must be the displaced values only — the catalog carries
// triggers, the call site supplies what was displaced, and nothing else may
// appear in the escalation.
func TestFastPolicyTargetsDisplacedValuesOnly(t *testing.T) {
	policy := Fast.Policy(80, true)
	if policy == nil {
		t.Fatal("fast must arm an escalation policy")
	}
	if policy.Name != "fast" {
		t.Fatalf("policy.Name = %q, want fast", policy.Name)
	}
	esc := policy.Escalate
	if esc == nil {
		t.Fatal("fast policy must carry an escalation")
	}
	if esc.MaxTurns != 80 {
		t.Fatalf("Escalate.MaxTurns = %d, want the displaced 80", esc.MaxTurns)
	}
	if esc.ReasoningEffort != "" {
		t.Fatalf("Escalate.ReasoningEffort = %q, want empty (displaced effort is the provider default by construction)", esc.ReasoningEffort)
	}
	if !esc.RestoreDefaultEffort {
		t.Fatal("effort was profile-filled, so escalation must restore the provider default")
	}
	if esc.RestoreCompletionGate {
		t.Fatal("fast does not touch the completion gate, so escalation must not either")
	}
	if esc.OnToolFailureStreak != Fast.EscalateOnToolFailureStreak ||
		esc.OnCompletionUncertain != Fast.EscalateOnCompletionUncertain ||
		esc.OnSelfCorrectFailure != Fast.EscalateOnSelfCorrectFailure ||
		esc.OnRiskyMutation != Fast.EscalateOnRiskyMutation {
		t.Fatalf("escalation triggers must mirror the catalog, got %+v", esc)
	}
}

// The effort restore arms only when the profile actually filled the effort:
// with an explicit user effort the profile backed off, so escalation must not
// clear the user's choice.
func TestFastPolicyExplicitEffortIsNeverRestored(t *testing.T) {
	policy := Fast.Policy(80, false)
	if policy == nil || policy.Escalate == nil {
		t.Fatal("fast must still arm its triggers")
	}
	if policy.Escalate.RestoreDefaultEffort {
		t.Fatal("the profile did not fill the effort, so escalation must leave it alone")
	}
}

// A profile that did not displace the budget (explicit --max-turns pinned it)
// must not let escalation move the ceiling at all.
func TestFastPolicyZeroDisplacedLeavesCeilingUntouched(t *testing.T) {
	policy := Fast.Policy(0, false)
	if policy == nil || policy.Escalate == nil {
		t.Fatal("fast must still arm its triggers with a zero displaced budget")
	}
	if policy.Escalate.MaxTurns != 0 {
		t.Fatalf("Escalate.MaxTurns = %d, want 0 (no displaced value to restore)", policy.Escalate.MaxTurns)
	}
}

// Thorough is already the maximum posture: no triggers, so no policy — the
// loop must stay byte-identical to the same knobs set by hand.
func TestThoroughPolicyIsNil(t *testing.T) {
	if policy := Thorough.Policy(80, true); policy != nil {
		t.Fatalf("thorough Policy must be nil, got %+v", policy)
	}
}

// Pin the catalog's provisional values so a retune is a deliberate,
// test-visible diff (these floors came from the Phase 0 baseline's read-class
// evidence and are expected to move after the post-oracle-fix re-capture).
func TestCatalogProvisionalValues(t *testing.T) {
	if Fast.MaxTurns != 30 || Fast.ReasoningEffort != "low" || Fast.SelfCorrect {
		t.Fatalf("fast knobs changed: %+v", Fast)
	}
	// Critical, not High: the sandbox classifies every shell command as at
	// least high risk, so a High trigger would spend the one-shot escalation
	// on the first benign `go test` of any coding task.
	if Fast.EscalateOnRiskyMutation != sandbox.RiskCritical {
		t.Fatalf("fast risky-mutation trigger = %q, want %q", Fast.EscalateOnRiskyMutation, sandbox.RiskCritical)
	}
	if Thorough.MaxTurns != 160 || Thorough.ReasoningEffort != "high" || !Thorough.SelfCorrect {
		t.Fatalf("thorough knobs changed: %+v", Thorough)
	}
}
