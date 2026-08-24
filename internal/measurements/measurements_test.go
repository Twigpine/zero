package measurements

import (
	"fmt"
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
		// And the parent alongside it. The subtest alone left the prefix-trimming
		// unpinned from this side: a change that dropped parent lines, or folded
		// the parent's time into the child, passed every assertion here.
		"TestNested": 0.03,
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
	if n := ledger.Record(Run{}, goTestOutput); n == 0 {
		t.Fatal("nothing was recorded, so no conflict could ever be found")
	}

	conflicts := ledger.Conflicts(Run{}, "| TestChattyChild | 4.20s | passes |")
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
		fresh.Record(Run{}, goTestOutput)
		if got := fresh.Conflicts(Run{}, claim); len(got) != 0 {
			t.Errorf("%s produced a false conflict: %+v", name, got)
		}
	}
}

// A NUMBER THREE PARAGRAPHS AWAY IS NOT THIS NAME'S TIMING. Pairing across lines
// would invent disagreements rather than find them.
func TestADurationOnAnotherLineIsNotPairedWithTheName(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Run{}, goTestOutput)

	claim := "TestChattyChild is the one to look at.\n\nSeparately, the whole suite took 4.20s."
	if got := ledger.Conflicts(Run{}, claim); len(got) != 0 {
		t.Errorf("a duration from an unrelated line was attributed to the test: %+v", got)
	}
}

// EACH NAME IS RAISED ONCE. The caller feeds this back to the model, so a second
// pass over an uncorrected answer has to be silent or the loop never ends.
func TestAConflictIsRaisedOnlyOnce(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Run{}, goTestOutput)
	claim := "TestChattyChild took 4.20s."

	if got := ledger.Conflicts(Run{}, claim); len(got) != 1 {
		t.Fatalf("first pass found %d conflicts, want 1", len(got))
	}
	if got := ledger.Conflicts(Run{}, claim); len(got) != 0 {
		t.Fatalf("the same conflict was raised twice, so an unchanged answer would loop: %+v", got)
	}
}

// A test run twice legitimately has two timings, and matching EITHER is honest.
func TestMatchingAnyRecordedValueIsEnough(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Run{}, "--- PASS: TestFlaky (0.10s)\n")
	ledger.Record(Run{}, "--- PASS: TestFlaky (9.90s)\n")

	if got := ledger.Conflicts(Run{}, "TestFlaky took 9.90s."); len(got) != 0 {
		t.Errorf("matching the second of two recorded runs was called a conflict: %+v", got)
	}
	if got := ledger.Conflicts(Run{}, "TestFlaky took 45.0s."); len(got) != 1 {
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
	if got := ledger.Record(Run{}, goTestOutput); got != 0 {
		t.Errorf("Record on a nil ledger returned %d", got)
	}
	if got := ledger.Conflicts(Run{}, "TestChattyChild took 4.20s."); got != nil {
		t.Errorf("Conflicts on a nil ledger returned %+v", got)
	}
	// BOTH entry points, since the loop calls this one and not the other.
	if got := ledger.ConflictsAcrossRuns("TestChattyChild took 4.20s."); got != nil {
		t.Errorf("ConflictsAcrossRuns on a nil ledger returned %+v", got)
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
			ledger.Record(Run{}, goTestOutput)
			ledger.Conflicts(Run{}, "nothing to see")
		}()
	}
	wait.Wait()
	if got := ledger.Conflicts(Run{}, "TestChattyChild took 4.20s."); len(got) != 1 {
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
	//
	// THE TWO DURATIONS MUST BE FAR APART. At 0.03s and 0.01s they sit inside
	// tolerance of each other, so a subtest claim matching the PARENT's entry
	// reads as agreement and this test passes whether or not the prefix boundary
	// works — it certified nothing. Five seconds against a hundredth cannot be
	// confused for the same measurement.
	ledger.Record(Run{}, "--- PASS: TestNested (5.00s)\n    --- PASS: TestNested/subcase (0.01s)\n")
	if conflicts := ledger.Conflicts(Run{}, "TestNested/subcase took 0.01s"); len(conflicts) != 0 {
		t.Errorf("an honest subtest claim was reported as a conflict: %+v", conflicts)
	}

	packages := NewLedger()
	packages.Record(Run{}, "ok  \tgithub.com/Gitlawb/zero/internal/agent\t35.58s\nok  \tgithub.com/Gitlawb/zero/internal/agentinit\t1.66s\n")
	if conflicts := packages.Conflicts(Run{}, "github.com/Gitlawb/zero/internal/agentinit took 1.66s"); len(conflicts) != 0 {
		t.Errorf("an honest package claim was reported as a conflict: %+v", conflicts)
	}

	// And the check still bites: a genuinely wrong subtest number is caught, and
	// attributed to the subtest rather than to its parent.
	caught := NewLedger()
	caught.Record(Run{}, "--- PASS: TestNested (5.00s)\n    --- PASS: TestNested/subcase (0.01s)\n")
	conflicts := caught.Conflicts(Run{}, "TestNested/subcase took 4.20s")
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
	ledger.Record(Run{}, "--- PASS: TestSlow (70.00s)\n")
	if conflicts := ledger.Conflicts(Run{}, "TestSlow took 1m10s"); len(conflicts) != 0 {
		t.Errorf("an honest 1m10s claim was reported as a conflict: %+v", conflicts)
	}

	bare := NewLedger()
	bare.Record(Run{}, "--- PASS: TestTwoMinutes (120.00s)\n")
	if conflicts := bare.Conflicts(Run{}, "TestTwoMinutes took 2m"); len(conflicts) != 0 {
		t.Errorf("an honest bare-minute claim was reported as a conflict: %+v", conflicts)
	}

	// Still caught when the minutes really disagree.
	wrong := NewLedger()
	wrong.Record(Run{}, "--- PASS: TestSlow (70.00s)\n")
	conflicts := wrong.Conflicts(Run{}, "TestSlow took 5m00s")
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
	honest.Record(Run{}, "--- PASS: TestChattyChild (0.86s)\n")
	// The package total trails the test's own timing, exactly as `go test` prints it.
	if conflicts := honest.Conflicts(Run{}, "TestChattyChild took 0.86s (package total 1m20s)"); len(conflicts) != 0 {
		t.Errorf("a correct 0.86s claim was reported as a conflict because a later 1m20s was read instead: %+v", conflicts)
	}

	ms := NewLedger()
	ms.Record(Run{}, "--- PASS: TestQuick (0.45s)\n")
	if conflicts := ms.Conflicts(Run{}, "TestQuick took 450ms, well under the 2m budget"); len(conflicts) != 0 {
		t.Errorf("a correct 450ms claim was reported as a conflict: %+v", conflicts)
	}

	// And a seconds figure sitting nearby does not rescue a wrong minute claim
	// when the minute figure is the one being stated.
	wrong := NewLedger()
	wrong.Record(Run{}, "--- PASS: TestSlow (70.00s)\n")
	conflicts := wrong.Conflicts(Run{}, "TestSlow took 5m00s, not the 70s you might expect")
	if len(conflicts) != 1 || conflicts[0].Claimed != 300 {
		t.Errorf("a fabricated 5m00s was not caught as 300s: %+v", conflicts)
	}
}

// A NAME'S DURATION IS THE ONE IN ITS OWN CLAUSE.
//
// Searching the whole remainder of the line let one name take another's number:
// with TestFoo at 0.10s and TestBar at 4.20s recorded, the entirely truthful
// "TestFoo passed; TestBar took 4.20s" reported TestFoo as claiming 4.2. Every
// word of that sentence is correct, and the check called it a fabrication.
func TestADurationBelongsToTheNameBesideIt(t *testing.T) {
	honest := NewLedger()
	honest.Record(Run{}, "--- PASS: TestFoo (0.10s)\n--- PASS: TestBar (4.20s)\n")
	for _, claim := range []string{
		"TestFoo passed; TestBar took 4.20s",
		"TestFoo was fine, TestBar took 4.20s",
		"TestFoo and TestBar both ran; TestBar took 4.20s",
	} {
		if conflicts := honest.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("a truthful claim was reported as a conflict: %q -> %+v", claim, conflicts)
		}
	}

	// The name's OWN number is still read, and a wrong one still caught.
	caught := NewLedger()
	caught.Record(Run{}, "--- PASS: TestFoo (0.10s)\n--- PASS: TestBar (4.20s)\n")
	conflicts := caught.Conflicts(Run{}, "TestFoo took 9.90s; TestBar took 4.20s")
	if len(conflicts) != 1 || conflicts[0].Name != "TestFoo" || conflicts[0].Claimed != 9.9 {
		t.Errorf("a fabricated TestFoo timing beside an honest TestBar one was not caught: %+v", conflicts)
	}
}

// A SECOND, DIFFERENT WRONG NUMBER IS A SECOND THING TO SAY.
//
// Suppressing by name alone switched the check off for that test permanently:
// after one bad 4.20s, a later and differently bad 9.90s was silent. Re-reading
// the same answer must still say nothing, which is what the dedupe is for.
func TestEachWrongValueIsReportedOnce(t *testing.T) {
	ledger := NewLedger()
	ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")

	if got := ledger.Conflicts(Run{}, "TestFoo took 4.20s"); len(got) != 1 {
		t.Fatalf("the first wrong value was not reported: %+v", got)
	}
	if got := ledger.Conflicts(Run{}, "TestFoo took 9.90s"); len(got) != 1 || got[0].Claimed != 9.9 {
		t.Errorf("a second, differently wrong value was swallowed: %+v", got)
	}
	// The SAME wrong value again says nothing, so feeding a correction back
	// cannot loop.
	if got := ledger.Conflicts(Run{}, "TestFoo took 4.20s"); len(got) != 0 {
		t.Errorf("the same conflict was raised twice: %+v", got)
	}
}

// TIMINGS FROM DIFFERENT COMMANDS ARE DIFFERENT MEASUREMENTS.
//
// Pooling every value under the name alone let a claim about an ordinary run be
// satisfied by a number only the -race run produced — and -race is routinely
// several times slower, which is the size of discrepancy this package exists to
// catch.
func TestAClaimIsCheckedAgainstItsOwnRun(t *testing.T) {
	plain := Run{Command: "go", Args: []string{"test", "./..."}}
	race := Run{Command: "go", Args: []string{"test", "-race", "./..."}}

	ledger := NewLedger()
	ledger.Record(plain, "--- PASS: TestSlow (1.00s)\n")
	ledger.Record(race, "--- PASS: TestSlow (9.00s)\n")

	// The race value must not excuse a plain-run claim of 9s.
	conflicts := ledger.Conflicts(plain, "TestSlow took 9.00s")
	if len(conflicts) != 1 {
		t.Fatalf("a plain-run claim borrowed the -race value: %+v", conflicts)
	}
	if len(conflicts[0].Recorded) != 1 || conflicts[0].Recorded[0] != 1.00 {
		t.Errorf("the conflict quoted values from another run: %+v", conflicts[0].Recorded)
	}
	// And the same number IS right for the run that produced it.
	if got := ledger.Conflicts(race, "TestSlow took 9.00s"); len(got) != 0 {
		t.Errorf("a truthful -race claim was reported as a conflict: %+v", got)
	}

	// The nudge names the command, so the model is told which run to re-run.
	nudge := Nudge(conflicts)
	if !strings.Contains(nudge, "go test ./...") {
		t.Errorf("the nudge does not name the run it is about: %q", nudge)
	}
	// A ledger that never distinguishes runs reads exactly as before.
	if label := (Run{}).Label(); label != "" {
		t.Errorf("the zero Run has a label: %q", label)
	}
}

// A CALLER THAT CANNOT NAME THE RUN ASKS A DIFFERENT QUESTION.
//
// The agent loop checks a final answer that may summarise several commands, so
// it has no single run to hold the claim to. Quoting a number one of those
// commands really printed is not invention, and accusing it would be the false
// accusation this package must never produce — so across runs, a value that
// matches ANY of them agrees.
func TestAcrossRunsAcceptsAValueAnyRunPrinted(t *testing.T) {
	plain := Run{Command: "go", Args: []string{"test", "./..."}}
	race := Run{Command: "go", Args: []string{"test", "-race", "./..."}}

	ledger := NewLedger()
	ledger.Record(plain, "--- PASS: TestSlow (1.00s)\n")
	ledger.Record(race, "--- PASS: TestSlow (9.00s)\n")

	// Either number is one this session really printed.
	if got := ledger.ConflictsAcrossRuns("TestSlow took 9.00s"); len(got) != 0 {
		t.Errorf("a value the -race run printed was called a fabrication: %+v", got)
	}
	if got := ledger.ConflictsAcrossRuns("TestSlow took 1.00s"); len(got) != 0 {
		t.Errorf("a value the plain run printed was called a fabrication: %+v", got)
	}
	// A number NO run printed is still caught, and both runs' values are quoted.
	conflicts := ledger.ConflictsAcrossRuns("TestSlow took 45.00s")
	if len(conflicts) != 1 {
		t.Fatalf("a number no run printed was not caught: %+v", conflicts)
	}
	if len(conflicts[0].Recorded) != 2 {
		t.Errorf("the conflict quoted %v; both runs' values belong in it", conflicts[0].Recorded)
	}

	// THE SAME CONFLICT IS RAISED ONCE, however the map happened to iterate.
	//
	// The dedupe key was built from whichever run map iteration picked first, and
	// Go randomises that per range, so a name recorded under two commands was
	// re-reported on a later pass one time in four. The agent loop feeds this
	// back to the model as a correction; raising it again is how that loops.
	//
	// Repeated 200 times because a key that depends on map order fails
	// intermittently, and a single pass would call it fixed.
	for attempt := 0; attempt < 200; attempt++ {
		repeat := NewLedger()
		repeat.Record(plain, "--- PASS: TestSlow (1.00s)\n")
		repeat.Record(race, "--- PASS: TestSlow (9.00s)\n")
		if first := repeat.ConflictsAcrossRuns("TestSlow took 45.00s"); len(first) != 1 {
			t.Fatalf("attempt %d: the first pass did not report: %+v", attempt, first)
		}
		if again := repeat.ConflictsAcrossRuns("TestSlow took 45.00s"); len(again) != 0 {
			t.Fatalf("attempt %d: the same conflict was raised twice: %+v", attempt, again)
		}
	}

	// AND THE RUN IT QUOTES IS THE ONE THAT RECORDED THE NAME, named here rather
	// than checked for being non-empty.
	//
	// THIS IS NOT THE DETERMINISM CHECK, though it used to be dressed as one:
	// 200 identical passes collected the label into a set and required the set to
	// hold one entry. TestOnlyPlain is recorded by a single run, so a single
	// candidate exists however the map iterates and the set could not hold two.
	// It was vacuous by accident before that — the same name under both runs
	// makes the report a merged one, and merged reports drop the label, so it
	// watched an empty string — and vacuous by construction after the repair.
	// Determinism is asserted where something can actually reshuffle, in
	// TestTheReportIsIdenticalBetweenIdenticalPasses.
	stable := NewLedger()
	stable.Record(plain, "--- PASS: TestOnlyPlain (1.00s)\n")
	stable.Record(race, "--- PASS: TestOnlyRace (9.00s)\n")
	reported := stable.ConflictsAcrossRuns("TestOnlyPlain took 45.00s")
	if len(reported) != 1 || reported[0].Run.Label() != "go test ./..." {
		t.Fatalf("a single-run conflict must quote the command that recorded it: %+v", reported)
	}

	// THE TWO ENTRY POINTS KEEP SEPARATE BOOKS, deliberately. They answer
	// different questions, so neither suppresses the other — asking both on one
	// ledger reports the same number twice, once per question. No caller does:
	// the agent loop and the specialist each use ConflictsAcrossRuns, and the
	// per-run form is for a caller that knows its command. Pinned because it is a
	// real consequence of the split rather than an accident.
	shared := NewLedger()
	shared.Record(plain, "--- PASS: TestSlow (1.00s)\n")
	if got := shared.Conflicts(plain, "TestSlow took 45.00s"); len(got) != 1 {
		t.Errorf("the per-run question was not answered: %+v", got)
	}
	if got := shared.ConflictsAcrossRuns("TestSlow took 45.00s"); len(got) != 1 {
		t.Errorf("the cross-run question was suppressed by the per-run one: %+v", got)
	}

	// And the strict, per-run check still holds a claim to its own run — that is
	// the whole difference between the two entry points.
	strict := NewLedger()
	strict.Record(plain, "--- PASS: TestSlow (1.00s)\n")
	strict.Record(race, "--- PASS: TestSlow (9.00s)\n")
	if got := strict.Conflicts(plain, "TestSlow took 9.00s"); len(got) != 1 {
		t.Errorf("the per-run check accepted another run's value: %+v", got)
	}
}

// A NEIGHBOUR BOUNDS THE CLAUSE WHETHER OR NOT IT WAS MEASURED.
//
// Cutting only at names the ledger knows left an unrecorded neighbour holding
// the number, and the name before it took the blame:
//
//	recorded: TestFoo 0.10s
//	claim:    "TestFoo passed; TestUnrecorded took 4.20s"
//	  -> [{Name:TestFoo Claimed:4.2 Recorded:[0.1]}]
//
// Whether a number belongs to a name cannot depend on whether some OTHER name
// happened to be measured this session. Name shape and clause separators bound
// it too, and all three only ever shorten the search — each can cost a
// detection, none can invent one.
func TestAnUnrecordedNeighbourStillEndsTheClause(t *testing.T) {
	for _, claim := range []string{
		"TestFoo passed; TestUnrecorded took 4.20s",
		"TestFoo passed and TestUnrecorded took 4.20s",
		"TestFoo was fine, BenchmarkThing took 4.20s",
		// Not name-shaped at all — the separator is what ends this one.
		"TestFoo passed, the suite took 4.20s",
		// NO SEPARATOR ANYWHERE. These can only be stopped by the name-shape
		// bound, which is the point: every case above contains a comma, a
		// semicolon or an " and ", so they all passed while nextNameShaped was
		// returning -1 for every realistic input and the dead branch looked
		// alive. A bound that only its neighbours can certify is not covered.
		"TestFoo passed TestUnrecorded took 4.20s",
		"TestFoo was fine BenchmarkThing took 4.20s",
		"TestFoo ok FuzzParse took 4.20s",
		"TestFoo ok Example_usage took 4.20s",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("a truthful claim was blamed for a neighbour's number: %q -> %+v", claim, conflicts)
		}
	}

	// The name's own number is still read, with a neighbour present and without.
	for _, claim := range []string{
		"TestFoo took 9.90s",
		"TestFoo took 9.90s and TestBar took 1.00s",
		"TestFoo took 9.90s; TestUnrecorded took 4.20s",
		"TestFoo took 9.90s TestBar took 1.00s",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")
		conflicts := ledger.Conflicts(Run{}, claim)
		if len(conflicts) != 1 || conflicts[0].Claimed != 9.9 {
			t.Errorf("a fabricated number beside a neighbour was not caught: %q -> %+v", claim, conflicts)
		}
	}

	// "testing", "tested" and a bare "test" are ordinary words, not names, and
	// must not cut the clause short.
	for _, claim := range []string{
		"TestFoo took 9.90s after testing the parser",
		"TestFoo took 9.90s, tested twice",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 1 {
			t.Errorf("an ordinary word beginning with a name prefix ended the clause: %q -> %+v", claim, conflicts)
		}
	}

	// A package name is a name too, and its own claim still lands.
	packages := NewLedger()
	packages.Record(Run{}, "ok  \tgithub.com/x/y\t8.00s\n")
	if conflicts := packages.Conflicts(Run{}, "github.com/x/y took 30.00s"); len(conflicts) != 1 {
		t.Errorf("a package claim stopped being read: %+v", conflicts)
	}
}

// A FULL STOP ENDS A CLAUSE.
//
// The separator list had no sentence terminator, so when the next sentence's
// subject was an ordinary noun phrase — not a recorded name, not name-shaped —
// nothing bounded the clause and its number was charged to the previous
// sentence's test. Both sentences true, both numbers really measured, each
// attached by the writer to the right subject, and the report accused of
// fabricating one of them.
func TestASentenceTerminatorEndsTheClause(t *testing.T) {
	for _, claim := range []string{
		"TestNested is green. The full run took 34.249s.",
		"TestNested passed! The full run took 34.249s.",
		"TestNested is green? The full run took 34.249s.",
		"TestNested ok: package total 34.249s.",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestNested (0.03s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("the next sentence's number was charged to this test: %q -> %+v", claim, conflicts)
		}
	}

	// A DECIMAL POINT IS NOT A TERMINATOR, and neither is the dot in an import
	// path — otherwise this bound would silence every claim it is meant to read.
	for _, tc := range []struct {
		recorded, claim string
		want            float64
	}{
		{"--- PASS: TestNested (0.03s)\n", "TestNested took 9.90s.", 9.9},
		{"--- PASS: TestNested (0.03s)\n", "TestNested took 9.90s. The full run took 34.249s.", 9.9},
		{"ok  \tgithub.com/x/y\t8.00s\n", "github.com/x/y took 30.00s.", 30},
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, tc.recorded)
		conflicts := ledger.Conflicts(Run{}, tc.claim)
		if len(conflicts) != 1 || conflicts[0].Claimed != tc.want {
			t.Errorf("a fabricated number stopped being read: %q -> %+v", tc.claim, conflicts)
		}
	}

	// The first duration in the clause still wins, so an honest report that gives
	// its own timing before the total is untouched.
	honest := NewLedger()
	honest.Record(Run{}, "--- PASS: TestNested (0.03s)\n")
	if conflicts := honest.Conflicts(Run{}, "TestNested passed in 0.03s. The full run took 34.249s."); len(conflicts) != 0 {
		t.Errorf("an honest report carrying both numbers was flagged: %+v", conflicts)
	}
}

// A COMMAND IS NAMED ONLY WHEN THAT COMMAND PRINTED ALL OF THESE VALUES.
//
// ConflictsAcrossRuns merges every run's values for a name. Labelling that union
// with one run said the command reported a number it never printed —
// "`go test ./a` in this session reported 0.1s, 0.2s" where 0.2s came only from
// ./b. Choosing the run deterministically fixed the reshuffling and left the
// attribution just as untrue.
func TestAMergedResultIsNotAttributedToOneCommand(t *testing.T) {
	merged := NewLedger()
	merged.Record(Run{Command: "go", Args: []string{"test", "./a"}}, "--- PASS: TestFoo (0.10s)\n")
	merged.Record(Run{Command: "go", Args: []string{"test", "./b"}}, "--- PASS: TestFoo (0.20s)\n")
	conflicts := merged.ConflictsAcrossRuns("TestFoo took 9.90s")
	if len(conflicts) != 1 {
		t.Fatalf("expected one conflict, got %+v", conflicts)
	}
	if label := conflicts[0].Run.Label(); label != "" {
		t.Errorf("values from two runs were attributed to %q", label)
	}
	if nudge := Nudge(conflicts); strings.Contains(nudge, "go test ./a") || strings.Contains(nudge, "go test ./b") {
		t.Errorf("the nudge names one command for a merged set: %q", nudge)
	}

	// One run behind the values keeps the label, which is the useful case: the
	// model is told exactly which command to re-run.
	single := NewLedger()
	single.Record(Run{Command: "go", Args: []string{"test", "./a"}}, "--- PASS: TestFoo (0.10s)\n")
	one := single.ConflictsAcrossRuns("TestFoo took 9.90s")
	if len(one) != 1 || one[0].Run.Label() != "go test ./a" {
		t.Errorf("a single-run result lost its command label: %+v", one)
	}
}

// CLAUSE PUNCTUATION IS A CLOSED SET, and the plain hyphen was missing from it.
//
// Walking the shapes rather than reasoning about them found five that leaked,
// not one: the ASCII hyphen everyone types, both typographic dashes, a
// parenthetical and a pipe. Each let a following clause's number be charged to
// the name in front of it.
func TestEveryClausePunctuationEndsTheClause(t *testing.T) {
	for _, claim := range []string{
		"TestChattyChild passed. the suite took 34.249s.",
		"TestChattyChild passed; the suite took 34.249s.",
		"TestChattyChild passed: the suite took 34.249s.",
		"TestChattyChild passed, the suite took 34.249s.",
		"TestChattyChild passed - the suite took 34.249s.",
		"TestChattyChild passed -- the suite took 34.249s.",
		"TestChattyChild passed — the suite took 34.249s.",
		"TestChattyChild passed – the suite took 34.249s.",
		"TestChattyChild passed (the suite took 34.249s)",
		"TestChattyChild passed | suite 34.249s",
		"TestChattyChild - the suite took 34.249s.",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestChattyChild (0.86s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("a following clause's number was charged to this test: %q -> %+v", claim, conflicts)
		}
	}
}

// PUNCTUATION ALONE DOES NOT END A CLAUSE — A NEW SUBJECT DOES.
//
// Treating the punctuation itself as the boundary cuts a test's own number away
// from its name, which silences a real fabrication. "TestFoo (9.99s)" and
// "TestFoo passed - 9.99s" are ordinary ways of stating one test's timing; what
// makes the same punctuation a break is a subject named after it.
//
// The last two were MISSED before this rule existed, because the comma and colon
// were already unconditional separators — so this recovers detections rather than
// only adding bounds.
func TestPunctuationCarryingThisTestsOwnNumberIsNotABoundary(t *testing.T) {
	for _, claim := range []string{
		"TestChattyChild (9.99s)",
		"TestChattyChild - 9.99s",
		"TestChattyChild — 9.99s",
		"TestChattyChild | 9.99s",
		"TestChattyChild took 9.99s",
		"TestChattyChild passed in 9.99s",
		"TestChattyChild passed (9.99s)",
		"TestChattyChild passed - 9.99s",
		"TestChattyChild passed, 9.99s",
		"TestChattyChild passed: 9.99s",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestChattyChild (0.86s)\n")
		conflicts := ledger.Conflicts(Run{}, claim)
		if len(conflicts) != 1 || conflicts[0].Claimed != 9.99 {
			t.Errorf("a test's own number stopped being read: %q -> %+v", claim, conflicts)
		}
	}
}

// HOURS COUNT, for the reason minutes did one round earlier. Without an hour
// form "1h10m0s" matched only its minute remainder and read as 600s, so a
// truthful restatement of a recorded 4200s was reported as a fabrication.
//
// The hour form is its OWN pattern rather than an optional prefix on the minute
// one: making every part optional lets the expression match the EMPTY string,
// which regexp finds at offset 0 ahead of any real duration — that version read
// "1h10m0s" as 0s, which is worse than the bug it was fixing.
func TestAnHourDurationIsReadWhole(t *testing.T) {
	for claim, want := range map[string]float64{
		" took 1h10m0s": 4200,
		" took 1h":      3600,
		" took 2h30m":   9000,
		" took 1h30s":   3630,
		" took 1h2m3s":  3723,
		// The forms that already worked must keep working — an hour pattern that
		// swallows these would trade one fabrication for another.
		" took 1m10s":   70,
		" took 2m":      120,
		" took 0.86s":   0.86,
		" took 450ms":   0.45,
		" took 34.249s": 34.249,
	} {
		if got, ok := parseClaimedDuration(claim); !ok || got != want {
			t.Errorf("parseClaimedDuration(%q) = %v, %v; want %v", claim, got, ok, want)
		}
	}

	honest := NewLedger()
	honest.Record(Run{}, "--- PASS: TestVerySlow (4200.00s)\n")
	if conflicts := honest.Conflicts(Run{}, "TestVerySlow took 1h10m0s"); len(conflicts) != 0 {
		t.Errorf("a truthful 1h10m0s claim was reported as a conflict: %+v", conflicts)
	}
	wrong := NewLedger()
	wrong.Record(Run{}, "--- PASS: TestVerySlow (4200.00s)\n")
	if conflicts := wrong.Conflicts(Run{}, "TestVerySlow took 9h"); len(conflicts) != 1 || conflicts[0].Claimed != 32400 {
		t.Errorf("a fabricated 9h was not caught as 32400s: %+v", conflicts)
	}
}

// A DURATION THIS PACKAGE CAN PARSE MUST BE ONE THE CLAUSE SCAN CAN SEE.
//
// separatorBreaksClause locates the next duration to decide whether a separator
// introduces a new subject, and it did not know the hour form. So it read the
// "h" of "9h" as the first letter of a new subject, turned the punctuation into
// a clause boundary, and cut the test's own number away from its name. Four
// ordinary spellings were missed; only the separator-free "took 9h" survived.
func TestTheClauseScanSeesTheHourForm(t *testing.T) {
	for _, claim := range []string{
		"TestVerySlow - 9h",
		"TestVerySlow passed - 9h",
		"TestVerySlow (9h)",
		"TestVerySlow: 9h",
		"TestVerySlow took 9h",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestVerySlow (4200.00s)\n")
		conflicts := ledger.Conflicts(Run{}, claim)
		if len(conflicts) != 1 || conflicts[0].Claimed != 32400 {
			t.Errorf("a fabricated hour figure was not read: %q -> %+v", claim, conflicts)
		}
	}

	// And the bounds still bind: an hour figure belonging to another subject
	// stays that subject's, and a truthful hour restatement is not a conflict.
	bleed := NewLedger()
	bleed.Record(Run{}, "--- PASS: TestVerySlow (4200.00s)\n")
	if conflicts := bleed.Conflicts(Run{}, "TestVerySlow passed - the whole suite took 9h"); len(conflicts) != 0 {
		t.Errorf("another subject's hour figure was charged to this test: %+v", conflicts)
	}
	honest := NewLedger()
	honest.Record(Run{}, "--- PASS: TestVerySlow (4200.00s)\n")
	if conflicts := honest.Conflicts(Run{}, "TestVerySlow - 1h10m0s"); len(conflicts) != 0 {
		t.Errorf("a truthful 1h10m0s restatement was reported as a conflict: %+v", conflicts)
	}
}

// A DECIMAL DURATION IS ONE NUMBER, NOT ITS REMAINDER. The minute and hour
// components accepted integers only, so neither pattern could match "1.5m" at
// the digit it starts on — the leftmost match began after the point instead and
// read the claim as 5 minutes. "0.5m", half a minute, read as five, and "10.25h"
// as twenty-five hours.
//
// That is the exact failure this package exists to prevent, in its own parser: a
// model that truthfully restated a recorded 90s as "1.5m" was accused of
// fabricating a number, and the nudge quoted 300s back at it — a figure nothing
// in the run ever produced.
func TestADecimalDurationIsReadWhole(t *testing.T) {
	for _, c := range []struct {
		recorded string
		seconds  float64
		claim    string
	}{
		{"--- PASS: TestNinety (90.00s)\n", 90, "TestNinety took 1.5m"},
		{"--- PASS: TestHalf (30.00s)\n", 30, "TestHalf took 0.5m"},
		{"--- PASS: TestLong (5400.00s)\n", 5400, "TestLong took 1.5h"},
		{"--- PASS: TestVeryLong (36900.00s)\n", 36900, "TestVeryLong took 10.25h"},
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, c.recorded)
		if conflicts := ledger.Conflicts(Run{}, c.claim); len(conflicts) != 0 {
			t.Errorf("an honest claim %q against %vs was reported as a conflict: %+v", c.claim, c.seconds, conflicts)
		}
	}

	// AND THE UNITS THAT WERE ALREADY RIGHT STAY RIGHT. "1.5ms" must not become
	// a minute claim: the word boundary after "m" is what kept milliseconds out
	// of the minute pattern, and widening the number must not cost that.
	milli := NewLedger()
	milli.Record(Run{}, "--- PASS: TestQuick (0.0015s)\n")
	if conflicts := milli.Conflicts(Run{}, "TestQuick took 1.5ms"); len(conflicts) != 0 {
		t.Errorf("an honest 1.5ms claim was read as minutes: %+v", conflicts)
	}

	// STILL CAUGHT WHEN A DECIMAL CLAIM REALLY DISAGREES, and reported as the
	// number that was written. 4.5m is 270s; under the old patterns it matched
	// its remainder and came back as 300, so asserting the reported value — not
	// merely that something was reported — is what distinguishes a whole read
	// from a lucky one. (2.5m would also disagree, but 150s against a recorded
	// 90s sits inside the deliberate 50% tolerance band and is not a conflict.)
	wrong := NewLedger()
	wrong.Record(Run{}, "--- PASS: TestNinety (90.00s)\n")
	if conflicts := wrong.Conflicts(Run{}, "TestNinety took 4.5m"); len(conflicts) != 1 || conflicts[0].Claimed != 270 {
		t.Errorf("a fabricated 4.5m was not caught as 270s: %+v", conflicts)
	}
}

// A Ledger that was declared rather than constructed still has to behave. The
// nil receiver is already handled; a zero value got past that guard and panicked
// with "assignment to entry in nil map" on its first Record.
func TestAZeroValueLedgerRecordsWithoutPanicking(t *testing.T) {
	var ledger Ledger
	if n := ledger.Record(Run{}, "--- PASS: TestSomething (1.25s)\n"); n != 1 {
		t.Errorf("a zero-value ledger recorded %d measurements, want 1", n)
	}
	if conflicts := ledger.Conflicts(Run{}, "TestSomething took 1.25s"); len(conflicts) != 0 {
		t.Errorf("a zero-value ledger reported a conflict against its own record: %+v", conflicts)
	}
}

// THE AGREEING CLAIM ABOVE NEVER REACHES THE MAP THAT PANICS. `raised` is
// written only when a contradiction is actually found — it is the dedupe that
// stops the same wrong number being reported twice — so a test that asks a
// truthful claim returns early and proves nothing about it. The first fix for
// the zero-value Ledger initialised the two maps Record touches and left
// `raised` nil, and this test passed anyway. Reported by @jatmn.
//
// Both conflict entry points are covered: they write different key shapes into
// the same map, so one of them holding says nothing about the other.
func TestAZeroValueLedgerSurvivesAContradiction(t *testing.T) {
	var single Ledger
	single.Record(Run{}, "--- PASS: TestSomething (1.25s)\n")
	conflicts := single.Conflicts(Run{}, "TestSomething took 99s")
	if len(conflicts) != 1 || conflicts[0].Claimed != 99 {
		t.Errorf("a zero-value ledger did not report the contradiction: %+v", conflicts)
	}
	// And the dedupe it just wrote actually works, which is what that map is for.
	if again := single.Conflicts(Run{}, "TestSomething took 99s"); len(again) != 0 {
		t.Errorf("the same wrong number was reported twice: %+v", again)
	}

	var across Ledger
	across.Record(Run{}, "--- PASS: TestSomething (1.25s)\n")
	if conflicts := across.ConflictsAcrossRuns("TestSomething took 99s"); len(conflicts) != 1 {
		t.Errorf("a zero-value ledger did not report the contradiction across runs: %+v", conflicts)
	}
}

// DETERMINISM IS ASSERTED WHERE SOMETHING CAN ACTUALLY RESHUFFLE.
//
// Both entry points build their report by ranging over maps, and Go randomises
// that order per range, so the sort at the end of each is the only thing keeping
// two identical passes from producing two different texts. This text reaches a
// model, where a message that reshuffles between passes is a diff nobody can
// read — the reason the sorts are there at all.
//
// Nothing observed them. The assertion that claimed to was written against a
// name only one run had recorded, which leaves one candidate however the map
// iterates; deleting BOTH sorts left the whole suite green over twenty runs. One
// name cannot catch an ordering, so this uses four and pins the exact report:
// name, value, quoted command and recorded values, in order.
func TestTheReportIsIdenticalBetweenIdenticalPasses(t *testing.T) {
	plain := Run{Command: "go", Args: []string{"test", "./..."}}
	race := Run{Command: "go", Args: []string{"test", "-race", "./..."}}
	const fromPlain = "--- PASS: TestAlpha (1.00s)\n--- PASS: TestBravo (2.00s)\n--- PASS: TestShared (1.00s)\n"
	const fromRace = "--- PASS: TestCharlie (3.00s)\n--- PASS: TestShared (9.00s)\n"
	const claim = "TestAlpha took 45.00s; TestBravo took 46.00s; TestCharlie took 47.00s; TestShared took 48.00s"

	report := func(conflicts []Conflict) string {
		var b strings.Builder
		for _, conflict := range conflicts {
			fmt.Fprintf(&b, "%s=%v@%q%v|", conflict.Name, conflict.Claimed, conflict.Run.Label(), conflict.Recorded)
		}
		return b.String()
	}

	// TestShared is recorded by BOTH runs deliberately: its label is dropped as
	// untrue of either one, so the ordering has a fourth distinct rendering to
	// get wrong rather than three that differ only in their command.
	const wantAcross = `TestAlpha=45@"go test ./..."[1]|TestBravo=46@"go test ./..."[2]|TestCharlie=47@"go test -race ./..."[3]|TestShared=48@""[1 9]|`
	const wantPerRun = `TestAlpha=45@"go test ./..."[1]|TestBravo=46@"go test ./..."[2]|TestShared=48@"go test ./..."[1]|`

	// A fresh ledger each pass: the dedupe is per-Ledger, so a reused one would
	// report nothing after the first attempt and the loop would assert on empty.
	for attempt := 0; attempt < 200; attempt++ {
		across := NewLedger()
		across.Record(plain, fromPlain)
		across.Record(race, fromRace)
		if got := report(across.ConflictsAcrossRuns(claim)); got != wantAcross {
			t.Fatalf("attempt %d: the cross-run report is not what identical passes must produce:\n got %s\nwant %s", attempt, got, wantAcross)
		}

		perRun := NewLedger()
		perRun.Record(plain, fromPlain)
		perRun.Record(race, fromRace)
		if got := report(perRun.Conflicts(plain, claim)); got != wantPerRun {
			t.Fatalf("attempt %d: the per-run report is not what identical passes must produce:\n got %s\nwant %s", attempt, got, wantPerRun)
		}
	}
}

// THE ORDER THE MERGE WALKS IS PINNED HERE BECAUSE NOTHING DOWNSTREAM CAN SEE IT.
//
// sortedRunKeys decides which run a merged sighting keeps. A name recorded by
// several runs has its label dropped as untrue of any one of them, and a name
// recorded by one run has a single candidate, so every assertion about the
// report passes whatever order this returns — reversing it, or deleting the sort
// inside it, leaves the suite green. That is the shape of bound this package has
// already shipped twice: alive-looking and certifying nothing. It is asserted
// directly instead, so it stays true for the next caller that does depend on it.
func TestTheRunOrderTheMergeWalksIsStable(t *testing.T) {
	runs := []Run{
		{},
		{Command: "go", Args: []string{"test", "-race", "./..."}},
		{Command: "go", Args: []string{"test", "./..."}},
		{Command: "go", Args: []string{"test", "./internal/agent"}},
		{Command: "go", Args: []string{"test", "./..."}, Dir: "/w"},
	}
	observed := map[string]map[string][]float64{}
	for _, run := range runs {
		observed[run.key()] = map[string][]float64{"TestFoo": {1}}
	}
	// Byte order of the keys, written out rather than computed with the function
	// under test: the zero run first, then "-race" ahead of "./..." because the
	// hyphen sorts below the dot, and the directory-qualified key last.
	want := []string{
		runs[0].key(), runs[1].key(), runs[2].key(), runs[3].key(), runs[4].key(),
	}

	for attempt := 0; attempt < 200; attempt++ {
		got := sortedRunKeys(observed)
		if len(got) != len(want) {
			t.Fatalf("attempt %d: got %d keys, want %d", attempt, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attempt %d: key %d is %q, want %q — the merge walks the runs in map order", attempt, i, got[i], want[i])
			}
		}
	}
}

// A SUBJECT THAT FOLLOWS ITS OWN NUMBER STILL OWNS IT.
//
// The subject rule scanned only what came BEFORE the next duration, so a
// separator with the figure first and its subject second looked like plain
// presentation — the shape a table or an aside uses for a test's own timing — and
// the neighbouring number was charged to the name in front of it:
//
//	recorded: TestFoo 0.10s
//	claim:    "TestFoo passed; 4.20s was the whole suite."
//	  -> [{Name:TestFoo Claimed:4.2 Recorded:[0.1]}]
//
// Every word of that claim is true, and 4.20s belongs to the suite named
// immediately after it. Which side of a figure its subject sits on says nothing
// about who owns it.
func TestASubjectFollowingItsNumberEndsTheClause(t *testing.T) {
	for _, claim := range []string{
		"TestFoo passed; 4.20s was the whole suite.",
		"TestFoo passed - 4.20s was the package total",
		"TestFoo passed | 4.20s for the whole package",
		"TestFoo ok: 4.20s across every package",
		"TestFoo passed (4.20s for the suite)",
		"TestFoo was fine, 4.20s covered every package",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("a following subject's number was charged to this test: %q -> %+v", claim, conflicts)
		}
	}

	// AND THE PRESENTATION FORMS STILL READ, which is what the trailing scan
	// stopping at the end of the figure's own segment buys: the words in the next
	// table cell and the next sentence are not that figure's subject, and cutting
	// there would silence a real fabrication.
	for _, claim := range []string{
		"TestFoo (9.90s)",
		"TestFoo passed, 9.90s",
		"| TestFoo | 9.90s | passes |",
		"TestFoo passed, 9.90s. The suite took 34.249s.",
		"TestFoo passed (9.90s) and the suite took 34.249s",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")
		conflicts := ledger.Conflicts(Run{}, claim)
		if len(conflicts) != 1 || conflicts[0].Claimed != 9.9 {
			t.Errorf("a test's own number stopped being read: %q -> %+v", claim, conflicts)
		}
	}
}

// AN "m" THAT IS NOT A DURATION IS NOT MINUTES.
//
// The minute pattern matches any figure with an "m" and a word boundary after
// it, and position decides between the forms, so a count of rows won over the
// timing standing beside it:
//
//	recorded: TestParseCorpus 0.86s
//	claim:    "TestParseCorpus handled 5m rows in 0.86s"
//	  -> [{Name:TestParseCorpus Claimed:300 Recorded:[0.86]}]
//
// The report is truthful and the nudge quotes 300s back at it, a number its
// answer never contained — this package's own failure mode, in its own parser.
//
// An ambiguous bare unit now reports NOTHING rather than a second-choice figure,
// because reaching past it would decide the same question by guessing. The cost
// is asserted below too: "took 2m to finish" is unreadable, and a figure
// fabricated in that spelling goes uncaught.
func TestANonDurationUnitIsNotReadAsMinutes(t *testing.T) {
	for _, claim := range []string{
		"TestParseCorpus handled 5m rows in 0.86s",
		"TestParseCorpus processed 5m tokens",
		"TestParseCorpus scanned 12m records and passed",
		"TestParseCorpus walked 5m lines",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestParseCorpus (0.86s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("a count was read as minutes and a truthful report accused of it: %q -> %+v", claim, conflicts)
		}
	}

	// The exact readings, so a change that trades one wrong number for another
	// cannot pass. A compound form cannot be a count and a bare figure with no
	// word after it has no noun to be counting, so both still read.
	for _, c := range []struct {
		tail    string
		seconds float64
		ok      bool
	}{
		{" handled 5m rows in 0.86s", 0, false},
		{" 5m rows", 0, false},
		{" 9h of wall time", 0, false},
		{" took 2m to finish", 0, false},
		{" took 2m", 120, true},
		{" took 5m.", 300, true},
		{" (5m)", 300, true},
		{" | 2m |", 120, true},
		{" took 1m10s to finish", 70, true},
		{" took 2h30m of wall time", 9000, true},
		{" took 0.86s over 5m rows", 0.86, true},
	} {
		got, ok := parseClaimedDuration(c.tail)
		if got != c.seconds || ok != c.ok {
			t.Errorf("parseClaimedDuration(%q) = %v, %v; want %v, %v", c.tail, got, ok, c.seconds, c.ok)
		}
	}

	// And a minute figure that really is one is still caught, whole.
	caught := NewLedger()
	caught.Record(Run{}, "--- PASS: TestSlow (70.00s)\n")
	if conflicts := caught.Conflicts(Run{}, "TestSlow took 5m"); len(conflicts) != 1 || conflicts[0].Claimed != 300 {
		t.Errorf("a bare minute figure with no word after it stopped being read: %+v", conflicts)
	}
}

// AND THE CLAUSE SCAN REFUSES WHAT THE PARSER REFUSES.
//
// The scan locates the next duration to decide whether a separator introduces a
// new subject, so the two have to agree about what a duration is — the hour form
// was added to it for that reason, and an ambiguous bare unit is the same
// requirement from the other side. While the scan still counted "5m" as a
// duration, it found no word before it, read the punctuation as presentation,
// and handed the count to the name in front as a timing the parser itself would
// have refused:
//
//	recorded: TestFoo 0.10s
//	claim:    "TestFoo passed; 5m and the suite took 4.20s"
//	  -> [{Name:TestFoo Claimed:300 Recorded:[0.1]}]
//
// A conjunction directly after the figure is what exposes it: the trailing scan
// stops at that separator, so the words beyond it cannot end the clause either,
// and the ambiguous token is the only thing standing between the name and a
// number that is not its own.
func TestTheClauseScanRefusesWhatTheParserRefuses(t *testing.T) {
	for _, claim := range []string{
		"TestFoo passed; 5m and the suite took 4.20s",
		"TestFoo passed, 5m and the whole run took 4.20s",
		"TestFoo passed: 5m but the suite took 4.20s",
		"TestFoo passed | 5m while the suite took 4.20s",
		"TestFoo passed (5m and the suite took 4.20s)",
		"TestFoo passed; 5m though the suite took 4.20s",
		// The hour form the same way, at 32400s rather than 300s.
		"TestFoo ok; 9h and the suite took 4.20s",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "--- PASS: TestFoo (0.10s)\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("the clause scan read a token the parser refuses: %q -> %+v", claim, conflicts)
		}
	}
}

// A DURATION IS READ WHOLE OR NOT AT ALL.
//
// Three unanchored regexes each hunted for their own suffix with no shared left
// boundary, so a failed outer match restarted inside the same token. Every one of
// these was a truthful claim turned into a fabricated correction — the single
// failure this package exists to prevent. Reported by @jatmn.
func TestADurationTokenIsReadWholeOrRefused(t *testing.T) {
	for _, honest := range []struct {
		recorded string
		claim    string
	}{
		{"--- PASS: TestQ (0.86s)\n", "TestQ took .86s"},         // was read as 86s
		{"--- PASS: TestQ (90.00s)\n", "TestQ took .5m"},         // was read as 300s
		{"--- PASS: TestQ (1.20s)\n", "TestQ took 1,200ms"},      // was read as 0.2s
		{"--- PASS: TestQ (70.01s)\n", "TestQ took 1m10ms"},      // was read as 0.01s
		{"--- PASS: TestQ (3660.50s)\n", "TestQ took 1h1m500ms"}, // was read as 0.5s
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, honest.recorded)
		if conflicts := ledger.Conflicts(Run{}, honest.claim); len(conflicts) != 0 {
			t.Errorf("an honest claim %q was accused: %+v", honest.claim, conflicts)
		}
	}

	// AND THE FORMS THAT DO PARSE STILL PARSE, so refusing partial tokens has not
	// simply made the detector blind.
	for _, readable := range []struct {
		recorded string
		claim    string
	}{
		{"--- PASS: TestQ (0.86s)\n", "TestQ took 0.86s"},
		{"--- PASS: TestQ (70.00s)\n", "TestQ took 1m10s"},
		{"--- PASS: TestQ (0.05s)\n", "TestQ took 50ms"},
		{"--- PASS: TestQ (4200.00s)\n", "TestQ took 1h10m0s"},
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, readable.recorded)
		if conflicts := ledger.Conflicts(Run{}, readable.claim); len(conflicts) != 0 {
			t.Errorf("a valid duration %q stopped being read: %+v", readable.claim, conflicts)
		}
	}

	// A real fabrication is still caught.
	wrong := NewLedger()
	wrong.Record(Run{}, "--- PASS: TestQ (0.86s)\n")
	if conflicts := wrong.Conflicts(Run{}, "TestQ took 9.00s"); len(conflicts) != 1 || conflicts[0].Claimed != 9 {
		t.Errorf("a fabricated 9.00s was not caught as 9: %+v", conflicts)
	}
}

// EVERY TIMED MENTION IS CHECKED, not the first that parsed.
//
// claimedSecondsFor returned at its first successful occurrence, so an agreeing
// mention shielded every later one and "TestFoo took 1.00s; TestFoo later took
// 9.00s" reported nothing against a recorded 1s. That also contradicted the
// ledger's per-VALUE dedupe, which exists so two different wrong numbers are two
// findings. Reported by @jatmn.
func TestEveryTimedMentionOfANameIsChecked(t *testing.T) {
	for _, c := range []struct {
		label string
		claim string
		want  []float64
	}{
		{"agreeing then wrong", "TestFoo took 1.00s; TestFoo later took 9.00s", []float64{9}},
		{"wrong then agreeing", "TestFoo took 9.00s; TestFoo actually took 1.00s", []float64{9}},
		{"two distinct wrong", "TestFoo took 9.00s; TestFoo took 20.00s", []float64{9, 20}},
		{"repeated equivalent", "TestFoo took 9.00s; TestFoo took 9.00s", []float64{9}},
		{"all agreeing", "TestFoo took 1.00s; TestFoo took 1.00s", nil},
	} {
		for _, entry := range []struct {
			name string
			run  func(*Ledger) []Conflict
		}{
			{"per-run", func(l *Ledger) []Conflict { return l.Conflicts(Run{}, c.claim) }},
			{"across-runs", func(l *Ledger) []Conflict { return l.ConflictsAcrossRuns(c.claim) }},
		} {
			ledger := NewLedger()
			ledger.Record(Run{}, "--- PASS: TestFoo (1.00s)\n")
			got := entry.run(ledger)
			if len(got) != len(c.want) {
				t.Errorf("%s/%s: %d conflicts, want %d: %+v", entry.name, c.label, len(got), len(c.want), got)
				continue
			}
			for i, want := range c.want {
				if got[i].Claimed != want {
					t.Errorf("%s/%s: conflict %d claimed %v, want %v", entry.name, c.label, i, got[i].Claimed, want)
				}
			}
		}
	}
}

// AN UNRECORDED PACKAGE BOUNDS A CLAUSE, exactly as an unrecorded test-shaped
// name already did.
//
// Package neighbours were only recognised when that package had itself been
// recorded, so a truthful sentence charged an unrecorded neighbour's figure
// backwards to the first package. Whether a neighbouring subject ends a clause
// cannot depend on whether that neighbour happened to produce a parseable
// timing. Reported by @jatmn.
func TestAnUnrecordedPackageNeighbourBoundsTheClause(t *testing.T) {
	for _, claim := range []string{
		"github.com/x/first passed github.com/x/unrecorded took 4.20s",
		"github.com/x/first passed, github.com/x/unrecorded took 4.20s",
		"github.com/x/first ok; github.com/x/unrecorded took 4.20s",
	} {
		ledger := NewLedger()
		ledger.Record(Run{}, "ok  \tgithub.com/x/first\t0.10s\n")
		if conflicts := ledger.Conflicts(Run{}, claim); len(conflicts) != 0 {
			t.Errorf("a neighbour's figure was charged to the first package: %q -> %+v", claim, conflicts)
		}
	}
	// THE CONTROL: when the duration really is the first package's, it still reads.
	ledger := NewLedger()
	ledger.Record(Run{}, "ok  \tgithub.com/x/first\t0.10s\n")
	if conflicts := ledger.Conflicts(Run{}, "github.com/x/first took 4.20s"); len(conflicts) != 1 {
		t.Errorf("a genuine package fabrication stopped being caught: %+v", conflicts)
	}
}
