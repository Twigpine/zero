package specialist

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// taskIdentity is a stable fingerprint of everything about a task that changes
// what it does or how it runs. Two tasks with the same identity are the same
// unit of work — so a completed one can be resumed as done — and a task whose
// identity differs from the one recorded when it ran is a DIFFERENT unit that
// must run again, along with everything that depends on it.
//
// WHAT IS IN, AND WHY EACH — this is invariant 10 (a key that omits a field that
// varies serves one result for many): a fingerprint that dropped any field below
// would let an edited task resume a stale result.
//   - Prompt: the instruction. Editing it is the whole reason to re-run.
//   - Model: a different model may answer differently; a resume must not replay
//     one model's answer for another's.
//   - Tools: the authority grant bounds what the task may do, so it bounds the
//     result. Sorted — the grant is a set, not a sequence.
//   - DependsOn: the edge set is part of the task's input, because it decides
//     which briefings the task receives. Adding or removing a dependency changes
//     the task even when the dependencies themselves did not re-run. Sorted, for
//     the same reason as Tools.
//
// WHAT IS OUT, AND WHY:
//   - ID is the MATCH KEY, not part of the fingerprint: identity answers "is the
//     same-id task still the same work?", so folding id in would make every task
//     match only itself and defeat edit detection.
//   - Phase is a display and ordering label with no execution semantics (see
//     Task.Phase) — relabelling it must not force a re-run.
//   - Per-task reasoning effort is not a Task field today. If one is ever added
//     it MUST join this list, or a raised-effort resume replays the lower-effort
//     result. This comment is the tripwire for that.
//
// The encoding is LENGTH-DELIMITED so field values cannot alias into one
// another: {"ab","c"} and {"a","bc"} must not collapse to one fingerprint.
func taskIdentity(task Task) string {
	hash := sha256.New()
	write := func(tag, value string) {
		hash.Write([]byte(tag))
		hash.Write([]byte{0})
		hash.Write([]byte(strconv.Itoa(len(value))))
		hash.Write([]byte{0})
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	write("prompt", task.Prompt)
	write("model", task.Model)

	tools := append([]string(nil), task.Tools...)
	sort.Strings(tools)
	for _, tool := range tools {
		write("tool", tool)
	}

	deps := append([]string(nil), task.DependsOn...)
	sort.Strings(deps)
	for _, dep := range deps {
		write("dep", dep)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
