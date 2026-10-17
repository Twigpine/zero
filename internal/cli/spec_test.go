package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specmode"
)

func TestRunSpecApproveCreatesImplementationSession(t *testing.T) {
	store, deps, draft := seedSpecDraft(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"spec", "approve", draft.SpecID, "--comment", "Keep changes focused.", "--json"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", exitCode, stderr.String())
	}
	var payload specCommandResult
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode approve json: %v\n%s", err, stdout.String())
	}
	if payload.Status != string(sessions.SpecStatusApproved) || payload.ImplementationSessionID == "" {
		t.Fatalf("unexpected approve payload: %#v", payload)
	}
	updatedDraft, err := store.Get(draft.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedDraft.SpecStatus != sessions.SpecStatusApproved || updatedDraft.SpecImplSessionID != payload.ImplementationSessionID {
		t.Fatalf("draft metadata not updated: %#v", updatedDraft)
	}
	impl, err := store.Get(payload.ImplementationSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if impl == nil || impl.SessionKind != sessions.SessionKindSpecImpl || impl.SpecSourceSessionID != draft.SessionID {
		t.Fatalf("unexpected impl session: %#v", impl)
	}
	events, err := store.ReadEvents(payload.ImplementationSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != sessions.EventMessage {
		t.Fatalf("implementation events = %#v", events)
	}
	if !strings.Contains(string(events[0].Payload), "Keep changes focused.") || !strings.Contains(string(events[0].Payload), "# Goal") {
		t.Fatalf("implementation prompt missing approved spec context: %s", string(events[0].Payload))
	}
}

func TestRunSpecApproveIsIdempotent(t *testing.T) {
	store, deps, draft := seedSpecDraft(t)

	var first bytes.Buffer
	if exitCode := runWithDeps([]string{"spec", "approve", draft.SessionID, "--json"}, &first, &bytes.Buffer{}, deps); exitCode != exitSuccess {
		t.Fatalf("first approve exit = %d", exitCode)
	}
	var firstPayload specCommandResult
	if err := json.Unmarshal(first.Bytes(), &firstPayload); err != nil {
		t.Fatal(err)
	}

	var second bytes.Buffer
	if exitCode := runWithDeps([]string{"spec", "approve", draft.SpecID, "--json"}, &second, &bytes.Buffer{}, deps); exitCode != exitSuccess {
		t.Fatalf("second approve exit = %d", exitCode)
	}
	var secondPayload specCommandResult
	if err := json.Unmarshal(second.Bytes(), &secondPayload); err != nil {
		t.Fatal(err)
	}
	if secondPayload.ImplementationSessionID != firstPayload.ImplementationSessionID {
		t.Fatalf("approve created a second implementation session: first=%q second=%q", firstPayload.ImplementationSessionID, secondPayload.ImplementationSessionID)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	implCount := 0
	for _, item := range items {
		if item.SessionKind == sessions.SessionKindSpecImpl {
			implCount++
		}
	}
	if implCount != 1 {
		t.Fatalf("implementation session count = %d, want 1", implCount)
	}
}

func TestRunSpecRejectMarksDraftRejected(t *testing.T) {
	store, deps, draft := seedSpecDraft(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"spec", "reject", draft.SpecID, "--reason=Needs narrower scope"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", exitCode, stderr.String())
	}
	updated, err := store.Get(draft.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SpecStatus != sessions.SpecStatusRejected || updated.SpecRejectReason != "Needs narrower scope" {
		t.Fatalf("draft metadata not rejected: %#v", updated)
	}
	if !strings.Contains(stdout.String(), "Spec rejected.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestParseSpecRejectsCommandSpecificFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "comment on show",
			args: []string{"show", "draft", "--comment", "ship it"},
			want: "--comment is only valid for zero spec approve",
		},
		{
			name: "reason on approve",
			args: []string{"approve", "draft", "--reason", "too broad"},
			want: "--reason is only valid for zero spec reject",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := parseSpecArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q validation, got %v", tc.want, err)
			}
		})
	}
}

func TestRunSpecShowPrintsSavedDraft(t *testing.T) {
	_, deps, draft := seedSpecDraft(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"spec", "show", draft.SessionID}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "# Goal") || !strings.Contains(stdout.String(), "Add review flow.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func seedSpecDraft(t *testing.T) (*sessions.Store, appDeps, sessions.Metadata) {
	t.Helper()
	workspaceRoot := t.TempDir()
	saved, err := specmode.SaveDraft(specmode.SaveOptions{
		WorkspaceRoot: workspaceRoot,
		Title:         "Review Flow",
		Plan:          "# Goal\n\nAdd review flow.",
		Now:           fixedCLISpecTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	storeRoot := t.TempDir()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: storeRoot, Now: fixedCLISpecTime})
	draft, err := store.Create(sessions.CreateInput{
		SessionKind:        sessions.SessionKindSpecDraft,
		Title:              "Review flow",
		Cwd:                workspaceRoot,
		ModelID:            "test-model",
		Provider:           "test",
		SpecID:             saved.ID,
		SpecFilePath:       saved.Path,
		SpecStatus:         sessions.SpecStatusDraft,
		SpecDraftModelID:   "test-model",
		SpecDraftReasoning: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := appDeps{
		newSessionStore: func() *sessions.Store {
			return store
		},
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(storeRoot)
	})
	return store, deps, draft
}

func fixedCLISpecTime() time.Time {
	return time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
}
