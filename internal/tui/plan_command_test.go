package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planmode"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func newPlanModeTestModel(t *testing.T, cwd string, permissionMode agent.PermissionMode) model {
	t.Helper()
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	m := newModel(context.Background(), Options{
		Cwd:            cwd,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		Provider:       &fakeProvider{},
		Registry:       registry,
		PermissionMode: permissionMode,
	})
	m.activeSession = sessions.Metadata{SessionID: "plan-test-session"}
	return m
}

func TestShiftTabDoesNotExitPlanMode(t *testing.T) {
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.input.SetValue("/plan")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected /plan to enter plan mode, got %s", next.permissionMode)
	}

	updated, _ = next.Update(testKeyShift(tea.KeyTab))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected shift+tab to leave plan mode untouched, got %s", next.permissionMode)
	}
}

func TestPlanOffRestoresPreviousPermissionMode(t *testing.T) {
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.input.SetValue("/plan")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected /plan to enter plan mode, got %s", next.permissionMode)
	}

	next.input.SetValue("/plan off")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("expected /plan off to restore the prior Ask mode, got %s", next.permissionMode)
	}
}

func TestBarePlanTogglesOff(t *testing.T) {
	// Regression: a second bare /plan used to just re-print the current plan
	// and leave PermissionModePlan active, contradicting the advertised
	// on/off toggle and stranding the user in read-only mode until they
	// discovered /plan off.
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.input.SetValue("/plan")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected /plan to enter plan mode, got %s", next.permissionMode)
	}

	next.input.SetValue("/plan")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("expected a second bare /plan to toggle plan mode off, got %s", next.permissionMode)
	}
	if !transcriptContains(next.transcript, "Exited plan mode") {
		t.Fatalf("expected an exit notice in the transcript, got %#v", next.transcript)
	}
}

func TestPlanCommandCreatesSessionBeforeWritingPlanFile(t *testing.T) {
	// Regression: on a fresh TUI (or after /new) the session ID is empty
	// until the first prompt lazily creates it. /plan open used to write the
	// plan file under the empty-session slug ("plan.md"), which every other
	// fresh session would also reuse and which orphaned its content once the
	// real session ID appeared. Entering plan mode must create the session
	// first so the plan file is named for it from the start.
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	cwd := t.TempDir()
	m := newModel(context.Background(), Options{
		Cwd:          cwd,
		SessionStore: testSessionStore(t),
		Registry:     registry,
	})
	if m.activeSession.SessionID != "" {
		t.Fatal("setup: expected a fresh model to have no active session")
	}

	m.input.SetValue("/plan")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.activeSession.SessionID == "" {
		t.Fatal("expected /plan to create a session before entering plan mode")
	}
	path, err := planmode.PlanFilePath(cwd, next.activeSession.SessionID)
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if !transcriptContains(next.transcript, path) {
		t.Fatalf("expected the plan-enter text to reference the real session's plan file %q, got %#v", path, next.transcript)
	}
}

func TestPlanOpenLaunchesEditorCommand(t *testing.T) {
	// Regression for the model being copied by value into tea.NewProgram
	// before the (now-removed) m.program field was assigned in run.go: /plan
	// open always took the "no live program" fallback and never actually
	// suspended the TUI to run $EDITOR.
	t.Setenv("EDITOR", "true")
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModePlan)

	m.input.SetValue("/plan open")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected /plan open to return a command that launches $EDITOR")
	}
	if transcriptContains(next.transcript, "Plan file:") {
		t.Fatalf("expected the editor to be launched instead of just reporting the path: %#v", next.transcript)
	}
}

func TestPlanOpenSeedsFileFromDraft(t *testing.T) {
	registry := tools.NewRegistry()
	planTool := tools.NewUpdatePlanTool()
	result := planTool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"content": "Wire model catalog", "status": "completed"},
		},
	})
	if result.Status != tools.StatusOK {
		t.Fatalf("update_plan setup failed: %#v", result)
	}
	registry.Register(planTool)

	// File seeding happens before the $VISUAL/$EDITOR check, so it must not
	// depend on an editor being configured; unset both explicitly so this test
	// doesn't depend on (or shell out to) whatever the host environment has set.
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	cwd := t.TempDir()
	m := newModel(context.Background(), Options{
		Cwd:            cwd,
		Registry:       registry,
		PermissionMode: agent.PermissionModePlan,
	})
	m.activeSession = sessions.Metadata{SessionID: "plan-test-session"}

	m.input.SetValue("/plan open")
	m.Update(testKey(tea.KeyEnter))

	path, err := planmode.PlanFilePath(cwd, "plan-test-session")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected the plan file to be created, got: %v", err)
	}
	if !strings.Contains(string(content), "Wire model catalog") {
		t.Fatalf("expected the new plan file to be seeded with the update_plan draft, got: %q", content)
	}
}

func TestUpdatePlanPersistsToPlanFile(t *testing.T) {
	// Regression: update_plan only updated the in-memory tool, so a plan built
	// entirely through the agent's prescribed workflow (the user never ran
	// /plan open) disappeared on restart/resume, and a plan file seeded once
	// by /plan open never reflected later update_plan calls. The plan file
	// must be the durable source of truth, refreshed on every update_plan call.
	store := testSessionStore(t)
	cwd := t.TempDir()
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_1", ToolName: "update_plan"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_1", ArgumentsFragment: `{"plan":[{"content":"Wire model catalog","status":"in_progress"}]}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "planned"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	m := newModel(context.Background(), Options{
		Cwd:          cwd,
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     registry,
		SessionStore: store,
	})
	m.input.SetValue("outline the approach")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}
	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)

	if next.activeSession.SessionID == "" {
		t.Fatal("expected the run to create a session")
	}
	content, ok, err := planmode.ReadPlan(cwd, next.activeSession.SessionID)
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if !ok {
		t.Fatal("expected update_plan to persist a plan file")
	}
	if !strings.Contains(content, "Wire model catalog") {
		t.Fatalf("expected the persisted plan file to reflect the update_plan call, got: %q", content)
	}
}

func TestPlanOpenEditorExitReloadsFileIntoPlan(t *testing.T) {
	// After /plan open edits the plan file in $EDITOR, the edited content
	// must be reloaded into the in-memory update_plan so it drives
	// execution, rather than being shadowed.
	registry := tools.NewRegistry()
	planTool := tools.NewUpdatePlanTool()
	registry.Register(planTool)

	cwd := t.TempDir()
	m := newModel(context.Background(), Options{
		Cwd:            cwd,
		Registry:       registry,
		PermissionMode: agent.PermissionModePlan,
	})
	m.activeSession = sessions.Metadata{SessionID: "plan-test-session"}

	path, err := planmode.PlanFilePath(cwd, "plan-test-session")
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if _, err := planmode.WritePlan(cwd, "plan-test-session", "1. [pending] original step"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}

	// Simulate the editor exiting after the user rewrote the file.
	if err := os.WriteFile(path, []byte("edited first step\nedited second step\n"), 0o600); err != nil {
		t.Fatalf("rewrite plan file: %v", err)
	}
	m.reloadPlanFromFile()

	got := planTool.CurrentPlan()
	if len(got) != 2 {
		t.Fatalf("expected 2 reloaded plan items, got %d: %+v", len(got), got)
	}
	if got[0].Content != "edited first step" || got[1].Content != "edited second step" {
		t.Fatalf("expected edited contents reloaded, got %+v", got)
	}
}

func TestPlanOpenEditorReloadPreservesStatusAndNotes(t *testing.T) {
	// Regression: parsePlanFileLines used to discard the "[status]" bracket
	// (resetting every reloaded item to "pending") and treat a "Notes: ..."
	// continuation line as its own bogus plan item instead of folding it
	// into the preceding step.
	registry := tools.NewRegistry()
	planTool := tools.NewUpdatePlanTool()
	registry.Register(planTool)

	cwd := t.TempDir()
	m := newModel(context.Background(), Options{
		Cwd:            cwd,
		Registry:       registry,
		PermissionMode: agent.PermissionModePlan,
	})
	m.activeSession = sessions.Metadata{SessionID: "plan-test-session"}

	content := "1. [completed] step one\n2. [in_progress] step two\n   Notes: half done\n3. [pending] step three"
	if _, err := planmode.WritePlan(cwd, "plan-test-session", content); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}

	m.reloadPlanFromFile()

	got := planTool.CurrentPlan()
	if len(got) != 3 {
		t.Fatalf("expected 3 plan items (no bogus 'Notes' item), got %d: %+v", len(got), got)
	}
	if got[0].Status != "completed" {
		t.Fatalf("expected step one to stay completed, got %q", got[0].Status)
	}
	if got[1].Status != "in_progress" || got[1].Notes != "half done" {
		t.Fatalf("expected step two to stay in_progress with notes preserved, got status=%q notes=%q", got[1].Status, got[1].Notes)
	}
	if got[2].Status != "pending" || got[2].Content != "step three" {
		t.Fatalf("expected step three unchanged, got %+v", got[2])
	}
}

func TestPlanModeWiresDraftSystemPrompt(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "planning"},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModePlan)
	m.provider = provider
	m.input.SetValue("outline the approach")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected submitting a prompt in plan mode to start an agent run")
	}
	updated, _ = next.Update(execCmd(cmd))
	_ = updated.(model)

	if len(provider.requests) != 1 {
		t.Fatalf("expected one provider request, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Messages) == 0 {
		t.Fatal("expected provider request to include a system message")
	}
	systemPrompt := provider.requests[0].Messages[0].Content
	if !strings.Contains(systemPrompt, "Plan mode is active on this session") {
		t.Fatalf("expected planmode.DraftSystemPrompt to be wired in, got:\n%s", systemPrompt)
	}
}
