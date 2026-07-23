package tui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planmode"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// isolatePlanConfig redirects XDG_CONFIG_HOME so durable plan files and
// editor staging land under a throwaway directory. The directory is kept
// outside os.TempDir(): StageForEditor rejects staging roots that sit in the
// sandbox's default-writable temp tree.
func isolatePlanConfig(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	// t.Name() can contain slashes (subtests); flatten so MkdirAll gets one leaf.
	name := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ' ', ':':
			return '_'
		default:
			return r
		}
	}, t.Name())
	root := filepath.Join(home, ".cache", "zero-planmode-test", name)
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll plan config: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll plan config: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// os.UserConfigDir (which config.UserConfigDir defers to outside darwin)
	// reads %AppData% on Windows and ignores XDG_CONFIG_HOME there, so both
	// must be set for this override to actually take effect cross-platform.
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", root)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
}

func newPlanModeTestModel(t *testing.T, cwd string, permissionMode agent.PermissionMode) model {
	t.Helper()
	isolatePlanConfig(t)
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

func TestPlanOpenOutsidePlanModeDoesNotCreateSession(t *testing.T) {
	// Regression: /plan open when plan mode is inactive used to call
	// ensureActiveSession before openPlanInEditor's own guard rejected the
	// command, leaving a persistent empty session behind in /resume for what
	// should have been a pure no-op error.
	store := testSessionStore(t)
	m := newModel(context.Background(), Options{
		Cwd:            t.TempDir(),
		SessionStore:   store,
		PermissionMode: agent.PermissionModeAsk,
	})
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	m.registry = registry

	updated, _ := m.handlePlanCommand("open")
	next := updated.(model)
	if next.activeSession.SessionID != "" {
		t.Fatalf("expected no session to be created for an invalid /plan open, got %+v", next.activeSession)
	}
	if !transcriptContains(next.transcript, "Enter plan mode (/plan) before opening the plan file.") {
		t.Fatalf("expected a plan-mode-required notice in the transcript, got %#v", next.transcript)
	}
}

func TestPlanOpenBlockedWhileRunActive(t *testing.T) {
	// Regression: the bare /plan toggle refused to run while m.pending (a run
	// in flight), but "/plan open" had no such guard, letting it race a live
	// run to suspend the TUI into $EDITOR.
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModePlan)
	m.pending = true

	updated, cmd := m.handlePlanCommand("open")
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /plan open to return no command while a run is active")
	}
	if !transcriptContains(next.transcript, "Cannot open the plan file while a run is active") {
		t.Fatalf("expected a blocked-run notice in the transcript, got %#v", next.transcript)
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
	isolatePlanConfig(t)
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
	isolatePlanConfig(t)
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
	// It must also stay outside the workspace so the read-only auto-allow
	// contract remains honest.
	isolatePlanConfig(t)
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
	path, err := planmode.PlanFilePath(cwd, next.activeSession.SessionID)
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if strings.HasPrefix(path, cwd+string(os.PathSeparator)) || path == cwd {
		t.Fatalf("durable plan path %q must not live under the workspace %q", path, cwd)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".zero")); !os.IsNotExist(err) {
		t.Fatalf("update_plan must not create .zero under the workspace, stat err=%v", err)
	}
}

func TestPlanOpenEditorExitReloadsFileIntoPlan(t *testing.T) {
	// After /plan open edits the plan file in $EDITOR, the edited content
	// must be reloaded into the in-memory update_plan so it drives
	// execution, rather than being shadowed.
	isolatePlanConfig(t)
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

func TestPlanEditorFinishedMsgReloadsPanelAndConfirms(t *testing.T) {
	// The editor-completion path must run through the real planEditorFinishedMsg
	// case in Update (not just reloadPlanFromFile, which tests can call
	// directly): it reloads the edited file into BOTH the update_plan tool (the
	// execution source of truth) and the sticky panel, and confirms the reload
	// in the transcript so a bare /plan open doesn't look like a silent no-op.
	isolatePlanConfig(t)
	registry := tools.NewRegistry()
	planTool := tools.NewUpdatePlanTool()
	registry.Register(planTool)

	cwd := t.TempDir()
	m := newModel(context.Background(), Options{
		Cwd:            cwd,
		SessionStore:   testSessionStore(t),
		Registry:       registry,
		PermissionMode: agent.PermissionModePlan,
	})
	m, err := m.ensureActiveSession("plan editor completion")
	if err != nil {
		t.Fatalf("ensureActiveSession: %v", err)
	}
	if _, err := planmode.WritePlan(cwd, m.activeSession.SessionID, "1. [in_progress] edited step\n   Notes: from editor"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}

	updated, _ := m.Update(planEditorFinishedMsg{err: nil})
	next := updated.(model)

	// update_plan (what drives execution) reflects the edited file.
	got := planTool.CurrentPlan()
	if len(got) != 1 || got[0].Content != "edited step" || got[0].Status != "in_progress" {
		t.Fatalf("expected update_plan reloaded from the edited file, got %+v", got)
	}
	// The sticky panel was refreshed too, not just the tool state.
	if next.plan.isEmpty() {
		t.Fatal("expected the sticky plan panel to be refreshed from the reloaded file")
	}
	// A completion message reaches the transcript.
	if !transcriptContains(next.transcript, "Reloaded the edited plan.") {
		t.Fatalf("expected an editor-reload completion message, got %#v", next.transcript)
	}
}

func TestPlanOpenEditorReloadPreservesStatusAndNotes(t *testing.T) {
	// Regression: parsePlanFileLines used to discard the "[status]" bracket
	// (resetting every reloaded item to "pending") and treat a "Notes: ..."
	// continuation line as its own bogus plan item instead of folding it
	// into the preceding step.
	isolatePlanConfig(t)
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

func TestPlanItemsRoundTripMultilineContent(t *testing.T) {
	// Regression: a multi-line PlanItem.Content (e.g. from an agent-authored
	// update_plan call) used to be written verbatim by formatPlanItems, and
	// its continuation lines then reloaded as bogus new freeform pending
	// steps instead of staying part of the original item's Content.
	items := []tools.PlanItem{
		{Content: "first line\nsecond line\nthird line", Status: "in_progress", Notes: "a note\nsecond note line"},
		{Content: "step two", Status: "pending"},
	}
	reloaded := parsePlanFileLines(formatPlanItems(items))
	if len(reloaded) != 2 {
		t.Fatalf("expected 2 items after round-trip, got %d: %+v", len(reloaded), reloaded)
	}
	if reloaded[0].Content != items[0].Content {
		t.Fatalf("expected multi-line content preserved, got %q", reloaded[0].Content)
	}
	if reloaded[0].Status != "in_progress" || reloaded[0].Notes != items[0].Notes {
		t.Fatalf("expected status/notes preserved, got %+v", reloaded[0])
	}
	if reloaded[1].Content != "step two" {
		t.Fatalf("expected step two unaffected, got %+v", reloaded[1])
	}
}

func TestPlanItemsRoundTripAmbiguousContinuations(t *testing.T) {
	// Regression for the encoding ambiguities that silently rewrote plans on
	// an open-and-save: a continuation that looks like a numbered step used
	// to shatter into a new item, a continuation beginning "Notes:" used to
	// become the notes delimiter, and blank continuation lines vanished.
	items := []tools.PlanItem{
		{Content: "Investigate\n2. validate", Status: "pending"},
		{Content: "Header\nNotes: literal content line", Status: "pending", Notes: "real note"},
		{Content: "before\n\nafter", Status: "pending"},
		{Content: "escape\n\\Notes: already escaped", Status: "pending"},
	}
	reloaded := parsePlanFileLines(formatPlanItems(items))
	if len(reloaded) != len(items) {
		t.Fatalf("expected %d items after round-trip, got %d: %+v", len(items), len(reloaded), reloaded)
	}
	for index := range items {
		if reloaded[index].Content != items[index].Content {
			t.Fatalf("item %d content changed on round-trip: %q -> %q", index, items[index].Content, reloaded[index].Content)
		}
		if reloaded[index].Notes != items[index].Notes {
			t.Fatalf("item %d notes changed on round-trip: %q -> %q", index, items[index].Notes, reloaded[index].Notes)
		}
	}
	// A second pass must be a fixed point: open-and-save twice changes nothing.
	again := parsePlanFileLines(formatPlanItems(reloaded))
	if len(again) != len(reloaded) {
		t.Fatalf("second round-trip changed item count: %d -> %d", len(reloaded), len(again))
	}
	for index := range reloaded {
		if again[index] != reloaded[index] {
			t.Fatalf("second round-trip changed item %d: %+v -> %+v", index, reloaded[index], again[index])
		}
	}
}

func TestParsePlanFileLinesFoldsMultilineNotes(t *testing.T) {
	// Regression: a "Notes: ..." block spanning more than one line used to
	// have its continuation lines treated as bogus new pending steps instead
	// of folding into the preceding item's Notes.
	content := "1. [in_progress] step one\n" +
		"   Notes: first line\n" +
		"   second line continuation\n" +
		"2. [pending] step two\n" +
		"a freeform unnumbered line"

	items := parsePlanFileLines(content)
	if len(items) != 3 {
		t.Fatalf("expected 3 items (2 numbered steps + 1 freeform), got %d: %+v", len(items), items)
	}
	if items[0].Notes != "first line\nsecond line continuation" {
		t.Fatalf("expected multi-line notes folded, got %q", items[0].Notes)
	}
	if items[1].Content != "step two" || items[1].Notes != "" {
		t.Fatalf("expected step two unaffected, got %+v", items[1])
	}
	if items[2].Content != "a freeform unnumbered line" || items[2].Status != "pending" {
		t.Fatalf("expected a trailing unnumbered line to become its own step, got %+v", items[2])
	}
}

func TestPlanModeWiresDraftSystemPrompt(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "planning"},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newPlanModeTestModel(t, t.TempDir(), agent.PermissionModePlan)
	// Embedders set product policy via agentOptions.SystemPrompt. Plan mode
	// must layer its restriction onto that prompt rather than replace it.
	const configuredPrompt = "Custom product policy for this embedder."
	m.agentOptions.SystemPrompt = configuredPrompt
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
	if !strings.Contains(systemPrompt, configuredPrompt) {
		t.Fatalf("expected configured SystemPrompt to be preserved under plan mode, got:\n%s", systemPrompt)
	}
	if !strings.HasPrefix(systemPrompt, configuredPrompt) {
		t.Fatalf("expected plan-mode layer to follow the configured prompt, got:\n%s", systemPrompt)
	}
}

func TestReenteringPlanModePreservesExistingPlanFile(t *testing.T) {
	dir := t.TempDir()
	m := newPlanModeTestModel(t, dir, agent.PermissionModeAsk)
	const initialPlan = "1. [pending] Step one from disk\n2. [completed] Step two from disk"
	if _, err := planmode.WritePlan(dir, m.activeSession.SessionID, initialPlan); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}

	m.input.SetValue("/plan")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected /plan to enter plan mode, got %s", next.permissionMode)
	}
	if len(next.plan.steps) != 2 {
		t.Fatalf("expected 2 plan items reloaded from disk, got %d", len(next.plan.steps))
	}
	if next.plan.steps[0].content != "Step one from disk" || next.plan.steps[1].status != "completed" {
		t.Fatalf("unexpected plan steps: %+v", next.plan.steps)
	}
}

func TestParsePlanFileLinesPreservesContinuationWhitespace(t *testing.T) {
	content := "1. [pending] Step with indented code\n" +
		"   ```go\n" +
		"     func hello() {}\n" +
		"   ```\n" +
		"   Notes:\n" +
		"     - note line 1\n" +
		"     - note line 2  "

	items := parsePlanFileLines(content)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	expectedContent := "Step with indented code\n```go\n  func hello() {}\n```"
	if items[0].Content != expectedContent {
		t.Fatalf("content = %q, want %q", items[0].Content, expectedContent)
	}
	expectedNotes := "  - note line 1\n  - note line 2  "
	if items[0].Notes != expectedNotes {
		t.Fatalf("notes = %q, want %q", items[0].Notes, expectedNotes)
	}
}
