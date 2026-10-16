package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// profileSwitchModel builds a session on a model whose catalog entry supports
// the fast profile's "low" effort, with a second saved provider whose custom
// model has no catalog entry (so no effort ring) — the two directions the
// model-switch reconciliation must handle.
func profileSwitchModel(t *testing.T) model {
	t.Helper()
	return newModel(context.Background(), Options{
		ProviderName:    "anthropic",
		ModelName:       "claude-sonnet-4.5",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
		SavedProviders: []config.ProviderProfile{
			{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "kimi-k2.7-code:cloud"},
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
}

// The profile's effort fill is per-model and must be re-derived on a model
// switch: dropped when the destination does not support the level, refilled
// when a later destination does, with the escalation's effort restore tracking
// whether the profile currently governs the effort.
func TestProfileEffortReconciledOnModelSwitch(t *testing.T) {
	m := profileSwitchModel(t)

	m, _ = m.handleProfileCommand("fast")
	if m.reasoningEffort != "low" || m.agentOptions.Profile == nil || !m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatalf("fast on a low-supporting model must fill low and arm the restore, got effort %q", m.reasoningEffort)
	}

	// Supported -> unsupported: the profile-applied level must not survive
	// onto a model with no effort ring.
	m, text, ok, _, _ := m.switchProviderModel("ollama", "kimi-k2.7-code:cloud")
	if !ok {
		t.Fatalf("switch to ollama failed: %q", text)
	}
	if m.reasoningEffort != "" || m.execProfileAppliedEffort != "" {
		t.Fatalf("effort = %q applied = %q, want both cleared on an unsupported destination", m.reasoningEffort, m.execProfileAppliedEffort)
	}
	if m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("the profile no longer governs the effort, so the restore must disarm")
	}
	if m.execProfileName != "fast" || m.agentOptions.MaxTurns != 30 {
		t.Fatalf("profile must stay active across the switch: name %q turns %d", m.execProfileName, m.agentOptions.MaxTurns)
	}

	// Unsupported -> supported: switching (back) to a supporting model must
	// behave like selecting the profile there.
	m, text, ok, _, _ = m.switchProviderModel("anthropic", "claude-sonnet-4.5")
	if !ok {
		t.Fatalf("switch back to anthropic failed: %q", text)
	}
	if m.reasoningEffort != "low" || m.execProfileAppliedEffort != "low" {
		t.Fatalf("effort = %q applied = %q, want the profile's low refilled", m.reasoningEffort, m.execProfileAppliedEffort)
	}
	if !m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("the profile governs the effort again, so the restore must re-arm")
	}
}

// The direct /model path (reconcileEffortForModelSwitch, driven by the
// target's authoritative ring) drops a KNOWN-unsupported effort — the TUI run
// path forwards the effort unfiltered, so it must not survive. When that drop
// voids a choice made under an active profile, the profile's effort
// bookkeeping resets and the profile resumes governing: a later switch to a
// supporting ring fills the profile's level again.
func TestProfileEffortVoidedByModelSwitchDrop(t *testing.T) {
	m := profileSwitchModel(t)

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleEffortCommand("high")
	if m.reasoningEffort != "high" || !m.execProfileEffortTouched {
		t.Fatalf("setup: effort %q touched %v, want an explicit high", m.reasoningEffort, m.execProfileEffortTouched)
	}

	// Destination with an AUTHORITATIVE empty ring (a catalog model without
	// effort controls): "high" is known-unsupported, the drop fires, and the
	// voided choice resets the profile's bookkeeping.
	m, reset := m.reconcileEffortForModelSwitch(nil, true)
	if !reset {
		t.Fatal("a dropped preference with nothing refilled must report the reset")
	}
	if m.reasoningEffort != "" {
		t.Fatalf("effort = %q, a known-unsupported level must not survive", m.reasoningEffort)
	}
	if m.execProfileEffortTouched || m.execProfileAppliedEffort != "" {
		t.Fatalf("voided choice must reset the bookkeeping: touched %v applied %q", m.execProfileEffortTouched, m.execProfileAppliedEffort)
	}
	if m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("no profile-governed effort on this model, so the restore must be disarmed")
	}
	if m.execProfileName != "fast" || m.agentOptions.MaxTurns != 30 {
		t.Fatalf("profile must stay active: name %q turns %d", m.execProfileName, m.agentOptions.MaxTurns)
	}

	// With the void cleared, a destination that supports the profile's level
	// behaves like selecting the profile there: the fill returns.
	ring := []modelregistry.ReasoningEffort{"low", "medium", "high"}
	m, reset = m.reconcileEffortForModelSwitch(ring, true)
	if reset {
		t.Fatal("a refill is not a reset to auto")
	}
	if m.reasoningEffort != "low" || m.execProfileAppliedEffort != "low" {
		t.Fatalf("effort = %q applied = %q, want the profile's low refilled", m.reasoningEffort, m.execProfileAppliedEffort)
	}
	if !m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("the profile governs the effort again, so the restore must re-arm")
	}
}

// A touched effort the destination DOES support survives the direct /model
// path untouched — the drop only fires for known-unsupported levels.
func TestProfileEffortTouchedSurvivesSupportedModelSwitch(t *testing.T) {
	m := profileSwitchModel(t)

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleEffortCommand("high")
	m, reset := m.reconcileEffortForModelSwitch([]modelregistry.ReasoningEffort{"low", "medium", "high"}, true)
	if reset {
		t.Fatal("nothing was dropped, so no reset")
	}
	if m.reasoningEffort != "high" || !m.execProfileEffortTouched {
		t.Fatalf("effort = %q touched %v, an explicit choice the destination supports must survive", m.reasoningEffort, m.execProfileEffortTouched)
	}
	if m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("an explicit choice keeps the restore disarmed")
	}
}

// An UNKNOWN ring (live-discovered/custom target with no catalog entry) is
// not evidence of unsupported: an explicit preference survives, exactly as it
// does on the cross-provider picker path, while the profile's own fill still
// retreats to models where support is known.
func TestProfileEffortUnknownRingPreservesExplicitChoice(t *testing.T) {
	m := profileSwitchModel(t)

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleEffortCommand("high")
	m, reset := m.reconcileEffortForModelSwitch(nil, false)
	if reset {
		t.Fatal("an unknown ring must not report a reset")
	}
	if m.reasoningEffort != "high" || !m.execProfileEffortTouched {
		t.Fatalf("effort = %q touched %v, an explicit choice must survive an unknown ring", m.reasoningEffort, m.execProfileEffortTouched)
	}

	// Untouched profile fill on an unknown ring: the fill retreats (the
	// profile only governs where support is known), with clean bookkeeping.
	m2 := profileSwitchModel(t)
	m2, _ = m2.handleProfileCommand("fast")
	m2, reset = m2.reconcileEffortForModelSwitch(nil, false)
	if reset {
		t.Fatal("a profile-fill retreat is not an unsupported-preference reset")
	}
	if m2.reasoningEffort != "" || m2.execProfileAppliedEffort != "" {
		t.Fatalf("effort = %q applied = %q, the profile fill must retreat on an unknown ring", m2.reasoningEffort, m2.execProfileAppliedEffort)
	}
	if m2.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("no profile-governed effort, so the restore must be disarmed")
	}
}

// An explicitly touched effort is the user's choice: the reconciliation must
// leave it alone entirely across model switches.
func TestProfileEffortTouchedSurvivesModelSwitch(t *testing.T) {
	m := profileSwitchModel(t)

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleEffortCommand("high")
	m, text, ok, _, _ := m.switchProviderModel("ollama", "kimi-k2.7-code:cloud")
	if !ok {
		t.Fatalf("switch to ollama failed: %q", text)
	}
	if m.reasoningEffort != "high" {
		t.Fatalf("effort = %q, an explicit choice must survive the switch untouched", m.reasoningEffort)
	}
	if m.agentOptions.Profile.Escalate.RestoreDefaultEffort {
		t.Fatal("an explicit choice keeps the restore disarmed")
	}
}

func TestProfileCommandStatus(t *testing.T) {
	_, out := model{}.handleProfileCommand("")
	if !strings.Contains(out, "balanced (default)") {
		t.Fatalf("bare /profile must report the balanced default, got %q", out)
	}
	_, out = model{}.handleProfileCommand("status")
	if !strings.Contains(out, "balanced (default)") {
		t.Fatalf("/profile status must report the balanced default, got %q", out)
	}
}

func TestProfileCommandUnknownValue(t *testing.T) {
	got, out := model{}.handleProfileCommand("turbo")
	if !strings.Contains(out, "Usage:") {
		t.Fatalf("unknown profile must print usage, got %q", out)
	}
	if got.execProfileName != "" || got.agentOptions.Profile != nil {
		t.Fatal("unknown profile must not change any state")
	}
}

// Fast applies to the next run: the turn budget drops, the escalation policy
// is armed with the DISPLACED budget as its restore target, and effort stays
// untouched when the session has no model-supported effort ring (model{}).
func TestProfileCommandSetsFastPosture(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	got, out := m.handleProfileCommand("fast")
	if got.agentOptions.MaxTurns != 30 {
		t.Fatalf("MaxTurns = %d, want the fast profile's 30", got.agentOptions.MaxTurns)
	}
	if got.execProfileName != "fast" {
		t.Fatalf("execProfileName = %q, want fast", got.execProfileName)
	}
	policy := got.agentOptions.Profile
	if policy == nil || policy.Escalate == nil {
		t.Fatal("fast must arm an escalation policy on agentOptions")
	}
	if policy.Escalate.MaxTurns != 80 {
		t.Fatalf("Escalate.MaxTurns = %d, want the displaced 80", policy.Escalate.MaxTurns)
	}
	if got.reasoningEffort != "" {
		t.Fatalf("effort = %q, must stay auto when the model exposes no effort ring", got.reasoningEffort)
	}
	if !strings.Contains(out, "fast") || !strings.Contains(out, "headless-only") {
		t.Fatalf("status must name the profile and the TUI signal asymmetry, got %q", out)
	}
}

func TestProfileCommandThoroughArmsSelfCorrect(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	got, _ := m.handleProfileCommand("thorough")
	if got.agentOptions.MaxTurns != 160 {
		t.Fatalf("MaxTurns = %d, want thorough's 160", got.agentOptions.MaxTurns)
	}
	if !got.selfCorrectTests {
		t.Fatal("thorough must arm full self-correction")
	}
	if got.agentOptions.Profile != nil {
		t.Fatal("thorough has no escalation triggers, so the loop policy must stay nil")
	}
}

func TestProfileCommandBalancedRestoresDisplaced(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	m, _ = m.handleProfileCommand("fast")
	got, _ := m.handleProfileCommand("balanced")
	if got.agentOptions.MaxTurns != 80 {
		t.Fatalf("MaxTurns = %d, want the displaced 80 restored", got.agentOptions.MaxTurns)
	}
	if got.execProfileName != "" || got.agentOptions.Profile != nil {
		t.Fatal("balanced must clear the profile state and loop policy")
	}
}

// Profiles must not stack: switching fast -> thorough restores fast's
// displacement first, so thorough displaces the ORIGINAL budget and a later
// balanced lands back on it.
func TestProfileCommandSwitchDoesNotStack(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleProfileCommand("thorough")
	if m.agentOptions.MaxTurns != 160 {
		t.Fatalf("MaxTurns = %d, want thorough's 160", m.agentOptions.MaxTurns)
	}
	if m.execProfileDisplacedMaxTurns != 80 {
		t.Fatalf("displaced = %d, want the original 80 (not fast's 30)", m.execProfileDisplacedMaxTurns)
	}
	m, _ = m.handleProfileCommand("balanced")
	if m.agentOptions.MaxTurns != 80 {
		t.Fatalf("MaxTurns = %d, want the original 80 restored", m.agentOptions.MaxTurns)
	}
}

// An explicit /turns while a profile is active is a pinned budget: it must
// disarm the escalation's turn target (mirroring headless exec, where an
// explicit --max-turns leaves nothing displaced) while the other triggers
// stay armed, and it must survive a later revert.
func TestProfileCommandTurnsPinDisarmsEscalationTarget(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleTurnsCommand("20")
	if m.agentOptions.MaxTurns != 20 {
		t.Fatalf("MaxTurns = %d, want the pinned 20", m.agentOptions.MaxTurns)
	}
	policy := m.agentOptions.Profile
	if policy == nil || policy.Escalate == nil {
		t.Fatal("the profile's other escalation triggers must stay armed")
	}
	if policy.Escalate.MaxTurns != 0 {
		t.Fatalf("Escalate.MaxTurns = %d, want 0 (a pinned budget must never be raised by escalation)", policy.Escalate.MaxTurns)
	}
	if policy.Escalate.OnToolFailureStreak == 0 {
		t.Fatal("disarming the turn target must not disarm the other triggers")
	}
	m, _ = m.handleProfileCommand("balanced")
	if m.agentOptions.MaxTurns != 20 {
		t.Fatalf("MaxTurns = %d, the pinned 20 must survive the revert", m.agentOptions.MaxTurns)
	}
}

// A manual override that COINCIDES with the profile's own value must still
// survive the revert: touched beats value equality.
func TestProfileCommandRevertLeavesCoincidingOverride(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleTurnsCommand("30") // explicit, happens to equal fast's 30
	m, _ = m.handleProfileCommand("balanced")
	if m.agentOptions.MaxTurns != 30 {
		t.Fatalf("MaxTurns = %d, the explicit /turns 30 must survive even though it equals fast's value", m.agentOptions.MaxTurns)
	}

	// Same for self-correct: /selfcorrect off then on under thorough is an
	// explicit opt-in, not the profile's arming.
	m2 := model{}
	m2.agentOptions.MaxTurns = 80
	m2, _ = m2.handleProfileCommand("thorough")
	m2, _ = m2.handleSelfCorrectCommand("off")
	m2, _ = m2.handleSelfCorrectCommand("on")
	m2, _ = m2.handleProfileCommand("balanced")
	if !m2.selfCorrectTests {
		t.Fatal("an explicit /selfcorrect on must survive the revert even though thorough also armed it")
	}
}

// Reverting a profile whose displaced budget was zero (nothing was set before
// it) must clear ZERO_MAX_TURNS: SetMaxTurnsEnv ignores zero, so without an
// explicit unset, spawned sub-agents would keep the removed profile's budget.
func TestProfileCommandRevertFromZeroClearsMaxTurnsEnv(t *testing.T) {
	t.Setenv(config.MaxTurnsEnv, "")
	m := model{} // MaxTurns 0: the profile displaces nothing

	m, _ = m.handleProfileCommand("fast")
	if got := os.Getenv(config.MaxTurnsEnv); got != "30" {
		t.Fatalf("env after apply = %q, want the profile's 30", got)
	}
	m, _ = m.handleProfileCommand("balanced")
	if m.agentOptions.MaxTurns != 0 {
		t.Fatalf("MaxTurns = %d, want the displaced 0 restored", m.agentOptions.MaxTurns)
	}
	if got, present := os.LookupEnv(config.MaxTurnsEnv); present {
		t.Fatalf("env after revert = %q (present), want the key removed", got)
	}
}

// An explicit effort choice after /profile must disarm the escalation's
// effort restore (the effort analog of the /turns pin): a mid-run escalation
// must never clear an effort the user pinned by hand. Needs a catalog model
// with an effort ring so the profile's fill actually applies.
func TestProfileCommandExplicitEffortDisarmsRestore(t *testing.T) {
	m := model{modelName: "claude-sonnet-4.5"}
	m.agentOptions.MaxTurns = 80

	m, _ = m.handleProfileCommand("fast")
	if m.reasoningEffort != "low" {
		t.Fatalf("profile did not fill low effort for the test model: got %q", m.reasoningEffort)
	}
	policy := m.agentOptions.Profile
	if policy == nil || policy.Escalate == nil || !policy.Escalate.RestoreDefaultEffort {
		t.Fatal("profile-filled effort must arm the escalation effort restore")
	}

	m, _ = m.handleEffortCommand("high")
	if m.reasoningEffort != "high" {
		t.Fatalf("effort = %q, want the explicit high", m.reasoningEffort)
	}
	policy = m.agentOptions.Profile
	if policy == nil || policy.Escalate == nil {
		t.Fatal("the escalation policy must stay armed")
	}
	if policy.Escalate.RestoreDefaultEffort {
		t.Fatal("an explicit /effort must disarm the effort restore")
	}
	if policy.Escalate.MaxTurns != 80 {
		t.Fatalf("Escalate.MaxTurns = %d, disarming effort must not touch the turn target", policy.Escalate.MaxTurns)
	}
	// And the explicit choice survives a later revert (touched bit).
	m, _ = m.handleProfileCommand("balanced")
	if m.reasoningEffort != "high" {
		t.Fatalf("effort = %q, the explicit high must survive the revert", m.reasoningEffort)
	}
}

// A manual override made after selecting a profile survives the revert: the
// profile only restores knobs that still hold the value it applied.
func TestProfileCommandRevertLeavesManualOverride(t *testing.T) {
	m := model{}
	m.agentOptions.MaxTurns = 80

	m, _ = m.handleProfileCommand("fast")
	m, _ = m.handleTurnsCommand("100")
	got, _ := m.handleProfileCommand("balanced")
	if got.agentOptions.MaxTurns != 100 {
		t.Fatalf("MaxTurns = %d, the user's explicit /turns 100 must survive the revert", got.agentOptions.MaxTurns)
	}
	// An explicit /selfcorrect choice armed BEFORE thorough must survive too.
	m2 := model{selfCorrectTests: true}
	m2.agentOptions.MaxTurns = 80
	m2, _ = m2.handleProfileCommand("thorough")
	m2, _ = m2.handleProfileCommand("balanced")
	if !m2.selfCorrectTests {
		t.Fatal("an explicit self-correct opt-in must survive profile revert")
	}
}
