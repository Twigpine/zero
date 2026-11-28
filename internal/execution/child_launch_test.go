package execution

import "testing"

// scriptedReport is an adapter report whose answer changes over time, the way a
// real one does: the file is opened empty, decodes to an error while nothing has
// been written, and only later carries the fact.
type scriptedReport struct {
	launched bool
	reads    int
	err      error
}

func (script *scriptedReport) read() (AdapterReport, error) {
	script.reads++
	if script.err != nil {
		return AdapterReport{}, script.err
	}
	if !script.launched {
		// What an empty or half-written file decodes to. NOT childLaunched=false,
		// which would be the adapter stating an outcome.
		return AdapterReport{}, nil
	}
	launched := true
	return AdapterReport{ChildLaunched: &launched}, nil
}

func trackerOver(script *scriptedReport) (*ChildLaunchTracker, func()) {
	return NewChildLaunchTracker(PreparedCommand{
		ChildLaunchOwnedByAdapter: true,
		Report:                    script.read,
	})
}

// A NEGATIVE READ IS NOT AN OUTCOME UNTIL THE ADAPTER IS DONE.
//
// This is the whole point of the type. Reading the report between the adapter
// creating the child and recording it answers "nothing published", and caching
// that answer freezes "not yet" into "never" for the rest of the session.
func TestAnUnsettledNegativeIsNotRemembered(t *testing.T) {
	script := &scriptedReport{}
	tracker, _ := trackerOver(script)

	if tracker.Launched() {
		t.Fatal("SETUP INVALID: an unpublished report answered launched, so there is no negative here to cache")
	}
	if tracker.Settled() {
		t.Fatal("a read that found nothing settled the decision; the adapter may still be about to publish")
	}

	// The adapter publishes, as it does a moment after CreateProcessAsUser.
	script.launched = true
	if !tracker.Launched() {
		t.Fatal("the launch published after the first read was never seen; the earlier negative was cached")
	}
	if !tracker.Settled() {
		t.Fatal("a confirmed launch left the decision open")
	}
}

// And once it IS an outcome it stays one, so the answer is monotonic and two
// consumers of the same prepared command cannot disagree.
func TestASettledLaunchIsNeverWithdrawn(t *testing.T) {
	script := &scriptedReport{launched: true}
	tracker, cleanup := trackerOver(script)

	if !tracker.Launched() {
		t.Fatal("SETUP INVALID: a published report did not answer launched")
	}
	// Cleanup deletes the report in production, which is what a later read would
	// find. The decision must not follow it.
	script.launched = false
	cleanup()
	if !tracker.Launched() {
		t.Fatal("the decision followed the report file into deletion; a server that ran became one that did not")
	}
}

// CLEANUP SETTLES BEFORE IT DESTROYS THE EVIDENCE.
//
// The report is a file the plan's cleanup removes. A consumer that asks after
// cleanup used to read an absent report and answer "no child" about a server that
// really did run, which is why the decision was being taken early and hitting the
// unsettled window instead.
func TestCleanupSettlesBeforeReleasingThePlan(t *testing.T) {
	script := &scriptedReport{launched: true}
	var order []string
	tracker, cleanup := NewChildLaunchTracker(PreparedCommand{
		ChildLaunchOwnedByAdapter: true,
		Report: func() (AdapterReport, error) {
			order = append(order, "read")
			return script.read()
		},
		Cleanup: func() { order = append(order, "release") },
	})

	cleanup()
	if len(order) != 2 || order[0] != "read" || order[1] != "release" {
		t.Fatalf("cleanup order = %v, want the report read before the plan is released", order)
	}
	if !tracker.Launched() {
		t.Fatal("the launch published just before cleanup was lost; nothing read the report on the way out")
	}
}

// An adapter that exits having published nothing settles negative, and that
// answer is final: this is the case the unsettled rule must not swallow.
func TestAnAdapterThatPublishedNothingSettlesNegative(t *testing.T) {
	script := &scriptedReport{}
	tracker, cleanup := trackerOver(script)
	cleanup()

	if !tracker.Settled() {
		t.Fatal("the adapter finished without the decision becoming final")
	}
	if tracker.Launched() {
		t.Fatal("an adapter that published nothing was credited with a launch")
	}
	// And a report that changes after the adapter is gone changes nothing.
	script.launched = true
	if tracker.Launched() {
		t.Fatal("a settled decision was reopened by a later read")
	}
}

// Direct evidence outranks the report and needs no settlement, because nothing
// that happens later can make a child that answered not have run.
func TestConfirmIsTerminalWithoutAReport(t *testing.T) {
	script := &scriptedReport{}
	tracker, cleanup := trackerOver(script)

	tracker.Confirm()
	if !tracker.Launched() || !tracker.Settled() {
		t.Fatal("an observed child did not settle the decision")
	}
	cleanup()
	if !tracker.Launched() {
		t.Fatal("settling from cleanup overwrote an observed launch with an absent report")
	}
}

// A plan whose started process IS the requested command has nothing to decide,
// and must not be made to wait on a report it will never have.
func TestAPlanTheAdapterDoesNotOwnIsLaunchedFromTheStart(t *testing.T) {
	tracker, _ := NewChildLaunchTracker(PreparedCommand{ChildLaunchOwnedByAdapter: false})
	if !tracker.Launched() || !tracker.Settled() {
		t.Fatal("a directly started command was not treated as launched")
	}
}

// A report that fails to read is the unsettled state, not a negative outcome.
func TestADecodeErrorIsNotAnAnswer(t *testing.T) {
	script := &scriptedReport{err: errScriptedReportBroken}
	tracker, _ := trackerOver(script)

	if tracker.Launched() {
		t.Fatal("a broken report was read as a launch")
	}
	if tracker.Settled() {
		t.Fatal("a broken report settled the decision; a half-written file is a moment, not an outcome")
	}
}

var errScriptedReportBroken = errScriptedReport("unexpected end of JSON input")

type errScriptedReport string

func (e errScriptedReport) Error() string { return string(e) }
