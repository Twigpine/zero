package measurements

import (
	"strings"
	"sync"
	"testing"
)

const goTestOutput = `
ok  	github.com/Gitlawb/zero/internal/specialist	8.337s
FAIL	github.com/Gitlawb/zero/internal/cli	34.249s
ok  	github.com/Gitlawb/zero/internal/config	20.047s	coverage: 61.2% of statements
ok  	github.com/Gitlawb/zero/internal/minify	(cached)
--- PASS: TestChattyChild (0.86s)
--- FAIL: TestWallBackstop (1.00s)
--- PASS: TestNested (0.03s)
    --- PASS: TestNested/subcase (0.02s)
`

// THE PARENT LINE ABOVE IS NOT OPTIONAL. go test -v always prints a parent
// before its subtests, and the earlier fixture omitted it — a shape the tool
// never emits. That single omission hid a whole class: with only the subtest
// present, no ledger name was ever a strict prefix of another, so the raw
// substring match in claimedSecondsFor looked correct. A fixture has to be the
// output a real run produced, because the trimming is where the bug lives.

func TestParseGoTestReadsBothLineShapes(t *testing.T) {
	got := ParseGoTest(goTestOutput)
	byName := map[string]float64{}
	for _, m := range got {
		byName[m.Name] = m.Seconds
	}

	for name, want := range map[string]float64{
		"github.com/Gitlawb/zero/internal/specialist": 8.337,
		"github.com/Gitlawb/zero/internal/cli":        34.249,
		// A trailing coverage suffix must not stop the line being read.
		"github.com/Gitlawb/zero/internal/config": 20.047,
		"TestChattyChild":                         0.86,
		"TestWallBackstop":                        1.00,
		// Indented subtests count: they are what a per-test table is built from.
		"TestNested/subcase": 0.02,
	} {
		if byName[name] != want {
			t.Errorf("%s = %v, want %v", name, byName[name], want)
		}
	}
	// A cached package reports no duration, so there is nothing to record — and
	// inventing a zero for it would make every later claim look like a conflict.
	if _, present := byName["github.com/Gitlawb/zero/internal/minify"]; present {
		t.Error("a (cached) package was recorded with a duration it never reported")
	}
}

// THE FAILURE THIS WAS BUILT FOR: the same test reported at 0.86s in one paste
// and 4.20s in the next, with nothing said about the difference.
func TestAClaimThatContradictsTheTranscriptIsCaught(t *testing.T) {
	ledger := NewLedger()
	if n := ledger.Record(goTestOutput); n == 0 {
		t.Fatal("nothing was recorded, so no conflict could ever be found")
	}

	conflicts := ledger.Conflicts("| TestChattyChild | 4.20s | passes |")
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(conflicts), conflicts)
	}
	if conflicts[0].Name != "TestChattyChild" || conflicts[0].Claimed != 4.20 {
		t.Fatalf("wrong conflict: %+v", conflicts[0])
	}
	if len(conflicts[0].Recorded) != 1 || conflicts[0].Recorded[0] != 0.86 {
		t.Fatalf("the recorded value is not carried back to the reader: %+v", conflicts[0])
	}
}

// ...and the honest cases stay silent. A tripwire that fires on ordinary
// variation gets switched off, and then it catches nothing at all.
func TestHonestReportingProducesNoConflict(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(goTestOutput)

	for name, claim := range map[string]string{
		"the number as recorded":         "TestChattyChild took 0.86s.",
		"ordinary run-to-run variation":  "TestChattyChild took 0.91s.",
		"a package line restated":        "ok github.com/Gitlawb/zero/internal/specialist 8.4s",
		"sub-centisecond jitter":         "TestNested/subcase (0.03s)",
		"named without a timing":         "TestChattyChild passes.",
		"a name this session never ran":  "TestSomethingElse took 99.0s.",
		"the same value in milliseconds": "TestChattyChild took 860ms.",
	} {
		fresh := NewLedger()
		fresh.Record(goTestOutput)
		if got := fresh.Conflicts(claim); len(got) != 0 {
			t.Errorf("%s produced a false conflict: %+v", name, got)
		}
	}
}

// A NUMBER THREE PARAGRAPHS AWAY IS NOT THIS NAME'S TIMING. Pairing across lines
// would invent disagreements rather than find them.
func TestADurationOnAnotherLineIsNotPairedWithTheName(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(goTestOutput)

	claim := "TestChattyChild is the one to look at.\n\nSeparately, the whole suite took 4.20s."
	if got := ledger.Conflicts(claim); len(got) != 0 {
		t.Errorf("a duration from an unrelated line was attributed to the test: %+v", got)
	}
}

// EACH NAME IS RAISED ONCE. The caller feeds this back to the model, so a second
// pass over an uncorrected answer has to be silent or the loop never ends.
func TestAConflictIsRaisedOnlyOnce(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(goTestOutput)
	claim := "TestChattyChild took 4.20s."

	if got := ledger.Conflicts(claim); len(got) != 1 {
		t.Fatalf("first pass found %d conflicts, want 1", len(got))
	}
	if got := ledger.Conflicts(claim); len(got) != 0 {
		t.Fatalf("the same conflict was raised twice, so an unchanged answer would loop: %+v", got)
	}
}

// A test run twice legitimately has two timings, and matching EITHER is honest.
func TestMatchingAnyRecordedValueIsEnough(t *testing.T) {
	ledger := NewLedger()
	ledger.Record("--- PASS: TestFlaky (0.10s)\n")
	ledger.Record("--- PASS: TestFlaky (9.90s)\n")

	if got := ledger.Conflicts("TestFlaky took 9.90s."); len(got) != 0 {
		t.Errorf("matching the second of two recorded runs was called a conflict: %+v", got)
	}
	if got := ledger.Conflicts("TestFlaky took 45.0s."); len(got) != 1 {
		t.Errorf("a value matching neither run was not caught: %+v", got)
	}
}

// The nudge has to name the number, the recorded value, and what to do — a
// warning a model cannot act on is a warning it will not act on.
func TestTheNudgeNamesBothNumbersAndTheRemedy(t *testing.T) {
	nudge := Nudge([]Conflict{{Name: "TestChattyChild", Claimed: 4.2, Recorded: []float64{0.86}}})
	for _, required := range []string{"TestChattyChild", "4.2s", "0.86s", "Re-run the command", "give both"} {
		if !strings.Contains(nudge, required) {
			t.Errorf("the nudge does not contain %q:\n%s", required, nudge)
		}
	}
	if Nudge(nil) != "" {
		t.Error("an empty conflict set must render nothing")
	}
}

// A nil Ledger is a working no-op: the loop calls these unconditionally and only
// holds a real ledger under the posture.
func TestANilLedgerIsSafe(t *testing.T) {
	var ledger *Ledger
	if got := ledger.Record(goTestOutput); got != 0 {
		t.Errorf("Record on a nil ledger returned %d", got)
	}
	if got := ledger.Conflicts("TestChattyChild took 4.20s."); got != nil {
		t.Errorf("Conflicts on a nil ledger returned %+v", got)
	}
}

// Tool results arrive from concurrently executed tool calls, so recording races
// against recording and against the final check.
func TestTheLedgerIsSafeUnderConcurrentRecording(t *testing.T) {
	ledger := NewLedger()
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ledger.Record(goTestOutput)
			ledger.Conflicts("nothing to see")
		}()
	}
	wait.Wait()
	if got := ledger.Conflicts("TestChattyChild took 4.20s."); len(got) != 1 {
		t.Fatalf("got %d conflicts after concurrent recording, want 1", len(got))
	}
}

// An HONEST report must not be called a fabrication because one recorded name is
// a prefix of another. go test -v guarantees the collision: it prints the parent
// above every subtest and the ledger records both, so a truthful subtest claim
// matched the parent's entry and was told it invented the number. Package names
// collide with no subtests at all — internal/agent is a prefix of
// internal/agentinit, and this repo has several such pairs.
func TestAPrefixNameDoesNotAccuseAnHonestClaim(t *testing.T) {
	ledger := NewLedger()
	// Exactly what go test -v emits: parent first, subtest indented under it.
	ledger.Record("--- PASS: TestNested (0.03s)\n    --- PASS: TestNested/subcase (0.01s)\n")
	if conflicts := ledger.Conflicts("TestNested/subcase took 0.01s"); len(conflicts) != 0 {
		t.Errorf("an honest subtest claim was reported as a conflict: %+v", conflicts)
	}

	packages := NewLedger()
	packages.Record("ok  \tgithub.com/Gitlawb/zero/internal/agent\t35.58s\nok  \tgithub.com/Gitlawb/zero/internal/agentinit\t1.66s\n")
	if conflicts := packages.Conflicts("github.com/Gitlawb/zero/internal/agentinit took 1.66s"); len(conflicts) != 0 {
		t.Errorf("an honest package claim was reported as a conflict: %+v", conflicts)
	}

	// And the check still bites: a genuinely wrong subtest number is caught, and
	// attributed to the subtest rather than to its parent.
	caught := NewLedger()
	caught.Record("--- PASS: TestNested (0.03s)\n    --- PASS: TestNested/subcase (0.01s)\n")
	conflicts := caught.Conflicts("TestNested/subcase took 4.20s")
	if len(conflicts) != 1 || conflicts[0].Name != "TestNested/subcase" {
		t.Errorf("a fabricated subtest number was not caught against its own name: %+v", conflicts)
	}
}

// A duration carrying a minute component must be read whole. The pattern was
// ms-or-s only, so "1m10s" failed on "1m" and "10s" won — a truthful restatement
// of a recorded 70 seconds became a conflict, and the nudge quoted 10s back at
// the model, a number its answer never contained.
func TestAMinuteDurationIsReadWhole(t *testing.T) {
	ledger := NewLedger()
	ledger.Record("--- PASS: TestSlow (70.00s)\n")
	if conflicts := ledger.Conflicts("TestSlow took 1m10s"); len(conflicts) != 0 {
		t.Errorf("an honest 1m10s claim was reported as a conflict: %+v", conflicts)
	}

	bare := NewLedger()
	bare.Record("--- PASS: TestTwoMinutes (120.00s)\n")
	if conflicts := bare.Conflicts("TestTwoMinutes took 2m"); len(conflicts) != 0 {
		t.Errorf("an honest bare-minute claim was reported as a conflict: %+v", conflicts)
	}

	// Still caught when the minutes really disagree.
	wrong := NewLedger()
	wrong.Record("--- PASS: TestSlow (70.00s)\n")
	conflicts := wrong.Conflicts("TestSlow took 5m00s")
	if len(conflicts) != 1 || conflicts[0].Claimed != 300 {
		t.Errorf("a fabricated 5m00s was not caught as 300s: %+v", conflicts)
	}
}

// The nearest duration is the claim, whichever form it is written in. Every case
// above puts the minute figure FIRST, so trying that pattern over the whole tail
// ahead of the seconds pattern passed them all while reaching past a nearer
// seconds figure to claim a later one — inventing a conflict against a number the
// model had right, the one failure this package must never produce.
func TestTheNearestDurationIsTheClaim(t *testing.T) {
	honest := NewLedger()
	honest.Record("--- PASS: TestChattyChild (0.86s)\n")
	// The package total trails the test's own timing, exactly as `go test` prints it.
	if conflicts := honest.Conflicts("TestChattyChild took 0.86s (package total 1m20s)"); len(conflicts) != 0 {
		t.Errorf("a correct 0.86s claim was reported as a conflict because a later 1m20s was read instead: %+v", conflicts)
	}

	ms := NewLedger()
	ms.Record("--- PASS: TestQuick (0.45s)\n")
	if conflicts := ms.Conflicts("TestQuick took 450ms, well under the 2m budget"); len(conflicts) != 0 {
		t.Errorf("a correct 450ms claim was reported as a conflict: %+v", conflicts)
	}

	// And a seconds figure sitting nearby does not rescue a wrong minute claim
	// when the minute figure is the one being stated.
	wrong := NewLedger()
	wrong.Record("--- PASS: TestSlow (70.00s)\n")
	conflicts := wrong.Conflicts("TestSlow took 5m00s, not the 70s you might expect")
	if len(conflicts) != 1 || conflicts[0].Claimed != 300 {
		t.Errorf("a fabricated 5m00s was not caught as 300s: %+v", conflicts)
	}
}
