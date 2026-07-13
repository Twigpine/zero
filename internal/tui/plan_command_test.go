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
