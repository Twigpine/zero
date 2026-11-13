package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// recentEdits extracts each mutated file's path and a one-line note from the
// matching tool result, latest note per path in last-seen order.
func TestRecentEditsExtractsPathsAndNotes(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
			{ID: "e1", Name: "write_file", Arguments: `{"path":"internal/foo.go","content":"package foo"}`},
		}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "e1", Content: "Wrote internal/foo.go (12 lines)"},
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
			{ID: "e2", Name: "edit_file", Arguments: `{"path":"internal/bar.go","old_string":"a","new_string":"b"}`},
		}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "e2", Content: "Applied edit to internal/bar.go"},
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
			{ID: "e3", Name: "apply_patch", Arguments: `{"patch":"*** Update File: internal/baz.go"}`},
		}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "e3", Content: "Done!", ChangedFiles: []string{"internal/baz.go"}},
	}

	edits := recentEdits(messages)
	if len(edits) != 3 {
		t.Fatalf("expected 3 edited files, got %d: %#v", len(edits), edits)
	}
	if edits[0].name != "internal/foo.go" || !strings.Contains(edits[0].body, "12 lines") {
		t.Fatalf("first edit = %#v, want foo.go with its note", edits[0])
	}
	if edits[1].name != "internal/bar.go" || !strings.Contains(edits[1].body, "Applied edit") {
		t.Fatalf("second edit = %#v, want bar.go with its note", edits[1])
	}
	if edits[2].name != "internal/baz.go" || edits[2].body != "Done!" {
		t.Fatalf("third edit = %#v, want apply_patch changed file with its note", edits[2])
	}
}

// After compaction elides the editing turns, the preserved-state block still
// names the edited files and what changed, so the model needn't re-read them.
func TestCompactionPreservesRecentEdits(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system"},
		{Role: zeroruntime.MessageRoleUser, Content: "add a flag"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "editing", ToolCalls: []zeroruntime.ToolCall{
			{ID: "e1", Name: "write_file", Arguments: `{"path":"cmd/main.go","content":"..."}`},
		}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "e1", Content: "Wrote cmd/main.go (adds --version flag)"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "done"},
		{Role: zeroruntime.MessageRoleUser, Content: "continue"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "continuing"},
	}
	summary := compactStateConversation(t, messages)

	state := parsePreservedStateBlock(summary)
	if len(state.RecentEdits) != 1 {
		t.Fatalf("expected 1 preserved edit, got %#v", state.RecentEdits)
	}
	if state.RecentEdits[0].Path != "cmd/main.go" || !strings.Contains(state.RecentEdits[0].Note, "--version") {
		t.Fatalf("preserved edit = %#v, want cmd/main.go + its note", state.RecentEdits[0])
	}
}

// A fresh edit note for a path overrides the one carried from an earlier
// compaction (newer wins) and moves the path to the newest position rather than
// duplicating it or leaving it pinned to its old slot.
func TestRecentEditsMergeNewerWins(t *testing.T) {
	prior := preservedState{RecentEdits: []preservedEdit{{Path: "a.go", Note: "old note"}, {Path: "z.go", Note: "z"}}}
	older := preservedEditsToEntries(prior.RecentEdits)
	newer := []skillEntry{{name: "a.go", body: "new note"}, {name: "b.go", body: "added"}}

	merged := mergeRecentEdits(older, newer)
	// z.go (untouched) keeps its lead; a.go is re-edited so it takes the new note
	// and moves behind z.go, ahead of the brand-new b.go: [z.go, a.go, b.go].
	if len(merged) != 3 {
		t.Fatalf("expected z.go + refreshed a.go + b.go, got %#v", merged)
	}
	if merged[0].name != "z.go" {
		t.Fatalf("untouched z.go should stay first, got %#v", merged)
	}
	if merged[1].name != "a.go" || merged[1].body != "new note" {
		t.Fatalf("re-edited a.go should move to the newest edits with the new note, got %#v", merged[1])
	}
	if merged[2].name != "b.go" {
		t.Fatalf("brand-new b.go should be last, got %#v", merged[2])
	}
}

// Regression: once more than maxRecentEdits distinct files are tracked, re-editing
// an early file must move it into the capped tail rather than leaving it to be
// dropped — otherwise the next compaction omits exactly the file the model most
// recently touched.
func TestRecentEditsCapKeepsReeditedFile(t *testing.T) {
	// Earlier compaction preserved exactly maxRecentEdits files, f0 the oldest.
	older := make([]skillEntry, 0, maxRecentEdits)
	for i := 0; i < maxRecentEdits; i++ {
		older = append(older, skillEntry{name: fmt.Sprintf("f%d.go", i), body: "old"})
	}
	// Fresh window re-edits the OLDEST file (f0) and adds a brand-new file, so the
	// merged list exceeds the cap and something must be dropped.
	newer := []skillEntry{{name: "f0.go", body: "re-edited"}, {name: "fresh.go", body: "new"}}

	merged := capRecentEdits(mergeRecentEdits(older, newer))
	if len(merged) != maxRecentEdits {
		t.Fatalf("cap should keep %d entries, got %d", maxRecentEdits, len(merged))
	}

	var f0 *skillEntry
	for i := range merged {
		if merged[i].name == "f0.go" {
			f0 = &merged[i]
		}
		if merged[i].name == "f1.go" {
			t.Fatalf("f1.go (now the least-recently touched) should have been dropped, got %#v", merged)
		}
	}
	if f0 == nil {
		t.Fatalf("re-edited f0.go must survive the cap, got %#v", merged)
	}
	if f0.body != "re-edited" {
		t.Fatalf("surviving f0.go should carry the fresh note, got %q", f0.body)
	}
}
