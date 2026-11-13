package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/sessions"
)

// twoResumableSessions seeds a store with sessions "a" and "b" in workspace
// and returns a model whose active session is "a".
func twoResumableSessions(t *testing.T) (model, *sessions.Store) {
	t.Helper()
	store := testSessionStore(t)
	workspace := t.TempDir()
	var metaA sessions.Metadata
	for _, id := range []string{"a", "b"} {
		meta, err := store.Create(sessions.CreateInput{SessionID: id, Title: "session " + id, Cwd: workspace})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendEvents(id, []sessions.AppendEventInput{
			{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "ask " + id}},
			{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "answer " + id}},
		}); err != nil {
			t.Fatal(err)
		}
		if id == "a" {
			metaA = meta
		}
	}
	m := model{
		sessionStore:     store,
		agentSessionsEnv: agentsessions.Env{Home: t.TempDir()},
		cwd:              workspace,
		now:              func() time.Time { return time.Unix(0, 0) },
		activeSession:    metaA,
	}
	return m, store
}

// A LATE PICKER MUST NOT SWITCH A RUNNING SESSION. Bare /resume dispatches
// discovery asynchronously and checked m.pending only at dispatch. A prompt
// submitted before the result arrived started a run; the result then installed
// the picker anyway, and choosing another session called resumeZeroSession
// without rechecking, so the active session changed under the live run and its
// completion appended into the other conversation. Discovery stays asynchronous;
// what changes is that a result is tied to the request and session it was made
// for, is refused while a run is active, and the selection route rechecks on its
// own — the refusal comes before any mutation, never as repair afterwards.
func TestALatePickerResultCannotSwitchSessionsMidRun(t *testing.T) {
	m, _ := twoResumableSessions(t)
	m, cmd := m.sessionPickerCmd()
	// The user submits a prompt while discovery is still pending.
	m.pending = true
	msg, ok := cmd().(sessionPickerLoadedMsg)
	if !ok {
		t.Fatal("discovery did not return a picker message")
	}
	updated, _ := m.updateModel(msg)
	next := updated.(model)
	if next.picker != nil {
		t.Fatal("a picker was installed while a run was active")
	}
	if !transcriptContains(next.transcript, "out of date") {
		t.Fatalf("the stale result was not explained: %+v", next.transcript)
	}
	// And the selection route itself refuses, regardless of how it was reached.
	next, text, _ := next.startResumeCommand("b")
	if !strings.Contains(text, "Cannot resume sessions while a run is active") {
		t.Fatalf("a local selection during a run was not refused: %q", text)
	}
	if next.activeSession.SessionID != "a" {
		t.Fatalf("active session switched to %q under a live run", next.activeSession.SessionID)
	}
}

func TestAPickerResultForAnotherSessionOrRequestIsDiscarded(t *testing.T) {
	t.Run("session changed before the result", func(t *testing.T) {
		m, store := twoResumableSessions(t)
		m, cmd := m.sessionPickerCmd()
		metaB, err := store.Get("b")
		if err != nil {
			t.Fatal(err)
		}
		m.activeSession = *metaB
		updated, _ := m.updateModel(cmd())
		if next := updated.(model); next.picker != nil {
			t.Fatal("a result made for session a was installed on session b")
		}
	})
	t.Run("newer request supersedes the older", func(t *testing.T) {
		m, _ := twoResumableSessions(t)
		m, first := m.sessionPickerCmd()
		m, second := m.sessionPickerCmd()
		updated, _ := m.updateModel(first())
		if next := updated.(model); next.picker != nil {
			t.Fatal("a superseded request's result was installed")
		}
		updated, _ = m.updateModel(second())
		if next := updated.(model); next.picker == nil {
			t.Fatal("the current request's result was not installed")
		}
	})
	t.Run("idle selection still works", func(t *testing.T) {
		m, _ := twoResumableSessions(t)
		m, cmd := m.sessionPickerCmd()
		updated, _ := m.updateModel(cmd())
		next := updated.(model)
		if next.picker == nil {
			t.Fatal("an idle request's result was not installed")
		}
		next, text, _ := next.startResumeCommand("b")
		if text != "" || next.activeSession.SessionID != "b" {
			t.Fatalf("idle selection failed: text=%q active=%q", text, next.activeSession.SessionID)
		}
	})
}

// THE JOIN THE SANITIZER HAS TO SURVIVE: a stored tool argument is decoded by
// argHint/firstArgValue when the resumed tool row is drawn. A foreign input
// whose path is the JSON-escaped "ESC[2J FORGED ghp_AAAA…" is clean as encoded
// bytes and hostile once decoded, so this imports a transcript, reads the
// persisted event back and runs the renderer's own extraction on it.
func TestAResumedToolRowCannotDecodeAnImportedEscapeOrCredential(t *testing.T) {
	pat := "ghp_" + strings.Repeat("A", 36)
	home := t.TempDir()
	transcript := filepath.Join(home, ".claude", "projects", "-w", "hostile.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	// Spelled as the six-character JSON escape on purpose: that is the form the
	// foreign file carries, and the form the sanitizer could not see through.
	line := `{"type":"user","cwd":"/w","sessionId":"hostile","message":{"role":"user","content":"go"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"\u001b[2J FORGED ghp_` + strings.Repeat("A", 36) + `"}}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	env := agentsessions.Env{Home: home, Getenv: func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, ".claude")
		}
		return ""
	}}
	store := testSessionStore(t)
	result, err := agentsessions.Import(store, agentsessions.ClaudeCode(env), "hostile", agentsessions.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents(result.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, event := range events {
		if event.Type != sessions.EventToolCall {
			continue
		}
		checked = true
		hint := argHint(payloadString(sessionPayload(event), "arguments"))
		if strings.Contains(hint, "\x1b") {
			t.Fatalf("the resumed tool row decodes to a live escape: %q", hint)
		}
		if strings.Contains(hint, pat) {
			t.Fatalf("the resumed tool row decodes to the credential: %q", hint)
		}
		if !strings.Contains(hint, "FORGED") {
			t.Fatalf("the ordinary argument text was lost: %q", hint)
		}
	}
	if !checked {
		t.Fatal("no tool call was imported")
	}
}

// The post-import note compares a transcript-supplied cwd. It must complete
// through the shared lexical policy; on Windows that is what keeps a UNC path
// from being dialled, which is pinned in agentsessions where the resolver seam
// lives. Here the value flows through the real note.
func TestTheImportNoteComparesAForeignWorkspaceWithoutResolvingIt(t *testing.T) {
	result := agentsessions.ImportResult{
		Session: sessions.Metadata{SessionID: "zero-1"},
		Source:  agentsessions.ForeignSession{Agent: "claude-code", ID: "abc", Cwd: `\\server\share\repo`},
		Events:  3,
	}
	note := importedSessionNote(result, t.TempDir())
	if !strings.Contains(note, "It ran in") || !strings.Contains(note, `\\server\share\repo`) {
		t.Fatalf("note = %q", note)
	}
}

// foreignTranscriptModel seeds a real Claude transcript and returns a model
// pointed at it, so an import that is NOT refused actually does something —
// which is what makes the refusal assertions below mean anything.
func foreignTranscriptModel(t *testing.T) (model, *sessions.Store, agentsessions.ForeignSession) {
	t.Helper()
	home := t.TempDir()
	transcript := filepath.Join(home, ".claude", "projects", "-w", "abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(
		`{"type":"user","cwd":"/w","sessionId":"abc","message":{"role":"user","content":"port the parser"}}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := agentsessions.Env{Home: home, Getenv: func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, ".claude")
		}
		return ""
	}}
	agentsessions.InvalidateDiscovery()
	found, err := agentsessions.ClaudeCode(env).Discover("")
	if err != nil || len(found) != 1 {
		t.Fatalf("discover: %v (%d results)", err, len(found))
	}
	store := testSessionStore(t)
	m := model{
		sessionStore:     store,
		agentSessionsEnv: env,
		cwd:              t.TempDir(),
		now:              func() time.Time { return time.Unix(0, 0) },
	}
	return m, store, found[0]
}

func storedSessionCount(t *testing.T, store *sessions.Store) int {
	t.Helper()
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	return len(metas)
}

// A RUN IN FLIGHT REFUSES EVERY RESUME ROUTE, NOT JUST THE CHEAPEST ONE.
// resumeWhileRunningText calls itself the refusal every resume route shares,
// but the check lived inside startResumeCommand's local-id arm. Both foreign
// routes walked past it: a typed `agent:id`, and a picker row with a
// ForeignSource chosen through choosePicker, which reaches
// startForeignSessionImport directly. Neither switched the active session at
// completion — finishForeignSessionImport already declined that — but both ran
// the import first, which reads the foreign transcript, CREATES A DURABLE ZERO
// SESSION and invalidates the discovery cache. The local route refuses all of
// that up front, so these must too: no command dispatched, no in-flight flag
// left set, and no session in the store.
func TestForeignImportRoutesRefuseWhileARunIsActive(t *testing.T) {
	t.Run("typed agent:id", func(t *testing.T) {
		m, store, _ := foreignTranscriptModel(t)
		before := storedSessionCount(t, store)
		m.pending = true
		next, text, cmd := m.startResumeCommand("claude-code:abc")
		if text != resumeWhileRunningText {
			t.Fatalf("text = %q, want the shared mid-run refusal", text)
		}
		if cmd != nil {
			t.Fatal("an import command was dispatched during a run")
		}
		if next.sessionImportInFlight {
			t.Fatal("sessionImportInFlight was left set by a refused import")
		}
		if got := storedSessionCount(t, store); got != before {
			t.Fatalf("stored sessions = %d, want %d: a refused import still created one", got, before)
		}
	})

	t.Run("picker foreign row through choosePicker", func(t *testing.T) {
		m, store, source := foreignTranscriptModel(t)
		before := storedSessionCount(t, store)
		m.pending = true
		m.picker = &commandPicker{
			kind:     pickerSession,
			items:    []pickerItem{{Label: "port the parser", Value: "claude-code:abc", ForeignSource: &source}},
			allItems: []pickerItem{{Label: "port the parser", Value: "claude-code:abc", ForeignSource: &source}},
		}
		updated, cmd := m.choosePicker()
		next := updated.(model)
		if cmd != nil {
			t.Fatal("an import command was dispatched from the picker during a run")
		}
		if next.sessionImportInFlight {
			t.Fatal("sessionImportInFlight was left set by a refused picker import")
		}
		if !transcriptContains(next.transcript, "Cannot resume sessions while a run is active") {
			t.Fatalf("the picker refusal was not explained: %+v", next.transcript)
		}
		if got := storedSessionCount(t, store); got != before {
			t.Fatalf("stored sessions = %d, want %d: a refused picker import still created one", got, before)
		}
	})

	// THE OTHER DIRECTION, or the fix is just "imports never work". Idle, both
	// routes dispatch, and the dispatched command really imports.
	t.Run("idle typed agent:id still imports", func(t *testing.T) {
		m, store, _ := foreignTranscriptModel(t)
		next, text, cmd := m.startResumeCommand("claude-code:abc")
		if text != "" || cmd == nil || !next.sessionImportInFlight {
			t.Fatalf("idle import did not start: text=%q cmd=%v inFlight=%v", text, cmd != nil, next.sessionImportInFlight)
		}
		msg, ok := cmd().(foreignSessionImportedMsg)
		if !ok || msg.err != nil {
			t.Fatalf("idle import failed: %#v", msg)
		}
		if got := storedSessionCount(t, store); got != 1 {
			t.Fatalf("stored sessions = %d, want the imported one", got)
		}
	})

	t.Run("idle picker foreign row still imports", func(t *testing.T) {
		m, store, source := foreignTranscriptModel(t)
		next, text, cmd := m.startForeignSessionImport(source)
		if text != "" || cmd == nil || !next.sessionImportInFlight {
			t.Fatalf("idle picker import did not start: text=%q cmd=%v inFlight=%v", text, cmd != nil, next.sessionImportInFlight)
		}
		msg, ok := cmd().(foreignSessionImportedMsg)
		if !ok || msg.err != nil {
			t.Fatalf("idle picker import failed: %#v", msg)
		}
		if got := storedSessionCount(t, store); got != 1 {
			t.Fatalf("stored sessions = %d, want the imported one", got)
		}
	})
}
