package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStoreCreatesAppendsListsAndReadsEvents(t *testing.T) {
	now := fixedClock("2026-06-04T10:00:00Z")
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: now})

	session, err := store.Create(CreateInput{
		SessionID: "zero_test_1",
		Title:     "First run",
		Cwd:       "/repo",
		ModelID:   "gpt-4.1",
		Provider:  "openai",
		Tag:       "specialist",
		Depth:     1,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if session.EventCount != 0 || session.CreatedAt != "2026-06-04T10:00:00Z" {
		t.Fatalf("unexpected session metadata: %#v", session)
	}
	if session.Tag != "specialist" || session.Depth != 1 {
		t.Fatalf("session specialist metadata = %#v", session)
	}

	event, err := store.AppendEvent(session.SessionID, AppendEventInput{
		Type: EventMessage,
		Payload: map[string]any{
			"role":    "user",
			"content": "searchable hello",
		},
	})
	if err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}
	if event.ID != "zero_test_1:1" || event.Sequence != 1 {
		t.Fatalf("unexpected event identity: %#v", event)
	}

	loaded, err := store.Get(session.SessionID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded == nil || loaded.EventCount != 1 || loaded.LastEventType != EventMessage {
		t.Fatalf("metadata was not updated after append: %#v", loaded)
	}
	if loaded.Tag != "specialist" || loaded.Depth != 1 {
		t.Fatalf("loaded specialist metadata = %#v", loaded)
	}

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one event, got %#v", events)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if payload["content"] != "searchable hello" {
		t.Fatalf("unexpected payload: %#v", payload)
	}

	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != session.SessionID {
		t.Fatalf("unexpected session list: %#v", sessions)
	}
}

func TestReadEventsToleratesTornTail(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T10:00:00Z")})
	session, err := store.Create(CreateInput{SessionID: "zero_torn_1", Title: "t", Cwd: "/repo", ModelID: "m", Provider: "p"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.AppendEvent(session.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]any{"content": "ok"}}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	// Simulate a crash mid-append: a truncated final JSON line.
	file, err := os.OpenFile(store.eventsPath(session.SessionID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	if _, err := file.WriteString(`{"type":"message","payload":{"content":"tru`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close events: %v", err)
	}

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents must tolerate a torn tail, got error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 complete events (torn tail dropped), got %d", len(events))
	}
}

func TestReadEventsFailsOnMidFileCorruption(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T10:00:00Z")})
	session, err := store.Create(CreateInput{SessionID: "zero_corrupt_1", Title: "t", Cwd: "/repo", ModelID: "m", Provider: "p"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]any{"content": "ok"}}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	// A corrupt line BEFORE a later valid line is real corruption, not a torn
	// tail, and must still fail loudly.
	file, err := os.OpenFile(store.eventsPath(session.SessionID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	if _, err := file.WriteString("not json\n" + `{"type":"message","payload":{}}` + "\n"); err != nil {
		t.Fatalf("write corruption: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close events: %v", err)
	}

	if _, err := store.ReadEvents(session.SessionID); err == nil {
		t.Fatal("expected error on mid-file corruption")
	}
}

func TestStoreForkCopiesEventsAndLineage(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T11:00:00Z")})
	parent, err := store.Create(CreateInput{SessionID: "parent", Title: "Parent", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	for _, content := range []string{"first", "second"} {
		if _, err := store.AppendEvent(parent.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]string{"content": content}}); err != nil {
			t.Fatalf("AppendEvent returned error: %v", err)
		}
	}

	fork, err := store.Fork(parent.SessionID, ForkInput{SessionID: "fork"})
	if err != nil {
		t.Fatalf("Fork returned error: %v", err)
	}
	if fork.ParentSessionID != parent.SessionID || fork.ForkedFromEventID != "parent:2" || fork.ForkedFromSequence != 2 {
		t.Fatalf("fork lineage not recorded: %#v", fork)
	}
	if fork.EventCount != 3 || fork.LastEventType != EventSessionFork {
		t.Fatalf("fork event count/type wrong: %#v", fork)
	}
	events, err := store.ReadEvents(fork.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if len(events) != 3 || events[0].ID != "fork:1" || events[2].Type != EventSessionFork {
		t.Fatalf("fork events not copied/remapped: %#v", events)
	}
	got := []EventType{events[0].Type, events[1].Type, events[2].Type}
	want := []EventType{EventMessage, EventMessage, EventSessionFork}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fork event types = %#v, want %#v", got, want)
	}
}

func TestNativeImportedPrefixTagRetainsOperationalModelOnFork(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(CreateInput{
		SessionID: "native-tagged",
		ModelID:   "native-model",
		Tag:       "imported:archive",
	})
	if err != nil {
		t.Fatal(err)
	}

	fork, err := store.Fork(parent.SessionID, ForkInput{SessionID: "native-tagged-fork"})
	if err != nil {
		t.Fatal(err)
	}
	if fork.ModelID != "native-model" || fork.SourceModelID != "" {
		t.Fatalf("native tag was treated as foreign provenance: %+v", fork)
	}
}

func TestCreateChildMigratesImportedParentModelProvenance(t *testing.T) {
	for _, tc := range []struct {
		name              string
		parentModel       string
		parentSourceModel string
		selectLocalModel  string
		childModel        string
		wantModel         string
		wantSourceModel   string
	}{
		{
			name:            "legacy foreign model is provenance only",
			parentModel:     "foreign-model",
			wantSourceModel: "foreign-model",
		},
		{
			name:            "explicit child model overrides migrated parent",
			parentModel:     "foreign-model",
			childModel:      "specialist-model",
			wantModel:       "specialist-model",
			wantSourceModel: "foreign-model",
		},
		{
			name:              "locally selected parent model remains operational",
			parentSourceModel: "foreign-model",
			selectLocalModel:  "zero-local-model",
			wantModel:         "zero-local-model",
			wantSourceModel:   "foreign-model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(StoreOptions{RootDir: t.TempDir()})
			parent, err := store.Create(CreateInput{
				SessionID:     "imported-parent",
				Tag:           ImportedSessionTag("claude-code", "foreign-id"),
				ModelID:       tc.parentModel,
				SourceModelID: tc.parentSourceModel,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.selectLocalModel != "" {
				parent, err = store.UpdateModel(parent.SessionID, tc.selectLocalModel)
				if err != nil {
					t.Fatal(err)
				}
				if !parent.ModelSelectedLocally {
					t.Fatal("imported parent did not retain the local-selection authority bit")
				}
			}
			child, err := store.CreateChild(parent.SessionID, ChildInput{
				SessionID: "child",
				ModelID:   tc.childModel,
			})
			if err != nil {
				t.Fatal(err)
			}
			if child.ModelID != tc.wantModel || child.SourceModelID != tc.wantSourceModel {
				t.Fatalf("child model fields = (%q, %q), want (%q, %q)", child.ModelID, child.SourceModelID, tc.wantModel, tc.wantSourceModel)
			}
		})
	}
}

func TestStoreForkSupportsNonResumableSideSession(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(CreateInput{SessionID: "parent", Title: "Parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	if _, err := store.AppendEvent(parent.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]string{"content": "context"}}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	side, err := store.Fork(parent.SessionID, ForkInput{
		SessionID:   "side",
		SessionKind: SessionKindSide,
		Tag:         "btw",
	})
	if err != nil {
		t.Fatalf("Fork side: %v", err)
	}
	if side.SessionKind != SessionKindSide || side.Tag != "btw" || side.ParentSessionID != parent.SessionID {
		t.Fatalf("side metadata = %#v", side)
	}
	if IsResumableKind(side.SessionKind) {
		t.Fatal("side sessions must not appear as resumable conversations")
	}
}

func TestStoreForkSkipsUsageEvents(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T11:00:00Z")})
	parent, err := store.Create(CreateInput{SessionID: "parent", Title: "Parent"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(parent.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]string{"content": "hi"}}); err != nil {
		t.Fatalf("AppendEvent message: %v", err)
	}
	if _, err := store.AppendEvent(parent.SessionID, AppendEventInput{Type: EventUsage, Payload: map[string]any{"promptTokens": 100, "completionTokens": 20, "totalTokens": 120}}); err != nil {
		t.Fatalf("AppendEvent usage: %v", err)
	}

	fork, err := store.Fork(parent.SessionID, ForkInput{SessionID: "fork"})
	if err != nil {
		t.Fatalf("Fork returned error: %v", err)
	}
	events, err := store.ReadEvents(fork.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	// Usage accounting must NOT be copied (else parent + fork double-count); the
	// message is copied and the fork marker is appended.
	got := []EventType{}
	for _, event := range events {
		if event.Type == EventUsage {
			t.Fatalf("fork copied a usage event (would double-count): %#v", events)
		}
		got = append(got, event.Type)
	}
	want := []EventType{EventMessage, EventSessionFork}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fork event types = %#v, want %#v", got, want)
	}
}

func TestStoreCreatesChildSessionsAndRecordsLineageEvents(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T11:30:00Z")})
	parent, err := store.Create(CreateInput{
		SessionID: "parent",
		Title:     "Parent",
		Cwd:       "/repo",
		ModelID:   "gpt-4.1",
		Provider:  "openai",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(parent.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]string{"content": "plan"}}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	child, err := store.CreateChild(parent.SessionID, ChildInput{
		SessionID: "child",
		Title:     "Review agent",
		Tag:       "specialist",
		Depth:     1,
		AgentName: "code-review",
		TaskID:    "task-1",
		Prompt:    "Review the diff",
	})
	if err != nil {
		t.Fatalf("CreateChild returned error: %v", err)
	}
	if child.SessionKind != SessionKindChild || child.ParentSessionID != parent.SessionID || child.RootSessionID != parent.SessionID {
		t.Fatalf("child lineage metadata = %#v", child)
	}
	if child.Cwd != parent.Cwd || child.ModelID != parent.ModelID || child.Provider != parent.Provider {
		t.Fatalf("child did not inherit parent runtime context: %#v", child)
	}
	if child.AgentName != "code-review" || child.TaskID != "task-1" || child.SpawnedFromEventID != "parent:1" || child.SpawnedFromSequence != 1 {
		t.Fatalf("child agent metadata = %#v", child)
	}
	if child.Tag != "specialist" || child.Depth != 1 {
		t.Fatalf("child specialist metadata = %#v", child)
	}

	childEvents, err := store.ReadEvents(child.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents child returned error: %v", err)
	}
	if len(childEvents) != 1 || childEvents[0].Type != EventSessionChild {
		t.Fatalf("child events = %#v, want one session_child event", childEvents)
	}
	var childPayload map[string]any
	if err := json.Unmarshal(childEvents[0].Payload, &childPayload); err != nil {
		t.Fatalf("decode child payload: %v", err)
	}
	if childPayload["parentSessionId"] != parent.SessionID || childPayload["prompt"] != "Review the diff" {
		t.Fatalf("child payload = %#v, want parent and prompt", childPayload)
	}

	parentEvents, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents parent returned error: %v", err)
	}
	if len(parentEvents) != 2 || parentEvents[1].Type != EventSessionChild {
		t.Fatalf("parent events = %#v, want child linkage event", parentEvents)
	}
	var parentPayload map[string]any
	if err := json.Unmarshal(parentEvents[1].Payload, &parentPayload); err != nil {
		t.Fatalf("decode parent payload: %v", err)
	}
	if parentPayload["childSessionId"] != child.SessionID || parentPayload["agentName"] != "code-review" || parentPayload["tag"] != "specialist" || parentPayload["depth"] != float64(1) {
		t.Fatalf("parent payload = %#v, want child linkage", parentPayload)
	}
}

func TestStoreRejectsNegativeSessionDepth(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})

	_, err := store.Create(CreateInput{SessionID: "bad_depth", Depth: -1})

	if err == nil || !strings.Contains(err.Error(), "invalid zero session depth") {
		t.Fatalf("expected invalid depth error, got %v", err)
	}
}

func TestStoreListsChildrenLineageAndTree(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: sequenceClock([]time.Time{
		time.Date(2026, 6, 4, 11, 45, 0, 0, time.UTC),
		time.Date(2026, 6, 4, 11, 45, 1, 0, time.UTC),
		time.Date(2026, 6, 4, 11, 45, 2, 0, time.UTC),
		time.Date(2026, 6, 4, 11, 45, 3, 0, time.UTC),
		time.Date(2026, 6, 4, 11, 45, 4, 0, time.UTC),
		time.Date(2026, 6, 4, 11, 45, 5, 0, time.UTC),
		time.Date(2026, 6, 4, 11, 45, 6, 0, time.UTC),
	})})
	root, err := store.Create(CreateInput{SessionID: "root", Title: "Root"})
	if err != nil {
		t.Fatalf("Create root returned error: %v", err)
	}
	first, err := store.CreateChild(root.SessionID, ChildInput{SessionID: "first", AgentName: "review"})
	if err != nil {
		t.Fatalf("CreateChild first returned error: %v", err)
	}
	second, err := store.CreateChild(root.SessionID, ChildInput{SessionID: "second", AgentName: "test-gen"})
	if err != nil {
		t.Fatalf("CreateChild second returned error: %v", err)
	}
	grandchild, err := store.CreateChild(first.SessionID, ChildInput{SessionID: "grandchild", AgentName: "security"})
	if err != nil {
		t.Fatalf("CreateChild grandchild returned error: %v", err)
	}

	children, err := store.ListChildren(root.SessionID)
	if err != nil {
		t.Fatalf("ListChildren returned error: %v", err)
	}
	if got := []string{children[0].SessionID, children[1].SessionID}; !reflect.DeepEqual(got, []string{second.SessionID, first.SessionID}) {
		t.Fatalf("children order = %#v, want newest direct children first", got)
	}

	lineage, err := store.Lineage(grandchild.SessionID)
	if err != nil {
		t.Fatalf("Lineage returned error: %v", err)
	}
	if got := []string{lineage[0].SessionID, lineage[1].SessionID, lineage[2].SessionID}; !reflect.DeepEqual(got, []string{root.SessionID, first.SessionID, grandchild.SessionID}) {
		t.Fatalf("lineage = %#v, want root to child path", got)
	}

	tree, err := store.Tree(root.SessionID)
	if err != nil {
		t.Fatalf("Tree returned error: %v", err)
	}
	if tree.Session.SessionID != root.SessionID || len(tree.Children) != 2 {
		t.Fatalf("tree root = %#v, want two children", tree)
	}
	if tree.Children[1].Session.SessionID != first.SessionID || len(tree.Children[1].Children) != 1 {
		t.Fatalf("tree nested children = %#v, want grandchild under first", tree.Children)
	}
}

func TestListAndLatestResumableExcludeSubRuns(t *testing.T) {
	at, err := time.Parse(time.RFC3339, "2026-06-04T10:00:00Z")
	if err != nil {
		t.Fatalf("parse start time: %v", err)
	}
	// Advancing clock: each created session is strictly newer than the last, so
	// LatestResumable is deterministic regardless of how often Create reads Now.
	clock := func() time.Time {
		at = at.Add(time.Second)
		return at
	}
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: clock})

	mk := func(kind SessionKind, title string) Metadata {
		s, err := store.Create(CreateInput{Title: title, Cwd: "/repo", ModelID: "m", Provider: "p", SessionKind: kind})
		if err != nil {
			t.Fatalf("Create(%q): %v", kind, err)
		}
		return s
	}
	mk("", "conversation-1")
	mk(SessionKindFork, "fork-1")
	mk(SessionKindChild, "child-1")
	mk(SessionKindSpecDraft, "spec-draft-1")
	mk(SessionKindSpecImpl, "spec-impl-1")
	newestResumable := mk("", "conversation-2") // newest standalone conversation
	mk(SessionKindChild, "child-2")             // newer overall, but a sub-run
	mk(SessionKindSide, "side-1")               // newer overall, but non-resumable

	resumable, err := store.ListResumable()
	if err != nil {
		t.Fatalf("ListResumable: %v", err)
	}
	if len(resumable) != 3 {
		t.Fatalf("ListResumable returned %d sessions, want 3 (two regular + one fork)", len(resumable))
	}
	for _, session := range resumable {
		if !IsResumableKind(session.SessionKind) {
			t.Fatalf("ListResumable leaked sub-run kind %q (%s)", session.SessionKind, session.SessionID)
		}
	}

	latest, err := store.LatestResumable()
	if err != nil {
		t.Fatalf("LatestResumable: %v", err)
	}
	if latest == nil || latest.SessionID != newestResumable.SessionID {
		got := "nil"
		if latest != nil {
			got = latest.SessionID
		}
		t.Fatalf("LatestResumable = %s, want newest resumable %s (must skip newer child and side sessions)", got, newestResumable.SessionID)
	}
}

func TestDefaultRootHonorsXDGDataHome(t *testing.T) {
	got := DefaultRoot(map[string]string{
		"XDG_DATA_HOME": "/tmp/zero-data",
		"HOME":          "/tmp/home",
	})
	want := filepath.Join("/tmp/zero-data", "zero", "sessions")
	if got != want {
		t.Fatalf("DefaultRoot = %q, want %q", got, want)
	}
}

func TestStoreRejectsUnsafeSessionIDsAndBadJSONL(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T12:00:00Z")})
	if _, err := store.Create(CreateInput{SessionID: "../escape"}); err == nil {
		t.Fatal("expected unsafe session id to be rejected")
	}

	if err := os.MkdirAll(filepath.Join(store.RootDir, "bad"), 0o700); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.RootDir, "bad", "metadata.json"), []byte(`{"sessionId":"bad","createdAt":"x","updatedAt":"x","eventCount":1}`), 0o600); err != nil {
		t.Fatalf("metadata write failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.RootDir, "bad", "events.jsonl"), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatalf("events write failed: %v", err)
	}

	_, err := store.ReadEvents("bad")
	if err == nil || !strings.Contains(err.Error(), "events.jsonl at line 1") {
		t.Fatalf("expected JSONL line error, got %v", err)
	}
}

func TestStoreGeneratesUniqueIDsWithFixedClock(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T12:30:00Z")})
	first, err := store.Create(CreateInput{})
	if err != nil {
		t.Fatalf("Create first returned error: %v", err)
	}
	second, err := store.Create(CreateInput{})
	if err != nil {
		t.Fatalf("Create second returned error: %v", err)
	}
	if first.SessionID == second.SessionID {
		t.Fatalf("generated session ids collided: %q", first.SessionID)
	}
}

func TestStoreAppendEventSerializesConcurrentWriters(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T13:00:00Z")})
	session, err := store.Create(CreateInput{SessionID: "concurrent"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	const total = 24
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for index := 0; index < total; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := store.AppendEvent(session.SessionID, AppendEventInput{
				Type:    EventMessage,
				Payload: map[string]int{"index": index},
			})
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendEvent returned error: %v", err)
		}
	}

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if len(events) != total {
		t.Fatalf("expected %d events, got %d: %#v", total, len(events), events)
	}
	seen := map[int]bool{}
	for _, event := range events {
		if seen[event.Sequence] {
			t.Fatalf("duplicate event sequence %d in %#v", event.Sequence, events)
		}
		seen[event.Sequence] = true
		if event.ID != fmt.Sprintf("%s:%d", session.SessionID, event.Sequence) {
			t.Fatalf("event id/sequence mismatch: %#v", event)
		}
	}
	for sequence := 1; sequence <= total; sequence++ {
		if !seen[sequence] {
			t.Fatalf("missing event sequence %d in %#v", sequence, events)
		}
	}
	loaded, err := store.Get(session.SessionID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded == nil || loaded.EventCount != total || loaded.LastEventType != EventMessage {
		t.Fatalf("metadata not updated after concurrent append: %#v", loaded)
	}
}

func TestPrepareExecSessionResolvesResumeAndFork(t *testing.T) {
	store := NewStore(StoreOptions{
		RootDir: t.TempDir(),
		Now: sequenceClock([]time.Time{
			time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 6, 4, 10, 0, 1, 0, time.UTC),
			time.Date(2026, 6, 4, 10, 0, 2, 0, time.UTC),
			time.Date(2026, 6, 4, 10, 0, 3, 0, time.UTC),
		}),
	})
	if _, err := store.Create(CreateInput{SessionID: "older"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(CreateInput{SessionID: "latest", Title: "Latest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent("latest", AppendEventInput{Type: EventMessage, Payload: map[string]any{"content": "previous answer"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(CreateInput{SessionID: "newer-side", SessionKind: SessionKindSide}); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareExec(PrepareExecOptions{Store: store, ResumeLatest: true})
	if err != nil {
		t.Fatalf("PrepareExec returned error: %v", err)
	}
	if prepared.Mode != ModeResume || prepared.Session.SessionID != "latest" || len(prepared.ContextEvents) != 1 {
		t.Fatalf("prepared resume = %#v", prepared)
	}
	if got := FormatExecPrompt("continue", prepared); got == "continue" || !strings.Contains(got, "previous answer") {
		t.Fatalf("expected session context in prompt, got %q", got)
	}
	if _, err := PrepareExec(PrepareExecOptions{Store: store, Resume: "newer-side"}); err == nil || !strings.Contains(err.Error(), "Zero session is not resumable: newer-side") {
		t.Fatalf("explicit side-session resume error = %v, want non-resumable rejection", err)
	}

	forked, err := PrepareExec(PrepareExecOptions{Store: store, Fork: "latest", SessionID: "forked"})
	if err != nil {
		t.Fatalf("PrepareExec fork returned error: %v", err)
	}
	if forked.Mode != ModeFork || forked.Session.ParentSessionID != "latest" {
		t.Fatalf("prepared fork = %#v", forked)
	}
}

func TestFormatExecPromptKeepsConversationMessagesWhenNoisyEventsFollow(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: json.RawMessage(`{"role":"user","content":"first user request"}`)},
		{Sequence: 2, Type: EventMessage, Payload: json.RawMessage(`{"role":"assistant","content":"first assistant answer"}`)},
	}
	for sequence := 3; sequence <= 45; sequence++ {
		events = append(events, Event{Sequence: sequence, Type: EventToolResult, Payload: json.RawMessage(`{"name":"read_file","output":"noisy tool result"}`)})
	}
	events = append(events, Event{Sequence: 46, Type: EventMessage, Payload: json.RawMessage(`{"role":"user","content":"latest user request"}`)})

	prompt := FormatExecPrompt("continue", PreparedExec{
		Mode:          ModeResume,
		Session:       Metadata{SessionID: "session-with-noise"},
		ContextEvents: events,
	})

	for _, want := range []string{"first user request", "first assistant answer", "latest user request", "Current user request:", "continue"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got %q", want, prompt)
		}
	}
	// The tool results here follow the last assistant answer, so they are work
	// nothing has spoken for yet. They are carried on purpose: that is the whole
	// of what an interrupted turn leaves behind (#913). The property this test
	// was written for (#460) is that CONVERSATION survives a noisy turn, and the
	// assertions above still hold it.
	// The tool results here follow the last assistant answer, so they are work
	// nothing has spoken for yet, and an interrupted turn leaves exactly this
	// behind (#913). Their IDENTITY is carried; their OUTPUT is not, because the
	// call beside them already names what was touched and the body is the part
	// that can carry file contents into a later prompt.
	if !strings.Contains(prompt, "tool_result: read_file") {
		t.Fatalf("expected unanswered tool work to be named, got %q", prompt)
	}
	if strings.Contains(prompt, "noisy tool result") {
		t.Fatalf("tool output body reached the prompt, got %q", prompt)
	}
	if count := strings.Count(prompt, "tool_result:"); count > 24 {
		t.Fatalf("carried %d tool events, over the tail allowance of 24: %q", count, prompt)
	}
}

// AND WORK THE ASSISTANT ALREADY SPOKE FOR STAYS FILTERED.
//
// The counterpart to the case above, and what keeps that one from being
// satisfied by carrying every tool event ever recorded. Once an answer describes
// the work, repeating the raw results adds length and no information.
func TestFormatExecPromptOmitsToolWorkAnAnswerAlreadyCovers(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: json.RawMessage(`{"role":"user","content":"first user request"}`)},
	}
	for sequence := 2; sequence <= 20; sequence++ {
		events = append(events, Event{Sequence: sequence, Type: EventToolResult, Payload: json.RawMessage(`{"name":"read_file","output":"already summarized tool result"}`)})
	}
	events = append(events,
		Event{Sequence: 21, Type: EventMessage, Payload: json.RawMessage(`{"role":"assistant","content":"I read the files and here is the summary"}`)},
		Event{Sequence: 22, Type: EventMessage, Payload: json.RawMessage(`{"role":"user","content":"latest user request"}`)},
	)

	prompt := FormatExecPrompt("continue", PreparedExec{
		Mode:          ModeResume,
		Session:       Metadata{SessionID: "session-already-answered"},
		ContextEvents: events,
	})

	if strings.Contains(prompt, "already summarized tool result") {
		t.Fatalf("tool work the assistant already described was repeated verbatim, got %q", prompt)
	}
	if strings.Contains(prompt, "tool_result:") {
		t.Fatalf("an answered turn still listed its tool events, got %q", prompt)
	}
	for _, want := range []string{"first user request", "here is the summary", "latest user request"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got %q", want, prompt)
		}
	}
}

func TestFormatExecPromptTruncatesConversationMessagesAfterFilteringNoise(t *testing.T) {
	events := make([]Event, 0, 100)
	for sequence := 1; sequence <= 85; sequence++ {
		events = append(events, Event{
			Sequence: sequence,
			Type:     EventMessage,
			Payload:  json.RawMessage(fmt.Sprintf(`{"role":"user","content":"conversation message %03d"}`, sequence)),
		})
		if sequence%10 == 0 {
			events = append(events, Event{
				Sequence: 1000 + sequence,
				Type:     EventToolResult,
				Payload:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","output":"noisy tool result %03d"}`, sequence)),
			})
		}
	}
	events = append(events, Event{
		Sequence: 2000,
		Type:     EventToolResult,
		Payload:  json.RawMessage(`{"name":"bash","output":"trailing noisy tool result"}`),
	})

	contextEvents := promptContextEvents(events)
	if len(contextEvents) != 80 {
		t.Fatalf("expected prompt context to keep 80 events, got %d", len(contextEvents))
	}
	if contextEvents[0].Sequence != 6 || contextEvents[len(contextEvents)-1].Sequence != 85 {
		t.Fatalf("expected prompt context to keep message sequences 6-85, got first=%d last=%d", contextEvents[0].Sequence, contextEvents[len(contextEvents)-1].Sequence)
	}

	prompt := FormatExecPrompt("continue", PreparedExec{
		Mode:          ModeResume,
		Session:       Metadata{SessionID: "long-session-with-noise"},
		ContextEvents: events,
	})

	if got, want := strings.Count(prompt, "- #"), len(contextEvents); got != want {
		t.Fatalf("expected prompt to contain %d context event lines, got %d in %q", want, got, prompt)
	}
	for _, want := range []string{"conversation message 006", "conversation message 085", "Current user request:", "continue"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got %q", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"conversation message 001",
		"conversation message 002",
		"conversation message 003",
		"conversation message 004",
		"conversation message 005",
		"noisy tool result",
		"trailing noisy tool result",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("expected prompt to omit %q, got %q", unwanted, prompt)
		}
	}
}

// A truncation cut at a raw byte offset can land in the middle of a
// multi-byte UTF-8 rune (e.g. CJK text), embedding invalid UTF-8 into the
// exec prompt. summarizePayload must back off to a rune boundary instead.
func TestSummarizePayloadTruncatesOnRuneBoundary(t *testing.T) {
	content := strings.Repeat("中文", 300) // 900 bytes of 3-byte runes
	payload := json.RawMessage(fmt.Sprintf(`{"role":"user","content":%q}`, content))

	got := summarizePayload(payload)

	if !utf8.ValidString(got) {
		t.Fatalf("summarizePayload produced invalid UTF-8: %q", got)
	}
	if len(got) > 500 {
		t.Fatalf("expected summary to be at most 500 bytes, got %d", len(got))
	}
}

func TestPrepareExecPersistsSpecialistMetadataForNewSession(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T14:30:00Z")})

	prepared, err := PrepareExec(PrepareExecOptions{
		Store:     store,
		SessionID: "child_session",
		Title:     "Explorer child",
		Tag:       "specialist",
		Depth:     2,
	})
	if err != nil {
		t.Fatalf("PrepareExec returned error: %v", err)
	}
	if prepared.Mode != ModeNew || prepared.Session.Tag != "specialist" || prepared.Session.Depth != 2 {
		t.Fatalf("prepared specialist metadata = %#v", prepared)
	}
	loaded, err := store.Get("child_session")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded == nil || loaded.Tag != "specialist" || loaded.Depth != 2 {
		t.Fatalf("loaded specialist metadata = %#v", loaded)
	}
}

func TestPrepareExecWhitespaceResumeDoesNotFallbackToLatest(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T14:00:00Z")})
	if _, err := store.Create(CreateInput{SessionID: "latest"}); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareExec(PrepareExecOptions{Store: store, Resume: "   ", SessionID: "new_session"})
	if err != nil {
		t.Fatalf("PrepareExec returned error: %v", err)
	}
	if prepared.Mode != ModeNew || prepared.Session.SessionID != "new_session" {
		t.Fatalf("expected whitespace resume to create a new session, got %#v", prepared)
	}
}

func TestRecordSpecUpdatesMetadataAndAppendsEvents(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: sequenceClock([]time.Time{
		time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 10, 0, 1, 0, time.UTC),
		time.Date(2026, 6, 8, 10, 0, 2, 0, time.UTC),
	})})
	session, err := store.Create(CreateInput{
		SessionID:          "draft",
		SessionKind:        SessionKindSpecDraft,
		SpecDraftModelID:   "gpt-5",
		SpecDraftReasoning: "high",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	updated, event, err := store.RecordSpec(session.SessionID, RecordSpecInput{
		SpecID:       "2026-06-08-spec-mode",
		SpecFilePath: "/repo/.zero/specs/2026-06-08-spec-mode.md",
		SpecStatus:   SpecStatusDraft,
	})

	if err != nil {
		t.Fatalf("RecordSpec returned error: %v", err)
	}
	if updated.SpecID != "2026-06-08-spec-mode" || updated.SpecStatus != SpecStatusDraft || updated.LastEventType != EventSpecDraft {
		t.Fatalf("updated metadata = %#v", updated)
	}
	if event.Type != EventSpecDraft {
		t.Fatalf("event type = %s, want %s", event.Type, EventSpecDraft)
	}

	approved, event, err := store.RecordSpec(session.SessionID, RecordSpecInput{
		SpecStatus:        SpecStatusApproved,
		SpecUserComment:   "ship it",
		SpecImplSessionID: "impl",
	})
	if err != nil {
		t.Fatalf("RecordSpec approve returned error: %v", err)
	}
	if approved.SpecID != "2026-06-08-spec-mode" || approved.SpecStatus != SpecStatusApproved || approved.SpecUserComment != "ship it" || approved.SpecImplSessionID != "impl" {
		t.Fatalf("approved metadata = %#v", approved)
	}
	if event.Type != EventSpecApproved {
		t.Fatalf("event type = %s, want %s", event.Type, EventSpecApproved)
	}
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected two spec events, got %#v", events)
	}
}

func TestEnsureSpecImplementationReusesExistingPromptSession(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: sequenceClock([]time.Time{
		time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 11, 0, 1, 0, time.UTC),
		time.Date(2026, 6, 8, 11, 0, 2, 0, time.UTC),
		time.Date(2026, 6, 8, 11, 0, 3, 0, time.UTC),
	})})
	draft, err := store.Create(CreateInput{
		SessionID:   "draft",
		SessionKind: SessionKindSpecDraft,
		Title:       "Draft spec",
		Cwd:         "/repo",
		ModelID:     "gpt-5",
		Provider:    "openai",
		SpecID:      "2026-06-08-spec",
		SpecStatus:  SpecStatusDraft,
	})
	if err != nil {
		t.Fatalf("Create draft returned error: %v", err)
	}
	input := EnsureSpecImplementationInput{
		Title:               "Draft spec implementation",
		Cwd:                 draft.Cwd,
		ModelID:             draft.ModelID,
		Provider:            draft.Provider,
		SpecID:              draft.SpecID,
		SpecFilePath:        "/repo/.zero/specs/2026-06-08-spec.md",
		SpecDraftModelID:    "gpt-5",
		SpecDraftReasoning:  "high",
		SpecUserComment:     "ship it",
		SpecSourceSessionID: draft.SessionID,
		Prompt:              "Implement the approved spec.",
	}

	first, firstEvents, err := store.EnsureSpecImplementation(input)
	if err != nil {
		t.Fatalf("EnsureSpecImplementation first returned error: %v", err)
	}
	second, secondEvents, err := store.EnsureSpecImplementation(input)
	if err != nil {
		t.Fatalf("EnsureSpecImplementation second returned error: %v", err)
	}

	if second.SessionID != first.SessionID {
		t.Fatalf("implementation session was not reused: first=%s second=%s", first.SessionID, second.SessionID)
	}
	if len(firstEvents) != 1 || len(secondEvents) != 1 || second.EventCount != 1 {
		t.Fatalf("expected one implementation prompt event, first=%#v second=%#v metadata=%#v", firstEvents, secondEvents, second)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	implCount := 0
	for _, item := range items {
		if item.SessionKind == SessionKindSpecImpl {
			implCount++
		}
	}
	if implCount != 1 {
		t.Fatalf("implementation session count = %d, want 1", implCount)
	}
}

func TestTreeMissingRootReturnsNotFoundError(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-06-04T11:45:00Z")})
	// A syntactically valid but non-existent root must yield a not-found error, not
	// panic: store.Get returns (nil, nil) for a missing session, so Tree must guard
	// the nil before dereferencing it.
	if _, err := store.Tree("missingroot"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Tree on a missing root = %v, want a not-found error", err)
	}
}

func fixedClock(value string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}

func sequenceClock(values []time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		if index >= len(values) {
			return values[len(values)-1]
		}
		value := values[index]
		index++
		return value
	}
}

func TestWriteFileSyncRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	want := []byte("durable bytes\n")
	if err := writeFileSync(path, want, 0o600); err != nil {
		t.Fatalf("writeFileSync: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows does not honor Unix permission bits: Go reports 0o666 for a writable
	// file regardless of the mode passed to OpenFile, so only assert the exact perm
	// where the filesystem actually enforces it.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm = %o, want 600", perm)
		}
	}
}
