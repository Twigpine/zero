package tui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/peermsg"
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
	parent := filepath.Join(home, ".cache", "zero-planmode-test")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("MkdirAll plan config parent: %v", err)
	}
	root, err := os.MkdirTemp(parent, name+"-")
	if err != nil {
		t.Fatalf("MkdirTemp plan config: %v", err)
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

func newPlanCommandTestModel(t *testing.T, cwd string, permissionMode agent.PermissionMode) model {
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

func TestHandlePlanCommandSyncsPeerIdentityOnEnterAndExit(t *testing.T) {
	isolatePlanConfig(t)
	svc, err := peermsg.New(peermsg.Options{
		RootDir: t.TempDir(),
		Identity: peermsg.Identity{
			Name:            "zero",
			Cwd:             t.TempDir(),
			PermissionClass: peermsg.PermissionBypass,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(func(peermsg.InboundMessage) bool { return true }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModeUnsafe)
	m.peerService = svc

	updated, _ := m.handlePlanCommand("on")
	next := updated.(model)
	if got := next.peerService.Self().PermissionClass; got != peermsg.PermissionPrompting {
		t.Fatalf("after /plan on PermissionClass = %q, want %q", got, peermsg.PermissionPrompting)
	}

	updated, _ = next.handlePlanCommand("off")
	next = updated.(model)
	if got := next.peerService.Self().PermissionClass; got != peermsg.PermissionBypass {
		t.Fatalf("after /plan off PermissionClass = %q, want %q", got, peermsg.PermissionBypass)
	}
}

func TestShiftTabDoesNotExitPlanMode(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.input.SetValue("/plan on")
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
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.input.SetValue("/plan on")
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
	if !transcriptContains(next.transcript, "Enter plan mode (/plan on) before opening the plan file.") {
		t.Fatalf("expected a plan-mode-required notice in the transcript, got %#v", next.transcript)
	}
}

func TestPlanOpenBlockedWhileRunActive(t *testing.T) {
	// Regression: the bare /plan toggle refused to run while m.pending (a run
	// in flight), but "/plan open" had no such guard, letting it race a live
	// run to suspend the TUI into $EDITOR.
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
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

func TestPlanOffBlockedWhileRunActive(t *testing.T) {
	// Mid-run /plan off would flip permissionMode before agentResponseMsg,
	// so completeRemaining would mark every plan step completed for a
	// planning turn. Exit must wait for the run to finish (or cancel).
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	m.pending = true

	updated, cmd := m.handlePlanCommand("off")
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /plan off to return no command while a run is active")
	}
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected plan mode preserved while run pending, got %s", next.permissionMode)
	}
	if !transcriptContains(next.transcript, "Cannot exit plan mode while a run is active") {
		t.Fatalf("expected a blocked-exit notice in the transcript, got %#v", next.transcript)
	}

}

func TestSplitEditorCommandWindowsPaths(t *testing.T) {
	// shell.Fields treats unquoted backslash as a POSIX escape, so
	// C:\Windows\notepad.exe becomes C:Windowsnotepad.exe. Windows-style
	// absolute paths must keep separators literal.
	parts, err := splitEditorCommandFor("windows", `C:\Windows\System32\notepad.exe`)
	if err != nil {
		t.Fatalf("split unquoted drive path: %v", err)
	}
	if len(parts) != 1 || parts[0] != `C:\Windows\System32\notepad.exe` {
		t.Fatalf("unquoted Windows path: got %#v", parts)
	}

	parts, err = splitEditorCommandFor("windows", `"C:\Program Files\Git\bin\vim.exe" --wait`)
	if err != nil {
		t.Fatalf("split quoted Windows path: %v", err)
	}
	if len(parts) != 2 || parts[0] != `C:\Program Files\Git\bin\vim.exe` || parts[1] != "--wait" {
		t.Fatalf("quoted Windows path with args: got %#v", parts)
	}

	// Quoted Unix paths still use POSIX shell.Fields (spaces preserved).
	parts, err = splitEditorCommandFor("linux", `"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code" --wait`)
	if err != nil {
		t.Fatalf("split quoted Unix path: %v", err)
	}
	if len(parts) != 2 || !strings.Contains(parts[0], "Visual Studio Code") || parts[1] != "--wait" {
		t.Fatalf("quoted Unix path: got %#v", parts)
	}

	// Unquoted simple command on any OS.
	parts, err = splitEditorCommandFor("linux", "code --wait")
	if err != nil {
		t.Fatalf("split simple command: %v", err)
	}
	if len(parts) != 2 || parts[0] != "code" || parts[1] != "--wait" {
		t.Fatalf("simple command: got %#v", parts)
	}

	// Regression: a Windows command containing backslashes but not beginning
	// with a drive or UNC path (e.g. a relative .\tools\editor.exe) used to
	// fall through to shell.Fields, which drops the separators as POSIX
	// escapes. It must keep backslashes literal too.
	parts, err = splitEditorCommandFor("windows", `.\tools\editor.exe --wait`)
	if err != nil {
		t.Fatalf("split relative Windows path: %v", err)
	}
	if len(parts) != 2 || parts[0] != `.\tools\editor.exe` || parts[1] != "--wait" {
		t.Fatalf("relative Windows path: got %#v", parts)
	}

	_, err = splitEditorCommandFor("windows", `"C:\Program Files\editor.exe --wait`)
	if err == nil {
		t.Fatal("expected unterminated Windows quote to fail")
	}

	// Single-quoted values still go through POSIX shell.Fields (literal
	// content, backslashes preserved), matching the quoted-path contract.
	parts, err = splitEditorCommandFor("windows", `'C:\Program Files\editor.exe' --wait`)
	if err != nil {
		t.Fatalf("split single-quoted Windows path: %v", err)
	}
	if len(parts) != 2 || parts[0] != `C:\Program Files\editor.exe` || parts[1] != "--wait" {
		t.Fatalf("single-quoted Windows path: got %#v", parts)
	}
}

func TestBarePlanReportsStatusWithoutExiting(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.input.SetValue("/plan on")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected /plan on to enter plan mode, got %s", next.permissionMode)
	}

	next.input.SetValue("/plan")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected bare /plan to preserve plan mode, got %s", next.permissionMode)
	}
}
func TestPlanOpenCreatesSessionBeforeWritingPlanFile(t *testing.T) {
	// Regression: on a fresh TUI (or after /new) the session ID is empty
	// until the first prompt lazily creates it. /plan open must create the
	// session before writing its plan file so fresh sessions do not share the
	// empty-session plan path.
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

	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	m.input.SetValue("/plan on")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	next.input.SetValue("/plan open")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if next.activeSession.SessionID == "" {
		t.Fatal("expected /plan open to create a session before writing the plan file")
	}
	path, err := planmode.PlanFilePath(cwd, next.activeSession.SessionID)
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected plan file for the active session: %v", err)
	}
}

// TestPlanOnCreatesSessionAndNamesItsPlanFile covers plan-mode entry on its
// own, without the /plan open that follows it in the test above. On a fresh
// TUI (or after /new) the session ID is empty until the first prompt lazily
// creates it, and PlanFilePath maps an empty ID onto a single shared
// no-session slug. Entering plan mode must create the session first, so the
// banner names that session's own plan file rather than the shared fallback
// that every other fresh session would also resolve to.
func TestPlanOnCreatesSessionAndNamesItsPlanFile(t *testing.T) {
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

	m.input.SetValue("/plan on")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.activeSession.SessionID == "" {
		t.Fatal("expected /plan on to create a session before entering plan mode")
	}
	path, err := planmode.PlanFilePath(cwd, next.activeSession.SessionID)
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if !transcriptContains(next.transcript, path) {
		t.Fatalf("expected the plan-entry banner to name the real session's plan file %q, got %#v", path, next.transcript)
	}
	// The shared no-session path must never be what the user is pointed at.
	fallback, err := planmode.PlanFilePath(cwd, "")
	if err != nil {
		t.Fatalf("PlanFilePath(empty): %v", err)
	}
	if transcriptContains(next.transcript, fallback) {
		t.Fatalf("plan-entry banner named the shared no-session plan file %q", fallback)
	}
}

func TestPlanOpenLaunchesEditorCommand(t *testing.T) {
	// Regression for the model being copied by value into tea.NewProgram
	// before the (now-removed) m.program field was assigned in run.go: /plan
	// open always took the "no live program" fallback and never actually
	// suspended the TUI to run $EDITOR.
	t.Setenv("EDITOR", "true")
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)

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
	// Canonicalize both sides: on macOS t.TempDir() is under /var while
	// resolved paths live under /private/var, so a raw HasPrefix check can
	// pass even when the plan file is inside the workspace.
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("EvalSymlinks cwd: %v", err)
	}
	// Plan path itself may not exist yet on a pure path check; resolve the
	// deepest existing ancestor (the plans root or its parent) via Dir.
	resolvedPlanDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		// Fall back to physicalPath-style resolve of the parent only when the
		// plan dir was never created (ReadPlan above already confirmed it exists).
		t.Fatalf("EvalSymlinks plan dir: %v", err)
	}
	if resolvedPlanDir == resolvedCwd || strings.HasPrefix(resolvedPlanDir, resolvedCwd+string(os.PathSeparator)) {
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

// TestPlanEditorFinishedMsgNoOpEditRecordsNothing covers quitting $EDITOR
// without changing anything. The session event the handler writes is phrased as
// the user's own words ("I edited the plan file directly"), so recording it for
// an untouched file puts a false statement into the next turn's context, and
// repeated opens would each restate the whole plan into the session log.
func TestPlanEditorFinishedMsgNoOpEditRecordsNothing(t *testing.T) {
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
	m, err := m.ensureActiveSession("plan editor no-op")
	if err != nil {
		t.Fatalf("ensureActiveSession: %v", err)
	}
	if _, err := planmode.WritePlan(cwd, m.activeSession.SessionID, "1. [in_progress] untouched step\n   Notes: keep"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	// Load that plan in, so the tool state already matches the file exactly:
	// the editor opened it and quit without saving a change.
	if _, _, err := m.reloadPlanFromFile(); err != nil {
		t.Fatalf("reloadPlanFromFile: %v", err)
	}
	eventsBefore := len(m.sessionEvents)

	updated, _ := m.Update(planEditorFinishedMsg{err: nil})
	next := updated.(model)

	if len(next.sessionEvents) != eventsBefore {
		t.Fatalf("an unchanged plan file must not record a session event: before=%d after=%d", eventsBefore, len(next.sessionEvents))
	}
	if transcriptContains(next.transcript, "Reloaded the edited plan.") {
		t.Fatalf("an unchanged plan file must not claim a reload, got %#v", next.transcript)
	}
	// The plan itself must survive untouched.
	if got := planTool.CurrentPlan(); len(got) != 1 || got[0].Content != "untouched step" || got[0].Status != "in_progress" {
		t.Fatalf("no-op edit changed the plan: %+v", got)
	}
}

func TestPlanEditorFinishedMsgReloadErrorSurfaces(t *testing.T) {
	// Failure path: if ReadPlan fails after the editor exits (e.g. the durable
	// plan file was deleted or became unreadable), the reload error must surface
	// in the transcript instead of failing silently.
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
	m, err := m.ensureActiveSession("plan editor completion failure")
	if err != nil {
		t.Fatalf("ensureActiveSession: %v", err)
	}
	// Write a plan file, then replace it with a directory at the same path so
	// ReadPlan fails (refused as a non-regular file) between editor exit and
	// reload. A plain deletion would not do: ReadPlan treats a missing file as
	// ok=false, not an error, so the reload would silently no-op instead of
	// surfacing a failure.
	if _, err := planmode.WritePlan(cwd, m.activeSession.SessionID, "1. [in_progress] step"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	path, err := planmode.PlanFilePath(cwd, m.activeSession.SessionID)
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove plan file: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("replace plan file with directory: %v", err)
	}

	// Simulate editor completion with the plan file now missing
	updated, _ := m.Update(planEditorFinishedMsg{err: nil})
	next := updated.(model)

	// The reload error should appear in the transcript
	if !transcriptContains(next.transcript, "plan reload error:") {
		t.Fatalf("expected a plan reload error message in transcript, got %#v", next.transcript)
	}
}

func TestPlanOnReloadErrorPreservesExistingPlan(t *testing.T) {
	isolatePlanConfig(t)
	registry := tools.NewRegistry()
	planTool := tools.NewUpdatePlanTool()
	planTool.SetPlan([]tools.PlanItem{{Content: "in-memory step", Status: "pending"}})
	registry.Register(planTool)

	cwd := t.TempDir()
	store := testSessionStore(t)
	m := newModel(context.Background(), Options{
		Cwd:          cwd,
		SessionStore: store,
		Registry:     registry,
	})
	m, err := m.ensureActiveSession("plan reload failure test")
	if err != nil {
		t.Fatalf("ensureActiveSession: %v", err)
	}
	m.plan.updateFromItems(planTool.CurrentPlan(), m.now())

	if _, err := planmode.WritePlan(cwd, m.activeSession.SessionID, "1. [pending] on disk"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	path, err := planmode.PlanFilePath(cwd, m.activeSession.SessionID)
	if err != nil {
		t.Fatalf("PlanFilePath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove plan file: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("replace plan file with directory: %v", err)
	}

	updated, _ := m.handlePlanCommand("on")
	next := updated.(model)
	if len(planTool.CurrentPlan()) != 1 || planTool.CurrentPlan()[0].Content != "in-memory step" {
		t.Fatalf("expected in-memory plan preserved after /plan on reload error, got %+v", planTool.CurrentPlan())
	}
	if next.plan.isEmpty() {
		t.Fatal("expected sticky plan panel preserved after /plan on reload error")
	}
	if !transcriptContains(next.transcript, "plan reload error:") {
		t.Fatalf("expected a plan reload error message in transcript, got %#v", next.transcript)
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
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
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

// Regression: when permissionModeBeforePlan is empty (legacy/incomplete state),
// exitPlanMode must fall back to Ask, not Auto, so leaving plan mode does not
// silently re-enable unrestricted tools.
func TestExitPlanModeFallsBackToAsk(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	m.permissionModeBeforePlan = ""

	next := m.exitPlanMode()
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("expected empty permissionModeBeforePlan to fall back to Ask, got %s", next.permissionMode)
	}
	if next.permissionModeBeforePlan != "" {
		t.Fatalf("expected permissionModeBeforePlan to be cleared, got %q", next.permissionModeBeforePlan)
	}
}

func TestSessionToolResultMetaStripsPlanSnapshot(t *testing.T) {
	meta := map[string]string{
		tools.PlanSnapshotMeta: `[{"content":"step","status":"pending"}]`,
		"other":                "keep",
	}
	got := sessionToolResultMeta(meta)
	if _, ok := got[tools.PlanSnapshotMeta]; ok {
		t.Fatalf("expected plan_snapshot stripped from session meta, got %#v", got)
	}
	if got["other"] != "keep" {
		t.Fatalf("expected other meta keys preserved, got %#v", got)
	}
	if sessionToolResultMeta(map[string]string{tools.PlanSnapshotMeta: "x"}) != nil {
		t.Fatal("expected nil when only plan_snapshot was present")
	}
	if sessionToolResultMeta(nil) != nil {
		t.Fatal("expected nil for empty meta")
	}
}

// Regression: entering plan mode must pause armed /loop ticks and /goal
// continuations so they do not fire read-only turns that cannot make
// progress. /plan off unpauses loops and may resume an active goal.
func TestPlanCommandPausesArmedContinuations(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{SessionID: "plan_pause", Title: "plan pause", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = store.CreateGoal(session.SessionID, "Keep shipping", 0)
	if err != nil {
		t.Fatal(err)
	}
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModeAsk)
	m.sessionStore = store
	m.activeSession = session
	m.provider = &scriptedProvider{}
	m = startFixedLoop(m, "keep shipping", time.Minute)

	updated, cmd := m.handlePlanCommand("on")
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /plan on to be synchronous")
	}
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected plan mode, got %s", next.permissionMode)
	}
	if len(next.loops) != 1 || !next.loops[0].paused {
		t.Fatalf("expected the armed loop to be paused in plan mode, got %+v", next.loops)
	}
	if !transcriptContains(next.transcript, "Automatic /loop and /goal continuations are paused") {
		t.Fatalf("expected a pause notice, got %#v", next.transcript)
	}
	idle, fireCmd := next.fireDueLoopIfIdle()
	if fireCmd != nil || idle.activeLoopID != "" {
		t.Fatal("paused loop must not fire while plan mode is active")
	}
	idle, goalCmd := idle.launchGoalContinuationIfReady()
	if goalCmd != nil || idle.pending {
		t.Fatal("armed goal must not continue while plan mode is active")
	}

	updated, cmd = idle.handlePlanCommand("off")
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("expected Ask restored, got %s", next.permissionMode)
	}
	if len(next.loops) != 1 || next.loops[0].paused {
		t.Fatalf("expected the loop to resume after /plan off, got %+v", next.loops)
	}
	if cmd == nil || !next.pending {
		t.Fatal("expected /plan off to resume the armed goal continuation")
	}
	if !transcriptContains(next.transcript, "Continuing goal: Keep shipping") {
		t.Fatalf("expected goal continuation after /plan off, got %#v", next.transcript)
	}
}

func TestReenteringPlanModePreservesExistingPlanFile(t *testing.T) {
	dir := t.TempDir()
	m := newPlanCommandTestModel(t, dir, agent.PermissionModeAsk)
	const initialPlan = "1. [pending] Step one from disk\n2. [completed] Step two from disk"
	if _, err := planmode.WritePlan(dir, m.activeSession.SessionID, initialPlan); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}

	m.input.SetValue("/plan on")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("expected /plan on to enter plan mode, got %s", next.permissionMode)
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
