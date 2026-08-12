package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/memory"
)

func memoryTestPaths(t *testing.T) memory.Paths {
	t.Helper()
	return memory.DefaultPaths(t.TempDir())
}

// THE DELETE BLOCKER. An omitted or whitespace-only content used to fall through
// to Forget, so a call that merely left the field out destroyed the note —
// behind an approval whose text says only that this tool SAVES. The save tool
// must now refuse such a call rather than treat it as a deletion.
func TestMemoryWriteRefusesToDeleteThroughAnEmptyContent(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeLocal, "precious", "d", "keep me"); err != nil {
		t.Fatal(err)
	}
	write := NewMemoryWriteTool(paths)

	for _, args := range []map[string]any{
		{"name": "precious"},                     // content omitted entirely
		{"name": "precious", "content": ""},      // empty
		{"name": "precious", "content": " \n\t"}, // whitespace only
	} {
		result := write.Run(context.Background(), args)
		if result.Status != StatusError {
			t.Errorf("memory_write(%v) succeeded; an empty payload must not be a deletion: %q", args, result.Output)
		}
		if strings.Contains(strings.ToLower(result.Output), "forgot") {
			t.Errorf("memory_write(%v) deleted the note: %q", args, result.Output)
		}
	}
	// And the note is still there.
	if _, err := memory.Read(paths, memory.ScopeLocal, "precious"); err != nil {
		t.Fatalf("the note was destroyed by a save-shaped call: %v", err)
	}
}

// Deletion lives in its own tool so its approval text can say so. The safety
// disclosure is the point: a grant on "saves a note" must not authorise removal.
func TestMemoryForgetIsASeparateToolThatDisclosesDeletion(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeLocal, "doomed", "d", "bye"); err != nil {
		t.Fatal(err)
	}
	forget := NewMemoryForgetTool(paths)
	if forget.Name() == NewMemoryWriteTool(paths).Name() {
		t.Fatal("forget and write share a name, so they would share one approval")
	}
	disclosure := strings.ToLower(forget.Safety().Reason)
	if !strings.Contains(disclosure, "delet") {
		t.Errorf("the forget approval does not disclose deletion: %q", forget.Safety().Reason)
	}
	if saved := strings.ToLower(NewMemoryWriteTool(paths).Safety().Reason); strings.Contains(saved, "delet") {
		t.Errorf("the save approval mentions deletion, which it no longer performs: %q", saved)
	}

	result := forget.Run(context.Background(), map[string]any{"name": "doomed"})
	if result.Status == StatusError {
		t.Fatalf("memory_forget failed: %q", result.Output)
	}
	if _, err := memory.Read(paths, memory.ScopeLocal, "doomed"); err == nil {
		t.Error("memory_forget reported success but the note is still readable")
	}
}

// An unrecognised scope must be refused, not silently widened to every store.
func TestMemoryReadRefusesAnUnknownScope(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeProject, "shared", "d", "x"); err != nil {
		t.Fatal(err)
	}
	read := NewMemoryTool(paths)
	result := read.Run(context.Background(), map[string]any{"name": "shared", "scope": "invalid"})
	if result.Status != StatusError {
		t.Errorf("an unknown scope was accepted and the search widened: %q", result.Output)
	}
}

// A valid scope must actually narrow the listing; it used to be ignored.
func TestMemoryListHonoursTheRequestedScope(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeProject, "shared", "team", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Write(paths, memory.ScopeLocal, "private", "mine", "y"); err != nil {
		t.Fatal(err)
	}
	result := NewMemoryTool(paths).Run(context.Background(), map[string]any{"scope": "project"})
	if result.Status == StatusError {
		t.Fatalf("listing failed: %q", result.Output)
	}
	if strings.Contains(result.Output, "private") {
		t.Errorf("a project-scoped listing included a local note: %q", result.Output)
	}
	if !strings.Contains(result.Output, "shared") {
		t.Errorf("a project-scoped listing dropped the project note: %q", result.Output)
	}
}

// A store with directories but no containment Root is UNAVAILABLE, not empty.
// Every scope refuses with ErrNoStore, which List treats as "not configured", so
// the listing came back empty and told the user they had no notes when the store
// was switched off — the same absence-versus-unavailable confusion these tools
// exist to keep apart.
func TestMemoryReportsUnavailableWhenTheRootIsMissing(t *testing.T) {
	base := t.TempDir()
	paths := memory.Paths{
		ProjectDir: base + "/.zero/memory",
		LocalDir:   base + "/.zero/memory/local",
		// Root deliberately blank.
	}
	result := NewMemoryTool(paths).Run(context.Background(), map[string]any{})
	if result.Status != StatusError {
		t.Fatalf("a store with no Root reported success: %q", result.Output)
	}
	if strings.Contains(strings.ToLower(result.Output), "no saved notes") {
		t.Errorf("a switched-off store was rendered as an empty one: %q", result.Output)
	}
	if !strings.Contains(strings.ToLower(result.Output), "not available") {
		t.Errorf("the reason was not reported: %q", result.Output)
	}
}

// Forgetting a note that was never there must SAY so. memory.Forget is
// idempotent at the store layer, so the tool used to answer "Forgot" for a name
// that never existed — and a model that misspelled the name took that as
// confirmation and stopped looking for the real note.
func TestMemoryForgetReportsAbsenceRatherThanClaimingSuccess(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeLocal, "real-note", "d", "body"); err != nil {
		t.Fatal(err)
	}
	result := NewMemoryForgetTool(paths).Run(context.Background(), map[string]any{"name": "reel-note"})
	if result.Status == StatusError {
		t.Fatalf("a missing note should not be an error: %q", result.Output)
	}
	if strings.Contains(result.Output, "Forgot") {
		t.Errorf("memory_forget claimed it deleted a note that never existed: %q", result.Output)
	}
	if !strings.Contains(strings.ToLower(result.Output), "nothing to forget") {
		t.Errorf("absence was not reported distinctly: %q", result.Output)
	}
	// The real note is untouched.
	if _, err := memory.Read(paths, memory.ScopeLocal, "real-note"); err != nil {
		t.Errorf("the existing note was disturbed: %v", err)
	}
}

// One unreadable note must not empty the listing. memory.List deliberately
// returns what it could read alongside the failures; the tool returning an error
// instead threw that away, turning partial success back into total failure one
// bad directory entry away from an empty memory.
func TestAnUnreadableNoteDoesNotEmptyTheListing(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeProject, "readable", "still here", "body"); err != nil {
		t.Fatal(err)
	}
	// A note too large to read back: the ceiling holds on the way out.
	oversized := strings.Repeat("x", 70<<10)
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "huge.md"), []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}

	result := NewMemoryTool(paths).Run(context.Background(), map[string]any{})
	if !strings.Contains(result.Output, "readable") {
		t.Errorf("the readable note was dropped because another failed: %q", result.Output)
	}
	if !strings.Contains(strings.ToLower(result.Output), "could not be read") {
		t.Errorf("the failure was not reported alongside the notes: %q", result.Output)
	}
}

// A failure in the project scope must not hide the user's own local note.
// Project is searched first and arrives with a clone; local is the default write
// scope, so the shared scope masking the private one is the wrong way round.
func TestAProjectFailureDoesNotHideTheLocalNote(t *testing.T) {
	paths := memoryTestPaths(t)
	if _, err := memory.Write(paths, memory.ScopeLocal, "findings", "mine", "the local body"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An unreadable project note of the SAME name, searched first.
	oversized := strings.Repeat("x", 70<<10)
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "findings.md"), []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}

	result := NewMemoryTool(paths).Run(context.Background(), map[string]any{"name": "findings"})
	if result.Status == StatusError {
		t.Fatalf("a project-scope failure hid the readable local note: %q", result.Output)
	}
	if !strings.Contains(result.Output, "the local body") {
		t.Errorf("the local note was not returned: %q", result.Output)
	}
}
