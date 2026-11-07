package specialist

import "testing"

// WHOLE WORDS. Substring matching — the obvious implementation, and the one the
// source plan specified — fires "fix" on "prefix", "add" on "address" and "test"
// on "latest". A task misclassified by an accident of spelling is billed to the
// wrong model and nothing downstream can tell.
func TestClassificationMatchesWholeWordsNotSubstrings(t *testing.T) {
	for prompt, want := range map[string]TaskRole{
		"find the prefix used by the parser":      TaskRoleScan,
		"report the address format in use":        TaskRoleDefault,
		"summarise the latest release notes":      TaskRoleDefault,
		"fix the parser":                          TaskRoleImplement,
		"list every caller":                       TaskRoleScan,
		"review the change for correctness":       TaskRoleVerify,
		"describe how the scheduler is organised": TaskRoleDefault,
	} {
		if got := classifyTaskRole(Task{ID: "t", Prompt: prompt}); got != want {
			t.Errorf("%q → %s, want %s", prompt, got, want)
		}
	}
}

// VERIFY BEFORE IMPLEMENT. "verify the fix" and "review the change" contain
// implement words and are not implement tasks; the reverse order sends every
// review of a change to the coding model instead of the judging one.
func TestVerifyOutranksImplementWords(t *testing.T) {
	for _, prompt := range []string{
		"verify the fix landed correctly",
		"review the change to the scheduler",
		"check that the rename is complete",
	} {
		if got := classifyTaskRole(Task{ID: "t", Prompt: prompt}); got != TaskRoleVerify {
			t.Errorf("%q → %s, want verify", prompt, got)
		}
	}
}

// THE GRANT OUTRANKS THE PROSE. A prompt is an intention; a write grant is a
// fact. A task holding write_file will write whatever its prompt says.
func TestAWriteGrantOverridesThePrompt(t *testing.T) {
	task := Task{ID: "t", Prompt: "find every caller and list them", Tools: []string{"read_file", "write_file"}}
	if got := classifyTaskRole(task); got != TaskRoleImplement {
		t.Errorf("a task granted write_file classified as %s, want implement", got)
	}
	readOnly := Task{ID: "t", Prompt: "find every caller and list them", Tools: []string{"read_file"}}
	if got := classifyTaskRole(readOnly); got != TaskRoleScan {
		t.Errorf("a read-only scan classified as %s, want scan", got)
	}
}

// INFLECTED VERBS ARE THE NORMAL CASE. Real prompts say "auditing", "finding",
// "writing" — not the bare stem. Matching exact forms only meant the intended
// verb was invisible and whichever incidental word appeared decided the role: a
// real run classified nine read-only audit tasks as "implement".
func TestInflectedVerbsClassifyOnTheirIntent(t *testing.T) {
	for prompt, want := range map[string]TaskRole{
		"You are auditing package pkg/execprofile":            TaskRoleVerify,
		"You are finding duplicated logic inside pkg/reltime": TaskRoleScan,
		"reviewing the change for correctness":                TaskRoleVerify,
		"searching for every caller":                          TaskRoleScan,
		"creating the missing helper":                         TaskRoleImplement,
		"removing the dead branch":                            TaskRoleImplement,
		"writing the migration":                               TaskRoleImplement,
		"checked the invariants hold":                         TaskRoleVerify,
		"lists every exported symbol":                         TaskRoleScan,
	} {
		if got := classifyTaskRole(Task{ID: "t", Prompt: prompt}); got != want {
			t.Errorf("%q → %s, want %s", prompt, got, want)
		}
	}
}

// AND THE TRAP THE WHOLE-WORD RULE EXISTED FOR STAYS CLOSED. Prefix matching
// would have been the obvious repair and reopens it: "add" is a prefix of
// "address", "test" of "latest". Inflection matching enumerates the suffixes
// English uses and nothing else.
func TestInflectionMatchingDoesNotReopenTheSubstringTrap(t *testing.T) {
	for prompt, want := range map[string]TaskRole{
		"report the address format used by the parser": TaskRoleDefault,
		"summarise the latest release notes":           TaskRoleDefault,
		"describe the prefix convention":               TaskRoleDefault,
	} {
		if got := classifyTaskRole(Task{ID: "t", Prompt: prompt}); got != want {
			t.Errorf("%q → %s, want %s", prompt, got, want)
		}
	}
	for _, tc := range []struct {
		word, keyword string
		want          bool
	}{
		{"auditing", "audit", true},
		{"audits", "audit", true},
		{"audited", "audit", true},
		{"writing", "write", true},
		{"removing", "remove", true},
		{"changed", "change", true},
		{"address", "add", false},
		{"latest", "test", false},
		{"prefix", "fix", false},
		{"finder", "find", false},
	} {
		if got := matchesKeyword(tc.word, tc.keyword); got != tc.want {
			t.Errorf("matchesKeyword(%q, %q) = %v, want %v", tc.word, tc.keyword, got, tc.want)
		}
	}
}
