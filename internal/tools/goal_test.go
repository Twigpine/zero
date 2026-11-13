package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
)

func TestGoalToolsAreBoundToTheirSession(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	first, err := store.Create(sessions.CreateInput{SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(sessions.CreateInput{SessionID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	goalTools := NewGoalTools(store, first.SessionID)

	result := goalTools[1].Run(context.Background(), map[string]any{"objective": "Finish first"})
	if result.Status != StatusOK {
		t.Fatalf("create result = %#v", result)
	}
	loadedFirst, _ := store.Get(first.SessionID)
	loadedSecond, _ := store.Get(second.SessionID)
	if loadedFirst.Goal == nil || loadedFirst.Goal.Objective != "Finish first" {
		t.Fatalf("first goal = %#v", loadedFirst.Goal)
	}
	if loadedSecond.Goal != nil {
		t.Fatalf("second goal unexpectedly changed = %#v", loadedSecond.Goal)
	}
}

func TestCreateGoalToolDeclaresTokenBudgetMaximum(t *testing.T) {
	create := NewGoalTools(nil, "goal")[1]
	maximum := create.Parameters().Properties["token_budget"].Maximum
	if maximum == nil || *maximum != 1_000_000_000 {
		t.Fatalf("token_budget maximum = %v, want 1000000000", maximum)
	}
	maxLength := create.Parameters().Properties["objective"].MaxLength
	if maxLength == nil || *maxLength != sessions.GoalObjectiveMaxLength {
		t.Fatalf("objective maxLength = %v, want %d", maxLength, sessions.GoalObjectiveMaxLength)
	}
	safety := create.Safety()
	if safety.SideEffect != SideEffectWrite || safety.Permission != PermissionPrompt || !safety.AdvertiseInAuto {
		t.Fatalf("create_goal safety = %#v, want prompted persistent write advertised in auto mode", safety)
	}
}

func TestUpdateGoalToolRestrictsAgentTransitions(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{SessionID: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateGoal(session.SessionID, "Finish it", 0); err != nil {
		t.Fatal(err)
	}
	update := NewGoalTools(store, session.SessionID)[2]

	result := update.Run(context.Background(), map[string]any{"status": "paused"})
	if result.Status != StatusError || !strings.Contains(result.Output, "complete") {
		t.Fatalf("paused result = %#v", result)
	}
	result = update.Run(context.Background(), map[string]any{"status": "blocked"})
	if result.Status != StatusError || !strings.Contains(result.Output, "require a reason") {
		t.Fatalf("reasonless blocked result = %#v", result)
	}
	result = update.Run(context.Background(), map[string]any{"status": "complete"})
	if result.Status != StatusOK {
		t.Fatalf("complete result = %#v", result)
	}
}
