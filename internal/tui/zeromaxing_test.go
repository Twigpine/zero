package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/execprofile"
	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// zeromaxingTestModel builds a session on a reasoning model that supports
// "high", so the posture's effort fill applies. disabled drives the config gate.
func zeromaxingTestModel(t *testing.T, disabled bool) model {
	t.Helper()
	return newModel(context.Background(), Options{
		ProviderName:       "anthropic",
		ModelName:          "claude-sonnet-4.5",
		Provider:           &fakeProvider{},
		ProviderProfile:    config.ProviderProfile{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
		SavedProviders:     []config.ProviderProfile{{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"}},
		ZeromaxingDisabled: disabled,
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
}

// (g) THE ENTRY-POINT EQUIVALENCE. /effort zeromaxing and /profile zeromaxing
// must produce IDENTICAL resolved state. They do so by delegating to one
// implementation rather than by two parallel ones a test hopes agree — this
// asserts the result, and the delegation makes it true by construction.
func TestEffortAndProfileEntryPointsResolveIdentically(t *testing.T) {
	viaEffort, effortText := zeromaxingTestModel(t, false).handleEffortCommand(execprofile.Name)
	viaProfile, profileText := zeromaxingTestModel(t, false).handleProfileCommand(execprofile.Name)

	if viaEffort.execProfileName != viaProfile.execProfileName ||
		viaEffort.reasoningEffort != viaProfile.reasoningEffort ||
		viaEffort.agentOptions.MaxTurns != viaProfile.agentOptions.MaxTurns ||
		viaEffort.selfCorrectTests != viaProfile.selfCorrectTests ||
		viaEffort.zeromaxing != viaProfile.zeromaxing ||
		viaEffort.execProfileAppliedEffort != viaProfile.execProfileAppliedEffort ||
		viaEffort.execProfileEffortUnraised != viaProfile.execProfileEffortUnraised {
		t.Fatalf("the two entry points diverged:\n/effort:  profile=%q effort=%q turns=%d sc=%v posture=%v\n/profile: profile=%q effort=%q turns=%d sc=%v posture=%v",
			viaEffort.execProfileName, viaEffort.reasoningEffort, viaEffort.agentOptions.MaxTurns, viaEffort.selfCorrectTests, viaEffort.zeromaxing,
			viaProfile.execProfileName, viaProfile.reasoningEffort, viaProfile.agentOptions.MaxTurns, viaProfile.selfCorrectTests, viaProfile.zeromaxing)
	}
	if !reflect.DeepEqual(effortText, profileText) {
		t.Fatalf("the two entry points printed different output:\n/effort:\n%s\n/profile:\n%s", effortText, profileText)
	}
	if viaEffort.reasoningEffort != modelregistry.ReasoningEffortHigh {
		t.Fatalf("both must resolve the effort to high, got %q", viaEffort.reasoningEffort)
	}
}

// (f) THE RESERVATION. /effort max is UNCHANGED: it still parses as a raw
// provider level and still fails reasoningEffortAllowed on a model that does not
// list it. The posture must not have quietly claimed the spelling.
func TestEffortMaxReservationUnchangedTUI(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	before := m

	m, text := m.handleEffortCommand("max")
	if !strings.Contains(text, "not supported") {
		t.Fatalf("/effort max must still report an unsupported level, got:\n%s", text)
	}
	// It must not have selected the posture, changed the effort, or moved the budget.
	if m.execProfileName != before.execProfileName {
		t.Fatalf("/effort max must not select a profile, got %q", m.execProfileName)
	}
	if m.reasoningEffort != before.reasoningEffort {
		t.Fatalf("/effort max must not change the effort, got %q", m.reasoningEffort)
	}
	if m.zeromaxing != agent.ZeromaxingOff {
		t.Fatalf("/effort max must not arm the posture, got %v", m.zeromaxing)
	}
	if m.agentOptions.MaxTurns != before.agentOptions.MaxTurns {
		t.Fatalf("/effort max must not move the turn budget, got %d", m.agentOptions.MaxTurns)
	}
}

// (a) TUI: an explicit /effort <level> survives the posture.
func TestZeromaxingDoesNotOverrideExplicitEffortTUI(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleEffortCommand("low")
	if m.reasoningEffort != "low" {
		t.Fatalf("setup: effort = %q, want low", m.reasoningEffort)
	}
	m, _ = m.handleEffortCommand(execprofile.Name)
	if m.reasoningEffort != "low" {
		t.Fatalf("effort = %q, the explicit low must survive the posture's high", m.reasoningEffort)
	}
	if m.execProfileAppliedEffort != "" {
		t.Fatalf("must not claim to have applied an effort it did not: %q", m.execProfileAppliedEffort)
	}
}

// (c) TUI: an explicit /turns pins the budget.
func TestZeromaxingDoesNotOverrideExplicitTurnsTUI(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleEffortCommand(execprofile.Name)
	if m.agentOptions.MaxTurns != 480 {
		t.Fatalf("the posture must raise the budget to 480, got %d", m.agentOptions.MaxTurns)
	}
	m, _ = m.handleTurnsCommand("50")
	if m.agentOptions.MaxTurns != 50 {
		t.Fatalf("an explicit /turns must win, got %d", m.agentOptions.MaxTurns)
	}
	if !m.execProfileTurnsTouched {
		t.Fatal("/turns under a profile must mark the knob touched so a revert leaves it alone")
	}
}

// (p) revertExecProfile is knob-by-knob, and the posture is a FOURTH knob. All
// four must be restored — budget, effort, self-correct, and the posture moving
// to Exiting exactly once.
func TestRevertRestoresAllFourKnobs(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	baseTurns := m.agentOptions.MaxTurns
	baseEffort := m.reasoningEffort
	baseSelfCorrect := m.selfCorrectTests

	m, _ = m.handleEffortCommand(execprofile.Name)
	if m.agentOptions.MaxTurns != 480 || m.reasoningEffort != "high" || !m.selfCorrectTests {
		t.Fatalf("setup: knobs not applied: turns=%d effort=%q sc=%v",
			m.agentOptions.MaxTurns, m.reasoningEffort, m.selfCorrectTests)
	}
	if m.zeromaxing != agent.ZeromaxingEntering {
		t.Fatalf("selecting must enter the posture, got %v", m.zeromaxing)
	}

	// /effort auto is the effort namespace's off switch.
	m, _ = m.handleEffortCommand("auto")
	if m.agentOptions.MaxTurns != baseTurns {
		t.Fatalf("knob 1 (turn budget) = %d, want the displaced %d", m.agentOptions.MaxTurns, baseTurns)
	}
	if m.reasoningEffort != baseEffort {
		t.Fatalf("knob 2 (effort) = %q, want the displaced %q", m.reasoningEffort, baseEffort)
	}
	if m.selfCorrectTests != baseSelfCorrect {
		t.Fatalf("knob 3 (self-correct) = %v, want the displaced %v", m.selfCorrectTests, baseSelfCorrect)
	}
	if m.zeromaxing != agent.ZeromaxingExiting {
		t.Fatalf("knob 4 (posture) = %v, want ZeromaxingExiting so the exit notice fires once", m.zeromaxing)
	}
	if m.agentOptions.Zeromaxing != agent.ZeromaxingExiting {
		t.Fatal("the posture must reach agentOptions, or the loop never emits the exit notice")
	}
}

// /profile balanced is the other way out, and must behave identically.
func TestBothExitRoutesLeaveThePosture(t *testing.T) {
	viaEffort := zeromaxingTestModel(t, false)
	viaEffort, _ = viaEffort.handleEffortCommand(execprofile.Name)
	viaEffort, _ = viaEffort.handleEffortCommand("auto")

	viaProfile := zeromaxingTestModel(t, false)
	viaProfile, _ = viaProfile.handleProfileCommand(execprofile.Name)
	viaProfile, _ = viaProfile.handleProfileCommand("balanced")

	if viaEffort.zeromaxing != viaProfile.zeromaxing || viaEffort.zeromaxing != agent.ZeromaxingExiting {
		t.Fatalf("the two exit routes diverged: /effort auto -> %v, /profile balanced -> %v",
			viaEffort.zeromaxing, viaProfile.zeromaxing)
	}
	if viaEffort.execProfileName != "" || viaProfile.execProfileName != "" {
		t.Fatalf("both routes must clear the profile: %q / %q", viaEffort.execProfileName, viaProfile.execProfileName)
	}
}

// /effort auto under a NON-zeromaxing profile keeps its existing meaning: clear
// the effort, keep the profile. Reverting there would silently drop the profile.
func TestEffortAutoUnderOtherProfilesJustClearsTheEffort(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleProfileCommand("thorough")
	if m.reasoningEffort != "high" || m.execProfileName != "thorough" {
		t.Fatalf("setup: thorough not applied: effort=%q profile=%q", m.reasoningEffort, m.execProfileName)
	}
	m, _ = m.handleEffortCommand("auto")
	if m.reasoningEffort != "" {
		t.Fatalf("/effort auto must clear the effort, got %q", m.reasoningEffort)
	}
	if m.execProfileName != "thorough" {
		t.Fatalf("/effort auto must NOT drop a non-zeromaxing profile, got %q", m.execProfileName)
	}
	if m.zeromaxing != agent.ZeromaxingOff {
		t.Fatalf("leaving thorough must not announce a posture exit, got %v", m.zeromaxing)
	}
}

// The posture lifecycle across runs: enter and exit each announce exactly once
// no matter how many runs follow.
func TestZeromaxingLifecycleAcrossRuns(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleEffortCommand(execprofile.Name)
	if m.zeromaxing != agent.ZeromaxingEntering {
		t.Fatalf("after selecting: %v, want Entering", m.zeromaxing)
	}
	m = m.advanceZeromaxing()
	if m.zeromaxing != agent.ZeromaxingActive {
		t.Fatalf("after the first run: %v, want Active", m.zeromaxing)
	}
	m = m.advanceZeromaxing()
	if m.zeromaxing != agent.ZeromaxingActive {
		t.Fatalf("Active must be terminal while on, got %v", m.zeromaxing)
	}
	m, _ = m.handleEffortCommand("auto")
	if m.zeromaxing != agent.ZeromaxingExiting {
		t.Fatalf("after leaving: %v, want Exiting", m.zeromaxing)
	}
	m = m.advanceZeromaxing()
	if m.zeromaxing != agent.ZeromaxingOff {
		t.Fatalf("after the exit run: %v, want Off", m.zeromaxing)
	}
	m = m.advanceZeromaxing()
	if m.zeromaxing != agent.ZeromaxingOff {
		t.Fatalf("Off must be terminal, got %v", m.zeromaxing)
	}
}

// (m) A config that disabled the posture must refuse it, from BOTH entry
// points, and leave the active profile untouched rather than dropping to
// balanced.
func TestConfigCannotEnableZeromaxingTUI(t *testing.T) {
	for _, entry := range []struct {
		name string
		call func(model) (model, string)
	}{
		{"/effort", func(m model) (model, string) { return m.handleEffortCommand(execprofile.Name) }},
		{"/profile", func(m model) (model, string) { return m.handleProfileCommand(execprofile.Name) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			m := zeromaxingTestModel(t, true)
			m, _ = m.handleProfileCommand("thorough")
			turnsBefore := m.agentOptions.MaxTurns

			m, text := entry.call(m)
			if !strings.Contains(text, "Cannot use "+execprofile.Name) {
				t.Fatalf("a disabled workspace must refuse it, got %q", text)
			}
			if !strings.Contains(text, "disableZeromaxing") {
				t.Fatalf("the refusal must name the setting: %q", text)
			}
			if m.execProfileName != "thorough" || m.agentOptions.MaxTurns != turnsBefore {
				t.Fatalf("a refused switch must leave the active profile alone: profile=%q turns=%d",
					m.execProfileName, m.agentOptions.MaxTurns)
			}
			if m.zeromaxing != agent.ZeromaxingOff {
				t.Fatalf("a refused selection must not arm the posture, got %v", m.zeromaxing)
			}
		})
	}
}

// (n) The same session with it NOT disabled selects fine — the other half of
// the gate, so the test above cannot pass by the posture being broken outright.
func TestZeromaxingSelectableWhenNotDisabledTUI(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, text := m.handleEffortCommand(execprofile.Name)
	if m.execProfileName != execprofile.Name {
		t.Fatalf("must be selectable when not disabled, got %q (%s)", m.execProfileName, text)
	}
}

// Both selection paths consult the SAME rule. This is the standing-warning
// assertion: CLI and TUI apply profiles through different code with different
// state, so the one thing that must not diverge is the decision itself.
func TestSelectionRefusalAgreesAcrossPaths(t *testing.T) {
	profile, _ := execprofile.Lookup(execprofile.Name)
	for _, disabled := range []bool{true, false} {
		rule := execprofile.SelectionRefusal(profile, disabled)

		m := zeromaxingTestModel(t, disabled)
		m, text := m.handleEffortCommand(execprofile.Name)
		tuiRefused := m.execProfileName != execprofile.Name
		if tuiRefused != (rule != "") {
			t.Fatalf("disabled=%v: rule refusal=%q but TUI refused=%v (%s)", disabled, rule, tuiRefused, text)
		}
		if disabled && rule == "" {
			t.Fatal("a disabled workspace must produce a refusal for the CLI path too")
		}
	}
}

// (l) Degrade honestly. On a model with no effort ring the fill is skipped — and
// the status output must SAY so, while the rest of the posture still applies.
func TestZeromaxingOnUnsupportedModelStatesWhatItCouldNotRaise(t *testing.T) {
	// gpt-4.1 is a CATALOG model with no reasoning capability, so its empty ring
	// is authoritative. A custom endpoint with no catalog entry is deliberately
	// NOT used here: the catalog cannot vouch for it either way, so the posture
	// fills optimistically there (see TestProfileEffortFillsOnAnUnknownModel).
	m := newModel(context.Background(), Options{
		ProviderName:    "openai",
		ModelName:       "gpt-4.1",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-4.1", APIKey: "k"},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	if len(m.availableReasoningEfforts()) != 0 {
		t.Skip("gpt-4.1 gained an effort ring; the fixture no longer exercises the known-lacking path")
	}

	m, text := m.handleEffortCommand(execprofile.Name)
	if m.execProfileName != execprofile.Name {
		t.Fatalf("must still WORK on a model that cannot take the effort, got %q", m.execProfileName)
	}
	if m.agentOptions.MaxTurns != 480 {
		t.Fatalf("the rest of the posture must still apply: turns = %d", m.agentOptions.MaxTurns)
	}
	if m.reasoningEffort != "" {
		t.Fatalf("an unsupported effort must not be applied, got %q", m.reasoningEffort)
	}
	if m.execProfileEffortUnraised != modelregistry.ReasoningEffortHigh {
		t.Fatalf("the skipped level must be recorded, got %q", m.execProfileEffortUnraised)
	}
	if !strings.Contains(text, "NOT raised") {
		t.Fatalf("the output must state what it could not raise, got %q", text)
	}
}

// (q) reconcileProfileAfterModelSwitch is the re-derive SIBLING of the fill site
// in handleProfileCommand. Both decide whether the profile's effort applies to
// the ACTIVE model, so both must record WHY when it does not.
func TestZeromaxingUnraisedEffortIsRecordedOnModelSwitch(t *testing.T) {
	m := profileSwitchModel(t)
	m, _ = m.handleEffortCommand(execprofile.Name)
	if m.reasoningEffort != modelregistry.ReasoningEffortHigh {
		t.Fatalf("setup: must fill high on a supporting model, got %q", m.reasoningEffort)
	}
	if m.execProfileEffortUnraised != "" {
		t.Fatalf("nothing was skipped, so nothing should be recorded: %q", m.execProfileEffortUnraised)
	}

	// A destination the CATALOG VOUCHES has no reasoning controls. An
	// uncatalogued destination keeps the fill instead — see
	// TestProfileEffortDoorsAgree.
	m, text, ok, _, _ := m.switchProviderModel("openai", "gpt-4o")
	if !ok {
		t.Fatalf("switch to openai failed: %q", text)
	}
	if m.reasoningEffort != "" {
		t.Fatalf("the unsupported level must be dropped, got %q", m.reasoningEffort)
	}
	if m.execProfileEffortUnraised != modelregistry.ReasoningEffortHigh {
		t.Fatalf("the dropped level must be recorded so the status can state it, got %q", m.execProfileEffortUnraised)
	}
	if _, out := m.handleProfileCommand("status"); !strings.Contains(out, "NOT raised") {
		t.Fatalf("status must state the effort it could not raise after a switch:\n%s", out)
	}

	// Switching BACK to a supporting model clears the reason, so a stale
	// "NOT raised" line never outlives the model that caused it.
	m, text, ok, _, _ = m.switchProviderModel("anthropic", "claude-sonnet-4.5")
	if !ok {
		t.Fatalf("switch back failed: %q", text)
	}
	if m.reasoningEffort != modelregistry.ReasoningEffortHigh {
		t.Fatalf("returning to a supporting model must refill high, got %q", m.reasoningEffort)
	}
	if m.execProfileEffortUnraised != "" {
		t.Fatalf("the reason must clear once the effort applies again, got %q", m.execProfileEffortUnraised)
	}
}

// Gate 3: BOTH status surfaces show the resolved state unambiguously — a user
// must see what actually reached the provider, not just the posture name.
func TestBothStatusSurfacesShowResolvedState(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleEffortCommand(execprofile.Name)

	_, effortStatus := m.handleEffortCommand("")
	_, profileStatus := m.handleProfileCommand("status")

	delta := execprofile.Delta(execprofile.DeltaState{
		CurrentMaxTurns: m.execProfileDisplacedMaxTurns,
		Effort:          m.effortTransition(),
		SelfCorrect:     m.selfCorrectTransition(),
	})
	// THE SAME FACTS, EACH SURFACE'S OWN GRAMMAR. /effort keeps the shared
	// resolved-state line + the delta sentence; /profile renders key-value rows
	// with the transition as a clause on the row it qualifies ("raised by the
	// posture") — the stacked three-renderer card said every fact three times
	// and was called irritating to its face. What both must still guarantee:
	// the resolved values, the real delta facts, and exactly one effort
	// transition claim with no contradictions.
	resolved := map[string][]string{
		"/effort":         {"effort: high", "profile: " + execprofile.Name, "turns: 480", "turn budget:", "reasoning effort:", "self-correct:"},
		"/profile status": {"profile", execprofile.Name, "high", "480", "raised by the posture", "verify"},
	}
	for surface, text := range map[string]string{"/effort": effortStatus, "/profile status": profileStatus} {
		for _, want := range resolved[surface] {
			if !strings.Contains(text, want) {
				t.Fatalf("%s must show %q in its resolved state:\n%s", surface, want, text)
			}
		}
		for _, want := range []string{"480", "sub-agents"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s must mention %q:\n%s", surface, want, text)
			}
		}
		// (3) The delta is caller-relative: thorough's budget is about two
		// profiles, not about this user, and must not appear.
		if strings.Contains(text, "160") {
			t.Fatalf("%s must not compare against thorough's budget:\n%s", surface, text)
		}
		// (2) EXACTLY ONE effort TRANSITION statement, on either surface's
		// grammar — that duplication is what let "unchanged" and "NOT raised"
		// coexist.
		transitions := strings.Count(text, "raised to") + strings.Count(text, "raised by the posture") +
			strings.Count(text, "NOT raised") + strings.Count(text, "reasoning effort: unchanged")
		if transitions != 1 {
			t.Fatalf("%s must carry exactly one effort-transition claim, found %d:\n%s", surface, transitions, text)
		}
	}
	// /effort still carries the shared delta sentence VERBATIM, exactly once —
	// it is the one line the posture-switch notice and the status agree on.
	if n := strings.Count(effortStatus, delta); n != 1 {
		t.Fatalf("/effort must carry the delta exactly once, found %d:\n%s", n, effortStatus)
	}
	// Other profiles must NOT carry the posture's delta text.
	other := zeromaxingTestModel(t, false)
	_, otherText := other.handleProfileCommand("thorough")
	if strings.Contains(otherText, "turn budget:") {
		t.Fatalf("thorough must not claim the posture's delta:\n%s", otherText)
	}
}

// Gate 5: the footer chip is shown while the posture is on and hidden
// otherwise — including while Exiting, when the posture is already off.
func TestZeromaxingFooterChipVisibility(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	if m.zeromaxingActive() {
		t.Fatal("nothing selected: the chip must be hidden")
	}
	m, _ = m.handleEffortCommand(execprofile.Name)
	if !m.zeromaxingActive() {
		t.Fatal("Entering: the chip must be shown")
	}
	// STRIPPED, not raw. The chip paints each letter its own hue, so the label
	// is no longer a contiguous run in the styled output — and the production
	// hit-testers strip ANSI for exactly this reason. A raw Contains here was
	// asserting an accident of how the chip happened to be styled.
	if !strings.Contains(ansiStripLine(m.statusLine(120)), zeromaxingChipLabel) {
		t.Fatalf("the footer must carry %q while on:\n%s", zeromaxingChipLabel, ansiStripLine(m.statusLine(120)))
	}
	m = m.advanceZeromaxing()
	if !strings.Contains(ansiStripLine(m.statusLine(120)), zeromaxingChipLabel) {
		t.Fatal("Active: the chip must still be shown")
	}
	m, _ = m.handleEffortCommand("auto")
	if m.zeromaxingActive() {
		t.Fatal("Exiting: the posture is already off, so the chip must be hidden")
	}
	if strings.Contains(ansiStripLine(m.statusLine(120)), zeromaxingChipLabel) {
		t.Fatalf("the footer must drop the chip once off:\n%s", ansiStripLine(m.statusLine(120)))
	}
}

// The self-correct clause must track the user's LIVE state, not the state at
// selection time. A user who selects the posture and then turns self-correct
// back off must not keep reading "lsp → tests".
func TestSelfCorrectTransitionTracksLiveState(t *testing.T) {
	// Default session: LSP-only, so the posture raises it.
	m := zeromaxingTestModel(t, false)
	if m.selfCorrectTests {
		t.Skip("fixture no longer starts LSP-only")
	}
	m, text := m.handleEffortCommand(execprofile.Name)
	if got := m.selfCorrectTransition(); got != execprofile.SelfCorrectRaised {
		t.Fatalf("transition = %v, want SelfCorrectRaised for an LSP-only session", got)
	}
	if !strings.Contains(text, "lsp → tests") {
		t.Fatalf("an LSP-only user must be told what changes:\n%s", text)
	}

	// The user turns it back off: the posture no longer governs it, and the
	// output must stop claiming a raise.
	m, _ = m.handleSelfCorrectCommand("off")
	if got := m.selfCorrectTransition(); got != execprofile.SelfCorrectOverridden {
		t.Fatalf("transition = %v, want SelfCorrectOverridden after /selfcorrect off", got)
	}
	_, after := m.handleProfileCommand("status")
	if strings.Contains(after, "lsp → tests") {
		t.Fatalf("status must not claim a raise the session no longer has:\n%s", after)
	}
	if !strings.Contains(after, "overrides the posture") {
		t.Fatalf("status must say the user's choice is what is in effect:\n%s", after)
	}
}

// A user who ALREADY had the deeper verification on is told nothing changes —
// the one case where the old wording happened to be right.
func TestSelfCorrectTransitionAlreadyOn(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleSelfCorrectCommand("tests")
	if !m.selfCorrectTests {
		t.Fatal("setup: /selfcorrect tests did not arm it")
	}
	m, text := m.handleEffortCommand(execprofile.Name)
	if got := m.selfCorrectTransition(); got != execprofile.SelfCorrectAlreadyOn {
		t.Fatalf("transition = %v, want SelfCorrectAlreadyOn", got)
	}
	if !strings.Contains(text, "unchanged (tests)") {
		t.Fatalf("an already-on user must be told nothing changes:\n%s", text)
	}
	if strings.Contains(text, "lsp → tests") {
		t.Fatalf("must not claim a transition that did not happen:\n%s", text)
	}
}

// BUG 1 REGRESSION — and the hole it came from.
//
// The posture's effort fill silently did not happen on a custom/unknown model:
// /effort zeromaxing left the effort at "auto" while --exec-profile zeromaxing
// on the SAME model sent reasoning_effort:"high" on the wire. Same posture,
// same model, two answers.
//
// THE HOLE: every existing test asserted a surface against its OWN expectation.
// The TUI tests used claude-sonnet-4.5 (has high → fill works) and an unknown
// model (no fill → "correct" by the TUI's own rule). The CLI tests asserted the
// CLI's rule. Both suites were green while the two paths disagreed, because
// nothing compared them AGAINST EACH OTHER for the same model. A unit test on
// either helper could never have caught it.
//
// This is that missing comparison.
func TestProfileEffortFillAgreesWithTheHeadlessPath(t *testing.T) {
	for _, tc := range []struct {
		model    string
		wantFill bool
		why      string
	}{
		{"claude-sonnet-4.5", true, "catalog model that lists high"},
		{"gpt-5", true, "inferred reasoning family that lists high"},
		{"gpt-4.1", false, "catalog model whose EMPTY ring is authoritative"},
		{"some-custom-endpoint-model", true, "no catalog entry — the catalog cannot vouch either way, so do not decline"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			m := newModel(context.Background(), Options{
				ProviderName:    "p",
				ModelName:       tc.model,
				Provider:        &fakeProvider{},
				ProviderProfile: config.ProviderProfile{Name: "p", Model: tc.model, APIKey: "k"},
				NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
					return &fakeProvider{}, nil
				},
			})
			m, text := m.handleEffortCommand(execprofile.Name)

			filled := m.reasoningEffort == modelregistry.ReasoningEffortHigh
			if filled != tc.wantFill {
				t.Fatalf("%s (%s): effort filled = %v, want %v (effort=%q)\n%s",
					tc.model, tc.why, filled, tc.wantFill, m.reasoningEffort, text)
			}
			// The rest of the posture applies either way — a declined effort
			// must never cost the user the turn budget.
			if m.agentOptions.MaxTurns != 480 {
				t.Fatalf("%s: the turn budget must apply regardless of the effort, got %d",
					tc.model, m.agentOptions.MaxTurns)
			}
			// ...and the resolved-state line must not claim an effort the
			// session does not have.
			if filled && !strings.Contains(text, "effort high") {
				t.Fatalf("%s: filled but the status does not say so:\n%s", tc.model, text)
			}
			if !filled && strings.Contains(text, "effort high") {
				t.Fatalf("%s: not filled but the status claims high:\n%s", tc.model, text)
			}
		})
	}
}

// No model selected is NOT "an unknown model": there is nothing to make a
// support claim about, so the profile must not fill. This is the over-reach the
// pre-existing fast-posture test caught in the first version of the fix.
func TestProfileEffortDoesNotFillWithoutAModel(t *testing.T) {
	m := model{}
	if m.profileEffortApplies(modelregistry.ReasoningEffortHigh) {
		t.Fatal("no model name: the profile must not fill an effort")
	}
	got, _ := m.handleProfileCommand("fast")
	if got.reasoningEffort != "" {
		t.Fatalf("effort = %q, must stay auto when no model is selected", got.reasoningEffort)
	}
}

// BUG 4 — and it is the SAME root cause as bug 1.
//
// availableReasoningEfforts() returns an empty ring for a model with no catalog
// entry (the reporter's glm-5.2). That one fact produced two visible failures:
// /effort listed no levels, AND the posture's fill was declined with "the model
// does not support that level".
//
// It is PRE-EXISTING: verified on origin/main, where the same model makes the
// CLI forward reasoning_effort:"high" while the TUI refuses /effort high. This
// asserts the three consumers of "does this model take this level?" now give
// the same answer.
func TestEffortSettabilityAgreesAcrossAllThreeConsumers(t *testing.T) {
	cases := []struct {
		model    string
		settable bool
		why      string
	}{
		{"claude-sonnet-4.5", true, "catalog model that lists high"},
		{"gpt-5", true, "inferred reasoning family"},
		{"gpt-4.1", false, "catalog model whose EMPTY ring is authoritative"},
		{"glm-5.2", true, "the reporter's model — no catalog entry, so no support claim can be made"},
		{"some-custom-endpoint-model", true, "any unlisted endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			build := func() model {
				return newModel(context.Background(), Options{
					ProviderName: "p", ModelName: tc.model, Provider: &fakeProvider{},
					ProviderProfile: config.ProviderProfile{Name: "p", Model: tc.model, APIKey: "k"},
					NewProvider:     func(config.ProviderProfile) (zeroruntime.Provider, error) { return &fakeProvider{}, nil },
				})
			}
			// Consumer 1: a manual /effort high.
			manual, manualOut := build().handleEffortCommand("high")
			gotManual := manual.reasoningEffort == modelregistry.ReasoningEffortHigh
			if gotManual != tc.settable {
				t.Fatalf("%s (%s): /effort high set = %v, want %v\n%s",
					tc.model, tc.why, gotManual, tc.settable, manualOut)
			}
			// Consumer 2: the posture's fill. Must agree with the manual set.
			posture, _ := build().handleEffortCommand(execprofile.Name)
			gotFill := posture.reasoningEffort == modelregistry.ReasoningEffortHigh
			if gotFill != gotManual {
				t.Fatalf("%s: the posture fill (%v) and a manual set (%v) disagree — "+
					"the same question answered two ways", tc.model, gotFill, gotManual)
			}
			// Consumer 3: the headless path's forwarding decision.
			registry, err := modelregistry.DefaultRegistry()
			if err != nil {
				t.Fatalf("DefaultRegistry: %v", err)
			}
			forwarded := forwardedEffortForTest(registry, tc.model, "high")
			if (forwarded != "") != tc.settable {
				t.Fatalf("%s: headless forwards %q but the TUI settable = %v — "+
					"the two surfaces disagree about the same model",
					tc.model, forwarded, tc.settable)
			}
		})
	}
}

// The OTHER authoritative-refusal arm: a catalog model that HAS a ring but does
// not list the requested level. gpt-4.1 cannot exercise this — its empty ring
// trips the earlier arm — so without this case that branch is unreachable and a
// mutation removing it goes undetected.
func TestManualEffortRefusesALevelOutsideAKnownRing(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "anthropic", ModelName: "claude-sonnet-4.5", Provider: &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
		NewProvider:     func(config.ProviderProfile) (zeroruntime.Provider, error) { return &fakeProvider{}, nil },
	})
	efforts, known := m.availableReasoningEffortsKnown()
	if !known || len(efforts) == 0 {
		t.Skip("fixture is no longer a catalog model with a non-empty ring")
	}
	if reasoningEffortAllowed(efforts, modelregistry.ReasoningEffortMinimal) {
		t.Skip("claude-sonnet-4.5 gained \"minimal\"; pick another out-of-ring level")
	}
	got, text := m.handleEffortCommand("minimal")
	if got.reasoningEffort != "" {
		t.Fatalf("a level outside an AUTHORITATIVE ring must be refused, got %q", got.reasoningEffort)
	}
	if !strings.Contains(text, "is not supported by") {
		t.Fatalf("the refusal must name the model:\n%s", text)
	}
}

// A known model must list its levels — the surface symptom the reporter saw.
// This drives the real /effort path rather than the helper, because a green
// helper test alongside an empty user surface is what happened three times in
// this feature.
func TestEffortListShowsLevelsForAKnownModel(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "anthropic", ModelName: "claude-sonnet-4.5", Provider: &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
		NewProvider:     func(config.ProviderProfile) (zeroruntime.Provider, error) { return &fakeProvider{}, nil },
	})
	_, text := m.handleEffortCommand("")
	for _, want := range []string{"low", "medium", "high"} {
		if !strings.Contains(text, want) {
			t.Fatalf("/effort must list %q for a catalog model:\n%s", want, text)
		}
	}
	if strings.Contains(text, "no reasoning controls") {
		t.Fatalf("a catalog reasoning model must not be reported as having none:\n%s", text)
	}
}

// An unlisted model is reported as UNLISTED, not as having no controls — those
// are different facts and rendered the same before.
func TestEffortListDistinguishesUnlistedFromUnsupported(t *testing.T) {
	render := func(name string) string {
		m := newModel(context.Background(), Options{
			ProviderName: "p", ModelName: name, Provider: &fakeProvider{},
			ProviderProfile: config.ProviderProfile{Name: "p", Model: name, APIKey: "k"},
			NewProvider:     func(config.ProviderProfile) (zeroruntime.Provider, error) { return &fakeProvider{}, nil },
		})
		_, text := m.handleEffortCommand("")
		return text
	}
	unlisted := render("glm-5.2")
	if !strings.Contains(unlisted, "not in Zero's catalog") {
		t.Fatalf("an unlisted model must be described as unlisted:\n%s", unlisted)
	}
	if strings.Contains(unlisted, "no reasoning controls on this model") {
		t.Fatalf("an unlisted model must not be reported as having no controls:\n%s", unlisted)
	}
	unsupported := render("gpt-4.1")
	if !strings.Contains(unsupported, "no reasoning controls on this model") {
		t.Fatalf("a catalog model with an authoritative empty ring must say so:\n%s", unsupported)
	}
}

// forwardedEffortForTest mirrors internal/cli's forwardedReasoningEffort rule:
// a known model coerces to its effective level (empty when it has none); an
// unknown model forwards the request as-is. Duplicated here rather than
// imported because internal/tui does not depend on internal/cli — and pinned
// against the real one by TestEffortSettabilityAgreesAcrossAllThreeConsumers
// failing if they ever diverge in outcome.
func forwardedEffortForTest(registry modelregistry.Registry, modelID, requested string) string {
	entry, ok := registry.Get(modelID)
	if !ok {
		return requested
	}
	effective := modelregistry.EffectiveReasoningEffort(entry, modelregistry.ReasoningEffort(requested))
	if effective == modelregistry.ReasoningEffortNone {
		return ""
	}
	return string(effective)
}

// gateModel builds a session holding a real shared gate.
func gateModel(t *testing.T, gate *specialist.PostureGate) model {
	t.Helper()
	return newModel(context.Background(), Options{
		ProviderName: "anthropic", ModelName: "claude-sonnet-4.5", Provider: &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
		ZeromaxingGate:  gate,
		NewProvider:     func(config.ProviderProfile) (zeroruntime.Provider, error) { return &fakeProvider{}, nil },
	})
}

// The gate is written on posture ON and OFF, asserted through the REAL handlers
// rather than a helper — a helper test is what missed the wiring four times in
// this feature.
func TestPostureTransitionsWriteTheSharedGate(t *testing.T) {
	for _, route := range []struct {
		name string
		on   func(model) (model, string)
		off  func(model) (model, string)
	}{
		{"/effort",
			func(m model) (model, string) { return m.handleEffortCommand(execprofile.Name) },
			func(m model) (model, string) { return m.handleEffortCommand("auto") }},
		{"/profile",
			func(m model) (model, string) { return m.handleProfileCommand(execprofile.Name) },
			func(m model) (model, string) { return m.handleProfileCommand("balanced") }},
	} {
		t.Run(route.name, func(t *testing.T) {
			gate := &specialist.PostureGate{}
			m := gateModel(t, gate)
			if gate.Active() {
				t.Fatal("a fresh session must leave the gate off")
			}
			m, _ = route.on(m)
			if !gate.Active() {
				t.Fatalf("%s <posture> must turn the gate ON", route.name)
			}
			m, _ = route.off(m)
			if gate.Active() {
				t.Fatalf("%s <off> must turn the gate OFF", route.name)
			}
			_ = m
		})
	}
}

// A REFUSED selection must not arm the gate — a disabled workspace must not end
// up with the tool live.
func TestRefusedSelectionDoesNotArmTheGate(t *testing.T) {
	gate := &specialist.PostureGate{}
	m := newModel(context.Background(), Options{
		ProviderName: "anthropic", ModelName: "claude-sonnet-4.5", Provider: &fakeProvider{},
		ProviderProfile:    config.ProviderProfile{Name: "anthropic", CatalogID: "anthropic", Model: "claude-sonnet-4.5", APIKey: "k"},
		ZeromaxingGate:     gate,
		ZeromaxingDisabled: true,
		NewProvider:        func(config.ProviderProfile) (zeroruntime.Provider, error) { return &fakeProvider{}, nil },
	})
	if _, text := m.handleEffortCommand(execprofile.Name); !strings.Contains(text, "Cannot use") {
		t.Fatalf("setup: the selection should have been refused:\n%s", text)
	}
	if gate.Active() {
		t.Fatal("a refused selection must leave the gate off")
	}
}

// A nil gate must not panic — a caller that never wires one simply has no tool.
func TestNilGateIsSafe(t *testing.T) {
	m := gateModel(t, nil)
	m, _ = m.handleEffortCommand(execprofile.Name)
	m, _ = m.handleEffortCommand("auto")
	_ = m
}

// THE cloneToolRegistry HAZARD, proved rather than assumed.
//
// The TUI registers the tool once and clones the registry per run; the clone
// copies tool POINTERS. If the gate were a value or a closure over the model,
// the clone's tool would read a stale posture. This asserts the tool reachable
// from a CLONE observes a flip written after the clone was taken.
func TestClonedRegistrySharesTheGatePointer(t *testing.T) {
	gate := &specialist.PostureGate{}
	registry := tools.NewRegistry()
	registry.Register(&specialist.OrchestrateTool{PostureActive: gate.Active})

	// Clone FIRST, flip the posture AFTER — the order that would break a
	// captured copy.
	clone := cloneToolRegistry(registry)
	raw, ok := clone.Get(specialist.OrchestrateToolName)
	if !ok {
		t.Fatal("the clone must carry the tool")
	}
	// Read Deferred through the same interface the partition uses, so this
	// exercises the real path rather than a concrete type assertion.
	deferred := func() bool {
		d, ok := raw.(interface{ Deferred() bool })
		if !ok {
			t.Fatal("the cloned tool must still implement Deferred")
		}
		return d.Deferred()
	}
	cloned := raw
	if !deferred() || cloned.Safety().Permission != tools.PermissionDeny {
		t.Fatal("before the flip the cloned tool must be off")
	}

	gate.Set(true)

	if deferred() {
		t.Fatal("the CLONED tool must observe a posture flip written after cloning")
	}
	if got := cloned.Safety().Permission; got != tools.PermissionAllow {
		t.Fatalf("cloned tool permission = %v, want Allow after the flip", got)
	}
	// ...and back off again.
	gate.Set(false)
	if !deferred() || cloned.Safety().Permission != tools.PermissionDeny {
		t.Fatal("the cloned tool must observe the posture being turned off too")
	}
}

// Concurrent write/read, for -race: the TUI writes the gate from its update
// loop while a run's tool dispatch reads it from the agent goroutine.
func TestGateIsSafeUnderConcurrentAccess(t *testing.T) {
	gate := &specialist.PostureGate{}
	tool := &specialist.OrchestrateTool{PostureActive: gate.Active}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			gate.Set(i%2 == 0)
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = tool.Deferred()
		_ = tool.Safety().Permission
	}
	<-done
}

// THE POSTURE'S SPEND BUDGET MUST REACH THE RUN, and leave when it does.
//
// Asserted on agentOptions — the struct a run is actually launched with —
// because a profile field nothing carries is the defect this codebase has hit
// repeatedly: a value that exists at one layer, is read at another, and is
// carried by neither.
func TestThePostureSpendBudgetReachesAndLeavesTheRun(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleEffortCommand(execprofile.Name)
	if got := m.agentOptions.MaxTokens; got != execprofile.Zeromaxing.MaxTokens {
		t.Fatalf("agentOptions.MaxTokens = %d, want the posture's %d", got, execprofile.Zeromaxing.MaxTokens)
	}
	reverted := m.revertExecProfile()
	if got := reverted.agentOptions.MaxTokens; got != 0 {
		t.Errorf("removing the posture left a spend budget of %d behind", got)
	}
}

// LEAVING THE POSTURE MID-RUN IS THE SAME MUTATION AS ENTERING IT, IN REVERSE.
// /effort zeromaxing is guarded by the idle-session rule; /effort auto reached
// revertExecProfile — moving the turn budget, self-correct and the shared
// orchestrate gate — with no guard at all, leaving one turn running under two
// different budgets.
func TestEffortAutoCannotLeaveThePostureMidRun(t *testing.T) {
	m := zeromaxingTestModel(t, false)
	m, _ = m.handleEffortCommand(execprofile.Name)
	if m.agentOptions.MaxTurns != 480 {
		t.Fatalf("setup: posture not applied, turns=%d", m.agentOptions.MaxTurns)
	}
	m.pending = true
	m, text := m.handleEffortCommand("auto")
	if m.execProfileName != execprofile.Name || m.agentOptions.MaxTurns != 480 {
		t.Fatalf("a pending turn must block the revert: profile=%q turns=%d",
			m.execProfileName, m.agentOptions.MaxTurns)
	}
	if !strings.Contains(text, "Finish or stop the current run") {
		t.Fatalf("the refusal must say why: %q", text)
	}
	// Idle again: the same command reverts normally.
	m.pending = false
	m, _ = m.handleEffortCommand("auto")
	if m.execProfileName == execprofile.Name {
		t.Fatal("an idle session must still be able to leave the posture")
	}
	// Plain /effort auto OUTSIDE the posture keeps working mid-run: it only
	// clears the effort selection, which mutates no shared budget.
	plain := zeromaxingTestModel(t, false)
	plain.pending = true
	plain, _ = plain.handleEffortCommand("auto")
	if plain.reasoningEffort != "" {
		t.Fatalf("plain auto mid-run must still clear the effort, got %q", plain.reasoningEffort)
	}
}
