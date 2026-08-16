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
	ledger := NewLedger()
	ledger.Record(Run{}, goTestOutput)

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

	// AND THE RUN IT QUOTES IS STABLE. This text reaches a model; naming a
	// different command between identical passes is the same problem the sorted
	// output exists to avoid.
	labels := map[string]bool{}
	for attempt := 0; attempt < 200; attempt++ {
		stable := NewLedger()
		stable.Record(plain, "--- PASS: TestSlow (1.00s)\n")
		stable.Record(race, "--- PASS: TestSlow (9.00s)\n")
		reported := stable.ConflictsAcrossRuns("TestSlow took 45.00s")
		if len(reported) != 1 {
			t.Fatalf("attempt %d: expected one conflict, got %+v", attempt, reported)
		}
		labels[reported[0].Run.Label()] = true
	}
	if len(labels) != 1 {
		t.Errorf("the quoted run varies between identical passes: %v", labels)
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
