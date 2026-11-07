package specialist

import (
	"regexp"
	"strings"
)

// TaskRole is what a plan task is FOR, used to pick a model proportionate to the
// work. A label for spending, not for execution: nothing in the scheduler,
// the grant intersection or the budget reads it.
type TaskRole string

const (
	// TaskRoleScan reads and reports. Cheap, high-volume, no judgement.
	TaskRoleScan TaskRole = "scan"
	// TaskRoleImplement changes something.
	TaskRoleImplement TaskRole = "implement"
	// TaskRoleVerify judges work that already exists — the stage where a weaker
	// model costs the most, because a wrong "looks fine" is indistinguishable
	// from a right one.
	TaskRoleVerify TaskRole = "verify"
	// TaskRoleDefault is the honest answer when the heuristic cannot tell, and
	// means "inherit the parent's model".
	TaskRoleDefault TaskRole = "default"
)

// taskWordBoundary splits a prompt into words on anything that is not a letter
// or digit.
//
// WHOLE WORDS, and it is not pedantry. Substring matching — which is what the
// obvious implementation does — fires "implement" on "pre**fix**", "scan" on
// "**add**ress", and "verify" on "la**test**". A task classified by an accident
// of spelling gets silently billed to the wrong model, and nothing downstream
// can tell that happened.
var taskWordBoundary = regexp.MustCompile(`[^a-z0-9]+`)

var (
	implementWords = wordSet("write", "edit", "create", "refactor", "fix", "change",
		"add", "remove", "modify", "implement", "rename", "delete", "update", "patch")
	// The DECIDE family is here because a real plan asked tasks to "decide
	// whether a race exists" and "decide whether a project config can raise a
	// user limit" — the two hardest tasks in it — and neither word was listed, so
	// both fell through to default and inherited whatever the session was using.
	// Judging is the role where a weak model costs the most; the list has to
	// contain the words people actually use for it.
	verifyWords = wordSet("review", "verify", "check", "audit", "assess", "critique",
		"validate", "judge", "confirm", "compare", "decide", "determine", "conclude",
		"evaluate", "diagnose", "inspect", "prove", "disprove")
	scanWords = wordSet("find", "grep", "search", "list", "read", "scan", "locate",
		"identify", "enumerate", "map", "inventory")
)

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

// classifyTaskRole guesses what a task is for.
//
// THE GRANT OUTRANKS THE PROSE, and that ordering is the only part of this that
// is not a guess. A task holding write_file will write whatever its prompt says;
// a prompt is an intention and a grant is a fact, so when they disagree the fact
// wins.
//
// Everything below the grant is a heuristic and is allowed to be wrong — which
// is why TaskRoleDefault exists rather than a forced choice, and why the whole
// feature is off unless asked for.
func classifyTaskRole(task Task) TaskRole {
	for _, tool := range task.Tools {
		if planWriteTools[strings.TrimSpace(tool)] {
			return TaskRoleImplement
		}
	}

	words := promptWords(task.Prompt)
	// Verify is tested BEFORE implement: "verify the fix" and "review the change"
	// contain implement words and are not implement tasks. The reverse order
	// would send every review of a change to the coding model.
	switch {
	case anyWord(words, verifyWords):
		return TaskRoleVerify
	case anyWord(words, implementWords):
		return TaskRoleImplement
	case anyWord(words, scanWords):
		return TaskRoleScan
	default:
		return TaskRoleDefault
	}
}

func promptWords(prompt string) map[string]bool {
	found := map[string]bool{}
	for _, word := range taskWordBoundary.Split(strings.ToLower(prompt), -1) {
		if word != "" {
			found[word] = true
		}
	}
	return found
}

func anyWord(words, set map[string]bool) bool {
	for word := range words {
		for keyword := range set {
			if matchesKeyword(word, keyword) {
				return true
			}
		}
	}
	return false
}

// matchesKeyword matches a keyword and its ordinary inflections.
//
// EXACT FORMS ALONE DO NOT WORK. Real prompts say "auditing", "finding",
// "writing", "reviewing" — and matching only "audit", "find", "write", "review"
// missed every one of them, so the intended verb was ignored and whichever
// incidental word happened to appear decided the role. A real run classified
// nine read-only audit tasks as "implement" that way.
//
// The obvious repair — prefix matching — reintroduces exactly what whole-word
// matching was for: "add" is a prefix of "address", "test" of "latest". So this
// enumerates the suffixes English actually uses, including the dropped-e forms
// (write → writing, remove → removing), and nothing else. "address" is not
// "add" plus any of them.
func matchesKeyword(word, keyword string) bool {
	if word == keyword {
		return true
	}
	for _, suffix := range []string{"s", "es", "ed", "d", "ing"} {
		if word == keyword+suffix {
			return true
		}
	}
	if stem, dropped := strings.CutSuffix(keyword, "e"); dropped {
		for _, suffix := range []string{"ing", "ed", "es"} {
			if word == stem+suffix {
				return true
			}
		}
	}
	return false
}
