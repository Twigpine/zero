package specialist

import "sync/atomic"

// PostureGate is the shared, mutable answer to "is the zeromaxing posture on?".
//
// It exists because none of the simpler options work here:
//
//   - A func() bool closing over the TUI model is WRONG. model is a VALUE type
//     (every handler takes and returns `m model`), so a closure created at
//     registration captures a copy frozen at that instant and would report the
//     posture as it was when the session started, forever.
//   - Re-registering the tool on posture change is WRONG for the same reason
//     decision 2 rejected it earlier, and worse here: the TUI clones the
//     registry per run (cloneToolRegistry) but the clone copies tool POINTERS,
//     so a replacement registered into the session registry would not reach a
//     run already holding a clone.
//   - A pointer to shared state is what actually survives both: the tool holds
//     one gate pointer for the process's life, every clone shares it, and a
//     posture flip is visible to the next call with no re-registration.
//
// atomic.Bool rather than a mutex because the TUI writes it from the update
// loop while a run's tool dispatch reads it from the agent goroutine, and this
// is a single flag with no invariant spanning other fields.
type PostureGate struct {
	active atomic.Bool
}

// Set records whether the posture is currently on.
func (gate *PostureGate) Set(on bool) {
	if gate != nil {
		gate.active.Store(on)
	}
}

// Active reports the posture. Nil-safe and false by default, so a caller that
// never wires the gate gets the posture-off behaviour — the fail-safe direction
// for a tool that spends a token budget.
func (gate *PostureGate) Active() bool {
	return gate != nil && gate.active.Load()
}
