// Package execprofile defines named execution profiles: bundles of loop-posture
// knobs (turn budget, reasoning effort, self-correction, escalation triggers)
// that tune how hard the agent loop tries, independent of which model runs.
// Profiles COMPOSE with --mode: a mode answers "what runs" (model, tools),
// a profile answers "how hard the loop tries". Selection precedence is
// explicit flag > --mode > profile, enforced by the callers applying profiles
// last with the same fill-only-if-unset rule modes use.
//
// The catalog is deliberately tiny and value-stable: balanced is the empty
// profile (selecting it leaves the run's options, budget, and loop behavior
// identical to an unflagged run — asserted by test; only the opt-in trace and
// bench artifacts record the selected profile name, by design, so captures are
// self-describing), fast trades turn budget and effort for latency with a
// one-shot escalation back to the displaced posture as the safety net, and
// thorough raises the budget and arms full self-correction.
package execprofile

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sandbox"
)

// Profile is a named execution posture. Zero-valued fields inherit the run's
// existing value (flag, mode, config, or built-in default) — a profile only
// ever fills knobs the caller left unset, except MaxTurns which REPLACES the
// resolved budget (that displacement is what escalation restores).
type Profile struct {
	// Name identifies the profile in traces, help text, and diagnostics.
	Name string
	// MaxTurns replaces the resolved per-run tool-turn budget when > 0 and the
	// user did not pass an explicit --max-turns. The displaced resolved value
	// becomes the escalation target (see Policy).
	MaxTurns int
	// MaxTokens bounds what a run may SPEND, in provider-reported tokens. 0 is
	// unbounded, which is every profile that does not set it.
	//
	// A TURN COUNT IS NOT A COST BOUND. A measured heavy run reached the 320-turn
	// ceiling having spent 35,781,390 tokens; a lighter one reached it at a tenth
	// of that. Raising the turn budget without this would double the worst case
	// while still ending runs at an arbitrary place, so the two move together.
	MaxTokens int
	// ReasoningEffort fills the run's effort when the user left it unset. It
	// still flows through the model-supported gating at the call site, so an
	// unsupported level degrades exactly like an explicit flag would.
	ReasoningEffort string
	// SelfCorrect arms the post-edit verify-and-correct loop. Profiles can only
	// turn it ON (the flag is presence-only, so false is indistinguishable from
	// unset and must never override an explicit opt-in).
	SelfCorrect bool

	// Escalation triggers. All zero means the profile never escalates and
	// Policy returns nil (the loop stays byte-identical to a nil-profile run).
	// Targets are NOT part of the catalog: escalation restores the values the
	// profile displaced at selection time, never invented ones, so targets are
	// stamped by Policy from what the caller measured.
	EscalateOnToolFailureStreak   int
	EscalateOnCompletionUncertain int
	EscalateOnSelfCorrectFailure  bool
	EscalateOnRiskyMutation       sandbox.RiskLevel
}

// The catalog. Values marked provisional are floors/ceilings derived from the
// Phase 0 baseline's read-class evidence (successful nav runs used single-digit
// turns; the mutating classes produced no successful samples to tune from) and
// are expected to be re-tuned from the post-oracle-fix re-capture. The
// escalation safety net is what makes shipping provisional Fast values safe:
// a run that hits trouble gets its displaced budget back mid-run.
var (
	// Balanced is the default posture: empty on purpose. Selecting it changes
	// nothing — the no-regression invariant of the whole feature.
	Balanced = Profile{Name: "balanced"}

	// Fast starts cheap (30 turns, low effort) and arms every escalation
	// trigger so a struggling run restores its displaced posture: two
	// same-tool retriable failures in a row, a second uncertain completion
	// evaluation, a failing self-correction cycle, or a critical-risk
	// mutation. The risk threshold is deliberately Critical, not High: the
	// sandbox classifies EVERY shell command as at least high risk before any
	// command analysis, so a High trigger would fire on the first `go test`
	// of virtually any coding task and spend the one-shot on turn one.
	// Critical marks the genuinely scary categories (destructive commands,
	// piped installers, out-of-workspace writes, network mutations).
	Fast = Profile{
		Name:                          "fast",
		MaxTurns:                      30,
		ReasoningEffort:               "low",
		EscalateOnToolFailureStreak:   2,
		EscalateOnCompletionUncertain: 2,
		EscalateOnSelfCorrectFailure:  true,
		EscalateOnRiskyMutation:       sandbox.RiskCritical,
	}

	// Thorough doubles the default budget, asks for high effort, and arms the
	// full post-edit verify-and-correct loop. Already the maximum posture, so
	// it has nothing to escalate to.
	Thorough = Profile{
		Name:            "thorough",
		MaxTurns:        160,
		ReasoningEffort: "high",
		SelfCorrect:     true,
	}

	// Zeromaxing is the top posture: thorough's knobs with TRIPLE the turn
	// budget, plus the loop-posture reminders the agent injects while it is
	// active (see agent.Zeromaxing).
	//
	// It was double, and 320 turns was cutting real work short — a measured run
	// spent every one of them on legitimate sequential work and was forced into a
	// summary before it could report what it had found. The multiple stays exact
	// so the relationship remains something a reader can check.
	//
	// HONEST DELTA over thorough: the turn budget, and nothing else. Effort is
	// already at the ceiling and self-correction is already armed. Delta states
	// that to the user's face rather than leaving it in a commit message — a
	// posture whose advertised behaviour exceeds its real behaviour is worse
	// than no posture at all.
	//
	// ReasoningEffort is "high" and MUST STAY "high" — pinned by
	// TestZeromaxingReasoningEffortStaysHigh. "high" is the top rung every
	// provider actually maps: anthropic's thinkingBudgetForEffort and its gemini
	// twin fall through to their default arm (budget 0 => extended thinking
	// DISABLED) for anything above it, and the openai mapper drops the field
	// entirely. Raising this to "xhigh"/"max" would silently turn thinking OFF —
	// a downgrade wearing an upgrade's name. Raising the real ceiling means
	// teaching the providers those tiers first.
	Zeromaxing = Profile{
		Name: "zeromaxing",
		// 480, raised from 320 because 320 was cutting real work short: a measured
		// run spent all of it on legitimate sequential edit-verify work and was
		// forced into a summary before it could write up what it had found.
		//
		// NOT HIGHER, and the reason is the token curve rather than caution. In
		// that run the prompt grew monotonically from 35k to 186k tokens per turn
		// and had not flattened, so turns near the end cost five times what early
		// ones did. 640 would land around 360k per turn and push the run into
		// compaction — which is where "paste the actual terminal output" quietly
		// becomes a summary of the output, the exact failure the evidence contract
		// exists to prevent.
		MaxTurns: 480,
		// The bound that is meant to actually fire. Anchored on measurement, not
		// preference: the largest observed legitimate run spent 35.8M and needed
		// roughly 45M to finish, so this lets that work complete while stopping a
		// run that has stopped making progress — which at 480 turns and a growing
		// prompt would otherwise reach well over 100M.
		//
		// TWO RUNS IS A THIN BASIS. Revisit it once more heavy runs have been
		// measured; the mechanism is what matters here, and the number is the part
		// most likely to be wrong.
		MaxTokens:       50_000_000,
		ReasoningEffort: "high",
		SelfCorrect:     true,
	}
)

var catalog = map[string]Profile{
	Balanced.Name:   Balanced,
	Fast.Name:       Fast,
	Thorough.Name:   Thorough,
	Zeromaxing.Name: Zeromaxing,
}

// DeltaState is the caller's CURRENT state, from which Delta describes what
// selecting the posture will actually change for THEM.
//
// It exists because the honest answer is caller-specific in every clause. The
// first version of this text compared against THOROUGH — "reasoning effort:
// unchanged", "thorough uses 160" — which is information about two profiles,
// not about the user. Worse, it could contradict itself: the fixed effort
// clause claimed "unchanged" while a separate line underneath said the effort
// was NOT raised because the model refused it. Both cannot be true, and they
// were only ever consistent by coincidence because they came from two places.
//
// Delta now renders every clause from this one struct, so a contradiction is
// unrepresentable rather than merely unlikely.
type DeltaState struct {
	// CurrentMaxTurns is the turn budget in effect BEFORE the posture applies.
	// 0 means unknown, in which case the budget clause states the destination
	// without inventing an origin.
	CurrentMaxTurns int
	Effort          EffortTransition
	SelfCorrect     SelfCorrectTransition
}

// EffortTransition is what the posture does to reasoning effort for this
// caller. Exactly one applies, which is what makes the rendered clauses
// mutually exclusive by construction.
type EffortTransition int

const (
	// EffortRaised: the posture filled the effort with its level.
	EffortRaised EffortTransition = iota
	// EffortKeptExplicit: the caller had already chosen an effort, so the
	// posture backed off. Their choice stands, whatever it is.
	EffortKeptExplicit
	// EffortNotSupported: the active model is KNOWN not to accept the level, so
	// the effort is not raised. The rest of the posture still applies.
	EffortNotSupported
)

func (t EffortTransition) line(level string) string {
	switch t {
	case EffortKeptExplicit:
		return "reasoning effort: unchanged (your explicit choice stands)"
	case EffortNotSupported:
		return "reasoning effort: NOT raised to " + level + " — the active model does not accept that level; the rest of the posture still applies"
	default:
		return "reasoning effort: raised to " + level
	}
}

// SelfCorrectTransition is what selecting the posture does to post-edit
// verification, relative to the state the caller is ACTUALLY in.
//
// Telling a user sitting on the LSP-only default that self-correction is
// "already armed" while silently moving them to the full project test plan is
// documentation describing behaviour that is not happening.
type SelfCorrectTransition int

const (
	// SelfCorrectRaised: verification was LSP-only and the posture adds the
	// project test plan. The common case.
	SelfCorrectRaised SelfCorrectTransition = iota
	// SelfCorrectAlreadyOn: the deeper verification was already on, so the
	// posture genuinely changes nothing here.
	SelfCorrectAlreadyOn
	// SelfCorrectOverridden: an explicit /selfcorrect choice is holding it at
	// lsp despite the posture.
	SelfCorrectOverridden
)

// selfCorrectLine renders the transition using /selfcorrect's own vocabulary
// (lsp / tests), so it names states the user can actually type.
func (t SelfCorrectTransition) selfCorrectLine() string {
	switch t {
	case SelfCorrectAlreadyOn:
		return "self-correct: unchanged (tests)"
	case SelfCorrectOverridden:
		return "self-correct: lsp (your /selfcorrect choice overrides the posture)"
	default:
		return "self-correct: lsp → tests"
	}
}

// budgetLine states the turn-budget change from the caller's own budget, not
// from another profile's. A user on balanced/80 does not care what thorough
// uses; they care that their 80 becomes 320.
func budgetLine(current int) string {
	switch {
	case current <= 0:
		return "turn budget: " + strconv.Itoa(Zeromaxing.MaxTurns)
	case current == Zeromaxing.MaxTurns:
		return "turn budget: unchanged (" + strconv.Itoa(current) + ")"
	default:
		return "turn budget: " + strconv.Itoa(current) + " → " + strconv.Itoa(Zeromaxing.MaxTurns)
	}
}

// Delta is the user-facing statement of what selecting zeromaxing actually
// changes FOR THIS CALLER, shown by /effort, /profile, and the exec selection
// notice. It is deliberately concrete and deliberately admits what it does not
// move: a user paying for a higher posture is owed the real delta.
//
// The child-budget sentence is not a caveat, it is the point: this is a maximal
// posture, so the raised budget is exported to spawned sub-agents exactly as
// /turns does (asserted by TestZeromaxingTurnBudgetPropagatesToChildren).
func Delta(state DeltaState) string {
	return budgetLine(state.CurrentMaxTurns) + ", and that budget applies to spawned sub-agents too. " +
		state.Effort.line(Zeromaxing.ReasoningEffort) + ". " +
		state.SelfCorrect.selfCorrectLine() + "."
}

// Name is the single spelling of this posture, everywhere: /effort zeromaxing,
// /profile zeromaxing, --exec-profile zeromaxing, --reasoning-effort zeromaxing.
// One name, one table — no aliases, so there is no second spelling to keep in
// step with this one.
const Name = "zeromaxing"

// IsZeromaxing reports whether p is the zeromaxing posture. Callers use it
// instead of comparing name strings so the literal lives in exactly one place.
func (p Profile) IsZeromaxing() bool { return p.Name == Name }

// SelectionRefusal returns a non-empty, user-facing reason when the named
// profile may not be selected here, or "" when selection is allowed.
//
// It is the ONE authoritative selection rule, called by BOTH the headless exec
// path and the TUI /effort + /profile paths. Those paths already differ in how
// they apply a profile's knobs; letting each decide selection independently is
// exactly how a rule gets applied to one call path and silently omitted from
// its sibling. TestSelectionRefusalAgreesAcrossPaths pins that they agree.
//
// disabled comes from resolved config. A project .zero/config.json may set it
// (DISABLE) but can never clear it (ENABLE) — see mergeProjectConfig.
func SelectionRefusal(p Profile, disabled bool) string {
	if p.IsZeromaxing() && disabled {
		return "the zeromaxing posture is disabled for this workspace (profiles.disableZeromaxing in config)"
	}
	return ""
}

// Lookup resolves a profile by name, case-insensitively and ignoring
// surrounding whitespace.
func Lookup(name string) (Profile, bool) {
	profile, ok := catalog[strings.ToLower(strings.TrimSpace(name))]
	return profile, ok
}

// Names returns the catalog's profile names, sorted, for usage errors and help.
func Names() []string {
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Policy projects the profile into the loop-facing ProfilePolicy.
//
// displacedMaxTurns is the turn budget this profile displaced at selection time
// (what the run would have used without it) and becomes the escalation's
// restore target; pass 0 when the profile did not displace the budget (e.g.
// the user pinned --max-turns explicitly) so escalation leaves the ceiling
// untouched. effortFilled reports whether the profile actually filled the
// run's reasoning effort (it backs off when the user set one explicitly); the
// displaced effort is then "" (the provider default) by construction, which
// the escalation restores via RestoreDefaultEffort — a plain "" target would
// mean "leave untouched".
//
// Profiles with no armed triggers return nil: the loop treats a nil policy as
// "no profile" (no observation, no counters), which keeps balanced and
// thorough runs byte-identical to the same knob values set by hand.
func (p Profile) Policy(displacedMaxTurns int, effortFilled bool) *agent.ProfilePolicy {
	if p.EscalateOnToolFailureStreak == 0 && p.EscalateOnCompletionUncertain == 0 &&
		!p.EscalateOnSelfCorrectFailure && p.EscalateOnRiskyMutation == "" {
		return nil
	}
	return &agent.ProfilePolicy{
		Name: p.Name,
		Escalate: &agent.PostureEscalation{
			MaxTurns:              displacedMaxTurns,
			RestoreDefaultEffort:  effortFilled,
			OnToolFailureStreak:   p.EscalateOnToolFailureStreak,
			OnCompletionUncertain: p.EscalateOnCompletionUncertain,
			OnSelfCorrectFailure:  p.EscalateOnSelfCorrectFailure,
			OnRiskyMutation:       p.EscalateOnRiskyMutation,
		},
	}
}
