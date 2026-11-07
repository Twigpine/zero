package agent

import (
	"reflect"
	"strings"
	"testing"
)

// (i)(j)(k) The whole reminder state machine as data. zeromaxingReminders is
// pure, so "enter exactly once" and "still-on never on the first turn" are
// assertions about a table rather than about observed side effects.
func TestZeromaxingReminderSchedule(t *testing.T) {
	cases := []struct {
		name    string
		posture Zeromaxing
		turn    int
		want    []string
	}{
		// (i) enter fires exactly once, on the first turn of the entering run.
		{"entering turn 1 announces, states the budget and the evidence contract", ZeromaxingEntering, 1,
			[]string{ZeromaxingEnterNotice, ZeromaxingBudgetNotice, ZeromaxingEvidenceNotice}},
		// (j) still-on from turn 2, and it is the ONLY thing from turn 2 on —
		// no second enter, no repeated budget notice.
		{"entering turn 2 continues", ZeromaxingEntering, 2, []string{ZeromaxingStillOnNotice}},
		{"entering turn 3 continues", ZeromaxingEntering, 3, []string{ZeromaxingStillOnNotice}},
		{"entering turn 99 continues", ZeromaxingEntering, 99, []string{ZeromaxingStillOnNotice}},

		// (j) a run that began with the posture already on gets still-on even on
		// turn 1 — re-announcing entry every run would be wrong.
		// The evidence contract is per-RUN, not per-posture-change: turn 1 is
		// where the user's message sits, so it is where a claim about the code
		// arrives and where a report gets written.
		{"active turn 1 does not re-announce but restates the evidence contract", ZeromaxingActive, 1,
			[]string{ZeromaxingStillOnNotice, ZeromaxingEvidenceNotice}},
		{"active turn 2 continues", ZeromaxingActive, 2, []string{ZeromaxingStillOnNotice}},

		// (k) exit fires exactly once, then silence — the posture is off, so a
		// "still active" line afterwards would be a lie.
		{"exiting turn 1 announces once", ZeromaxingExiting, 1, []string{ZeromaxingExitNotice}},
		{"exiting turn 2 is silent", ZeromaxingExiting, 2, nil},
		{"exiting turn 5 is silent", ZeromaxingExiting, 5, nil},

		// Off is completely silent: an unaware caller's run is byte-identical.
		{"off turn 1 is silent", ZeromaxingOff, 1, nil},
		{"off turn 7 is silent", ZeromaxingOff, 7, nil},

		// Defensive: a non-positive turn never emits.
		{"turn 0 is silent", ZeromaxingEntering, 0, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := zeromaxingReminders(tc.posture, tc.turn, false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("zeromaxingReminders(%v, %d) = %#v, want %#v", tc.posture, tc.turn, got, tc.want)
			}
		})
	}
}

// Counting across a whole run, the way a reader of the gate would check it.
func TestZeromaxingEnterAndExitFireExactlyOncePerRun(t *testing.T) {
	count := func(posture Zeromaxing, needle string) int {
		n := 0
		for turn := 1; turn <= 50; turn++ {
			for _, line := range zeromaxingReminders(posture, turn, false) {
				if line == needle {
					n++
				}
			}
		}
		return n
	}
	if got := count(ZeromaxingEntering, ZeromaxingEnterNotice); got != 1 {
		t.Fatalf("enter fired %d times across 50 turns, want exactly 1", got)
	}
	if got := count(ZeromaxingEntering, ZeromaxingBudgetNotice); got != 1 {
		t.Fatalf("budget notice fired %d times across 50 turns, want exactly 1", got)
	}
	if got := count(ZeromaxingExiting, ZeromaxingExitNotice); got != 1 {
		t.Fatalf("exit fired %d times across 50 turns, want exactly 1", got)
	}
	if got := count(ZeromaxingActive, ZeromaxingEnterNotice); got != 0 {
		t.Fatalf("an already-active posture must never emit the enter notice, got %d", got)
	}
	if got := count(ZeromaxingEntering, ZeromaxingStillOnNotice); got != 49 {
		t.Fatalf("still-on fired on %d of turns 2..50, want 49", got)
	}
}

// still-on repeats on every continuing turn, so it must be BYTE-IDENTICAL every
// time. A turn counter, a remaining-budget number, or a timestamp here would be
// invisible today (it rides below the cache breakpoint) and a silent cost bug
// the moment anyone moved this text into the system prompt.
func TestZeromaxingStillOnNoticeIsInvariant(t *testing.T) {
	first := zeromaxingReminders(ZeromaxingEntering, 2, false)
	for turn := 3; turn <= 40; turn++ {
		if got := zeromaxingReminders(ZeromaxingEntering, turn, false); !reflect.DeepEqual(got, first) {
			t.Fatalf("still-on drifted at turn %d: %#v vs %#v", turn, got, first)
		}
	}
	if strings.ContainsAny(ZeromaxingStillOnNotice, "0123456789") {
		t.Fatalf("still-on must carry no digits (no counter, no budget, no timestamp): %q",
			ZeromaxingStillOnNotice)
	}
}

// Phase 1 ships no orchestration, so no reminder may imply any. A prompt that
// advertises capabilities the run does not have is a prompt-level lie, and the
// model will try to use them.
func TestZeromaxingRemindersPromiseNoOrchestration(t *testing.T) {
	all := []string{
		ZeromaxingEnterNotice, ZeromaxingBudgetNotice,
		ZeromaxingStillOnNotice, ZeromaxingExitNotice,
	}
	forbidden := []string{"orchestrat", "workflow", "fan out", "fan-out", "worker", "sub-agent", "subagent", "delegate", "parallel"}
	for _, notice := range all {
		lower := strings.ToLower(notice)
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Fatalf("Phase 1 has no orchestration; reminder must not mention %q: %q", word, notice)
			}
		}
	}
}

// Phase 1's no-orchestration rule, now CONDITIONAL rather than absolute.
//
// The rule was never "never mention a tool" — it was "never promise a
// capability the run does not have". With the orchestrate tool actually
// advertised, naming it is accurate; without it, naming it would be the
// prompt-level lie the original test guarded against. Both directions asserted,
// because deleting the guard and keeping only the positive case would lose
// exactly the protection that mattered.
func TestOrchestrateIsNamedOnlyWhenItExists(t *testing.T) {
	withTool := zeromaxingReminders(ZeromaxingEntering, 1, true)
	withoutTool := zeromaxingReminders(ZeromaxingEntering, 1, false)

	joined := func(lines []string) string { return strings.Join(lines, "\n") }

	if !strings.Contains(joined(withTool), "orchestrate") {
		t.Fatalf("with the tool available the enter notice must name it:\n%s", joined(withTool))
	}
	if strings.Contains(joined(withoutTool), "orchestrate") {
		t.Fatalf("without the tool NOTHING may mention it:\n%s", joined(withoutTool))
	}
	// The unavailable case keeps Phase 1's original vocabulary guard intact.
	for _, word := range []string{"orchestrat", "workflow", "fan-out", "worker", "delegate", "parallel"} {
		if strings.Contains(strings.ToLower(joined(withoutTool)), word) {
			t.Fatalf("without the tool the reminders must not mention %q:\n%s", word, joined(withoutTool))
		}
	}
	// The notice is one-shot like the rest: it rides the enter turn only.
	for turn := 2; turn <= 5; turn++ {
		if strings.Contains(joined(zeromaxingReminders(ZeromaxingEntering, turn, true)), "orchestrate") {
			t.Fatalf("the orchestrate notice must fire once, not on turn %d", turn)
		}
	}
	// Every OTHER posture state stays silent about it.
	for _, posture := range []Zeromaxing{ZeromaxingOff, ZeromaxingActive, ZeromaxingExiting} {
		if strings.Contains(joined(zeromaxingReminders(posture, 1, true)), "orchestrate") {
			t.Fatalf("posture %v must not name the tool", posture)
		}
	}
}

// THE EVIDENCE CONTRACT FIRES ONCE PER RUN, on the turn the user's message sits
// on — not once per posture change, and not on every tool turn.
//
// Once per posture change would leave every later run of a long session without
// it, and the long sessions are the ones that produce reports. Every tool turn
// would pay hundreds of copies to say something that only bears on the
// boundaries of a run.
func TestTheEvidenceContractFiresOnceOnTheFirstTurnOfEveryRun(t *testing.T) {
	for _, posture := range []Zeromaxing{ZeromaxingEntering, ZeromaxingActive} {
		fired := 0
		for turn := 1; turn <= 50; turn++ {
			for _, line := range zeromaxingReminders(posture, turn, false) {
				if line == ZeromaxingEvidenceNotice {
					fired++
					if turn != 1 {
						t.Errorf("posture %v repeated the evidence contract on turn %d; it belongs on turn 1 only", posture, turn)
					}
				}
			}
		}
		if fired != 1 {
			t.Errorf("posture %v fired the evidence contract %d times across 50 turns, want exactly 1", posture, fired)
		}
	}

	// A posture that is OFF or on its way out says nothing: a contract about how
	// to weigh evidence is part of the posture, and claiming it after the posture
	// ended would be as wrong as the still-on notice would be.
	for _, posture := range []Zeromaxing{ZeromaxingOff, ZeromaxingExiting} {
		for turn := 1; turn <= 5; turn++ {
			for _, line := range zeromaxingReminders(posture, turn, true) {
				if line == ZeromaxingEvidenceNotice {
					t.Errorf("posture %v emitted the evidence contract on turn %d", posture, turn)
				}
			}
		}
	}
}

// The contract has to actually carry all three rules it exists for. Asserted on
// the substance, not the wording, so a rewrite is free but a DELETION is not:
// each of these went missing in a measured run and cost a different thing.
func TestTheEvidenceContractCoversAllThreeFailures(t *testing.T) {
	for _, required := range []string{
		// A passing test treated as proof of the property.
		"passing test is not proof",
		"what it would still pass with",
		// Numbers reported that no command in the session produced.
		"must come from a command you ran in this session",
		"show both",
		// Folding to whoever asserts a defect most confidently.
		"prove or refute it from the code before you agree",
		"even when the claim turns out to be right",
	} {
		if !strings.Contains(ZeromaxingEvidenceNotice, required) {
			t.Errorf("the evidence contract no longer says %q:\n%s", required, ZeromaxingEvidenceNotice)
		}
	}
}
