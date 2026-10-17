package agent

import (
	"github.com/Twigpine/zero/internal/sandbox"
)

// profileController observes per-turn run signals and decides at most one
// posture escalation per run for the armed execution profile. A nil policy (or
// a policy without Escalate) makes every method a no-op, so runs without a
// profile are byte-identical to before — the same opt-in convention as Trace
// and SelfCorrect.
//
// The controller only ever CHANGES loop policy knobs (turn ceiling, reasoning
// effort, completion gate) through the values the loop applies at its act
// point; it never injects messages, never touches the model or session, and
// never interacts with the model-initiated escalate_model path.
type profileController struct {
	policy *PostureEscalation

	escalated     bool
	uncertainSeen int

	failureTripped bool
	riskTripped    bool
	scTripped      bool
	uncertainTrip  bool
}

// newProfileController is nil-safe: a nil ProfilePolicy (the default for every
// existing caller) or a nil Escalate yields a controller that observes and
// decides nothing.
func newProfileController(policy *ProfilePolicy) *profileController {
	if policy == nil {
		return &profileController{}
	}
	return &profileController{policy: policy.Escalate}
}

// observeToolOutcome watches two signals from an executed tool call: the
// repeated-failure guard's streak (reusing the guard's own counting, not a
// second failure model) and the sandbox risk classification of an executed
// result.
func (c *profileController) observeToolOutcome(outcome toolFailureOutcome, result ToolResult) {
	if c.policy == nil || c.escalated {
		return
	}
	if c.policy.OnToolFailureStreak > 0 && outcome.Count >= c.policy.OnToolFailureStreak {
		c.failureTripped = true
	}
	if threshold := riskRank(c.policy.OnRiskyMutation); threshold > 0 &&
		result.DenialReason == DenialNone &&
		riskRank(result.Risk.Level) >= threshold {
		// The call EXECUTED (was not denied): partial failures count too, since
		// the mutation ran. An unrecognized threshold ranks 0 and disables the
		// signal instead of matching everything.
		c.riskTripped = true
	}
}

// observeUncertain counts uncertain completion evaluations (continue nudges and
// semantic checks). Headless-only by construction: the completion gate never
// runs interactively.
func (c *profileController) observeUncertain() {
	if c.policy == nil || c.escalated || c.policy.OnCompletionUncertain <= 0 {
		return
	}
	c.uncertainSeen++
	if c.uncertainSeen >= c.policy.OnCompletionUncertain {
		c.uncertainTrip = true
	}
}

// observeSelfCorrect watches the post-edit verification outcome. Any failing
// outcome (correcting, reported, aborted) is a signal; passed and disabled are
// not.
func (c *profileController) observeSelfCorrect(outcome Outcome) {
	if c.policy == nil || c.escalated || !c.policy.OnSelfCorrectFailure {
		return
	}
	switch outcome {
	case OutcomeCorrecting, OutcomeReported, OutcomeAborted:
		c.scTripped = true
	}
}

// maybeEscalate reports the escalation target exactly once, the first time any
// armed trigger has fired. Subsequent calls return false for the rest of the
// run: escalation is one-shot and never de-escalates, so no cooldown state is
// needed.
func (c *profileController) maybeEscalate() (PostureEscalation, bool) {
	if c.policy == nil || c.escalated {
		return PostureEscalation{}, false
	}
	if !c.failureTripped && !c.riskTripped && !c.scTripped && !c.uncertainTrip {
		return PostureEscalation{}, false
	}
	c.escalated = true
	return *c.policy, true
}

// riskRank orders sandbox risk levels for threshold comparison. Unknown levels
// rank 0: an unrecognized RESULT level can never meet a valid threshold, and an
// unrecognized THRESHOLD is rejected before comparison (rank 0 disables the
// signal) so a typo in a profile can never make every result match.
func riskRank(level sandbox.RiskLevel) int {
	switch level {
	case sandbox.RiskLow:
		return 1
	case sandbox.RiskMedium:
		return 2
	case sandbox.RiskHigh:
		return 3
	case sandbox.RiskCritical:
		return 4
	default:
		return 0
	}
}
