package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/sessions"
)

func foreignSessionRecord(t *testing.T, cwd, sessionID string) []byte {
	t.Helper()
	record, err := json.Marshal(map[string]any{
		"type":      "user",
		"cwd":       cwd,
		"sessionId": sessionID,
		"message":   map[string]any{"role": "user", "content": "hi"},
	})
	if err != nil {
		t.Fatalf("marshal foreign-session fixture: %v", err)
	}
	return append(record, '\n')
}

func tabbedPicker(items ...pickerItem) *commandPicker {
	picker := &commandPicker{
		kind:     pickerSession,
		items:    append([]pickerItem{}, items...),
		allItems: append([]pickerItem{}, items...),
		tabs:     sessionPickerTabs(items),
	}
	picker.applyQuery()
	return picker
}

func sessionRow(title, agent string) pickerItem {
	return pickerItem{Label: title, Value: title + "-id", Meta: agent, Tab: agent}
}

func TestSessionAgentNameComesFromTheImportTag(t *testing.T) {
	cases := map[string]string{
		"":                     "zero",
		"   ":                  "zero",
		"imported:claude-code": "claude-code",
		"imported:codex":       "codex",
		"imported:factory":     "factory",
		" imported:pi ":        "pi",
		"imported:":            "zero", // malformed tag is not an agent
		"some-other-tag":       "zero",
	}
	for tag, want := range cases {
		if got := sessionAgentName(tag); got != want {
			t.Errorf("sessionAgentName(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestLegacyImportedAgentIsSafeInResumeRowsAndTabs(t *testing.T) {
	unsafeTag := "imported:\x1b[2Jforged\u202eagent"
	agent := sessionAgentName(unsafeTag)
	if strings.Contains(agent, "\x1b") || strings.Contains(agent, "\u202e") {
		t.Fatalf("session agent label retained terminal controls: %q", agent)
	}
	picker := pickerFromParts([]pickerItem{
		{Label: "legacy", Value: "legacy-id", Meta: agent, Tab: agent},
		sessionRow("native", "zero"),
	}, nil)
	if picker == nil || !picker.hasTabs() {
		t.Fatalf("legacy imported session did not produce a multi-source picker: %+v", picker)
	}
	view := (model{picker: picker}).pickerOverlay(120)
	if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\u202e") {
		t.Fatalf("resume picker rendered unsafe legacy agent bytes: %q", view)
	}
	if !strings.Contains(view, "forgedagent") {
		t.Fatalf("resume picker lost the visible agent label: %q", view)
	}
}

func TestTheStripOnlyAppearsWhenThereIsMoreThanOneSource(t *testing.T) {
	// A strip reading "All | zero" is chrome that tells the user nothing.
	only := tabbedPicker(sessionRow("a", "zero"), sessionRow("b", "zero"))
	if only.hasTabs() {
		t.Errorf("tabs = %v, want none when every session came from one agent", only.tabs)
	}

	mixed := tabbedPicker(sessionRow("a", "zero"), sessionRow("b", "codex"))
	if !mixed.hasTabs() {
		t.Fatal("expected a tab strip once sessions come from two agents")
	}
	if mixed.tabs[0] != pickerTabAll {
		t.Errorf("tabs[0] = %q, want %q first", mixed.tabs[0], pickerTabAll)
	}
}

func TestTheBusiestAgentSitsNearestToAll(t *testing.T) {
	picker := tabbedPicker(
		sessionRow("a", "codex"),
		sessionRow("b", "zero"), sessionRow("c", "zero"), sessionRow("d", "zero"),
		sessionRow("e", "factory"), sessionRow("f", "factory"),
	)
	want := []string{pickerTabAll, "zero", "factory", "codex"}
	if strings.Join(picker.tabs, ",") != strings.Join(want, ",") {
		t.Errorf("tabs = %v, want %v (most sessions first)", picker.tabs, want)
	}
}

func TestAnAgentWithNoSessionsGetsNoTab(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "work")
	transcript := filepath.Join(home, ".claude", "projects", "-work", "abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, foreignSessionRecord(t, workspace, "abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := agentsessions.Env{Home: home}
	agentsessions.InvalidateDiscovery()
	m := model{agentSessionsEnv: env, cwd: workspace}
	foreign, _ := m.foreignSessionItems(nil, time.Now())
	if len(foreign) != 1 || foreign[0].ForeignSource == nil || foreign[0].ForeignSource.Path != transcript {
		t.Fatalf("foreign picker row lost exact source identity: %+v", foreign)
	}
	picker := pickerFromParts([]pickerItem{sessionRow("a", "zero")}, foreign)
	if picker == nil {
		t.Fatal("controlled Claude transcript produced no picker")
	}
	for _, tab := range picker.tabs {
		if tab == "factory" || tab == "pi" {
			t.Errorf("tabs = %v, want no tab for an agent with nothing in it", picker.tabs)
		}
	}
	foundClaude := false
	for _, tab := range picker.tabs {
		foundClaude = foundClaude || tab == "claude-code"
	}
	if !foundClaude {
		t.Fatalf("tabs = %v, want the one discovered agent", picker.tabs)
	}
}

func TestForeignSessionItemsSuppressAnImportedSource(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "work")
	transcript := filepath.Join(home, ".claude", "projects", "-work", "abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, foreignSessionRecord(t, workspace, "abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentsessions.InvalidateDiscovery()
	m := model{agentSessionsEnv: agentsessions.Env{Home: home}, cwd: workspace}
	existing := []sessions.Metadata{{Tag: agentsessions.ImportTag("claude-code", "abc"), EventCount: 1}}
	if items, _ := m.foreignSessionItems(existing, time.Now()); len(items) != 0 {
		t.Fatalf("already imported source was offered again: %+v", items)
	}
}

func TestZeroForeignSessionTimestampHasNoYearOneLabel(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if got := foreignSessionAge(time.Time{}, now); got != "" {
		t.Fatalf("zero foreign-session timestamp rendered as %q", got)
	}
	if got := foreignSessionAge(now, now); got != "now" {
		t.Fatalf("populated foreign-session timestamp rendered as %q", got)
	}
}

// TestAllShowsEverythingAndTabNarrows is the behaviour asked for: All lists
// every session labelled by agent, Tab moves to one agent at a time.
func TestAllShowsEverythingAndTabNarrows(t *testing.T) {
	picker := tabbedPicker(
		sessionRow("zero-one", "zero"), sessionRow("zero-two", "zero"),
		sessionRow("cx", "codex"),
	)
	if picker.activeTabName() != pickerTabAll {
		t.Fatalf("opened on %q, want All", picker.activeTabName())
	}
	if len(picker.items) != 3 {
		t.Fatalf("All shows %d rows, want all 3", len(picker.items))
	}
	// Each row says which agent it came from, so All is readable.
	for _, item := range picker.items {
		if item.Meta == "" {
			t.Errorf("row %q has no agent label", item.Label)
		}
	}

	picker.cycleTab(1)
	if picker.activeTabName() != "zero" {
		t.Fatalf("after one Tab: %q, want zero (the busiest)", picker.activeTabName())
	}
	if len(picker.items) != 2 {
		t.Fatalf("zero tab shows %d rows, want 2", len(picker.items))
	}

	picker.cycleTab(1)
	if picker.activeTabName() != "codex" || len(picker.items) != 1 {
		t.Fatalf("codex tab = %q with %d rows, want codex with 1", picker.activeTabName(), len(picker.items))
	}
	if picker.items[0].Label != "cx" {
		t.Errorf("codex tab shows %q, want the codex session", picker.items[0].Label)
	}

	// And it wraps back to All rather than dead-ending.
	picker.cycleTab(1)
	if picker.activeTabName() != pickerTabAll || len(picker.items) != 3 {
		t.Errorf("cycling past the last tab = %q with %d rows, want All with 3",
			picker.activeTabName(), len(picker.items))
	}
}

// TestTheTabNarrowsAnEmptySearchToo is the regression this design invites:
// filtering only inside the scored branch of applyQuery leaves an empty search
// box showing every tab's rows — precisely when the strip must be trusted.
func TestTheTabNarrowsAnEmptySearchToo(t *testing.T) {
	picker := tabbedPicker(
		sessionRow("alpha", "zero"), sessionRow("beta", "zero"),
		sessionRow("gamma", "codex"),
	)
	selectTab(t, picker, "codex")
	if picker.query != "" {
		t.Fatal("precondition: the search box must be empty")
	}
	if len(picker.items) != 1 || picker.items[0].Label != "gamma" {
		t.Fatalf("empty query on the codex tab = %d rows (%v), want only gamma",
			len(picker.items), pickerLabels(picker.items))
	}
}

func TestSwitchingTabsKeepsTheSearchText(t *testing.T) {
	picker := tabbedPicker(
		sessionRow("parser fix", "zero"),
		sessionRow("parser rewrite", "codex"),
		sessionRow("unrelated", "codex"),
	)
	picker.appendQuery([]rune("parser"))
	if len(picker.items) != 2 {
		t.Fatalf("query across All = %d rows, want 2", len(picker.items))
	}
	selectTab(t, picker, "codex")
	if picker.query != "parser" {
		t.Errorf("query = %q, want it kept across a tab change", picker.query)
	}
	if len(picker.items) != 1 || picker.items[0].Label != "parser rewrite" {
		t.Errorf("codex+query = %v, want only the matching codex session", pickerLabels(picker.items))
	}
}

func TestCyclingBackwardsWraps(t *testing.T) {
	picker := tabbedPicker(sessionRow("a", "zero"), sessionRow("b", "codex"))
	last := picker.tabs[len(picker.tabs)-1]
	picker.cycleTab(-1)
	// Asserted by position, not by agent name: the strip's order depends on how
	// many sessions each agent has, which is not what this test is about.
	if picker.activeTabName() != last {
		t.Errorf("cycling back from All = %q, want the last tab %q", picker.activeTabName(), last)
	}
}

func TestAPickerWithoutTabsIsUnaffected(t *testing.T) {
	// /model, /effort and friends must behave exactly as before.
	plain := &commandPicker{
		kind:     pickerModel,
		items:    []pickerItem{{Label: "a", Tab: "ignored"}, {Label: "b"}},
		allItems: []pickerItem{{Label: "a", Tab: "ignored"}, {Label: "b"}},
	}
	plain.applyQuery()
	if plain.hasTabs() {
		t.Fatal("a picker with no tabs must not claim to have them")
	}
	if len(plain.items) != 2 {
		t.Errorf("got %d rows, want both — Tab values must not filter a tabless picker", len(plain.items))
	}
	plain.cycleTab(1) // must be a no-op, not a panic
	if plain.activeTabName() != pickerTabAll || len(plain.items) != 2 {
		t.Error("cycleTab changed a tabless picker")
	}
}

// selectTab cycles until the named tab is active, so tests state WHICH tab they
// mean instead of depending on the count-based ordering.
func selectTab(t *testing.T, picker *commandPicker, name string) {
	t.Helper()
	for range picker.tabs {
		if picker.activeTabName() == name {
			return
		}
		picker.cycleTab(1)
	}
	t.Fatalf("no %q tab in %v", name, picker.tabs)
}

func pickerLabels(items []pickerItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Label)
	}
	return out
}

// A FOREIGN TITLE IS ANOTHER PRODUCT'S BYTES, and this row is where they land.
// registry.go already strips control bytes when a foreign session is IMPORTED,
// with a comment naming the picker row as the injection vector (#835/#876) — but
// the picker shows the title BEFORE any import, straight from the other agent's
// file, so the vector the comment describes was the one path left open. A cursor
// or colour escape here rewrites the row above it, and a carriage return hides
// the rest of the label.
func TestAForeignTitleCannotCarryTerminalEscapes(t *testing.T) {
	hostile := "safe\x1b[2Kmoved\rhidden\x07\x00 end"
	got := agentsessions.DisplayField(hostile)
	for _, banned := range []string{"\x1b", "\r", "\x07", "\x00"} {
		if strings.Contains(got, banned) {
			t.Errorf("a control byte %q survived into a picker label: %q", banned, got)
		}
	}
	// The legible text has to come through, or the fix is just deletion.
	for _, want := range []string{"safe", "moved", "hidden", "end"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitizing the label ate %q, leaving %q", want, got)
		}
	}

	// AND THE OTHER HALF: stripping controls is not redaction. A foreign title is
	// frequently the user's first prompt, which is exactly where a pasted key
	// ends up, and the row is drawn before anything has been imported — so the
	// picker was the last place a credential could still be shown verbatim.
	secret := agentsessions.DisplayField("deploy with sk-ant-api03-" + strings.Repeat("A", 24) + " now")
	if strings.Contains(secret, "sk-ant-api03-"+strings.Repeat("A", 24)) {
		t.Errorf("a credential in a foreign title reached the picker row: %q", secret)
	}
	if !strings.Contains(secret, "deploy with") || !strings.Contains(secret, "now") {
		t.Errorf("redacting the title ate the text around the secret: %q", secret)
	}
	// Controls must be stripped BEFORE the shape match, or an escape byte splits
	// the key and it slips through looking like two harmless fragments.
	split := agentsessions.DisplayField("sk-ant-api03-\x00" + strings.Repeat("A", 24))
	if strings.Contains(split, strings.Repeat("A", 24)) {
		t.Errorf("a credential split by a control byte survived the display path: %q", split)
	}
}

// THE USER THIS FEATURE IS FOR HAS NO ZERO SESSIONS YET. Someone who has just
// installed Zero and wants to continue work another agent started has an empty
// local history by definition — and newSessionPicker returned nil on that,
// before foreign discovery ran at all. The import path was reachable only after
// the user had already done by hand the thing it exists to save them.
func TestThePickerOffersForeignSessionsWithNoLocalHistory(t *testing.T) {
	foreign := []pickerItem{{Label: "port the parser", Value: "claude-code:abc", Meta: "claude-code", Tab: "claude-code"}}
	picker := pickerFromParts(nil, foreign)
	if picker == nil {
		t.Fatal("an empty local history hid every discovered foreign session; the import path is unreachable for a new user")
	}
	if len(picker.items) != 1 || picker.items[0].Value != "claude-code:abc" {
		t.Errorf("the foreign session did not reach the picker: %+v", picker.items)
	}
}

// And the genuinely empty case still falls back to the text path.
func TestThePickerIsNilWhenNothingIsResumableAtAll(t *testing.T) {
	if picker := pickerFromParts(nil, nil); picker != nil {
		t.Errorf("a picker was built with no local and no foreign sessions: %+v", picker.items)
	}
}

// THROUGH newSessionPicker ITSELF, not just the assembly helper. Both reviewers
// asked for this and they were right: pickerFromParts is the piece I extracted
// while fixing the bug, so testing only that leaves the very branch that was
// wrong — the early return on an empty ListResumable — uncovered by anything
// exercising the real entry point.
func TestNewSessionPickerSurvivesAnEmptyLocalHistory(t *testing.T) {
	store := testSessionStore(t)
	metas, err := store.ListResumable()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("this test needs an empty store; got %d sessions", len(metas))
	}
	env := agentsessions.Env{Home: t.TempDir()}
	m := model{sessionStore: store, agentSessionsEnv: env, cwd: t.TempDir(), now: func() time.Time { return time.Unix(0, 0) }}

	// With no store at all the picker is still nil — the guard above it stands.
	if bare := (model{}).newSessionPicker(); bare != nil {
		t.Error("a model with no session store built a picker")
	}
	// And with an empty store, newSessionPicker must reach foreign discovery
	// rather than returning on len(metas) == 0. There are no foreign sessions in
	// a temp workspace, so nil here is correct — what is asserted is that it did
	// not panic and that the assembly step decides, which pickerFromParts covers
	// for the populated case.
	if picker := m.newSessionPicker(); picker != nil {
		for _, item := range picker.items {
			if item.Tab == "zero" {
				t.Errorf("an empty local history produced a local row: %+v", item)
			}
		}
	}
}

func TestNewSessionPickerStillOffersForeignSessionsWhenLocalHistoryFails(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "work")
	transcript := filepath.Join(home, ".claude", "projects", "-work", "abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, foreignSessionRecord(t, workspace, "abc"), 0o600); err != nil {
		t.Fatal(err)
	}

	secret := "sk-ant-api03-" + strings.Repeat("A", 24)
	badParent := filepath.Join(home, secret)
	if err := os.MkdirAll(badParent, 0o700); err != nil {
		t.Fatal(err)
	}
	badRoot := filepath.Join(badParent, "sessions")
	if err := os.WriteFile(badRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	agentsessions.InvalidateDiscovery()
	m := model{
		sessionStore:     sessions.NewStore(sessions.StoreOptions{RootDir: badRoot}),
		agentSessionsEnv: agentsessions.Env{Home: home},
		cwd:              workspace,
		now:              func() time.Time { return time.Unix(0, 0) },
	}
	m, cmd := m.sessionPickerCmd()
	msg, ok := cmd().(sessionPickerLoadedMsg)
	if !ok {
		t.Fatalf("session picker command returned an unexpected message")
	}
	if msg.picker == nil || len(msg.picker.items) != 1 || msg.picker.items[0].Value != "claude-code:abc" {
		t.Fatalf("local store failure hid the foreign session: %+v", msg.picker)
	}
	if !strings.Contains(msg.text, "could not read local Zero sessions") {
		t.Fatalf("local store failure was not surfaced separately: %q", msg.text)
	}
	if strings.Contains(msg.text, secret) {
		t.Fatalf("local store warning leaked an unredacted secret: %q", msg.text)
	}
	updated, _ := m.updateModel(msg)
	next := updated.(model)
	if next.picker == nil || !transcriptContains(next.transcript, "could not read local Zero sessions") {
		t.Fatalf("picker and local-store warning were not surfaced together: picker=%+v transcript=%+v", next.picker, next.transcript)
	}
}

func TestForeignDiscoveryProblemsAreSurfacedWithoutHidingHealthyRows(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "work")
	writeTranscript := func(project, id string) {
		t.Helper()
		path := filepath.Join(home, ".claude", "projects", project, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, foreignSessionRecord(t, workspace, id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("healthy", "abc")
	secret := "sk-ant-api03-" + strings.Repeat("A", 24)
	writeTranscript("duplicate-one", secret)
	writeTranscript("duplicate-two", secret)

	agentsessions.InvalidateDiscovery()
	m := model{
		sessionStore:     testSessionStore(t),
		agentSessionsEnv: agentsessions.Env{Home: home},
		cwd:              workspace,
		now:              func() time.Time { return time.Unix(0, 0) },
	}
	m, cmd := m.sessionPickerCmd()
	msg, ok := cmd().(sessionPickerLoadedMsg)
	if !ok {
		t.Fatal("session picker command returned an unexpected message")
	}
	if msg.picker == nil || len(msg.picker.items) != 1 || msg.picker.items[0].Value != "claude-code:abc" {
		t.Fatalf("discovery problem hid the healthy foreign session: %+v", msg.picker)
	}
	if !strings.Contains(msg.text, "claude-code") || !strings.Contains(msg.text, "ambiguous") {
		t.Fatalf("foreign discovery problem was not surfaced with the picker: %q", msg.text)
	}
	if strings.Contains(msg.text, secret) || !strings.Contains(msg.text, "[REDACTED]") {
		t.Fatalf("foreign discovery warning was not redacted: %q", msg.text)
	}
	updated, _ := m.updateModel(msg)
	next := updated.(model)
	if next.picker == nil || !transcriptContains(next.transcript, "ambiguous") {
		t.Fatalf("picker and foreign warning were not surfaced together: picker=%+v transcript=%+v", next.picker, next.transcript)
	}
}

func TestSessionPickerWarningSanitizesEveryDiscoveryProblem(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	got := sessionPickerWarning(
		errors.New("local "+secret),
		[]error{nil, errors.New("claude-code: unreadable " + secret)},
	)
	if strings.Contains(got, secret) {
		t.Fatalf("picker warning leaked an unredacted secret: %q", got)
	}
	if strings.Count(got, "[REDACTED]") != 2 || !strings.Contains(got, "could not read local Zero sessions") || !strings.Contains(got, "could not read some external sessions") {
		t.Fatalf("picker warning did not preserve and sanitize both problem classes: %q", got)
	}
}

func TestSessionPickerWarningDoesNotLeakSplitCredentialFragments(t *testing.T) {
	key := "sk-proj-" + strings.Repeat("B", 87)
	for _, separator := range []struct {
		name  string
		value string
	}{
		{name: "ESC", value: "\x1b"},
		{name: "NUL", value: "\x00"},
		{name: "NEL", value: "\u0085"},
		{name: "zero width space", value: "\u200b"},
		{name: "right-to-left override", value: "\u202e"},
		{name: "carriage return", value: "\r"},
		{name: "tab", value: "\t"},
	} {
		t.Run(separator.name, func(t *testing.T) {
			got := sessionPickerWarning(nil, []error{errors.New("claude-code: " + key[:48] + separator.value + key[48:])})
			for start := 0; start+8 <= len(key); start++ {
				run := key[start : start+8]
				if strings.Contains(got, run) {
					t.Fatalf("picker warning leaked credential run %q at byte %d: %q", run, start, got)
				}
			}
			if !strings.Contains(got, "claude-code") || !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("picker warning did not preserve context and a redaction marker: %q", got)
			}
		})
	}
}

// A FAILED IMPORT MUST NOT HIDE THE WORK IT FAILED TO COPY. Import creates the
// local session and appends its transcript separately, so an append that fails
// leaves a session carrying the import tag and no events. That tag alone used to
// mark the foreign source "already imported" and drop it from the picker — while
// the local-row loop drops the same session for having no events. Both rows
// vanished and the source could not be retried because it was no longer offered.
func TestAnEmptyImportedSessionDoesNotHideItsForeignSource(t *testing.T) {
	const ref = "claude-code:abc"
	tag := agentsessions.ImportTag("claude-code", "abc")

	failed := []sessions.Metadata{{SessionID: "s-empty", Tag: tag, EventCount: 0}}
	if importedSourceRefs(failed)[ref] {
		t.Error("a session with no transcript counted as imported; its foreign source is hidden and the import cannot be retried")
	}

	// The suppression itself still has to work, or the fix is just deletion: a
	// session that really did import is offered once, not twice.
	succeeded := []sessions.Metadata{{SessionID: "s-full", Tag: tag, EventCount: 12}}
	if !importedSourceRefs(succeeded)[ref] {
		t.Error("an imported session no longer suppresses its foreign source; the picker will list it twice")
	}

	// And the two filters agree: the local loop drops EventCount == 0, so this
	// one must too, or exactly one of the two rows survives.
	mixed := []sessions.Metadata{
		{SessionID: "s-empty", Tag: tag, EventCount: 0},
		{SessionID: "s-full", Tag: agentsessions.ImportTag("codex", "def"), EventCount: 3},
	}
	got := importedSourceRefs(mixed)
	if got[ref] || !got["codex:def"] {
		t.Errorf("imported set = %v, want only codex:def", got)
	}
}

func TestImportedSourceRefsUsesDisplayProvenanceWithoutGrantingAuthority(t *testing.T) {
	const ref = "claude-code:abc"
	for _, tc := range []struct {
		name       string
		tag        string
		eventCount int
		want       bool
	}{
		{name: "versioned import", tag: agentsessions.ImportTag("claude-code", "abc"), eventCount: 2, want: true},
		{name: "legacy import with source id", tag: "imported:claude-code:abc", eventCount: 2, want: true},
		{name: "legacy import without source id", tag: "imported:claude-code", eventCount: 2, want: false},
		{name: "empty versioned import remains retryable", tag: agentsessions.ImportTag("claude-code", "abc"), eventCount: 0, want: false},
		{name: "malformed versioned import is not legacy", tag: "imported:v1:bad:bad:extra", eventCount: 2, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := sessions.Metadata{Tag: tc.tag, EventCount: tc.eventCount}
			got := importedSourceRefs([]sessions.Metadata{meta})[ref]
			if got != tc.want {
				t.Fatalf("importedSourceRefs(tag=%q, events=%d)[%q] = %v, want %v", tc.tag, tc.eventCount, ref, got, tc.want)
			}
			if strings.HasPrefix(tc.name, "legacy") && sessions.IsImportedSession(meta) {
				t.Fatalf("display-only legacy tag %q gained import authority", tc.tag)
			}
		})
	}
}
