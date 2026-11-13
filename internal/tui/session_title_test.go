package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// titleProvider is a fakeProvider that streams a single line of text as the
// model's title response.
func titleProvider(title string) *fakeProvider {
	return &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: title},
		{Type: zeroruntime.StreamEventDone},
	}}
}

func appendSessionMessage(t *testing.T, store *sessions.Store, id, role, content string) {
	t.Helper()
	if _, err := store.AppendEvent(id, sessions.AppendEventInput{
		Type:    sessions.EventMessage,
		Payload: map[string]any{"role": role, "content": content},
	}); err != nil {
		t.Fatalf("append %s message: %v", role, err)
	}
}

// createAutoTitledSession creates a real, resumable session whose Title is the
// default first-message title (so it is an auto-title candidate).
func createAutoTitledSession(t *testing.T, store *sessions.Store, prompt, answer string) sessions.Metadata {
	t.Helper()
	session, err := store.Create(sessions.CreateInput{Title: tuiSessionTitle(prompt)})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	appendSessionMessage(t, store, session.SessionID, "user", prompt)
	appendSessionMessage(t, store, session.SessionID, "assistant", answer)
	return session
}

func TestCleanGeneratedTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"surrounding quotes", `"Add Fetch To MCP Client"`, "Add Fetch To MCP Client"},
		{"title label", "Title: Refactor Auth Flow", "Refactor Auth Flow"},
		{"explanation below", "Wire Resume Titles\n\nThis line explains the title.", "Wire Resume Titles"},
		{"code fence wrapper", "```\nFix DST Rollover Bug\n```", "Fix DST Rollover Bug"},
		{"trailing period", "Persist Session Metadata Atomically.", "Persist Session Metadata Atomically"},
		{"word cap", "one two three four five six seven eight nine ten", "one two three four five six seven eight"},
		{"blank", "   \n  ", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanGeneratedTitle(tc.in); got != tc.want {
				t.Fatalf("cleanGeneratedTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSessionTitleDigestSummarizesAndSkipsNoOutputStop(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	appendSessionMessage(t, store, session.SessionID, "user", "How do I add a fetch call to my MCP client?")
	appendSessionMessage(t, store, session.SessionID, "assistant", "Use the fetch tool in the client configuration.")
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventToolCall,
		Payload: map[string]any{"name": "read_file"},
	}); err != nil {
		t.Fatalf("append tool: %v", err)
	}
	// The no-output guardrail stop must never become the digest's topic.
	appendSessionMessage(t, store, session.SessionID, "assistant",
		"Agent stopped after 3 turns with no output (no visible text and no tool calls) to avoid consuming tokens without making progress.")

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	digest := sessionTitleDigest(events)
	for _, want := range []string{
		"User: How do I add a fetch call",
		"Assistant: Use the fetch tool",
		"Tool: read_file",
	} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest missing %q:\n%s", want, digest)
		}
	}
	if strings.Contains(digest, "no visible text and no tool calls") {
		t.Fatalf("digest must skip the no-output guardrail stop:\n%s", digest)
	}
}

func TestSessionTitleDigestRespectsCharBudget(t *testing.T) {
	long := strings.Repeat("alpha ", 2000) // ~12000 chars, far over the digest cap
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	appendSessionMessage(t, store, session.SessionID, "user", long)
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	digest := sessionTitleDigest(events)
	// A single over-long message is per-message capped, so the whole digest stays
	// well under the total budget rather than echoing the entire turn.
	if len([]rune(digest)) > sessionTitleMaxMessageChars+len("User: ")+8 {
		t.Fatalf("digest not capped: %d runes", len([]rune(digest)))
	}
}

func TestSessionTitleIsAuto(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	appendSessionMessage(t, store, session.SessionID, "user", "Build the resume picker")
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	firstMessageTitle := tuiSessionTitle("Build the resume picker")
	if !sessionTitleIsAuto(firstMessageTitle, events) {
		t.Fatal("a first-message title must count as auto")
	}
	if !sessionTitleIsAuto("", events) {
		t.Fatal("an empty title must count as auto")
	}
	if !sessionTitleIsAuto("Zero TUI session", events) {
		t.Fatal("the default placeholder title must count as auto")
	}
	if sessionTitleIsAuto("Hand Picked Name", events) {
		t.Fatal("a distinct (model/hand) title must not count as auto")
	}
}

func TestAutoTitleGeneratesTitleForActiveSession(t *testing.T) {
	store := testSessionStore(t)
	session := createAutoTitledSession(t, store, "how do I add a fetch call", "Use the fetch tool.")

	m := newModel(context.Background(), Options{
		SessionStore: store,
		Provider:     titleProvider("Add Fetch Call To Client"),
	})
	m.activeSession = session
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	m.sessionEvents = events

	next, cmd := m.maybeAutoTitleActiveSession()
	if cmd == nil {
		t.Fatal("expected an auto-title command for an auto-titled session")
	}
	if !next.titledSessions[session.SessionID] {
		t.Fatal("expected the session to be marked as title-attempted")
	}

	msg := execCmd(cmd)
	result, ok := msg.(sessionTitleGeneratedMsg)
	if !ok {
		t.Fatalf("expected sessionTitleGeneratedMsg, got %#v", msg)
	}
	if result.err != nil || !result.applied {
		t.Fatalf("unexpected auto-title result: %#v", result)
	}
	if result.title != "Add Fetch Call To Client" {
		t.Fatalf("title = %q, want %q", result.title, "Add Fetch Call To Client")
	}

	stored, err := store.Get(session.SessionID)
	if err != nil || stored == nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.Title != "Add Fetch Call To Client" {
		t.Fatalf("stored title = %q, want generated title", stored.Title)
	}

	updated, _ := next.Update(result)
	applied := updated.(model)
	if applied.activeSession.Title != "Add Fetch Call To Client" {
		t.Fatalf("active session title = %q, want generated title", applied.activeSession.Title)
	}
	// One-shot: a second pass must not re-fire now that it's been attempted.
	if _, cmd2 := applied.maybeAutoTitleActiveSession(); cmd2 != nil {
		t.Fatal("auto-title must fire at most once per session")
	}
}

func TestAutoTitleSkipsAlreadyNamedSession(t *testing.T) {
	store := testSessionStore(t)
	session := createAutoTitledSession(t, store, "how do I add a fetch call", "Use the fetch tool.")

	m := newModel(context.Background(), Options{
		SessionStore: store,
		Provider:     titleProvider("Should Not Be Used"),
	})
	m.activeSession = session
	m.activeSession.Title = "Already A Real Title"
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	m.sessionEvents = events

	if _, cmd := m.maybeAutoTitleActiveSession(); cmd != nil {
		t.Fatal("a session with a distinct title must not be auto-titled")
	}
}

func TestMaybeAutoTitleSkipsWhenStoreNil(t *testing.T) {
	store := testSessionStore(t)
	session := createAutoTitledSession(t, store, "how do I add a fetch call", "Use the fetch tool.")
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	// A session that WOULD be auto-titled, but the store is gone. Without the
	// nil-store guard the returned cmd would nil-deref in store.UpdateTitle.
	m := newModel(context.Background(), Options{Provider: titleProvider("Add Fetch Call")})
	m.activeSession = session
	m.sessionEvents = events
	m.sessionStore = nil

	if _, cmd := m.maybeAutoTitleActiveSession(); cmd != nil {
		t.Fatal("auto-title must be skipped when the session store is nil")
	}
}

func TestAutoTitleFailureReleasesRetryGate(t *testing.T) {
	const id = "sess-retry"

	// A failed generation must clear the optimistic gate so a later turn can retry.
	failed := newModel(context.Background(), Options{})
	failed.titledSessions = map[string]bool{id: true}
	failed, _ = failed.handleSessionTitleGenerated(sessionTitleGeneratedMsg{sessionID: id, err: errors.New("provider down")})
	if failed.titledSessions[id] {
		t.Fatal("a failed title generation must release the retry gate")
	}

	// An empty title (model returned nothing usable) counts as failure too.
	empty := newModel(context.Background(), Options{})
	empty.titledSessions = map[string]bool{id: true}
	empty, _ = empty.handleSessionTitleGenerated(sessionTitleGeneratedMsg{sessionID: id, title: ""})
	if empty.titledSessions[id] {
		t.Fatal("an empty generated title must release the retry gate")
	}

	// A successful generation keeps the session gated (one-shot, no re-fire).
	ok := newModel(context.Background(), Options{})
	ok.titledSessions = map[string]bool{id: true}
	ok, _ = ok.handleSessionTitleGenerated(sessionTitleGeneratedMsg{sessionID: id, title: "Real Title", applied: true})
	if !ok.titledSessions[id] {
		t.Fatal("a successful title generation must keep the session gated")
	}
}

func TestManualRenameWinsOverInFlightAutoTitle(t *testing.T) {
	store := testSessionStore(t)
	session := createAutoTitledSession(t, store, "build the resume picker", "Working on it.")

	m := newModel(context.Background(), Options{
		SessionStore: store,
		Provider:     titleProvider("Generated Automatic Title"),
	})
	m.activeSession = session
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	m.sessionEvents = events

	m, cmd := m.maybeAutoTitleActiveSession()
	if cmd == nil {
		t.Fatal("expected an automatic title command")
	}
	if _, err := store.UpdateTitle(session.SessionID, "Manual Name"); err != nil {
		t.Fatalf("manual rename: %v", err)
	}

	result, ok := execCmd(cmd).(sessionTitleGeneratedMsg)
	if !ok {
		t.Fatal("expected a title result")
	}
	if result.err != nil || result.applied {
		t.Fatalf("late automatic title should be skipped, got %#v", result)
	}
	got, err := store.Get(session.SessionID)
	if err != nil || got == nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Title != "Manual Name" {
		t.Fatalf("late automatic title overwrote manual name: %q", got.Title)
	}

	next, _ := m.handleSessionTitleGenerated(result)
	if !next.titledSessions[session.SessionID] {
		t.Fatal("a stale automatic title result must keep the retry gate set")
	}
}
