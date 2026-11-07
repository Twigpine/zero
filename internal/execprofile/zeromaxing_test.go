package execprofile

import (
	"strings"
	"testing"
)

// (d) THE TRAP PIN.
//
// Zeromaxing.ReasoningEffort must stay "high". Every level above it falls
// through the providers' effort→budget mappers into their DEFAULT arm, and
// those defaults mean "no extended thinking":
//
//	anthropic thinkingBudgetForEffort default -> 0   (thinking disabled)
//	gemini    thinkingBudgetForEffort default -> 0   (thinking disabled)
//	openai    openAIReasoningEffort    default -> "" (field omitted entirely)
//
// So "raising" this to xhigh/max would silently turn reasoning OFF while the UI
// claimed a higher posture — a downgrade wearing an upgrade's name, invisible
// at runtime. Raising the real ceiling means teaching the providers those tiers
// first; until then this test is what stands in the way.
func TestZeromaxingReasoningEffortStaysHigh(t *testing.T) {
	if Zeromaxing.ReasoningEffort != "high" {
		t.Fatalf("Zeromaxing.ReasoningEffort = %q, want \"high\" — see this test's comment: "+
			"any level above \"high\" hits the providers' default arm and DISABLES thinking",
			Zeromaxing.ReasoningEffort)
	}
	// Thorough is the rung below and shares the ceiling; if these ever diverge,
	// the honest-delta text is wrong.
	if Thorough.ReasoningEffort != Zeromaxing.ReasoningEffort {
		t.Fatalf("thorough effort %q != zeromaxing effort %q — Delta claims they are equal",
			Thorough.ReasoningEffort, Zeromaxing.ReasoningEffort)
	}
}

// One name, one table. No aliases means no second spelling to keep in step.
func TestZeromaxingHasExactlyOneSpelling(t *testing.T) {
	profile, ok := Lookup(Name)
	if !ok {
		t.Fatalf("%q is not in the catalog", Name)
	}
	if profile != Zeromaxing || !profile.IsZeromaxing() {
		t.Fatalf("Lookup(%q) = %+v, want %+v", Name, profile, Zeromaxing)
	}
	// Case/whitespace insensitivity, matching the other profiles.
	if p, ok := Lookup("  ZEROMAXING "); !ok || !p.IsZeromaxing() {
		t.Fatalf("Lookup is not case/space insensitive: %+v ok=%v", p, ok)
	}
	// The rejected spellings. Every one of these resolving would mean a second
	// name for one posture, which is the drift this design exists to avoid.
	for _, alias := range []string{"max", "deep", "deepmode", "zero-maxing", "zeromax", "zm"} {
		if _, ok := Lookup(alias); ok {
			t.Fatalf("%q must NOT resolve — the posture has exactly one name, %q", alias, Name)
		}
	}
	found := false
	for _, name := range Names() {
		if name == Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() omits %q, so usage text and hints will not offer it: %v", Name, Names())
	}
}

// The knobs, pinned. The turn budget is the ONLY mechanical delta over
// thorough — Delta says exactly that to the user, so if these drift apart the
// user-facing claim becomes false.
func TestZeromaxingKnobsMatchTheDeltaItAdvertises(t *testing.T) {
	if Zeromaxing.MaxTurns != 480 {
		t.Fatalf("Zeromaxing.MaxTurns = %d, want 480", Zeromaxing.MaxTurns)
	}
	if Zeromaxing.MaxTurns != Thorough.MaxTurns*3 {
		t.Fatalf("budget %d is not triple thorough's %d", Zeromaxing.MaxTurns, Thorough.MaxTurns)
	}
	if !Zeromaxing.SelfCorrect || !Thorough.SelfCorrect {
		t.Fatalf("Delta claims self-correction is already armed at thorough: zeromaxing=%v thorough=%v",
			Zeromaxing.SelfCorrect, Thorough.SelfCorrect)
	}
	// It arms no escalation triggers (it is the top rung — nothing to escalate
	// to), so Policy returns nil exactly like thorough and balanced.
	if policy := Zeromaxing.Policy(80, true); policy != nil {
		t.Fatalf("zeromaxing must arm no escalation policy, got %+v", policy)
	}
	// The budget clause states the CALLER's transition, not another profile's.
	got := Delta(DeltaState{CurrentMaxTurns: 80})
	for _, want := range []string{"turn budget: 80 → 480", "sub-agents"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Delta must state %q:\n%s", want, got)
		}
	}
	// ...and must NOT mention thorough's number, which is about two profiles
	// rather than about the user (bug 3).
	if strings.Contains(got, "160") {
		t.Fatalf("the delta must not compare against thorough's budget:\n%s", got)
	}
}

// (3) The budget clause is caller-relative in every case, including the two
// edges where a plain "X -> 480" would be wrong.
func TestDeltaBudgetLineIsCallerRelative(t *testing.T) {
	cases := []struct {
		name    string
		current int
		want    string
		absent  string
	}{
		{"balanced default", 80, "turn budget: 80 → 480", "160"},
		{"already at the posture budget", 480, "turn budget: unchanged (480)", "→"},
		{"unknown current budget", 0, "turn budget: 480", "→"},
		{"a pinned lower budget", 30, "turn budget: 30 → 480", "160"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Delta(DeltaState{CurrentMaxTurns: tc.current})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Delta(current=%d) must contain %q:\n%s", tc.current, tc.want, got)
			}
			clause := got[:strings.Index(got, ", and that budget")]
			if tc.absent != "" && strings.Contains(clause, tc.absent) {
				t.Fatalf("Delta(current=%d) budget clause must not contain %q: %q", tc.current, tc.absent, clause)
			}
		})
	}
}

// (2) THE CONTRADICTION. The effort clause has exactly one arm, so "unchanged"
// and "NOT raised" can never both appear. They did in real use, because the
// fixed clause lived in Delta and the refusal lived in a separate line.
func TestDeltaEffortClauseIsMutuallyExclusive(t *testing.T) {
	cases := []struct {
		name       string
		transition EffortTransition
		want       string
	}{
		{"raised", EffortRaised, "reasoning effort: raised to high"},
		{"kept explicit", EffortKeptExplicit, "reasoning effort: unchanged (your explicit choice stands)"},
		{"not supported", EffortNotSupported, "reasoning effort: NOT raised to high"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Delta(DeltaState{CurrentMaxTurns: 80, Effort: tc.transition})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Delta(%v) must contain %q:\n%s", tc.transition, tc.want, got)
			}
			// The contradiction, stated directly: never both.
			raised := strings.Contains(got, "NOT raised")
			unchanged := strings.Contains(got, "reasoning effort: unchanged")
			if raised && unchanged {
				t.Fatalf("Delta(%v) claims BOTH that the effort is unchanged and that it was not raised:\n%s",
					tc.transition, got)
			}
			if seen[got] {
				t.Fatalf("two effort transitions render identically:\n%s", got)
			}
			seen[got] = true
		})
	}
}

// The self-correct clause must describe the transition from the CALLER'S state,
// not from thorough. Telling a user sitting on LSP-only that self-correction is
// "already armed" while silently moving them to the project test plan is
// documentation describing behaviour that is not happening.
func TestDeltaSelfCorrectDescribesTheCallersTransition(t *testing.T) {
	cases := []struct {
		name       string
		transition SelfCorrectTransition
		want       string
		absent     string
	}{
		{"lsp-only user is told what changes", SelfCorrectRaised, "self-correct: lsp → tests", "unchanged"},
		{"already-on user is told nothing changes", SelfCorrectAlreadyOn, "self-correct: unchanged (tests)", "→"},
		{"overridden user is not promised a raise", SelfCorrectOverridden, "overrides the posture", "→"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Delta(DeltaState{CurrentMaxTurns: 80, SelfCorrect: tc.transition})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Delta(%v) must contain %q, got:\n%s", tc.transition, tc.want, got)
			}
			// Scoped to the self-correct CLAUSE: "unchanged" also appears in the
			// effort clause, where it is correct and caller-independent.
			clause := got[strings.Index(got, "self-correct:"):]
			if tc.absent != "" && strings.Contains(clause, tc.absent) {
				t.Fatalf("Delta(%v) self-correct clause must NOT contain %q, got:\n%s", tc.transition, tc.absent, clause)
			}
			// Every rendering keeps the caller-independent clauses.
			if !strings.Contains(got, "turn budget:") || !strings.Contains(got, "reasoning effort:") {
				t.Fatalf("Delta(%v) dropped a clause:\n%s", tc.transition, got)
			}
		})
	}
	// The three renderings must be distinguishable — a switch that fell through
	// to one arm would otherwise pass every containment check above.
	seen := map[string]bool{}
	for _, tr := range []SelfCorrectTransition{SelfCorrectRaised, SelfCorrectAlreadyOn, SelfCorrectOverridden} {
		out := Delta(DeltaState{CurrentMaxTurns: 80, SelfCorrect: tr})
		if seen[out] {
			t.Fatalf("two transitions render identically:\n%s", out)
		}
		seen[out] = true
	}
}

// (m)+(n) at the rule level: SelectionRefusal is the single authority both
// selection paths consult. Its callers are asserted separately (CLI and TUI).
func TestSelectionRefusalDisablesOnlyZeromaxingAndOnlyWhenDisabled(t *testing.T) {
	if refusal := SelectionRefusal(Zeromaxing, true); refusal == "" {
		t.Fatal("a disabled workspace must refuse zeromaxing")
	} else if !strings.Contains(refusal, "disableZeromaxing") {
		t.Fatalf("the refusal must name the setting so the user can act on it: %q", refusal)
	}
	if refusal := SelectionRefusal(Zeromaxing, false); refusal != "" {
		t.Fatalf("zeromaxing must be selectable when not disabled, got %q", refusal)
	}
	// The disable flag is posture-scoped: it must not silently take out the
	// other profiles, which have no cost-multiplier justification for gating.
	for _, other := range []Profile{Balanced, Fast, Thorough} {
		if refusal := SelectionRefusal(other, true); refusal != "" {
			t.Fatalf("disableZeromaxing must not refuse %q: %q", other.Name, refusal)
		}
	}
}

// THE SPEND BUDGET AND THE TURN BUDGET MOVE TOGETHER.
//
// Raising the turn ceiling without bounding spend doubles the worst case while
// still ending runs at an arbitrary place: the measured run that forced this
// change spent 35,781,390 tokens reaching 320 turns, and 480 turns on the same
// growing prompt would reach well over 100M. The number is anchored on that
// measurement — above what the largest legitimate run needed, below a runaway.
func TestZeromaxingBoundsSpendNotOnlyTurns(t *testing.T) {
	if Zeromaxing.MaxTokens <= 0 {
		t.Fatal("the posture raises the turn ceiling and bounds no spend at all")
	}
	if Zeromaxing.MaxTokens != 50_000_000 {
		t.Fatalf("Zeromaxing.MaxTokens = %d, want 50000000", Zeromaxing.MaxTokens)
	}
	// Every other profile leaves it unbounded, so nothing outside the posture
	// gains a ceiling nobody chose.
	for _, profile := range []Profile{Balanced, Thorough, Fast} {
		if profile.MaxTokens != 0 {
			t.Errorf("%s bounds spend at %d; only the posture should", profile.Name, profile.MaxTokens)
		}
	}
}
