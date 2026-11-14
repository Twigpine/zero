package specialist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Where a plan's tasks leave their full answers for each other.
//
// THE PROBLEM IT SOLVES. A dependent task is handed an EXCERPT of what its
// dependencies found — bounded, because a twenty-task chain inheriting every
// ancestor's full output is a context-overflow machine. Measured: a code-review
// child produced 18,349 characters and a dependent saw 4,000 of them. The other
// 78% existed, was paid for, and was unreachable.
//
// THE DESIGN THAT AVOIDS EVERY TRAP. The obvious build — give tasks a shared
// directory and let them write to it — fails twice over in this codebase:
//
//   - A READ-ONLY PLAN TASK HAS NO WRITE TOOL. planReadOnlyTools is read_file,
//     grep, glob, list_directory, lsp_navigate. Handing those tasks a scratchpad
//     to write into hands them something they cannot use, and the plans that most
//     need this — parallel finders feeding a synthesiser — are exactly the
//     read-only ones.
//   - GRANTING THEM ONE CHANGES WHAT A READ-ONLY PLAN IS. RequiresIsolation()
//     keys on precisely `!planReadOnlyTools[name]`, so adding a write tool would
//     silently make every scratchpad-using plan demand a git worktree.
//
// So tasks are not ASKED to write here. The EXECUTOR records each result,
// single-threaded, as it comes back — and dependents read with the read_file
// they already hold. The excerpt stops being lossy and becomes a summary with
// the whole thing one tool call behind it.
//
// WHAT IS AND IS NOT GUARANTEED. The directory sits under the OS temp dir, which
// is a default sandbox write root, so a task granted a write tool CAN write here:
// it can list the directory, and edit or delete a record after the executor
// wrote it. Nothing binds a record to the task that produced it, so this is not
// an integrity boundary — the dependent's prompt still carries the real excerpt
// inline, and a task holding write_file over the shared worktree already holds a
// larger target. A read-only plan's tasks cannot write here at all.
//
// What IS guaranteed is that the executor — the unsandboxed parent — never
// writes through something a task planted: Record creates each file with
// O_EXCL, so a file, symlink or hard link already at a task's destination makes
// the record fail instead of redirecting the write.
//
// DISPOSABLE, and deliberately not memory: memory is durable, user-facing and
// believed by later sessions. This is one plan's working notes and it is deleted
// when the plan ends.

// scratchpadTaskIDPattern is an ALLOW-LIST, because a task id becomes a
// filename. Enumerating what is permitted is the rule this repo already applies
// to plan names and memory names; a deny-list of traversal sequences is the
// pattern that has leaked here repeatedly.
var scratchpadTaskIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Scratchpad is one plan run's directory of task outputs.
//
// A nil *Scratchpad is a working value: every method tolerates it and does
// nothing, so a plan that never got one behaves exactly as it did before this
// existed.
type Scratchpad struct {
	root string
}

// NewScratchpad creates a plan's directory.
//
// UNDER THE OS TEMP DIR, which is already a default sandbox write root on every
// platform (defaultTempWriteRootCandidatesForGOOS: /tmp and $TMPDIR on unix,
// TEMP/TMP on Windows). So the executor can write here without the plan holding
// any grant it did not already have — and the tasks still need the root added to
// their own scope to READ it, which is what PlanTaskRequest.ScratchpadRoot is
// for.
func NewScratchpad(planName string) (*Scratchpad, error) {
	root, err := os.MkdirTemp("", "zero-plan-"+sanitizeScratchpadName(planName)+"-")
	if err != nil {
		return nil, fmt.Errorf("create a scratchpad for plan %q: %w", planName, err)
	}
	return &Scratchpad{root: root}, nil
}

// Root is the directory, or "" when there is none.
func (pad *Scratchpad) Root() string {
	if pad == nil {
		return ""
	}
	return pad.root
}

// Record persists one task's FULL output and returns the path to it.
//
// Returns "" with no error when there is no scratchpad or nothing to record —
// the caller then simply mentions nothing, rather than pointing a dependent at a
// file that does not exist.
func (pad *Scratchpad) Record(taskID, output string) (string, error) {
	if pad == nil || pad.root == "" || strings.TrimSpace(output) == "" {
		return "", nil
	}
	if !scratchpadTaskIDPattern.MatchString(taskID) {
		// A task id that cannot be spelled as a filename is not an error worth
		// failing the plan for — the task still ran and its excerpt still
		// reaches its dependents. It simply gets no file.
		return "", nil
	}
	path := filepath.Join(pad.root, taskID+".md")
	// EXCLUSIVE CREATE. Each task is recorded exactly once, so anything already
	// at the destination was put there by someone else — and following it would
	// let a planted symlink or hard link aim the parent's write at any file it
	// can reach. O_EXCL refuses every such case, a dangling symlink included.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("record task %q output: %w", taskID, err)
	}
	_, writeErr := file.WriteString(output)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("record task %q output: %w", taskID, errors.Join(writeErr, closeErr))
	}
	return path, nil
}

// Release deletes the whole directory. Safe to call twice and on nil, because
// it runs from a defer on a path that also has an error return.
func (pad *Scratchpad) Release() {
	if pad == nil || pad.root == "" {
		return
	}
	_ = os.RemoveAll(pad.root)
	pad.root = ""
}

// sanitizeScratchpadName keeps a plan's name out of the path unless it is
// obviously safe. The temp-file suffix guarantees uniqueness either way, so
// dropping an awkward name costs only readability.
func sanitizeScratchpadName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, name)
	if len(cleaned) > 32 {
		cleaned = cleaned[:32]
	}
	if cleaned == "" {
		return "plan"
	}
	return cleaned
}

// scratchpadPointer is the line appended to a truncated excerpt.
//
// ONLY WHEN TRUNCATED. A dependent that already received the whole answer has
// nothing to go and read, and a path in every briefing is noise that trains the
// reader to ignore the one that matters.
func scratchpadPointer(path string, fullLength int) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf(
		"[The excerpt above is truncated. The COMPLETE output of this task — %d characters — is at %s. "+
			"Read that file if you need more than the excerpt, rather than re-deriving it.]",
		fullLength, path)
}
