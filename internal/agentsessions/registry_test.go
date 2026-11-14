package agentsessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

type invalidImportAdapter struct{}

func (invalidImportAdapter) Name() string { return "invalid" }
func (invalidImportAdapter) Discover(string) ([]ForeignSession, error) {
	return []ForeignSession{{Agent: "invalid", ID: "broken", Title: "broken"}}, nil
}

func TestDuplicateAdapterIDsAreRejectedAcrossDiscoveryAndImportScopes(t *testing.T) {
	home := t.TempDir()
	older := filepath.Join(home, ".claude", "projects", "-old", "duplicate.jsonl")
	newer := filepath.Join(home, ".claude", "projects", "-new", "duplicate.jsonl")
	writeFile(t, older, `{"type":"user","cwd":"/old","sessionId":"duplicate","message":{"role":"user","content":"older content","model":"older-model"}}`+"\n")
	writeFile(t, newer, `{"type":"user","cwd":"/new","sessionId":"duplicate","message":{"role":"user","content":"newer content","model":"newer-model"}}`+"\n")
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	adapter := ClaudeCode(testEnv(home, nil))
	found, err := adapter.Discover("")
	if err != nil || len(found) != 2 {
		t.Fatalf("discover duplicate IDs: %v (%d results)", err, len(found))
	}
	if found[0].Cwd != "/new" {
		t.Fatalf("newest selected row = %+v, want /new", found[0])
	}
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	if _, err := ImportSource(store, adapter, found[0], ReadOptions{}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("exact-source import error = %v, want adapter-global duplicate ambiguity", err)
	}

	if _, err := Import(store, adapter, "duplicate", ReadOptions{}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ID-only import error = %v, want duplicate ambiguity", err)
	}
	listed, problems := DiscoverAll(testEnv(home, nil), "")
	for _, session := range listed {
		if session.Agent == "claude-code" && session.ID == "duplicate" {
			t.Fatalf("ambiguous duplicate was presented as importable: %+v", session)
		}
	}
	if !strings.Contains(fmt.Sprint(problems), "ambiguous") {
		t.Fatalf("duplicate discovery problems = %v, want ambiguity", problems)
	}
	for _, cwd := range []string{"/old", "/new"} {
		listed, problems := DiscoverAll(testEnv(home, nil), cwd)
		if len(listed) != 0 || !strings.Contains(fmt.Sprint(problems), "ambiguous") {
			t.Fatalf("scoped discovery for %s = %+v, %v; want no advertised duplicate and an ambiguity warning", cwd, listed, problems)
		}
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("ambiguous imports left durable provenance: %+v", metas)
	}
}

func TestImportRejectsTranscriptChangedAfterDiscovery(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-w", "changing.jsonl")
	writeFile(t, path, `{"type":"user","cwd":"/w","sessionId":"changing","message":{"role":"user","content":"before"}}`+"\n")
	adapter := ClaudeCode(testEnv(home, nil))
	found, err := adapter.Discover("")
	if err != nil || len(found) != 1 {
		t.Fatalf("discover: %v (%d results)", err, len(found))
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"assistant","cwd":"/w","message":{"role":"assistant","content":"after"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	if _, err := ImportSource(store, adapter, found[0], ReadOptions{}); err == nil || !strings.Contains(err.Error(), "changed after discovery") {
		t.Fatalf("changed-source import error = %v", err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("changed source left an imported session: %+v", metas)
	}
}
func (invalidImportAdapter) Read(ForeignSession, ReadOptions) ([]sessions.AppendEventInput, error) {
	return []sessions.AppendEventInput{{Type: sessions.EventMessage, Payload: map[string]any{
		"role": "user", "content": "hello", "invalid": make(chan int),
	}}}, nil
}

type staticImportAdapter struct {
	events []sessions.AppendEventInput
}

func (staticImportAdapter) Name() string { return "static" }
func (a staticImportAdapter) Discover(string) ([]ForeignSession, error) {
	return []ForeignSession{{Agent: a.Name(), ID: "one", Title: "one", Cwd: "/w"}}, nil
}
func (a staticImportAdapter) Read(ForeignSession, ReadOptions) ([]sessions.AppendEventInput, error) {
	return append([]sessions.AppendEventInput{}, a.events...), nil
}

func TestImportRejectsDiagnosticOnlyContentButKeepsPartialHistory(t *testing.T) {
	diagnostic := sessions.AppendEventInput{
		Type: sessions.EventError, Payload: map[string]any{"message": "one source record was omitted"},
	}
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	if _, err := Import(store, staticImportAdapter{events: []sessions.AppendEventInput{diagnostic}}, "one", ReadOptions{}); !errors.Is(err, ErrNoImportableContent) {
		t.Fatalf("diagnostic-only import error = %v, want ErrNoImportableContent", err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("diagnostic-only import created a local session: %+v", metas)
	}

	partial := staticImportAdapter{events: []sessions.AppendEventInput{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "retained question"}},
		diagnostic,
	}}
	result, err := Import(store, partial, "one", ReadOptions{})
	if err != nil {
		t.Fatalf("partial import: %v", err)
	}
	events, err := store.ReadEvents(result.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != sessions.EventMessage || events[1].Type != sessions.EventMessage || events[2].Type != sessions.EventError {
		t.Fatalf("partial import events = %+v", events)
	}
	var boundary map[string]any
	if err := json.Unmarshal(events[0].Payload, &boundary); err != nil {
		t.Fatal(err)
	}
	if !NoteEventIsBoundary(boundary) || !strings.Contains(fmt.Sprint(boundary["content"]), "reference context only") {
		t.Fatalf("first event is not the import trust boundary: %+v", boundary)
	}
	prepared, err := sessions.PrepareExec(sessions.PrepareExecOptions{Store: store, Resume: result.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	prompt := sessions.FormatExecPrompt("continue", prepared)
	if !strings.Contains(prompt, "reference context only") || !strings.Contains(prompt, "retained question") {
		t.Fatalf("resumed prompt lost boundary or retained history:\n%s", prompt)
	}
}

// WHAT THE STORE HOLDS IS WHAT EVERY CONSUMER DRAWS. The import used
// stripControl on the title and nothing at all on the cwd, which left two
// separate hazards in the record itself: stripControl deliberately keeps
// newlines — right for a transcript line, wrong for a label rendered as one row
// — and it does not redact, so a title (usually the user's first prompt, which
// is exactly where a pasted key lands) stayed a live credential for `zero
// sessions list`, the /resume picker and the import summary to leak
// independently. Sanitizing per consumer is how one of them gets forgotten;
// this pins the chokepoint instead.
func TestAnImportedSessionStoresADisplaySafeTitleAndCwd(t *testing.T) {
	home := t.TempDir()
	transcript := filepath.Join(home, ".claude", "projects", "-w", "hostile.jsonl")
	// \u001b, \u0007 and \u000d are how a control byte actually reaches these
	// fields: encoding/json rejects a raw one inside a string, so a transcript
	// that carries an escape carries it escaped.
	writeFile(t, transcript, strings.Join([]string{
		`{"type":"user","cwd":"/w/\u001b[2Kmoved\u000dhidden/proj","sessionId":"hostile","message":{"role":"user","content":"hi","model":"claude\u001b[2K-opus\nsk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA"}}`,
		`{"type":"ai-title","aiTitle":"deploy\u0007 it\nwith key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA","sessionId":"hostile"}`,
	}, "\n")+"\n")

	adapter := ClaudeCode(testEnv(home, nil))
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	result, err := Import(store, adapter, "hostile", ReadOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Exact values, not "contains no escape": the readable text has to survive,
	// or a sanitizer that deleted the field would pass. The newline became a
	// SPACE rather than vanishing, which is what leaves the word boundary the
	// secret pattern anchors on — see DisplayField.
	const wantTitle = "deploy it with key [REDACTED]"
	if result.Session.Title != wantTitle {
		t.Errorf("stored title = %q, want %q", result.Session.Title, wantTitle)
	}
	// The complete ANSI erase-line sequence is gone and the return left a space
	// behind it, so the stored path is one line and still legible.
	const wantCwd = "/w/moved hidden/proj"
	if result.Session.Cwd != wantCwd {
		t.Errorf("stored cwd = %q, want %q", result.Session.Cwd, wantCwd)
	}
	const wantWorkspaceKey = "/w/\x1b[2Kmoved\rhidden/proj"
	if result.Session.WorkspaceKey != filepath.Clean(wantWorkspaceKey) {
		t.Errorf("workspace key = %q, want %q", result.Session.WorkspaceKey, filepath.Clean(wantWorkspaceKey))
	}
	const wantModel = "claude-opus [REDACTED]"
	if result.Session.ModelID != "" || result.Session.SourceModelID != wantModel {
		t.Errorf("stored operational/source model = %q / %q, want empty / %q", result.Session.ModelID, result.Session.SourceModelID, wantModel)
	}

	// And the record on disk, not merely the value handed back: the metadata is
	// re-read by every later `zero sessions` verb and by /resume.
	reloaded, err := store.Get(result.Session.SessionID)
	if err != nil || reloaded == nil {
		t.Fatalf("reloading the imported session: %v", err)
	}
	if reloaded.Title != wantTitle || reloaded.Cwd != wantCwd || reloaded.WorkspaceKey != filepath.Clean(wantWorkspaceKey) || reloaded.ModelID != "" || reloaded.SourceModelID != wantModel {
		t.Errorf("reloaded title/cwd/operational/source model = %q / %q / %q / %q, want %q / %q / empty / %q",
			reloaded.Title, reloaded.Cwd, reloaded.ModelID, reloaded.SourceModelID, wantTitle, wantCwd, wantModel)
	}
	if got := sessions.OperationalCwd(*reloaded); got != filepath.Clean(wantWorkspaceKey) {
		t.Errorf("operational cwd = %q, want canonical key %q", got, filepath.Clean(wantWorkspaceKey))
	}
	if got := sessions.OperationalCwd(sessions.Metadata{Cwd: "/legacy/workspace"}); got != "/legacy/workspace" {
		t.Errorf("older metadata operational cwd = %q", got)
	}
}

// A TITLE IS THE USER'S FIRST PROMPT, so a credential in it arrives on a line of
// its own far more often than inline — and the separator is what decided whether
// redaction fired. Every secret pattern anchors on \b, so deleting the newline
// glued the preceding word onto the shape and the match stopped happening;
// "key:" happened to still match because a colon is already a boundary, which is
// how a test written with punctuation would have passed while the real case
// leaked. Both spellings are pinned here.
func TestACredentialAfterALineBreakIsStillRedactedInAMetadataField(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("A", 24)
	for _, separator := range []struct {
		name  string
		value string
	}{
		{name: "newline", value: "\n"},
		{name: "tab", value: "\t"},
		{name: "carriage return", value: "\r"},
	} {
		t.Run(separator.name, func(t *testing.T) {
			// "key" ends in a word character, so deleting the separator destroys
			// the boundary. This is the spelling that leaked.
			got := DisplayField("rotate the key" + separator.value + key)
			if strings.Contains(got, key) {
				t.Errorf("a credential after a %s reached a display field verbatim: %q", separator.name, got)
			}
			if got != "rotate the key [REDACTED]" {
				t.Errorf("DisplayField = %q, want %q", got, "rotate the key [REDACTED]")
			}
		})
	}
}

func TestImportRemovesSessionWhenAppendingEventsFails(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	if _, err := Import(store, invalidImportAdapter{}, "broken", ReadOptions{}); err == nil {
		t.Fatal("import with an unencodable event unexpectedly succeeded")
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("failed import left a durable session behind: %+v", metas)
	}
}

// THE FILE THAT WAS VERIFIED IS THE FILE THAT IS READ. Read used to check the
// selected path through one handle, translate through a second open and check
// through a third; a writer that renamed A aside, placed B under A's name for
// the middle open and restored A before the last check passed both snapshot
// checks while B's bytes were imported under A's provenance. The seam schedules
// exactly that replacement between the verified open and the first byte read.
func TestImportReadsTheFileItVerifiedNotTheNameItWasGiven(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-w", "swap.jsonl")
	writeFile(t, path, `{"type":"user","cwd":"/w","sessionId":"swap","message":{"role":"user","content":"genuine A"}}`+"\n")
	adapter := ClaudeCode(testEnv(home, nil))
	found, err := adapter.Discover("")
	if err != nil || len(found) != 1 {
		t.Fatalf("discover: %v (%d results)", err, len(found))
	}
	aside := path + ".aside"
	afterSourceOpen = func(opened string) {
		if opened != path {
			return
		}
		if err := os.Rename(path, aside); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, `{"type":"user","cwd":"/w","sessionId":"swap","message":{"role":"user","content":"impostor B"}}`+"\n")
	}
	t.Cleanup(func() { afterSourceOpen = nil })
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	result, err := ImportSource(store, adapter, found[0], ReadOptions{})
	if err != nil {
		t.Fatalf("import of the verified handle failed: %v", err)
	}
	events, err := store.ReadEvents(result.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, event := range events {
		joined += string(event.Payload)
	}
	if !strings.Contains(joined, "genuine A") || strings.Contains(joined, "impostor B") {
		t.Fatalf("imported bytes did not come from the verified file:\n%s", joined)
	}
}

// A source written DURING the read is refused on the same handle, as the
// existing changed-source failure, and nothing is committed.
func TestImportRefusesASourceWrittenDuringTheRead(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-w", "grow.jsonl")
	writeFile(t, path, `{"type":"user","cwd":"/w","sessionId":"grow","message":{"role":"user","content":"before"}}`+"\n")
	adapter := ClaudeCode(testEnv(home, nil))
	found, err := adapter.Discover("")
	if err != nil || len(found) != 1 {
		t.Fatalf("discover: %v (%d results)", err, len(found))
	}
	afterSourceOpen = func(opened string) {
		if opened != path {
			return
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(`{"type":"assistant","message":{"role":"assistant","content":"after"}}` + "\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		// Keep the timestamp moving even on coarse filesystems.
		future := time.Now().Add(2 * time.Second)
		_ = os.Chtimes(path, future, future)
	}
	t.Cleanup(func() { afterSourceOpen = nil })
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	if _, err := ImportSource(store, adapter, found[0], ReadOptions{}); err == nil || !strings.Contains(err.Error(), "changed after discovery") {
		t.Fatalf("import of a source written during the read = %v, want changed-source refusal", err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("a refused import left a session: %+v", metas)
	}
}

// The Codex reader shares the handle contract: same verified handle from the
// identity check to the last byte, same refusal of a source that moves.
func TestCodexImportReadsTheFileItVerifiedNotTheNameItWasGiven(t *testing.T) {
	env, path := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"019f73d7-e215-7ce0-ab38-d9e6db354717","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"genuine A"}]}}`,
	)
	adapter := Codex(env)
	found, err := adapter.Discover("")
	if err != nil || len(found) != 1 {
		t.Fatalf("discover: %v (%d results)", err, len(found))
	}
	afterSourceOpen = func(opened string) {
		if opened != path {
			return
		}
		if err := os.Rename(path, path+".aside"); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, strings.Join([]string{
			`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"019f73d7-e215-7ce0-ab38-d9e6db354717","cwd":"/w"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"impostor B"}]}}`,
		}, "\n")+"\n")
	}
	t.Cleanup(func() { afterSourceOpen = nil })
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	result, err := ImportSource(store, adapter, found[0], ReadOptions{})
	if err != nil {
		t.Fatalf("import of the verified handle failed: %v", err)
	}
	events, err := store.ReadEvents(result.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, event := range events {
		joined += string(event.Payload)
	}
	if !strings.Contains(joined, "genuine A") || strings.Contains(joined, "impostor B") {
		t.Fatalf("imported bytes did not come from the verified file:\n%s", joined)
	}
}
