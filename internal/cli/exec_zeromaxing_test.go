package cli

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/execprofile"
	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/specialist"
)

// (a) CLI: an explicit --reasoning-effort survives zeromaxing. The profile
// fills only what the caller left unset, exactly as fast/thorough do.
func TestZeromaxingDoesNotOverrideExplicitReasoningEffortCLI(t *testing.T) {
	options := execOptions{execProfile: execprofile.Name, reasoningEffort: "low"}
	profile, effortFilled, err := applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if !profile.IsZeromaxing() {
		t.Fatalf("profile = %q, want %q", profile.Name, execprofile.Name)
	}
	if options.reasoningEffort != "low" {
		t.Fatalf("reasoningEffort = %q, the explicit low must survive the posture's high", options.reasoningEffort)
	}
	if effortFilled {
		t.Fatal("must report it did NOT fill the effort, or a mid-run escalation would clear a hand-pinned value")
	}
}

// (b) CLI: --mode's fills survive zeromaxing. applyExecMode runs first, so its
// values read as "set" by the time the profile arrives — precedence is enforced
// by ordering, and this pins that the ordering still holds with a fourth profile.
func TestZeromaxingDoesNotOverrideModeFillsCLI(t *testing.T) {
	options := execOptions{mode: "fast", execProfile: execprofile.Name}
	if err := applyExecMode(&options); err != nil {
		t.Fatalf("applyExecMode: %v", err)
	}
	modeEffort := options.reasoningEffort
	modeTurns := options.maxTurns
	profile, _, err := applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if modeEffort != "" && options.reasoningEffort != modeEffort {
		t.Fatalf("reasoningEffort = %q, the mode's %q must win", options.reasoningEffort, modeEffort)
	}
	if modeTurns > 0 {
		// Mirror the real call site: a mode's --max-turns fill lands in
		// options.maxTurns AND flows through config overrides into
		// resolved.MaxTurns, so both arguments carry it. The profile must then
		// back off entirely — displaced 0, budget untouched.
		effective, displaced := applyProfileTurnBudget(profile, options.maxTurns, options.maxTurns)
		if effective != modeTurns || displaced != 0 {
			t.Fatalf("turn budget = (%d, displaced %d), the mode's %d must win with nothing displaced",
				effective, displaced, modeTurns)
		}
	}
}

// (c) CLI: fills only what is unset; an explicit --max-turns pins the budget so
// the profile backs off with nothing displaced.
func TestZeromaxingFillsOnlyUnsetCLI(t *testing.T) {
	options := execOptions{execProfile: execprofile.Name}
	profile, effortFilled, err := applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if options.reasoningEffort != "high" || !effortFilled {
		t.Fatalf("unset effort must be filled with high, got %q filled=%v", options.reasoningEffort, effortFilled)
	}
	if !options.selfCorrect {
		t.Fatal("must arm self-correction when it was unset")
	}
	if effective, displaced := applyProfileTurnBudget(profile, 0, 80); effective != 480 || displaced != 80 {
		t.Fatalf("over resolved 80 = (%d, %d), want (480, 80)", effective, displaced)
	}
	if effective, displaced := applyProfileTurnBudget(profile, 50, 50); effective != 50 || displaced != 0 {
		t.Fatalf("with explicit --max-turns 50 = (%d, %d), want (50, 0)", effective, displaced)
	}
}

// (g) CLI: --reasoning-effort zeromaxing and --exec-profile zeromaxing resolve
// to IDENTICAL state. One posture, one name, reachable from either flag.
func TestZeromaxingEffortFlagResolvesLikeProfileFlagCLI(t *testing.T) {
	viaEffort := execOptions{reasoningEffort: execprofile.Name}
	if err := normalizeZeromaxingEffort(&viaEffort); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	viaProfile := execOptions{execProfile: execprofile.Name}
	if err := normalizeZeromaxingEffort(&viaProfile); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !reflect.DeepEqual(viaEffort, viaProfile) {
		t.Fatalf("the two entry points diverged:\n--reasoning-effort: %+v\n--exec-profile:     %+v", viaEffort, viaProfile)
	}
	// ...and after profile expansion they are still identical.
	pe, fe, err := applyExecProfile(&viaEffort)
	if err != nil {
		t.Fatalf("applyExecProfile(effort path): %v", err)
	}
	pp, fp, err := applyExecProfile(&viaProfile)
	if err != nil {
		t.Fatalf("applyExecProfile(profile path): %v", err)
	}
	if pe != pp || fe != fp || !reflect.DeepEqual(viaEffort, viaProfile) {
		t.Fatalf("resolved state diverged after expansion:\n%+v %v\n%+v %v", pe, fe, pp, fp)
	}
	if viaEffort.reasoningEffort != "high" {
		t.Fatalf("both paths must resolve the effort to the profile's high, got %q", viaEffort.reasoningEffort)
	}
}

// A conflicting pair is a usage error, not a silent winner.
func TestZeromaxingEffortFlagConflictIsUsageError(t *testing.T) {
	options := execOptions{reasoningEffort: execprofile.Name, execProfile: "fast"}
	err := normalizeZeromaxingEffort(&options)
	if err == nil {
		t.Fatal("--reasoning-effort zeromaxing with --exec-profile fast must be a usage error")
	}
	if !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("the error must name the conflict: %v", err)
	}
	// The same profile on both flags is not a conflict.
	same := execOptions{reasoningEffort: execprofile.Name, execProfile: execprofile.Name}
	if err := normalizeZeromaxingEffort(&same); err != nil {
		t.Fatalf("naming the same posture twice must be accepted: %v", err)
	}
}

// (e) THE LOAD-BEARING GUARD. The posture name must never be forwarded to a
// provider as an effort value — not by any path, not for any model.
func TestZeromaxingIsNeverForwardedToAProvider(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	// Across a reasoning model, a non-reasoning model, and a model the catalog
	// has never heard of (where unknown values are otherwise passed through).
	for _, model := range []string{"claude-sonnet-4.5", "gpt-4.1", "some-custom-endpoint-model"} {
		for _, spelling := range []string{"zeromaxing", "ZEROMAXING", "  Zeromaxing  "} {
			if got := forwardedReasoningEffort(registry, model, spelling); got != "" {
				t.Fatalf("forwardedReasoningEffort(%q, %q) = %q, want \"\" — the posture name is not a provider value",
					model, spelling, got)
			}
		}
	}
	// The guard must not over-reach: real levels still forward.
	if got := forwardedReasoningEffort(registry, "claude-sonnet-4.5", "high"); got != "high" {
		t.Fatalf("a real level must still forward, got %q", got)
	}
}

// (f) THE RESERVATION. /effort max and --reasoning-effort max are UNCHANGED by
// this feature: "max" still parses as a raw provider level (ValidReasoningEffort
// accepts ReasoningEffortMax) and still resolves to nothing usable on today's
// models. The spelling stays free for a real provider rung.
func TestEffortMaxReservationUnchangedCLI(t *testing.T) {
	if !modelregistry.ValidReasoningEffort(modelregistry.ReasoningEffortMax) {
		t.Fatal("ReasoningEffortMax must remain a valid raw effort value — the reservation depends on it")
	}
	if _, ok := execprofile.Lookup("max"); ok {
		t.Fatal("\"max\" must NOT resolve to a profile — it is reserved for a provider rung")
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	// No curated model lists "max", so it coerces away rather than forwarding.
	if got := forwardedReasoningEffort(registry, "claude-sonnet-4.5", "max"); got == "max" {
		t.Fatal("no current model supports \"max\"; it must not be forwarded as-is")
	}
	// And it must NOT be swallowed by the zeromaxing normalizer.
	options := execOptions{reasoningEffort: "max"}
	if err := normalizeZeromaxingEffort(&options); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if options.reasoningEffort != "max" || options.execProfile != "" {
		t.Fatalf("\"max\" must pass through the normalizer untouched, got effort=%q profile=%q",
			options.reasoningEffort, options.execProfile)
	}
}

// (l) CLI leg of the honesty rule: a model that cannot take the effort is TOLD.
// reasoningEffortNotice is the existing coerce-and-tell helper, and it fires for
// a PROFILE-FILLED effort, not just an explicitly flagged one.
func TestZeromaxingUnsupportedEffortIsReportedCLI(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const nonReasoning = "gpt-4.1" // a catalog model with no reasoning capability
	notice := reasoningEffortNotice(registry, nonReasoning, "high")
	if notice == "" {
		t.Fatal("a non-reasoning model must produce a notice when an effort is requested")
	}
	if !strings.Contains(notice, "reasoning effort") {
		t.Fatalf("the notice must explain what was not applied: %q", notice)
	}
	if forwarded := forwardedReasoningEffort(registry, nonReasoning, "high"); forwarded != "" {
		t.Fatalf("forwarded effort = %q, want empty for a non-reasoning model", forwarded)
	}
	// ...and the rest of the posture still applies: the budget is unaffected.
	profile, _ := execprofile.Lookup(execprofile.Name)
	if effective, _ := applyProfileTurnBudget(profile, 0, 80); effective != 480 {
		t.Fatalf("the turn budget must still apply on an unsupported model, got %d", effective)
	}
}

// (m)+(n) CLI leg of the config gate.
func TestZeromaxingSelectionRefusalCLI(t *testing.T) {
	profile, _ := execprofile.Lookup(execprofile.Name)
	if refusal := execprofile.SelectionRefusal(profile, true); refusal == "" {
		t.Fatal("exec must refuse the posture when config disabled it")
	}
	if refusal := execprofile.SelectionRefusal(profile, false); refusal != "" {
		t.Fatalf("exec must allow it when config did not disable it, got %q", refusal)
	}
}

// (o) The raised budget PROPAGATES to spawned children. Deliberate — this is a
// maximal posture — so it is asserted explicitly rather than left to be
// discovered. applyProfileTurnBudget's effective value becomes resolved.MaxTurns,
// which is what the run exports as ZERO_MAX_TURNS for sub-agents.
func TestZeromaxingTurnBudgetPropagatesToChildren(t *testing.T) {
	profile, _ := execprofile.Lookup(execprofile.Name)
	effective, _ := applyProfileTurnBudget(profile, 0, 80)
	if effective != 480 {
		t.Fatalf("effective budget = %d, want 480", effective)
	}
	if effective > config.MaxTurnsCeiling {
		t.Fatalf("the budget %d exceeds the shared ceiling %d", effective, config.MaxTurnsCeiling)
	}
	// The delta text promises exactly this, so the promise and the number must
	// not drift apart.
	delta := execprofile.Delta(execprofile.DeltaState{CurrentMaxTurns: 80})
	if !strings.Contains(delta, "sub-agents") {
		t.Fatalf("the delta must tell the user the budget reaches sub-agents: %q", delta)
	}
}

// The headless posture mapping: selecting it enters; everything else is off.
func TestExecZeromaxingMapping(t *testing.T) {
	profile, _ := execprofile.Lookup(execprofile.Name)
	if got := execZeromaxing(profile); got != agent.ZeromaxingEntering {
		t.Fatalf("execZeromaxing(zeromaxing) = %v, want ZeromaxingEntering", got)
	}
	for _, name := range []string{"balanced", "fast", "thorough"} {
		other, _ := execprofile.Lookup(name)
		if got := execZeromaxing(other); got != agent.ZeromaxingOff {
			t.Fatalf("execZeromaxing(%s) = %v, want ZeromaxingOff", name, got)
		}
	}
	if got := execZeromaxing(execprofile.Profile{}); got != agent.ZeromaxingOff {
		t.Fatalf("execZeromaxing(zero) = %v, want ZeromaxingOff", got)
	}
}

// The flag PARSER must accept the posture name, and must still reject "max".
//
// This test exists because the unit tests above call normalizeZeromaxingEffort
// directly and never reach the parser — which rejected "zeromaxing" outright, so
// the whole entry point was dead at the user surface while every unit test
// passed. Driving the real binary is what found it; this is the regression.
func TestReasoningEffortFlagAcceptsZeromaxingButNotMax(t *testing.T) {
	accepted := func(t *testing.T, flag, value string) error {
		t.Helper()
		_, _, err := parseExecArgs([]string{flag, value, "-p", "x"})
		return err
	}
	if err := accepted(t, "--reasoning-effort", execprofile.Name); err != nil {
		t.Fatalf("--reasoning-effort %s must be accepted by the parser: %v", execprofile.Name, err)
	}
	if err := accepted(t, "--reasoning-effort", "ZEROMAXING"); err != nil {
		t.Fatalf("the parser must be case-insensitive: %v", err)
	}
	for _, level := range []string{"low", "medium", "high"} {
		if err := accepted(t, "--reasoning-effort", level); err != nil {
			t.Fatalf("--reasoning-effort %s must still be accepted: %v", level, err)
		}
	}
	// THE RESERVATION: "max" was rejected before this feature and must stay
	// rejected. Accepting it here would burn the spelling.
	err := accepted(t, "--reasoning-effort", "max")
	if err == nil {
		t.Fatal("--reasoning-effort max must still be rejected — the spelling is reserved")
	}
	if !strings.Contains(err.Error(), execprofile.Name) {
		t.Fatalf("the usage error should name the accepted values: %v", err)
	}
	// The spec-draft effort is a plain provider level; the posture has no
	// meaning for a draft, so it is NOT accepted there.
	if err := accepted(t, "--spec-reasoning-effort", execprofile.Name); err == nil {
		t.Fatalf("--spec-reasoning-effort %s must be rejected; it is a run posture, not a draft level", execprofile.Name)
	}
}

// The headless delta must describe the transition from the state the run was
// invoked in, captured BEFORE the profile arms self-correction as a side
// effect. Reading options.selfCorrect afterwards would always say "already on".
func TestExecSelfCorrectTransitionUsesPreProfileState(t *testing.T) {
	if got := execSelfCorrectTransition(false); got != execprofile.SelfCorrectRaised {
		t.Fatalf("an unflagged run = %v, want SelfCorrectRaised", got)
	}
	if got := execSelfCorrectTransition(true); got != execprofile.SelfCorrectAlreadyOn {
		t.Fatalf("an explicit --self-correct run = %v, want SelfCorrectAlreadyOn", got)
	}
	// And the rendered text differs, so the distinction reaches the user.
	raised := execprofile.Delta(execprofile.DeltaState{CurrentMaxTurns: 80, SelfCorrect: execSelfCorrectTransition(false)})
	already := execprofile.Delta(execprofile.DeltaState{CurrentMaxTurns: 80, SelfCorrect: execSelfCorrectTransition(true)})
	if raised == already {
		t.Fatalf("both states render identically:\n%s", raised)
	}
	if !strings.Contains(raised, "lsp → tests") {
		t.Fatalf("an unflagged run must be told what changes:\n%s", raised)
	}
}

// --spec-reasoning-effort zeromaxing stays rejected, but the message must
// explain WHY and name the way forward rather than reading like a bug.
func TestSpecReasoningEffortZeromaxingExplainsItself(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--spec-reasoning-effort", execprofile.Name, "-p", "x"})
	if err == nil {
		t.Fatalf("--spec-reasoning-effort %s must still be rejected", execprofile.Name)
	}
	message := err.Error()
	for _, want := range []string{
		"run posture",                            // why it does not apply
		"spec drafting",                          // where the user is
		"--spec-reasoning-effort high",           // the way forward for the draft
		"--reasoning-effort " + execprofile.Name, // ...and for the run
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the usage error must mention %q, got: %s", want, message)
		}
	}
	// A genuinely unknown value keeps the plain message — the explanation is
	// specific to the posture name, not a new blanket wording.
	_, _, err = parseExecArgs([]string{"--spec-reasoning-effort", "turbo", "-p", "x"})
	if err == nil || !strings.Contains(err.Error(), "Expected low, medium, or high.") {
		t.Fatalf("an unknown value must keep the plain message, got: %v", err)
	}
}

// The CAPTURE POINT, exercised through the real exec path.
//
// TestExecSelfCorrectTransitionUsesPreProfileState covers the helper; it cannot
// catch the capture being read at the wrong moment, because it never runs the
// code that does the capturing. applyExecProfile arms self-correction as a side
// effect, so reading options.selfCorrect after it would make every run report
// "unchanged (tests)" — the exact wording this change exists to remove.
func TestExecPrintsTheRaisedTransitionForAnUnflaggedRun(t *testing.T) {
	exitCode, _, stderr := runExecWithEcho(t, []string{
		"exec", "--exec-profile", execprofile.Name, "hello",
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	if !strings.Contains(stderr, "self-correct: lsp → tests") {
		t.Fatalf("an unflagged run starts LSP-only, so the delta must show the transition:\n%s", stderr)
	}
	if strings.Contains(stderr, "self-correct: unchanged") {
		t.Fatalf("the delta must not claim self-correction was already on:\n%s", stderr)
	}
}

// ...and the other direction: an explicit --self-correct run genuinely was
// already on, so it must say so.
func TestExecPrintsUnchangedWhenSelfCorrectWasAlreadyOn(t *testing.T) {
	exitCode, _, stderr := runExecWithEcho(t, []string{
		"exec", "--exec-profile", execprofile.Name, "--self-correct", "hello",
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	if !strings.Contains(stderr, "self-correct: unchanged (tests)") {
		t.Fatalf("an explicit --self-correct run must be told nothing changed:\n%s", stderr)
	}
	if strings.Contains(stderr, "lsp → tests") {
		t.Fatalf("must not claim a transition that did not happen:\n%s", stderr)
	}
}

// (3) The budget clause the USER sees must be caller-relative. Asserted through
// the real exec path: a unit test on budgetLine cannot catch the caller passing
// the wrong number into it, which is exactly what mutation found.
func TestExecDeltaShowsTheCallersOwnTurnBudget(t *testing.T) {
	exitCode, _, stderr := runExecWithEcho(t, []string{
		"exec", "--exec-profile", execprofile.Name, "hello",
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	// Assert the SHAPE, not a specific origin: the harness's resolved budget is
	// a fixture value, and pinning it here would test the fixture rather than
	// the behaviour. What matters is that the origin is the caller's own
	// resolved budget and the destination is the posture's.
	transition := regexp.MustCompile(`turn budget: (\d+) → 480`)
	match := transition.FindStringSubmatch(stderr)
	if match == nil {
		t.Fatalf("the delta must state a caller-relative budget transition:\n%s", stderr)
	}
	if match[1] == "480" {
		t.Fatalf("origin and destination are the same; the clause should read \"unchanged\":\n%s", stderr)
	}
	if strings.Contains(stderr, "160") {
		t.Fatalf("the delta must not compare against thorough's budget:\n%s", stderr)
	}
	if n := strings.Count(stderr, "reasoning effort:"); n != 1 {
		t.Fatalf("exactly one reasoning-effort statement expected, found %d:\n%s", n, stderr)
	}
}

// A partial plan must reach the PROCESS exit code, not just the tool result.
//
// Asserting the executor's status missed the tool's status; asserting the
// tool's status would miss the exit code the same way. This is the third link,
// asserted at the boundary that actually matters to a caller.
func TestPlanPartialMapsToIncompleteExit(t *testing.T) {
	recorder := &planSessionRecorder{}
	if _, incomplete := recorder.Incomplete(); incomplete {
		t.Fatal("a recorder that saw no plan must not mark the run incomplete")
	}
	plan, err := specialist.ParsePlan(map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100_000)},
	}, specialist.Limits{MaxTasks: 5})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	// A COMPLETED plan leaves the run alone.
	recorder.PlanCompleted(plan, specialist.PlanReport{Status: specialist.PlanCompleted, Succeeded: 1})
	if _, incomplete := recorder.Incomplete(); incomplete {
		t.Fatal("a completed plan must not mark the run incomplete")
	}
	// A PARTIAL one does, and names the counts.
	recorder.PlanCompleted(plan, specialist.PlanReport{Status: specialist.PlanPartial, Succeeded: 1, Skipped: 2})
	reason, incomplete := recorder.Incomplete()
	if !incomplete {
		t.Fatal("a partial plan must mark the run incomplete, which is exit 4")
	}
	for _, want := range []string{"partial", "1 succeeded", "2 skipped"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the reason must carry %q: %q", want, reason)
		}
	}
	// FAILED outranks partial: if any plan failed outright, that is the story.
	recorder.PlanCompleted(plan, specialist.PlanReport{Status: specialist.PlanFailed})
	if reason, _ := recorder.Incomplete(); !strings.Contains(reason, "failed") {
		t.Fatalf("a failed plan must outrank a partial one: %q", reason)
	}
}
