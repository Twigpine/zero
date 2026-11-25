package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/sessions"
)

func TestForeignImportErrorIsSanitizedAtTranscriptBoundary(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("A", 24)
	agent := "bad\x1b[2J\u009b" + secret
	m := model{sessionStore: testSessionStore(t)}
	started, text, cmd := m.startResumeCommand(agent + ":session")
	if text != "" || cmd == nil || !started.sessionImportInFlight {
		t.Fatalf("malformed foreign ref did not start asynchronously: text=%q cmd=%v", text, cmd != nil)
	}
	msg, ok := cmd().(foreignSessionImportedMsg)
	if !ok || msg.err == nil {
		t.Fatalf("malformed foreign ref returned %#v", msg)
	}
	updated, _ := started.updateModel(msg)
	next := updated.(model)
	rendered := transcriptText(next.transcript)
	if strings.Contains(rendered, "\x1b") || strings.ContainsRune(rendered, '\u009b') {
		t.Fatalf("live terminal controls reached the transcript: %q", rendered)
	}
	if strings.Contains(rendered, secret) || !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("foreign error leaked or dropped the redaction evidence: %q", rendered)
	}
}

func TestForeignResumeCanRetryAnEmptyTranslationWithoutCreatingSessions(t *testing.T) {
	home := t.TempDir()
	transcript := filepath.Join(home, ".claude", "projects", "-w", "empty.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","cwd":"/w","sessionId":"empty","message":{"role":"user","content":""}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := agentsessions.Env{Home: home, Getenv: func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, ".claude")
		}
		return ""
	}}
	store := testSessionStore(t)
	m := model{sessionStore: store, agentSessionsEnv: env, cwd: "/w"}

	for attempt := 1; attempt <= 2; attempt++ {
		msg, ok := m.importForeignSessionCmd("claude-code:empty")().(foreignSessionImportedMsg)
		if !ok || msg.err == nil || !strings.Contains(msg.err.Error(), "no importable content") {
			t.Fatalf("attempt %d returned %#v, want no-importable-content error", attempt, msg)
		}
		m.sessionImportInFlight = true
		var note string
		m, note = m.finishForeignSessionImport(msg)
		if !strings.Contains(note, "no importable content") || m.sessionImportInFlight {
			t.Fatalf("attempt %d finish state: inFlight=%v note=%q", attempt, m.sessionImportInFlight, note)
		}
		metas, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(metas) != 0 {
			t.Fatalf("attempt %d left durable empty sessions: %+v", attempt, metas)
		}
	}

	agentsessions.InvalidateDiscovery()
	items, _ := m.foreignSessionItems(nil, time.Now())
	if len(items) != 1 || items[0].Value != "claude-code:empty" {
		t.Fatalf("retryable foreign source disappeared from the picker: %+v", items)
	}
}

// THE IMPORT NOTE IS A TRANSCRIPT ROW, and it was the one foreign-bytes path in
// /resume still drawn raw. The picker row directly beside it already runs every
// title through agentsessions.DisplayField; this note went to appendRow with the
// foreign session id and the recorded cwd exactly as the other agent's store
// spelled them, so an escape in either repainted the rows around it. The id is
// the transcript's FILE NAME, which is a place a control byte survives on every
// platform Zero runs on.
func TestTheImportNoteSanitizesTheForeignIdAndCwd(t *testing.T) {
	home := t.TempDir()
	// The id is the base name, so the escape has to live in the file name for
	// this to exercise the field the note actually prints.
	id := "abc\x1b[2Kdef"
	transcript := filepath.Join(home, ".claude", "projects", "-w", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","cwd":"/elsewhere/\u001b[31mred/proj","sessionId":"x","message":{"role":"user","content":"hi"}}`
	if err := os.WriteFile(transcript, []byte(line+"\n"), 0o644); err != nil {
		t.Skipf("this platform will not hold a control byte in a file name: %v", err)
	}
	// Every root the adapters resolve, not only the redirect variable: three of
	// the four have no redirect and fall back to HOME.
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	env := agentsessions.Env{Home: home, Getenv: func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, ".claude")
		}
		return ""
	}}
	m := model{sessionStore: testSessionStore(t), agentSessionsEnv: env, cwd: t.TempDir()}
	started, text, cmd := m.startResumeCommand("claude-code:" + id)
	if text != "" || cmd == nil || !started.sessionImportInFlight || started.activeSession.SessionID != "" {
		t.Fatalf("foreign resume did not start asynchronously: text=%q cmd=%v inFlight=%v active=%q", text, cmd != nil, started.sessionImportInFlight, started.activeSession.SessionID)
	}
	msg, ok := cmd().(foreignSessionImportedMsg)
	if !ok || msg.err != nil {
		t.Fatalf("importing a foreign session: %v", msg.err)
	}
	zeroID := msg.result.Session.SessionID
	note := importedSessionNote(msg.result, m.cwd)
	if resumed, text := started.finishForeignSessionImport(msg); text != "" || resumed.activeSession.SessionID != zeroID {
		t.Fatalf("async import result was not resumed: text=%q active=%q", text, resumed.activeSession.SessionID)
	}
	if zeroID == "" {
		t.Fatal("import returned no Zero session id")
	}

	if strings.Contains(note, "\x1b") {
		t.Errorf("a terminal escape reached the transcript note: %q", note)
	}
	// Exact text, so a note that simply dropped the id would not pass. The
	// sanitizer deletes the escape and keeps the rest of the name legible.
	wantPrefix := "Imported claude-code session abcdef into Zero as " + zeroID
	if !strings.HasPrefix(note, wantPrefix) {
		t.Errorf("note = %q, want it to start %q", note, wantPrefix)
	}
	// And the second sentence, which names the recorded workspace. The temp cwd
	// above is never /elsewhere, so this branch always runs.
	const wantCwd = "\nIt ran in /elsewhere/red/proj, so paths it mentions refer to that tree."
	if !strings.HasSuffix(note, wantCwd) {
		t.Errorf("note = %q, want it to end %q", note, wantCwd)
	}
}

func TestImportedWorkspaceIdentitySurvivesDisplayRedaction(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("B", 24)
	workspace := filepath.Join(t.TempDir(), secret, "repo")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	transcript := filepath.Join(home, ".claude", "projects", "-w", "identity.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{
		"type": "user", "cwd": workspace, "sessionId": "identity",
		"message": map[string]any{"role": "user", "content": "inspect this workspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, append(record, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	env := agentsessions.Env{Home: home, Getenv: func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, ".claude")
		}
		return ""
	}}
	store := testSessionStore(t)
	result, err := agentsessions.Import(store, agentsessions.ClaudeCode(env), "identity", agentsessions.ReadOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if strings.Contains(result.Session.Cwd, secret) || !strings.Contains(result.Session.Cwd, "[REDACTED]") {
		t.Fatalf("display cwd is not redacted: %q", result.Session.Cwd)
	}
	wantWorkspace := filepath.Clean(workspace)
	if runtime.GOOS != "windows" {
		wantWorkspace, err = filepath.EvalSymlinks(workspace)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.Session.WorkspaceKey != wantWorkspace {
		t.Fatalf("workspace key = %q, want %q", result.Session.WorkspaceKey, wantWorkspace)
	}
	if _, err := store.AppendEvent(result.Session.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventMessage,
		Payload: map[string]any{"role": "assistant", "content": "done"},
	}); err != nil {
		t.Fatal(err)
	}

	agentsessions.InvalidateDiscovery()
	m := model{sessionStore: store, agentSessionsEnv: env, cwd: workspace, now: time.Now}
	picker := m.newSessionPicker()
	if picker == nil {
		t.Fatal("imported session disappeared from its real workspace")
	}
	foundImported := false
	for _, item := range picker.items {
		if strings.Contains(item.Label, secret) || strings.Contains(item.Meta, secret) {
			t.Fatalf("picker leaked the canonical workspace: %+v", item)
		}
		if item.Value == result.Session.SessionID {
			foundImported = true
		}
		if strings.HasPrefix(item.Value, "claude-code:") {
			t.Fatalf("imported source was not suppressed: %+v", item)
		}
	}
	if !foundImported {
		t.Fatalf("picker did not contain imported session %s: %+v", result.Session.SessionID, picker.items)
	}
	latest, err := m.latestResumableInWorkspace()
	if err != nil || latest == nil || latest.SessionID != result.Session.SessionID {
		t.Fatalf("resume latest = %+v, err=%v", latest, err)
	}
	if note := importedSessionNote(result, workspace); strings.Contains(note, secret) {
		t.Fatalf("import note leaked the canonical workspace: %q", note)
	}
}

// THE CWD HALF NEEDS THE RECORD AN OLDER BUILD LEFT. agentsessions.Import now
// sanitizes what it stores, so the end-to-end test above cannot see this call —
// it would hold with or without it. A session imported before that change still
// carries the foreign bytes, and this note is what puts them on a transcript row.
func TestTheImportNoteCleansAStoredCwdItDidNotWrite(t *testing.T) {
	result := agentsessions.ImportResult{
		Session: sessions.Metadata{SessionID: "zero_1", Cwd: "/elsewhere/\x1b[31mred\x1b[0m/proj"},
		Events:  2,
		Source: agentsessions.ForeignSession{
			Agent: "claude-code", ID: "abc\x1b[2Kdef", Cwd: "/elsewhere/\x1b[31mred\x1b[0m/proj",
		},
	}
	got := importedSessionNote(result, t.TempDir())
	const want = "Imported claude-code session abcdef into Zero as zero_1 (2 events).\n" +
		"It ran in /elsewhere/red/proj, so paths it mentions refer to that tree."
	if got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

// A session recorded in THIS workspace says nothing about a different tree — the
// sanitizing must not have disturbed the comparison that decides.
func TestTheImportNoteOmitsTheWorkspaceSentenceInTheSameTree(t *testing.T) {
	here := t.TempDir()
	got := importedSessionNote(agentsessions.ImportResult{
		Session: sessions.Metadata{SessionID: "zero_1", Cwd: here},
		Events:  1,
		Source:  agentsessions.ForeignSession{Agent: "codex", ID: "x", Cwd: here},
	}, here)
	if strings.Contains(got, "It ran in") {
		t.Errorf("a session imported from the current workspace was called foreign: %q", got)
	}
}

func TestCompletedForeignImportDoesNotReplaceAChangedActiveSession(t *testing.T) {
	m := model{
		activeSession:         sessions.Metadata{SessionID: "current"},
		sessionImportInFlight: true,
		cwd:                   t.TempDir(),
	}
	msg := foreignSessionImportedMsg{
		originSession: "previous",
		result: agentsessions.ImportResult{
			Session: sessions.Metadata{SessionID: "imported"},
			Source:  agentsessions.ForeignSession{Agent: "codex", ID: "foreign"},
		},
	}
	next, note := m.finishForeignSessionImport(msg)
	if next.activeSession.SessionID != "current" {
		t.Fatalf("completed background import replaced the active session: %q", next.activeSession.SessionID)
	}
	if next.sessionImportInFlight || !strings.Contains(note, "did not resume") {
		t.Fatalf("completion state/note = inFlight:%v note:%q", next.sessionImportInFlight, note)
	}
}
