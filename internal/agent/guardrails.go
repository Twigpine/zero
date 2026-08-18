package agent

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// Guardrail thresholds for the agent loop. These keep a runaway model from
// burning turns/tokens and nudge it toward keeping the plan current. They are
// deliberately conservative so trivial single-step tasks never trip them.
const (
	// maxEmptyTurns stops the run after this many consecutive turns that
	// produced no visible text AND no tool calls. A turn that produces either
	// resets the counter. Dropped-tool-call turns are handled by the existing
	// retry path and are not counted here.
	maxEmptyTurns = 3

	// staleToolCallThreshold injects a one-shot reminder once this many tool
	// calls have executed since the last update_plan call.
	staleToolCallThreshold = 10

	// stalePlanTurnThreshold injects the same one-shot reminder once this many
	// turns have passed since the last update_plan while plan items are still
	// pending — the turn-based complement to staleToolCallThreshold, catching a
	// plan that drifts stale across many turns that each make few tool calls.
	stalePlanTurnThreshold = 8

	// toolOnlyProgressReminderAt injects a one-shot progress nudge after this
	// many consecutive turns contain tool calls but no visible assistant text.
	// It does not stop the run; it tells the model to synthesize what it already
	// knows before spending more tool turns.
	toolOnlyProgressReminderAt = 6

	// planReminderTurn and planReminderToolThreshold keep bounded implementation
	// work plan-free while still nudging a genuinely sustained run that has not
	// established any structured progress. Both thresholds must be met.
	planReminderTurn          = 7
	planReminderToolThreshold = 7

	// planToolName is the planning tool the loop watches for by name.
	planToolName = "update_plan"

	// toolFailureHintAt injects a one-shot corrective hint (the tool's schema +
	// the exact error) after a tool fails this many times in a row with the same
	// error, so the model self-corrects instead of repeating the mistake.
	toolFailureHintAt = 2
	// toolFailureStopAt halts the run after a tool fails this many times in a row
	// with the same error, so NO model (weak or strong) burns turns looping on a
	// bad call. Set to 6 (not 4): a corrective hint fires at toolFailureHintAt (2),
	// and a model iterating on a genuinely tricky edit can legitimately fail a few
	// times after the hint while converging — stopping at 4 cut those runs short.
	// The streak still resets the moment the tool succeeds or hits a different
	// error, so this only affects true same-error loops.
	toolFailureStopAt = 6

	// toolFailureAnyErrorStopAt halts a tool that keeps failing with DIFFERENT
	// errors. The streak above cannot see that case by construction: a changed
	// signature rebuilds the record at 1, so a tool whose error text varies every
	// call never reaches toolFailureStopAt however long it loops.
	//
	// That is not hypothetical. A headless run made 384 denied calls over 26
	// minutes without tripping a halt set to 6, because each denial named a
	// different path. Keying denials on their category (see observeToolResult)
	// fixes that case; this bound covers every error with no category to key on.
	//
	// Deliberately well above toolFailureStopAt. A model iterating on a genuinely
	// tricky edit legitimately fails several times with different errors while it
	// converges, and this must not cut those runs short — the same reasoning that
	// moved toolFailureStopAt from 4 to 6. Both counters reset on success.
	toolFailureAnyErrorStopAt = 12

	// maxContinueNudges bounds how many times the headless completion gate
	// (Options.RequireCompletionSignal) re-prompts a model that stopped without a
	// tool call while work clearly remained. Once spent, the run finalizes as
	// INCOMPLETE rather than nudging forever (and it is still bounded by maxTurns
	// and the run deadline).
	maxContinueNudges = 3
)

// continueNudgeMarker is a stable substring for tests.
const continueNudgeMarker = "the task is not finished"

// continueNudge tells a model that stopped without a tool call — while work
// clearly remained — to keep going, or to mark the plan complete and summarize if
// it is genuinely done. The second path gives it a clean route to a legitimate
// completion (a finished plan + no continuation cue then exits as success).
func continueNudge(reason string) string {
	return "You stopped without calling a tool, but " + continueNudgeMarker + " (" + reason + "). " +
		"Do not stop here: take the next concrete action with a tool now. " +
		"If you are genuinely finished, first mark the plan complete with update_plan, then give your final summary."
}

// endsWithContinuationCue reports whether an assistant message ends mid-thought —
// the model announcing its OWN next action and then stopping on a colon
// ("…Let me check the SSH configuration:") rather than concluding. It requires
// BOTH a trailing colon AND an action lead-in on the last line, so genuine closers
// are NOT flagged: a forward-looking recommendation ("Next, I suggest reviewing
// the changes."), a sign-off ("Let me know if you need anything"), or a summary
// that merely ends in a colon ("Here is the summary:"). A bare trailing colon alone
// is too common in legitimate final answers to use as a signal.
func endsWithContinuationCue(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lines := strings.Split(trimmed, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			last = strings.ToLower(s)
			break
		}
	}
	if !strings.HasSuffix(last, ":") {
		return false
	}
	// Inspect the final clause (after the last sentence break) so a mid-line action
	// announcement is caught ("…configure the server. Let me check the config:")
	// while a plain summary colon ("Here is the summary:") or a recommendation is not.
	clause := last
	if idx := strings.LastIndex(last, ". "); idx >= 0 {
		clause = strings.TrimSpace(last[idx+2:])
	}
	if strings.HasPrefix(clause, "let me know") { // sign-off, not a mid-step action
		return false
	}
	for _, cue := range []string{
		"let me ", "let's ", "now i'll ", "now i will ", "now let me ", "let me now ",
		"i'll now ", "i will now ", "next i'll ", "next, i'll ", "first, i'll ", "first let me ",
	} {
		if strings.HasPrefix(clause, cue) {
			return true
		}
	}
	return false
}

// planStatusRemaining reports whether a raw update_plan status string represents
// unfinished work. Anything not clearly completed/failed (incl. empty/unknown,
// which the update_plan tool coerces to "pending") counts as remaining, matching
// that tool's own status normalization.
func planStatusRemaining(status string) bool {
	switch normalizeTaskPlanStatus(status) {
	case "completed", "failed":
		return false
	default:
		return true
	}
}

// acceptanceNudgeMarker is a stable substring for tests; it is embedded verbatim
// in acceptanceVerificationNudge.
const acceptanceNudgeMarker = "verify your work against the task's stated success criterion"

// selfReportPhrases are high-signal admissions of guessing/fabrication or stated
// uncertainty about the produced result. They are matched by plain substring with
// NO context guard, so every entry must be FIRST-PERSON or unambiguously about the
// model's own output — NOT a phrase that also describes implemented behavior.
// (Earlier versions included "fall back to" / "placeholder value" / "as a
// fallback" / bare "best guess" / "without proper", which match legitimate final
// answers like "the parser will fall back to UTF-8" or "I replaced the placeholder
// value", wrongly downgrading completed runs — so those are removed. Admissions of
// INABILITY are handled separately by inabilityStems, which carry a guard.)
var selfReportPhrases = []string{
	// first-person admissions of guessing / fabrication / assumption
	"i guessed", "my best guess", "i'm guessing", "this is a guess", "just a guess",
	"i made it up", "i fabricated", "i assumed a", "i had to assume",
	// first-person lack of capability (inability stems also cover cannot/could-not)
	"i do not have the ability", "i don't have the ability", "i lack the ability",
	"no way for me to", "not possible for me to",
	// stated uncertainty about the correctness of the produced result
	"may not be correct", "might not be correct", "may be incorrect", "might be incorrect",
	"this may not work", "this might not work", "not fully functional", "not fully working",
}

// inabilityStems are first-person "I cannot / can't / could not / am unable to /
// do not have" stems. Matching the STEM (not a fixed verb) generalizes over
// whatever action the model claims it could not perform — "analyze", "determine",
// "do", "complete", "verify", "see", etc. — so the detector is not defeated by
// re-phrasing (the chess case slipped a fixed "cannot analyze" list by writing
// "…which I cannot do without proper image analysis capabilities").
var inabilityStems = []string{
	"i cannot ", "i can't ", "i can not ", "i could not ", "i couldn't ",
	"i am unable to", "i'm unable to", "i was unable to", "i wasn't able to",
	"i was not able to", "i do not have", "i don't have",
	"we are unable to", "we were unable to",
	// THE SUBJECTLESS STEM IS KEPT, and the heading it fired on is handled where
	// the heading is, not by deleting the stem.
	//
	// It was removed once because a completed audit's own section heading —
	// "**Unable to verify (1):** - MCP #3 claim was truncated" — reads as an
	// admission. But it is the ONLY stem that catches an admission with no
	// first-person subject, and removing it lost every one of those:
	//
	//	"Unable to complete the task; the build never succeeded."
	//	"The agent was unable to finish the migration."
	//	"Unable to verify the fix, so the change is unverified."
	//
	// None name "i" or "we", so no other stem sees them. That traded one false
	// positive for three false negatives, in the direction this guard exists to
	// prevent. countedLabelSentence drops the heading shape instead.
	"unable to ",
	"without being able to",
}

// successNegationTails are negated phrasings that indicate SUCCESS, not an
// admission ("I could not find any remaining issues", "I cannot reproduce the
// bug"). When an inability stem is immediately followed by one of these, it is not
// treated as an admission, so a clean result is not misreported as INCOMPLETE.
var successNegationTails = []string{
	"find any", "found any", "find a ", "see any", "detect any", "identify any",
	"reproduce", "spot any", "locate any",
	// The OBSERVATION verbs. strongAbsence is only consulted after this list has
	// already matched, so the observation-family strong tails added a round
	// earlier were unreachable without these — seven of eight did nothing, and
	// only "reproduce" worked because it was already here.
	"trigger", "produce", "hit any", "encounter", "provoke", "surface", "measure",
	// A NEGATIVE SEARCH RESULT IS THE ANSWER, not a failure to produce one.
	//
	// The list above already encodes this — "I could not find any remaining
	// issues" is success — but only for the "any" phrasings. A finder reporting
	// "I could NOT find where AllowManifestToolAutoApproval is set to true in
	// production code" was marked INCOMPLETE after 53 tool calls and a 19,145
	// character audit, for doing precisely the job it was given: establishing
	// that something is not there.
	"find where", "find the", "find it", "find that", "find this",
	"found where", "found the",
	"locate where", "locate the", "locate it",
	"determine where", "identify where", "see where",
	"reproduce ", "confirm any", "observe any",
}

// narrativeMarkers flag a sentence as RETELLING a past exchange rather than
// reporting the outcome of the current objective, so an inability admission in
// that sentence is about THEN, not NOW. Grounded in a real false positive: a
// conversational recap ("You asked if I could work autonomously … I was honest
// that my sandbox had no repo … so I couldn't actually do it at the time") was
// downgraded to INCOMPLETE on the quoted "i couldn't " stem even though the
// current turn's objective (summarize where we left off) was fully met. Every
// marker is second-person or explicitly past-referential, so a first-person
// admission about the current task ("I couldn't complete the refactor") is
// never masked.
var narrativeMarkers = []string{
	"you asked", "you said", "you wanted", "you mentioned",
	"we discussed", "we talked", "we covered",
	"when we last", "last time", "last session", "previous session",
	"previous conversation", "earlier session", "earlier conversation",
	"at the time", "back then",
}

// stripQuoted removes spans enclosed in double quotes (straight or curly) or
// backticks, so an admission the model merely QUOTES — its own earlier message,
// a log line, an error string — cannot fire the detector. Only BALANCED spans
// are removed: an opening delimiter that never closes is kept as literal text,
// so a stray quote cannot swallow the rest of the message (and with it a
// genuine admission the detector must see). Single quotes are left alone: they
// are overwhelmingly apostrophes ("couldn't"), and treating them as quote
// delimiters would swallow the text between two contractions.
func stripQuoted(s string) string {
	var b strings.Builder
	var span strings.Builder // pending text since the open delimiter, kept if it never closes
	open := rune(0)
	for _, r := range s {
		switch {
		case open != 0:
			if (open == '"' && r == '"') || (open == '“' && r == '”') || (open == '`' && r == '`') {
				open = 0
				span.Reset()
				continue
			}
			span.WriteRune(r)
		case r == '"' || r == '“' || r == '`':
			open = r
			span.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(span.String()) // dangling delimiter: restore the span verbatim
	return b.String()
}

// admissionSentences splits lowered text into sentence-ish fragments so the
// detector judges each claim in its own context. Newlines split too (markdown
// bullets are separate claims); the exact boundaries only need to keep an
// admission next to its own narrative/negation context, not be grammatical.
func admissionSentences(lower string) []string {
	return strings.FieldsFunc(lower, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})
}

// selfReportedIncompletion returns a short reason when the model's final text
// admits it guessed or could not meet the objective, else "". Case-insensitive.
// Matching is per sentence, after dropping quoted spans, and a sentence that
// retells a past exchange (narrativeMarkers) is skipped entirely — an admission
// must be the model's own report about the CURRENT objective, not general
// language that merely resembles one.
// toolGrantMarkers flag a sentence as reporting WHICH TOOLS this run was given,
// not whether the work was done.
//
// A read-only plan task is SUPPOSED to say this. One wrote "I don't have an
// update_plan tool available in this specialist context (only read-only
// exploration tools were provided)" and then delivered the complete answer —
// helper name, file, line 214, full source — and was marked INCOMPLETE on the
// "i don't have" stem. The prompt asks tasks to name their limits plainly; a
// detector that punishes exactly that teaches the opposite.
//
// NARROW ON PURPOSE: the sentence must name a TOOL or a GRANT. "I do not have
// enough evidence" is still an admission and still fires.
// NARROWED AFTER AN AUDIT OF THIS VERY FIX. The first version listed a bare
// " tool", which exempts any inability sentence that merely mentions one —
// measured at 5/5 on ordinary phrasings:
//
//	"I cannot run the build tool, so the change is unverified"
//	"I could not use the migration tool and the data is untouched"
//	"I was unable to invoke the formatting tool on the output"
//
// Those are genuine admissions, and silently exempting them is the WORSE
// direction: a false positive costs a re-run, a false negative reports
// unfinished work as done. The markers now have to be about what the run WAS
// GIVEN, not about a tool being mentioned at all.
var toolGrantMarkers = []string{
	"tool available", "tools available", "no such tool", "not available in this",
	"tool is available", "tools are available", "tool was available", "tools were available",
	"tool is not available", "tools are not available",
	"tool isn't available", "tools aren't available",
	"read-only tools", "read only tools", "only read-only", "only read only",
	"tools were provided", "tools were given", "toolset provided",
	"in this specialist context", "in this context only",
	"is not in my toolset", "not in my toolset", "not in this toolset",
}

// objectiveFailureMarkers name the OBJECTIVE rather than a capability. A
// sentence carrying one is about whether the job got done, so the tool-grant
// exemption above does not apply to it however many tools it mentions.
// VERB-ANCHORED, not bare nouns. "this task" alone was too crude: a task that
// finished wrote "so i could not record a plan; the task is a single
// read-and-report step and is now complete" — it names the task in order to
// report SUCCESS, and a bare-noun override read that as failure. The marker has
// to be the objective NOT BEING DONE, which needs the verb.
var objectiveFailureMarkers = []string{
	"complete this task", "complete the task", "completing this task", "completing the task",
	"finish this task", "finish the task", "finishing this task",
	"complete it", "completing it", "finish it", "finishing it",
	// The objective and the assignment need the verb too, for the same reason
	// "this task" did. "the objective is met" and "the assignment is complete"
	// name the objective in order to report SUCCESS, and a bare noun read both
	// as failure — so a finished answer carrying a tool caveat was told it had
	// not finished, which is the worst thing this detector can do.
	"complete the objective", "completing the objective",
	"finish the objective", "finishing the objective",
	"meet the objective", "meeting the objective", "achieve the objective",
	"complete the assignment", "completing the assignment",
	"finish the assignment", "finishing the assignment",
	"as requested", "what was asked",
	"do this task", "perform this task", "carry out this task",
}

// blockedWorkMarkers are what turns an absence-establishing sentence back into
// an admission of failure.
//
// MEASURED, NOT ARGUED. The successNegationTails list above exists so a finder
// reporting "I could not find where X is set" is not marked incomplete for doing
// its job. But the stems it added — "find the", "reproduce ", "confirm any",
// "observe any" — also head the most ordinary way of admitting defeat, and the
// allowance fired on the whole sentence regardless of how it ended. Measured
// against eleven genuine admissions, TEN passed the detector undetected:
//
//	"I could not reproduce the crash, so the fix is unverified."
//	"I could not find the root cause; someone else will need to pick this up."
//	"I could not locate the source of the regression and have run out of ideas."
//
// That is the guard's entire purpose defeated — it is the last thing between a
// stalled run and a report that reads like success.
//
// So the allowance now yields when the sentence ALSO says the work is blocked.
// "I could not find where X is set in production code" still passes, because
// nothing in it claims the objective was left undone; the three above do not.
// strongAbsenceTails are the allowance tails carrying an explicit "any": the
// model asserting it looked and found NOTHING. That is a finding whatever the
// rest of the sentence says, so blockedWorkMarkers does not override them.
var strongAbsenceTails = []string{
	"find any", "found any", "see any", "detect any", "identify any",
	"spot any", "locate any", "confirm any", "observe any",
	// The OBSERVATION family. Looking for a failure and not producing one is the
	// same kind of result as looking for an issue and not finding one, and
	// leaving these out meant "I could not reproduce any failure in the parser"
	// was not a strong absence — so an unrelated blocked statement in the next
	// sentence flipped a clean negative result into an admission.
	"reproduce any", "trigger any", "produce any", "hit any",
	"encounter any", "provoke any", "surface any", "measure any",
}

// EVERY STRONG TAIL MUST BE REACHABLE. strongAbsence is consulted only after
// hasAnyPrefix(tail, successNegationTails) has already matched, so a strong tail
// whose verb is missing from that list is never asked about. Seven of the eight
// observation verbs added a round earlier were dead on arrival that way — only
// "reproduce" was already a success-negation stem, which is why the corpus that
// was supposed to cover them passed.

// strongAbsenceObjects are the things whose ABSENCE IS THE RESULT: you go
// looking for them precisely so you can report there are none, and finding none
// is the work succeeding.
//
// WHAT FOLLOWS "any" DECIDES, and treating every "find any" as success was too
// broad:
//
//	"I could not find any remaining issues"  -> a finding, the search succeeded
//	"I could not find any solution"          -> an admission, the work did not
//
// Both carry the explicit "any". Only the object separates them, so only the
// object can classify them.
//
// AN ALLOW-LIST, because this grants the exemption. A deny-list of deliverables
// would have to anticipate every noun a model might reach for, and everything
// forgotten would be waved through as success — the failure direction this
// detector exists to prevent. An unrecognised object is simply not strong, which
// leaves the sentence to the ordinary blocked-work handling rather than
// flagging it outright.
var strongAbsenceObjects = []string{
	"issue", "issues", "problem", "problems", "bug", "bugs", "defect", "defects",
	"error", "errors", "failure", "failures", "regression", "regressions",
	"evidence", "example", "examples", "occurrence", "occurrences",
	"instance", "instances", "reference", "references", "match", "matches",
	"caller", "callers", "usage", "usages", "use", "uses", "case", "cases",
	"vulnerability", "vulnerabilities", "leak", "leaks", "race", "races",
	"sign", "signs", "trace", "traces", "mention", "mentions", "difference", "differences",
	"blocker", "blockers", "gap", "gaps", "omission", "omissions", "discrepancy", "discrepancies",
	"conflict", "conflicts", "violation", "violations", "warning", "warnings",
	"call", "calls", "site", "sites", "callsite", "callsites", "consumer", "consumers",
	"dependency", "dependencies", "user", "users", "path", "paths",
}

// absenceQualifiers sit between "any" and the object without changing it.
var absenceQualifiers = []string{
	"remaining", "other", "further", "more", "additional", "obvious", "such",
	"outstanding", "new", "existing", "actual", "real", "clear", "direct",
}

// strongAbsence reports whether tail is an "any"-family absence whose OBJECT
// makes finding nothing the result rather than the shortfall.
func strongAbsence(tail string) bool {
	for _, prefix := range strongAbsenceTails {
		if !strings.HasPrefix(tail, prefix) {
			continue
		}
		rest := strings.TrimSpace(tail[len(prefix):])
		for trimmed := true; trimmed; {
			trimmed = false
			for _, qualifier := range absenceQualifiers {
				if word, remainder, ok := cutFirstWord(rest); ok && word == qualifier {
					rest, trimmed = remainder, true
					break
				}
			}
		}
		word, _, ok := cutFirstWord(rest)
		if !ok {
			continue
		}
		for _, object := range strongAbsenceObjects {
			if word == object {
				return true
			}
		}
	}
	return false
}

// cutFirstWord returns the first bare word of text, lowercased by the caller's
// own normalisation, with surrounding punctuation removed.
func cutFirstWord(text string) (word string, rest string, ok bool) {
	text = strings.TrimLeft(text, " \t")
	end := 0
	for end < len(text) {
		c := text[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return "", text, false
	}
	return text[:end], text[end:], true
}

var blockedWorkMarkers = []string{
	"unverified", "not verified", "cannot verify", "could not verify",
	"someone else", "will need to", "needs someone", "handed off", "hand off",
	// "someone else" and "will need to" are the two that also appear in
	// SUCCESSFUL reports, describing somebody else's future work — "I could not
	// find any remaining issues, though a follow-up will need to cover the
	// Windows path" is a finding. They are kept because they do carry a genuine
	// handoff-because-blocked ("someone else will need to pick this up"), and the
	// strongAbsenceTails exemption above is what stops them flipping a finding.
	"ran out of", "run out of", "out of time", "in the time available",
	"stopped there", "stopping here", "gave up", "giving up",
	"nothing was modified", "no changes were made", "left unchanged", "left undone",
	"may be inert", "is unresolved", "remains unresolved", "still broken",
	"so i cannot", "so i could not",
	// Saying the work is BLOCKED, in as many words. "I could not find the root
	// cause, so the work is blocked" carries the statement in the same sentence
	// and still passed, because every marker above names a symptom of being
	// blocked and none named the thing itself.
	"is blocked", "are blocked", "remains blocked", "stays blocked", "still blocked",
	"cannot proceed", "could not proceed", "can not proceed", "unable to proceed",
	"is unfinished", "remains unfinished", "left unfinished", "still unfinished",
	"is incomplete", "remains incomplete",
	"still unresolved", "still unverified",
}

// bareInabilityStems are the two entries above that are STEMS rather than
// descriptions of a blocked state. The stem loop below already scans them with
// its own tail logic, and a sentence can carry one on its way to reporting
// success: "so i could not record a plan; the task is a single read-and-report
// step and is now complete" is a finished task that did not need the plan.
var bareInabilityStems = []string{"so i cannot", "so i could not"}

// unambiguousFailureStates say the work is in a bad state, with no reading on
// which it is a result.
//
// AN OBJECT CANNOT OUTRANK AN EXPLICIT STATE. strongAbsence returns true for
// "evidence", and that suppressed every blocked-work marker in the sentence — so
// "I could not find any evidence supporting the fix, so it remains unverified"
// reported success while saying in as many words that the work is unverified.
// The absence protection exists for ownership and follow-up wording, where "I
// could not find any remaining issues, though a follow-up will need to cover the
// Windows path" really is a finding. It was never meant to cover a sentence that
// states the outcome.
//
// So this is the SHORT list: "unverified" and "still broken" have one reading,
// while "someone else", "will need to" and "nothing was modified" have two and
// stay ambiguous. Same-sentence only — a state in the NEXT sentence may belong
// to another subject, which is what the lookahead's topic-shift guard is for.
var unambiguousFailureStates = []string{
	"unverified", "not verified",
	"is unresolved", "remains unresolved",
	"still broken",
	"is blocked", "are blocked", "remains blocked", "stays blocked", "still blocked",
	"is unfinished", "remains unfinished", "left unfinished", "still unfinished",
	"still unresolved", "still unverified",
	"is incomplete", "remains incomplete",
	"cannot proceed", "could not proceed", "unable to proceed",
	"gave up", "giving up", "ran out of", "run out of",
	"left undone",
}

// consequenceBoundaries separate what was looked for from what followed.
//
// THE STATE HAS TO BE THE OUTCOME, NOT PART OF WHAT WAS NEGATED. Matching
// "is unresolved" anywhere in the sentence read the negated PROPOSITION as the
// reported result:
//
//	"I could not find any evidence that the issue is unresolved."
//
// That is a successful negative finding — there is no evidence the issue remains
// unresolved — and it was marked incomplete, which is the opposite polarity of
// the case the override was added for. What separates them is position: after a
// consequence boundary the state is being asserted, inside a "that…" clause it is
// the thing being denied.
var consequenceBoundaries = []string{", so ", "; ", ", but ", ", therefore", ", leaving ", ", which "}

// reportedConsequence returns the part of a sentence that states the OUTCOME,
// or "" when the sentence never turns to one. Everything before the first
// boundary is what was searched for.
func reportedConsequence(sentence string) string {
	earliest := -1
	width := 0
	for _, boundary := range consequenceBoundaries {
		if index := strings.Index(sentence, boundary); index >= 0 && (earliest < 0 || index < earliest) {
			earliest, width = index, len(boundary)
		}
	}
	if earliest < 0 {
		return ""
	}
	return sentence[earliest+width:]
}

// blockedStateMarkers describe WORK LEFT BLOCKED — unverified, unresolved,
// handed off, abandoned for time. Only these break the tool-grant exemption:
// they say something about the state the work is in, which a tool caveat cannot
// excuse, whereas a bare stem says only that one step did not happen.
//
// DERIVED, not copied. Two hand-maintained lists that must stay in step drift,
// and the drift here would be silent in both directions.
var blockedStateMarkers = func() []string {
	bare := make(map[string]bool, len(bareInabilityStems))
	for _, stem := range bareInabilityStems {
		bare[stem] = true
	}
	out := make([]string, 0, len(blockedWorkMarkers))
	for _, marker := range blockedWorkMarkers {
		if !bare[marker] {
			out = append(out, marker)
		}
	}
	return out
}()

// carriesTheConsequence reports whether the sentence after an allowance should
// be read as that allowance's consequence.
//
// The lookahead exists because the consequence usually IS the next sentence, but
// "usually" is not "always" — a message can turn to something else, and reading
// a blocked statement about a different subject as this one's consequence is the
// cost of the lookahead. A sentence that announces the change of subject, or
// disclaims the thing as out of scope, is taken at its word.
//
// This does not catch every unrelated follow-on, and deliberately errs toward
// reading the next sentence: an admission reported as success is the failure this
// guard exists to prevent, and a message that says something is unverified has
// said it whether or not it is the same something.
func carriesTheConsequence(next string) bool {
	return !containsAny(next, topicShiftMarkers)
}

// topicShiftMarkers say the message has moved on to something else.
var topicShiftMarkers = []string{
	"separately", "unrelatedly", "unrelated", "as an aside", "aside from",
	"out of scope", "outside the scope", "not in scope", "for a different",
	"in a different", "on another", "elsewhere in", "in other news",
}

// countedLabelSentence reports whether a sentence is a markdown LABEL that
// introduces and counts a bucket of findings, rather than a claim about the
// objective.
//
// "**Unable to verify (1):** - MCP #3 claim was truncated" is a section heading
// in a COMPLETED audit. Read as a sentence it is indistinguishable from an
// admission, which is why the subjectless stem was once deleted to silence it.
//
// NARROW ON PURPOSE: the sentence must BEGIN with the inability phrase, after
// markdown emphasis, AND carry a parenthesised count. "Unable to complete the
// task; the build never succeeded" begins the same way and has no count, so it
// still fires — which is the whole reason this is preferable to dropping the
// stem.
func countedLabelSentence(sentence string) bool {
	trimmed := strings.TrimLeft(strings.TrimSpace(sentence), "-*#> \t")
	if !strings.HasPrefix(trimmed, "unable to ") {
		return false
	}
	return countedLabelSuffix.MatchString(sentence)
}

var countedLabelSuffix = regexp.MustCompile(`\(\s*\d+\s*\)`)

func selfReportedIncompletion(text string) string {
	sentences := admissionSentences(strings.ToLower(stripQuoted(text)))
	for index, sentence := range sentences {
		// THE CONSEQUENCE IS OFTEN THE NEXT SENTENCE. The blocked-work override
		// only ever saw the sentence the allowance fired in, so the same
		// admission escaped or was caught purely on its punctuation:
		//
		//	"I could not reproduce the crash, so the fix is unverified."  caught
		//	"I could not reproduce the crash. The fix is unverified."     missed
		//
		// A full stop is not a claim that the work finished, and writing the
		// consequence as its own sentence is how most people write. The
		// blocked-work question is asked of this sentence AND the one after it;
		// everything else is still decided on the sentence alone, so a stem in
		// one sentence cannot be paired with an allowance tail in another.
		blockedContext := sentence
		if index+1 < len(sentences) && carriesTheConsequence(sentences[index+1]) {
			blockedContext += " " + sentences[index+1]
		}
		if containsAny(sentence, narrativeMarkers) {
			continue
		}
		// A counted label is a heading, not a claim about the objective.
		if countedLabelSentence(sentence) {
			continue
		}
		// A sentence about the tool grant is about CAPABILITY, not about the
		// objective — UNLESS it also says the task itself could not be done.
		//
		// THE OVERRIDE IS NOT OPTIONAL. Without it the exemption swallowed a
		// genuine failure: "I am unable to complete this task with the current
		// tool set … Only write_file is enabled … so I cannot inspect the
		// codebase." That task really did fail, and it mentions tools, so a bare
		// tool-marker check waved it through. Naming the task is what separates
		// "I lack a tool I did not need" from "I lack the tools this needed".
		// BLOCKED WORK BREAKS THE EXEMPTION TOO, not just a named objective. The
		// override above asks whether the sentence names the task, which a whole
		// class of real admissions never does:
		//
		//	"No write tool is available in this context, so the fix is unverified."
		//	"There is no edit tool available here, so the change remains unapplied."
		//	"Write tools are not available in this setup, so the tests were never run."
		//
		// Every one mentions tools, none names the objective, and all four were
		// waved through. The tool caveat is the REASON the work is blocked, not a
		// reason to stop reading — so a sentence that also says something is
		// unverified, unapplied or untested goes on to the blocked-work handling
		// below instead of being exempted here.
		if containsAny(sentence, toolGrantMarkers) &&
			!containsAny(sentence, objectiveFailureMarkers) &&
			!containsAny(sentence, blockedStateMarkers) {
			continue
		}
		for _, phrase := range selfReportPhrases {
			if strings.Contains(sentence, phrase) {
				return selfReportReason(phrase)
			}
		}
		for _, stem := range inabilityStems {
			// Scan EVERY occurrence of the stem, not just the first: an earlier
			// success-negation use ("I could not find any examples, so I could not
			// implement it") must not mask a later genuine admission with the same stem.
			for start := 0; ; {
				rel := strings.Index(sentence[start:], stem)
				if rel < 0 {
					break
				}
				abs := start + rel
				tail := strings.TrimSpace(sentence[abs+len(stem):])
				// The allowance yields to a sentence that also says the work is
				// blocked: "could not reproduce the crash" is a finding, "could not
				// reproduce the crash, so the fix is unverified" is an admission,
				// and the tail prefix alone cannot tell them apart.
				strong := strongAbsence(tail)
				// The object decides whether finding nothing is a result; an
				// explicit failure state in the SAME sentence decides that it is
				// not, whatever the object.
				if strong && containsAny(reportedConsequence(sentence), unambiguousFailureStates) {
					strong = false
				}
				if !hasAnyPrefix(tail, successNegationTails) || (!strong && containsAny(blockedContext, blockedWorkMarkers)) {
					return selfReportReason(strings.TrimSpace(stem) + " …")
				}
				start = abs + len(stem)
			}
		}
	}
	return ""
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func selfReportReason(marker string) string {
	return `the final message admits the objective was not met ("` + marker + `")`
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// acceptanceVerificationNudge forces a TASK-GROUNDED acceptance check before a run
// may finalize as success. It explicitly rejects the three false-success patterns
// the bug-hunt surfaced: well-formed output treated as correct; pre-existing tests
// passing treated as the objective being met; and a result that merely matches a
// baseline the task asked to beat or improve. General — no task-specific content.
func acceptanceVerificationNudge(objective string) string {
	nudge := "Before this task can be marked complete, " + acceptanceNudgeMarker + " — " +
		"NOT the shape or format of your output, NOT that pre-existing tests pass, and NOT that your " +
		"result merely matches a baseline you were asked to beat or improve. " +
		"Re-read the original task, then run a concrete check that exercises the actual requirement: " +
		"execute the program or tests that demonstrate the required behavior, or directly probe the specific " +
		"thing the task asked you to produce, recover, fix, or optimize. " +
		"If that check passes, reply PASS and cite the evidence. " +
		"If it does not pass — or you cannot run such a check — say so plainly and keep working; do not claim success."
	if objective = capTaskObjective(objective); objective != "" {
		nudge += "\n\nTask objective: " + objective
	}
	return nudge
}

// toolFailureHintMarker is a stable substring for tests.
const toolFailureHintMarker = "kept failing with the same error"

type toolFailureRecord struct {
	count    int
	identity failureIdentity
	// sawExecuted and sawRefused are the AGGREGATE over the whole streak, not
	// the current identity. The content-blind bound needs them because it fires
	// exactly when no identity repeated, so the last one is unrepresentative.
	sawExecuted bool
	sawRefused  bool
	hintShown   bool
	// anyErrorCount counts consecutive failures of this tool REGARDLESS of the
	// error, and is cleared only by a success. count above restarts whenever the
	// signature changes, which is exactly what a varying error message defeats;
	// this one cannot be reset by changing the text.
	anyErrorCount int
}

// failureKind separates a failure the TOOL produced from one POLICY produced.
//
// The two used to share one string namespace, with provenance recovered
// afterwards by testing that string for a "denial:" prefix. Trusted provenance
// and untrusted content in one namespace is a namespace the untrusted side can
// write into: a command that printed exactly "denial:permission_denied" and
// exited non-zero acquired the identity of a real permission refusal, merged
// its streak with one, and had the run report it as refused although it ran
// every time. The kind is carried as its own field so no output can spell it.
type failureKind uint8

const (
	// The zero value: no failure recorded.
	_ failureKind = iota
	// failureKindExecuted: the tool ran and returned an error. The key is the
	// normalized error signature, which is tool-controlled text.
	failureKindExecuted
	// failureKindRefused: policy refused the call before the tool ran. The key is
	// the denial category, a small closed enum the loop sets.
	failureKindRefused
)

// failureIdentity is what the same-identity streak counts. Comparing the pair
// means an executed error can never equal a refusal however it is spelled.
type failureIdentity struct {
	kind failureKind
	key  string
}

// toolFailureCause is why the guard stopped, decided where the facts are rather
// than reconstructed by the caller from overlapping booleans.
type toolFailureCause uint8

const (
	// The zero value: the guard did not stop.
	_ toolFailureCause = iota
	// toolFailureCauseSameError: the same executed error signature, six times.
	toolFailureCauseSameError
	// toolFailureCauseSameRefusal: the same denial category, six times. The prose
	// differs on every call because it names what was refused, so this is NOT the
	// same-error case.
	toolFailureCauseSameRefusal
	// toolFailureCauseVariedExecuted: the content-blind bound, and every failure
	// was the tool executing and failing.
	toolFailureCauseVariedExecuted
	// toolFailureCauseVariedRefused: the content-blind bound, and every failure
	// was a policy refusal. Alternating two categories reaches twelve without
	// either reaching six, and the tool never ran once.
	toolFailureCauseVariedRefused
	// toolFailureCauseVariedMixed: the content-blind bound over both kinds.
	// Named deliberately rather than left to whichever field a switch tested
	// first.
	toolFailureCauseVariedMixed
)

type toolFailureOutcome struct {
	InjectHint bool
	Stop       bool
	Count      int
	// Cause is set only when Stop is. It carries the aggregate fact about the
	// sequence that tripped a bound, which the last record alone cannot supply:
	// the content-blind bound is reached precisely when no single identity
	// repeated enough, so the identity present at the end says nothing about the
	// eleven before it.
	Cause toolFailureCause
}

// Refused reports whether policy, rather than the tool, produced the failures.
func (cause toolFailureCause) Refused() bool {
	return cause == toolFailureCauseSameRefusal || cause == toolFailureCauseVariedRefused
}

// errorSignature normalizes a tool error to a short, comparable signature so
// repeated identical failures are detected while a genuinely different error
// resets the streak.
func errorSignature(output string) string {
	s := strings.ToLower(strings.Join(strings.Fields(output), " "))
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// toolFailureHint tells the model exactly how a tool's arguments must look after
// it has repeated the same failing call. Injected at most once per failure streak.
func toolFailureHint(toolName, schemaJSON, errOutput string) string {
	return "Your calls to the `" + toolName + "` tool " + toolFailureHintMarker + ":\n" +
		strings.TrimSpace(errOutput) +
		"\n\nThe `" + toolName + "` tool expects arguments matching this schema — match it exactly:\n" +
		strings.TrimSpace(schemaJSON) +
		"\n\nFix the arguments and try once more, or take a different approach."
}

// toolFailureStopAnswer is the final answer when the repeated-failure guard halts
// a run.
func toolFailureStopAnswer(toolName string, count int, cause toolFailureCause) string {
	// Each branch claims only what its counter established, and the mixed case is
	// spelled out rather than decided by whichever field a switch happened to
	// test first. The wording used to overclaim in both directions: the
	// content-blind bound said every failure differed, which it does not track,
	// and it described a run of policy refusals as the tool failing, when the
	// tool had not run at all.
	verb, tail := " tool failed ", " times in a row with the same error, "
	switch cause {
	case toolFailureCauseSameRefusal:
		verb, tail = " tool was refused ", " times in a row, "
	case toolFailureCauseVariedExecuted:
		tail = " times in a row with varying errors, "
	case toolFailureCauseVariedRefused:
		verb, tail = " tool was refused ", " times in a row for different reasons, "
	case toolFailureCauseVariedMixed:
		verb, tail = " tool failed or was refused ", " times in a row, "
	}
	return "Agent stopped: the `" + toolName + "`" + verb + strconv.Itoa(count) +
		tail + "so I halted instead of looping further. " +
		"Please check the request or adjust the tool arguments."
}

// toolFailureIncompleteReason describes a guard halt for the headless
// completion gate. A guard halt is BY DEFINITION unfinished work: the run
// stopped because a tool would not stop failing, not because the task was done.
//
// It makes the same claim as toolFailureStopAnswer for the same halt, including
// the mixed case: a headless caller reads this reason and an interactive one
// reads the stop answer, and the two must not describe one halt differently.
// Reported by @jatmn.
func toolFailureIncompleteReason(toolName string, cause toolFailureCause) string {
	switch {
	case cause.Refused():
		return "halted after `" + toolName + "` was refused repeatedly, without completing the task"
	case cause == toolFailureCauseVariedMixed:
		return "halted after `" + toolName + "` failed or was refused repeatedly, without completing the task"
	}
	return "halted after `" + toolName + "` failed repeatedly, without completing the task"
}

// The no-output stop answer is assembled from these fixed parts (only the turn
// count varies). IsNoProgressStop matches all three so a legitimate message that
// merely quotes the marker substring is not misclassified as a failed empty run.
const (
	noOutputStopPrefix = "Agent stopped after "
	noOutputStopMarker = "with no output (no visible text and no tool calls)"
	noOutputStopSuffix = "to avoid consuming tokens without making progress."
)

// noOutputStopAnswer is the final answer returned when the no-output guard
// stops the run. The turn count is interpolated at the call site.
func noOutputStopAnswer(turns int) string {
	return noOutputStopPrefix + strconv.Itoa(turns) + " turns " + noOutputStopMarker + " " + noOutputStopSuffix
}

// IsNoProgressStop reports whether content IS the no-output guardrail stop answer
// (a run that produced no visible text and no tool calls). It matches the EXACT
// structure noOutputStopAnswer emits — prefix + "<int> turns " + marker + " " +
// suffix, where only the integer turn count varies — rather than just looking for
// the three parts in order. A loose check (prefix && contains-marker && suffix)
// would misclassify a genuine assistant/tool message that merely quotes the
// marker amid other prose, which would wrongly hide a real session from /resume
// and skip its title generation.
func IsNoProgressStop(content string) bool {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, noOutputStopPrefix) {
		return false
	}
	rest := trimmed[len(noOutputStopPrefix):]
	const turnsSep = " turns "
	sep := strings.Index(rest, turnsSep)
	if sep < 0 {
		return false
	}
	// The text between the prefix and " turns " must be exactly the bare integer
	// count; anything else means this isn't the guard's own answer.
	if _, err := strconv.Atoi(rest[:sep]); err != nil {
		return false
	}
	// The marker must be immediately followed (one space) by the suffix and then
	// end — no arbitrary text wedged in between.
	return rest[sep+len(turnsSep):] == noOutputStopMarker+" "+noOutputStopSuffix
}

// Reminder markers are stable substrings used both to build the reminder text
// and to assert in tests that the right reminder was injected exactly once.
const (
	planNotCalledReminderMarker    = "you have not called update_plan"
	planStaleReminderMarker        = "haven't updated the plan via update_plan"
	toolOnlyProgressReminderMarker = "consecutive tool-only turns"
)

// planNotCalledReminder nudges the model to track a multi-step task with
// update_plan. Injected at most once per run.
func planNotCalledReminder() string {
	return "Reminder: this looks like a multi-step task and " + planNotCalledReminderMarker +
		". Use the update_plan tool to record the steps and keep progress visible. " +
		"Continue with your work after updating the plan."
}

// planStaleReminder nudges the model to refresh the plan after a stretch of
// tool calls without a plan update. Injected at most once per stale interval.
func planStaleReminder(callsSinceUpdate int) string {
	return "Reminder: you've made " + strconv.Itoa(callsSinceUpdate) +
		" tool calls but " + planStaleReminderMarker +
		" in a while. Update the plan to reflect completed and remaining steps, then continue."
}

func toolOnlyProgressReminder(turns int) string {
	return "Reminder: you've made " + strconv.Itoa(turns) + " " + toolOnlyProgressReminderMarker +
		" without visible progress. Before calling more tools, summarize what you already know, state the next concrete step, and finish if you have enough information."
}

// guardState tracks the per-run signals the guardrails need. It is observable
// purely from tool-call names and per-turn output, matching what the loop holds.
type guardState struct {
	emptyTurns               int
	totalToolCalls           int
	toolCallsSincePlanUpdate int
	// turnsSincePlanUpdate counts turns (not individual tool calls) since the last
	// update_plan, so a plan that goes stale across many low-tool-call turns is
	// still caught — the tool-call counter alone can take many turns to trip when
	// the model makes only one call per turn.
	turnsSincePlanUpdate  int
	planEverCalled        bool
	notCalledReminderSent bool
	// staleReminderSent records whether the stale reminder has already fired for
	// the current stale interval. It is cleared when a plan update opens a new
	// interval, making the reminder one-shot per interval rather than per turn.
	staleReminderSent    bool
	toolOnlyTurns        int
	toolOnlyReminderSent bool
	// planItemsPending is the number of remaining (pending/in_progress) items in
	// the most recent update_plan call, so the headless completion gate can tell
	// whether work is unfinished when the model stops without a tool call.
	planItemsPending int
	// toolFailures tracks consecutive same-error failures per tool, keyed by tool
	// name, so the loop can hint then halt instead of looping forever.
	toolFailures map[string]*toolFailureRecord
}

func newGuardState() *guardState {
	return &guardState{toolFailures: map[string]*toolFailureRecord{}}
}

// observeToolResult tracks repeated identical failures of a tool. A successful
// result clears that tool's failure streak. Returns whether to inject a one-shot
// corrective hint and/or stop the run.
// hintable is separate from failed on purpose. A categorized denial MUST count
// toward the streaks — that is the whole point of keying on the category — but a
// schema hint is the wrong response to a policy refusal: the call shape is fine,
// the answer was no. Collapsing the two is what made the earlier version of this
// fix a no-op, since the caller could only pass a flag that excluded denials.
func (state *guardState) observeToolResult(name string, failed bool, hintable bool, output string, denial DenialCategory) toolFailureOutcome {
	if state.toolFailures == nil {
		state.toolFailures = map[string]*toolFailureRecord{}
	}
	if !failed {
		delete(state.toolFailures, name) // success resets both counters
		return toolFailureOutcome{}
	}
	// A denial keys on its CATEGORY, not its prose. The message embeds the path
	// or command that was refused, so it differs on every call while describing
	// the same unchanging refusal — which rebuilt the record at 1 each time and
	// let a denied tool loop indefinitely under a halt set to 6. The category is
	// a small closed enum the loop already sets on the result.
	identity := failureIdentity{kind: failureKindExecuted, key: errorSignature(output)}
	if denial != DenialNone {
		identity = failureIdentity{kind: failureKindRefused, key: string(denial)}
	}
	record := state.toolFailures[name]
	if record == nil {
		record = &toolFailureRecord{identity: identity}
		state.toolFailures[name] = record
	}
	if record.identity != identity {
		// A different failure restarts the same-identity streak but NOT the
		// content-blind one: changing how a tool fails is not progress.
		record.count = 1
		record.identity = identity
		record.hintShown = false
	} else {
		record.count++
	}
	record.anyErrorCount++
	if identity.kind == failureKindRefused {
		record.sawRefused = true
	} else {
		record.sawExecuted = true
	}

	outcome := toolFailureOutcome{Count: record.count}
	switch {
	case record.count >= toolFailureStopAt:
		outcome.Stop = true
		outcome.Cause = toolFailureCauseSameError
		if identity.kind == failureKindRefused {
			outcome.Cause = toolFailureCauseSameRefusal
		}
		return outcome
	case record.anyErrorCount >= toolFailureAnyErrorStopAt:
		// Report the counter that actually tripped. record.count is the
		// same-identity streak and is often 1 here, which would describe a tool
		// that failed a dozen different ways as having failed once.
		outcome.Stop = true
		outcome.Count = record.anyErrorCount
		outcome.Cause = record.variedCause()
		return outcome
	}
	if hintable && record.count >= toolFailureHintAt && !record.hintShown {
		record.hintShown = true
		outcome.InjectHint = true
	}
	return outcome
}

// observeTurn updates counters from a turn's collected stream. It returns
// whether the no-output guard should stop the run.
//
// Callers must NOT invoke this for turns handled by the dropped-tool-call retry
// path; those are not "empty" in the runaway sense and are handled separately.
func (state *guardState) observeTurn(collected zeroruntime.CollectedStream) (stop bool) {
	hasToolCalls := len(collected.ToolCalls) > 0
	hasVisibleText := strings.TrimSpace(collected.Text) != ""
	hasReasoning := collected.HasReasoning || len(collected.ReasoningBlocks) > 0

	if hasToolCalls || hasVisibleText || hasReasoning {
		state.emptyTurns = 0
	} else {
		state.emptyTurns++
	}
	if hasToolCalls && !hasVisibleText {
		state.toolOnlyTurns++
	} else {
		state.toolOnlyTurns = 0
		state.toolOnlyReminderSent = false
	}

	// One turn has passed; the plan-update below resets this to 0 when the model
	// refreshes the plan this turn.
	state.turnsSincePlanUpdate++

	for _, call := range collected.ToolCalls {
		state.totalToolCalls++
		if call.Name == planToolName {
			state.planEverCalled = true
			state.toolCallsSincePlanUpdate = 0
			state.turnsSincePlanUpdate = 0
			// A fresh plan update opens a new stale interval.
			state.staleReminderSent = false
			// Record how many items remain so the completion gate knows whether
			// work is unfinished if the model later stops without a tool call.
			state.observePlanUpdate(call.Arguments)
		} else {
			state.toolCallsSincePlanUpdate++
		}
	}

	return state.emptyTurns >= maxEmptyTurns
}

// pendingPlanItems reports whether the most recent update_plan call still has
// unfinished (pending/in_progress) items. False when no plan was ever recorded.
func (state *guardState) pendingPlanItems() bool {
	return state.planItemsPending > 0
}

// observePlanUpdate parses an update_plan call's raw arguments and records how
// many items are still remaining. Malformed arguments leave the prior count
// unchanged (best-effort — the plan panel itself tolerates the same).
func (state *guardState) observePlanUpdate(arguments string) {
	plan, ok := parseTaskPlan(arguments)
	if !ok {
		return
	}
	pending := 0
	for _, item := range plan {
		if planStatusRemaining(item.Status) {
			pending++
		}
	}
	state.planItemsPending = pending
}

func (state *guardState) progressReminder() string {
	if state.toolOnlyReminderSent || state.toolOnlyTurns < toolOnlyProgressReminderAt {
		return ""
	}
	state.toolOnlyReminderSent = true
	return toolOnlyProgressReminder(state.toolOnlyTurns)
}

// planReminder returns a one-shot reminder message to inject before the next
// turn, or an empty string when no reminder applies. `turn` is 1-based (the
// number of turns completed so far).
func (state *guardState) planReminder(turn int) string {
	// STALE reminder takes priority: a long run without a plan update is the
	// stronger signal. Fires on either the tool-call streak OR a turn streak with
	// pending items (so a plan drifting stale across many low-call turns is caught,
	// while a fully-completed plan is left alone). One-shot per stale interval.
	if state.planEverCalled && !state.staleReminderSent &&
		(state.toolCallsSincePlanUpdate >= staleToolCallThreshold ||
			(state.turnsSincePlanUpdate >= stalePlanTurnThreshold && state.planItemsPending > 0)) {
		state.staleReminderSent = true
		return planStaleReminder(state.toolCallsSincePlanUpdate)
	}

	// NOT-CALLED reminder: once both the turn and tool-call thresholds are met,
	// sustained work should have called update_plan. One-shot for the whole run.
	if !state.notCalledReminderSent &&
		!state.planEverCalled &&
		turn >= planReminderTurn &&
		state.totalToolCalls >= planReminderToolThreshold {
		state.notCalledReminderSent = true
		return planNotCalledReminder()
	}

	return ""
}

// variedCause classifies a content-blind stop by what the whole streak held.
func (record *toolFailureRecord) variedCause() toolFailureCause {
	switch {
	case record.sawRefused && record.sawExecuted:
		return toolFailureCauseVariedMixed
	case record.sawRefused:
		return toolFailureCauseVariedRefused
	default:
		return toolFailureCauseVariedExecuted
	}
}
