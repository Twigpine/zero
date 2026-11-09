package execution

import "sync"

// ChildLaunchTracker is the one monotonic answer to "did the requested child
// run", shared by every consumer of a prepared command.
//
// THE REPORT HAS THREE STATES, NOT TWO. For a plan whose child is created inside
// an adapter, the report file passes through: not settled yet, settled with no
// child, and child created. An absent file, an empty file the adapter opened but
// has not written, a half-written one, and a decode error are all the FIRST
// state, and collapsing them to "no child" is a claim about a process that may be
// running at that instant. Consumers used to make that collapse independently and
// then compensate for the timing on their own, which is why fixing one
// presentation site kept exposing the next.
//
// Two things make an answer terminal here:
//
//   - Confirm, when the consumer has observed the child directly. A stdio MCP
//     server answering initialize is that observation: the adapter speaks no MCP,
//     so a well-formed response can only have come from the requested child.
//   - Settle, when the adapter process itself has exited. After that the report
//     will never change, so whatever it says, including nothing, is the truth.
//
// Settle runs from Cleanup, BEFORE the report file is deleted. Reading the
// evidence and then destroying it in one step is what lets a consumer ask after
// close and still get an answer, rather than racing a deletion it does not
// control.
//
// The decision only ever moves from unknown to known and never back, so two
// consumers of the same prepared command cannot disagree, and a retry cannot turn
// a launch that happened into one that did not.
type ChildLaunchTracker struct {
	mu       sync.Mutex
	settled  bool
	launched bool

	ownedByAdapter bool
	report         func() (AdapterReport, error)
}

// NewChildLaunchTracker builds the tracker for a prepared command and returns it
// with a Cleanup that settles before releasing the plan's resources.
//
// The returned cleanup is the one the caller must use. Calling the prepared
// command's own Cleanup instead deletes the report while the decision is still
// unknown, which is the ordering hazard this type exists to remove.
func NewChildLaunchTracker(prepared PreparedCommand) (*ChildLaunchTracker, func()) {
	tracker := &ChildLaunchTracker{
		ownedByAdapter: prepared.ChildLaunchOwnedByAdapter,
		report:         prepared.Report,
	}
	// A plan whose child is the started process needs no evidence: Start
	// succeeding IS the launch, so the answer is terminal from the beginning.
	if !tracker.ownedByAdapter {
		tracker.settled = true
		tracker.launched = true
	}
	inner := prepared.Cleanup
	return tracker, func() {
		tracker.Settle()
		if inner != nil {
			inner()
		}
	}
}

// Confirm latches a launch the consumer observed for itself.
//
// Positive evidence is always terminal. Nothing that happens later can make a
// child that answered not have run, so this needs no settlement and cannot be
// undone by a subsequent Settle finding no report.
func (tracker *ChildLaunchTracker) Confirm() {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.settled = true
	tracker.launched = true
}

// Settle freezes the answer because the adapter has finished.
//
// One last read first: the adapter may have published between the consumer's
// last look and its exit, and this is the only remaining chance to see it.
func (tracker *ChildLaunchTracker) Settle() {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.settled {
		return
	}
	tracker.launched = tracker.readReportLocked()
	tracker.settled = true
}

// Launched reports the decision so far.
//
// Before settlement this reads the report and answers true only for a published
// launch. A negative answer here is NOT remembered, because the adapter may still
// be between creating the child and recording it; the next caller asks again.
// Consumers that need a final answer settle first, which Cleanup does for them.
func (tracker *ChildLaunchTracker) Launched() bool {
	if tracker == nil {
		return false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.settled {
		return tracker.launched
	}
	if tracker.readReportLocked() {
		tracker.launched = true
		tracker.settled = true
		return true
	}
	return false
}

// Settled reports whether the answer is final, for callers that would otherwise
// present an unknown as a fact.
func (tracker *ChildLaunchTracker) Settled() bool {
	if tracker == nil {
		return false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.settled
}

// readReportLocked answers only the positive case. A missing file, an empty one,
// a partial write and a decode error are all "nothing published yet", which is
// the state this type refuses to record as an outcome.
func (tracker *ChildLaunchTracker) readReportLocked() bool {
	if tracker.report == nil {
		return false
	}
	report, err := tracker.report()
	if err != nil {
		return false
	}
	return ResolveChildLaunched(true, tracker.ownedByAdapter, report)
}
