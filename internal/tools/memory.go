package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Gitlawb/zero/internal/memory"
)

// The memory tools: read freely, write on approval.
//
// THE ASYMMETRY IS THE DESIGN. Reading a note is reading a file the user already
// has, so it needs no more ceremony than read_file. Writing one puts text into
// the user's repo that will be read back in every future session and believed —
// a note that says "this package is safe to change freely" is load-bearing the
// moment anyone trusts it. So memory_write prompts like every other write tool,
// and nothing lands silently.
//
// PLAN TASKS GET NEITHER BY DEFAULT. planReadOnlyTools is the allow-list a plan
// task's grant is validated against, and neither of these is in it — so a task
// cannot read a note (which would let a stale note steer a fan-out) or write one
// (which would let twenty tasks race to describe the same finding). That is a
// default, not a ceiling: a considered decision to grant them is a one-line
// change in that list.

const (
	MemoryToolName       = "memory"
	MemoryWriteToolName  = "memory_write"
	MemoryForgetToolName = "memory_forget"
)

type memoryTool struct {
	baseTool
	paths memory.Paths
}

// NewMemoryTool reads durable notes. Paths with no directories configured makes
// the tool report that memory is unavailable rather than silently finding none —
// "you have no notes" and "notes are switched off here" are different answers.
func NewMemoryTool(paths memory.Paths) Tool {
	return memoryTool{
		baseTool: baseTool{
			name: MemoryToolName,
			description: "Read durable notes saved in earlier sessions: project conventions, decisions, and confirmed findings. " +
				"Call with no arguments to list what exists (name, scope and a one-line description) and with a name to read one. " +
				"Prefer listing first: the descriptions are there so you can choose what to open instead of reading everything.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"name":  {Type: "string", Description: "The note to read. Omit to list every note instead."},
					"scope": {Type: "string", Description: `Which store to read: "project" (shared, checked in) or "local" (this machine). Omit to search both.`},
				},
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Reads saved notes."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true},
		},
		paths: paths,
	}
}

func (tool memoryTool) Run(_ context.Context, args map[string]any) Result {
	name, err := aliasedStringArg(args, []string{"name", "note", "key"}, "", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory: " + err.Error())
	}
	scope, err := aliasedStringArg(args, []string{"scope"}, "", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory: " + err.Error())
	}
	// Asked of the store rather than tested here, because the fields alone are
	// not the answer: a blank Root makes every scope refuse with ErrNoStore,
	// which List treats as "not configured" — so a Paths with both directories
	// and no Root rendered as "No saved notes yet" and told the user the memory
	// was empty when it was switched off.
	if !tool.paths.Available() {
		return errorResult("Error: memory is not available in this run.")
	}

	// Resolved ONCE, and refused when the caller names a scope this package does
	// not know. The two paths used to disagree: an unrecognised spelling widened
	// the named read to BOTH stores, while the listing ignored a perfectly valid
	// scope and always read both.
	scopes, err := memory.ResolveScopes(scope)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: %v. Use \"project\" or \"local\", or omit it to search both.", err))
	}

	if strings.TrimSpace(name) == "" {
		notes, listErr := memory.List(tool.paths, scopes...)
		// BESIDE the notes, not instead of them. memory.List deliberately returns
		// what it could read alongside the failures, and returning an error here
		// threw that away — turning the library's careful partial success back
		// into total failure, one unreadable directory entry away from an empty
		// memory. A store that cannot be read still has to be reported, so it is
		// appended rather than substituted.
		rendered := renderMemoryList(notes)
		if listErr != nil {
			rendered += "\n\nSome notes could not be read: " + listErr.Error()
		}
		return okResult(rendered)
	}
	// A failure in one scope must not hide a readable note in the next. Project is
	// searched first, is checked in, and arrives with a clone; local is the user's
	// own default write scope. An unreadable project note called "findings" would
	// otherwise make the user's own local "findings" unreachable — the shared,
	// externally-supplied scope masking the private one. The failure is carried and
	// reported only when nothing readable turns up.
	var problems []error
	for _, candidate := range scopes {
		note, readErr := memory.Read(tool.paths, candidate, name)
		if readErr != nil {
			// Absence is silent; anything else is an operational failure worth
			// telling the caller about, since reporting it as "no memory named" is
			// how the model concludes the note is gone and writes over it.
			if !errors.Is(readErr, memory.ErrNotFound) {
				problems = append(problems, fmt.Errorf("%s: %w", candidate, readErr))
			}
			continue
		}
		return okResult(fmt.Sprintf("memory %q (%s)\n\n%s", note.Name, note.Scope, note.Body))
	}
	if len(problems) > 0 {
		return errorResult(fmt.Sprintf("Error: reading memory %q: %v", name, errors.Join(problems...)))
	}
	return errorResult(fmt.Sprintf("Error: no memory named %q. Call memory with no arguments to see what exists.", name))
}

type memoryWriteTool struct {
	baseTool
	paths memory.Paths
}

// NewMemoryWriteTool saves a durable note.
func NewMemoryWriteTool(paths memory.Paths) Tool {
	return memoryWriteTool{
		baseTool: baseTool{
			name: MemoryWriteToolName,
			description: "Save a durable note for future sessions. " +
				"Write what a later session could not work out for itself — a convention, a decision and its reason, a finding already confirmed. " +
				"Do NOT write what the code, the tests or git history already say; a note that repeats them is one more thing to keep true. " +
				`Use scope "local" by default; "project" is checked in and shared with everyone who clones the repo, so save there only what the whole team should read. ` +
				"To delete a note, use " + MemoryForgetToolName + ".",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"name":        {Type: "string", Description: `Short identifier: lowercase letters, digits and hyphen, starting with a letter (for example "error-handling").`},
					"content":     {Type: "string", Description: "The note itself."},
					"description": {Type: "string", Description: "One line saying what this note is for. Shown when listing, so a reader can choose without opening it."},
					"scope":       {Type: "string", Description: `"local" (this machine, the default) or "project" (checked in, shared).`},
				},
				// content is REQUIRED, and that is the fix rather than a tidy-up.
				// It used to be optional with allowEmpty, so a call that merely
				// left it out — or sent "  \n\t" — fell through to Forget and
				// destroyed the note, behind an approval whose text says only
				// that this tool saves. Deletion now lives in its own tool with
				// its own disclosure.
				Required:             []string{"name", "content"},
				AdditionalProperties: false,
			},
			safety:       promptSafety(SideEffectWrite, "Saves a note that future sessions will read and believe."),
			capabilities: ToolCapabilities{Effect: EffectWorkspaceWrite, ThreadSafe: false},
		},
		paths: paths,
	}
}

func (tool memoryWriteTool) Run(_ context.Context, args map[string]any) Result {
	name, err := aliasedStringArg(args, []string{"name", "note", "key"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory_write: " + err.Error())
	}
	// Required and non-empty: an omitted or whitespace-only payload is a
	// malformed save, not a request to delete.
	content, err := aliasedStringArg(args, []string{"content", "body", "text"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory_write: " + err.Error())
	}
	// Checked after trimming as well, because the argument helper only rejects a
	// genuinely empty string. "  \n\t" used to reach the delete branch and destroy
	// the note; it must now be refused rather than saved as a blank one.
	if strings.TrimSpace(content) == "" {
		return errorResult("Error: memory_write needs the note's content. To delete a note, use " + MemoryForgetToolName + ".")
	}
	description, err := aliasedStringArg(args, []string{"description", "summary"}, "", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory_write: " + err.Error())
	}
	rawScope, err := aliasedStringArg(args, []string{"scope"}, "", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory_write: " + err.Error())
	}
	// LOCAL BY DEFAULT. A note the model chose to keep should not land in a
	// shared, checked-in file unless someone said so — the same reasoning that
	// makes project config unable to raise a spend ceiling.
	scope := memory.ScopeLocal
	if trimmed := strings.TrimSpace(strings.ToLower(rawScope)); trimmed != "" {
		scope = memory.Scope(trimmed)
	}

	if _, err := memory.Write(tool.paths, scope, name, description, content); err != nil {
		return errorResult("Error: " + err.Error())
	}
	return okResult(fmt.Sprintf("Saved %q (%s).", name, scope))
}

type memoryForgetTool struct {
	baseTool
	paths memory.Paths
}

// NewMemoryForgetTool deletes a durable note.
//
// A SEPARATE TOOL, not a mode of memory_write, because the approval text is
// fixed per tool. memory_write's says it "saves a note that future sessions will
// read and believe", and deleting under that sentence is what made an omitted
// content field destructive: the human approved a save and got a permanent
// removal, and an "always allow" on that prompt made every later deletion
// unattended. Splitting them lets each prompt say what its tool actually does,
// and keeps a save grant from authorising a delete.
func NewMemoryForgetTool(paths memory.Paths) Tool {
	return memoryForgetTool{
		baseTool: baseTool{
			name: MemoryForgetToolName,
			description: "Permanently delete a saved note. " +
				"There is no undo and no copy is returned, so read the note first if you are not certain it should go.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"name":  {Type: "string", Description: "The note to delete."},
					"scope": {Type: "string", Description: `"local" (this machine, the default) or "project" (checked in, shared).`},
				},
				Required:             []string{"name"},
				AdditionalProperties: false,
			},
			safety:       promptSafety(SideEffectWrite, "PERMANENTLY DELETES a saved note. There is no undo."),
			capabilities: ToolCapabilities{Effect: EffectWorkspaceWrite, ThreadSafe: false},
		},
		paths: paths,
	}
}

func (tool memoryForgetTool) Run(_ context.Context, args map[string]any) Result {
	name, err := aliasedStringArg(args, []string{"name", "note", "key"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory_forget: " + err.Error())
	}
	rawScope, err := aliasedStringArg(args, []string{"scope"}, "", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for memory_forget: " + err.Error())
	}
	scope := memory.ScopeLocal
	if trimmed := strings.TrimSpace(strings.ToLower(rawScope)); trimmed != "" {
		scope = memory.Scope(trimmed)
	}
	// Absence is reported as absence. memory.Forget is idempotent by design — a
	// missing note is not an error at the store layer — but saying "Forgot" for a
	// note that never existed tells a model which misspelled the name that the
	// deletion happened, and it stops looking for the real one.
	if _, err := memory.Read(tool.paths, scope, name); errors.Is(err, memory.ErrNotFound) {
		return okResult(fmt.Sprintf("No note named %q in %s, so there was nothing to forget.", name, scope))
	}
	if err := memory.Forget(tool.paths, scope, name); err != nil {
		return errorResult("Error: " + err.Error())
	}
	return okResult(fmt.Sprintf("Forgot %q (%s).", name, scope))
}

func renderMemoryList(notes []memory.Note) string {
	if len(notes) == 0 {
		return "No saved notes yet. Use memory_write to keep something a future session could not work out for itself."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d saved note(s):\n", len(notes))
	for _, note := range notes {
		fmt.Fprintf(&b, "- %s (%s)", note.Name, note.Scope)
		if note.Description != "" {
			b.WriteString(" — ")
			b.WriteString(note.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
