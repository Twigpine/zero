package sessions

import (
	"strings"
	"testing"
)

// A child session stays out of user-facing resume, but the run that started it
// can continue it: the Task tool resumes a specialist through `zero exec
// --resume`, naming the child's parent as its calling session and passing the
// child's tag. Every other combination is refused as before.
func TestPrepareExecResumesAChildOnlyForItsOwner(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	for _, input := range []CreateInput{
		{SessionID: "parent"},
		{SessionID: "other_parent"},
		{SessionID: "child_task", SessionKind: SessionKindChild, ParentSessionID: "parent", Tag: "specialist", Depth: 1},
		{SessionID: "side_task", SessionKind: SessionKindSide, ParentSessionID: "parent", Tag: "specialist"},
	} {
		if _, err := store.Create(input); err != nil {
			t.Fatalf("create %s: %v", input.SessionID, err)
		}
	}
	if _, err := store.AppendEvent("child_task", AppendEventInput{Type: EventMessage, Payload: map[string]any{"role": "assistant", "content": "found the config"}}); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareExec(PrepareExecOptions{Store: store, Resume: "child_task", CallingSessionID: "parent", Tag: "specialist"})
	if err != nil {
		t.Fatalf("owner resume of a child: %v", err)
	}
	if prepared.Mode != ModeResume || prepared.Session.SessionID != "child_task" || len(prepared.ContextEvents) != 1 {
		t.Fatalf("owner resume = mode %q session %q with %d context events, want the child resumed with its history", prepared.Mode, prepared.Session.SessionID, len(prepared.ContextEvents))
	}

	for _, tc := range []struct {
		name    string
		options PrepareExecOptions
	}{
		{name: "plain resume", options: PrepareExecOptions{Resume: "child_task"}},
		{name: "tag without a caller", options: PrepareExecOptions{Resume: "child_task", Tag: "specialist"}},
		{name: "caller without the tag", options: PrepareExecOptions{Resume: "child_task", CallingSessionID: "parent"}},
		{name: "another parent", options: PrepareExecOptions{Resume: "child_task", CallingSessionID: "other_parent", Tag: "specialist"}},
		{name: "another tag", options: PrepareExecOptions{Resume: "child_task", CallingSessionID: "parent", Tag: "spec"}},
		{name: "side session", options: PrepareExecOptions{Resume: "side_task", CallingSessionID: "parent", Tag: "specialist"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := tc.options
			options.Store = store
			_, err := PrepareExec(options)
			if err == nil || !strings.Contains(err.Error(), "Zero session is not resumable: "+options.Resume) {
				t.Fatalf("PrepareExec(%+v) error = %v, want the not-resumable refusal", tc.options, err)
			}
		})
	}
}
