package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// TestPlanCommandEntersAndExitsPlanMode drives /plan on then /plan off and
// confirms m.permissionMode actually flips to PermissionModePlan and back —
// the entry/exit path the previous /plan (display-only) command was missing.
func TestPlanCommandEntersAndExitsPlanMode(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})

	updated, cmd := m.dispatchCommand(parseCommand("/plan on"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /plan on to be synchronous")
	}
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("permissionMode after /plan on = %s, want plan", next.permissionMode)
	}
	if !transcriptContains(next.transcript, "read-only planning") {
		t.Fatalf("expected activation notice in transcript, got %#v", next.transcript)
	}

	updated, cmd = next.dispatchCommand(parseCommand("/plan off"))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected /plan off to be synchronous")
	}
	if next.permissionMode != agent.PermissionModeAuto {
		t.Fatalf("permissionMode after /plan off = %s, want auto (restored)", next.permissionMode)
	}
	if !transcriptContains(next.transcript, "restored to auto") {
		t.Fatalf("expected restore notice in transcript, got %#v", next.transcript)
	}
}

// TestPlanCommandRestoresPriorModeOnExit confirms /plan off restores whatever
// mode was active before /plan on, not a hardcoded default.
func TestPlanCommandRestoresPriorModeOnExit(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAsk})

	updated, _ := m.dispatchCommand(parseCommand("/plan on"))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("permissionMode after /plan on = %s, want plan", next.permissionMode)
	}

	updated, _ = next.dispatchCommand(parseCommand("/plan off"))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("permissionMode after /plan off = %s, want ask (restored)", next.permissionMode)
	}
}

// TestPlanCommandStatusDoesNotChangeMode is a regression guard for the
// pre-existing display-only behavior: bare /plan and /plan status must keep
// reporting the plan without touching the active permission mode.
func TestPlanCommandStatusDoesNotChangeMode(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto, Registry: tools.NewRegistry()})

	updated, _ := m.dispatchCommand(parseCommand("/plan"))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModeAuto {
		t.Fatalf("bare /plan changed permissionMode to %s", next.permissionMode)
	}

	updated, _ = next.dispatchCommand(parseCommand("/plan status"))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAuto {
		t.Fatalf("/plan status changed permissionMode to %s", next.permissionMode)
	}
}

// TestPlanCommandOffWithoutActivePlanIsNoop confirms /plan off is a harmless
// no-op (not an error, not a mode change) when plan mode was never entered.
func TestPlanCommandOffWithoutActivePlanIsNoop(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})

	updated, _ := m.dispatchCommand(parseCommand("/plan off"))
	next := updated.(model)
	if next.permissionMode != agent.PermissionModeAuto {
		t.Fatalf("permissionMode = %s, want unchanged auto", next.permissionMode)
	}
	if !transcriptContains(next.transcript, "Not currently active") {
		t.Fatalf("expected not-active notice in transcript, got %#v", next.transcript)
	}
}

// TestPlanCommandOnTwiceDoesNotClobberSavedMode confirms a redundant /plan on
// doesn't overwrite the saved prior mode with Plan itself, which would strand
// /plan off unable to restore the real original mode.
func TestPlanCommandOnTwiceDoesNotClobberSavedMode(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAsk})

	updated, _ := m.dispatchCommand(parseCommand("/plan on"))
	next := updated.(model)
	updated, _ = next.dispatchCommand(parseCommand("/plan on"))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModePlan {
		t.Fatalf("permissionMode = %s, want plan", next.permissionMode)
	}

	updated, _ = next.dispatchCommand(parseCommand("/plan off"))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("permissionMode after off = %s, want ask restored (double /plan on must not clobber it)", next.permissionMode)
	}
}

// TestNextPermissionModeLeavesPlanUntouched confirms the shift+tab Auto<->Ask
// toggle cannot silently exit plan mode: folding Plan to Ask would be a LESS
// strict landing (Ask still permits write/shell tools with a prompt), so the
// read-only guarantee must only be given up via the explicit /plan off exit.
func TestNextPermissionModeLeavesPlanUntouched(t *testing.T) {
	if got := nextPermissionMode(agent.PermissionModePlan); got != agent.PermissionModePlan {
		t.Fatalf("nextPermissionMode(Plan) = %s, want Plan unchanged", got)
	}
}

// TestPlanModeBlocksRewindMutation guards the coderabbitai finding that /plan
// on only flips the agent permission mode: local TUI commands bypass tool
// filtering entirely, so /rewind — which restores workspace files straight
// from a checkpoint — needed its own gate. This drives a real checkpoint and
// confirms the file on disk is untouched (and no rewind summary appears)
// while plan mode is active.
func TestPlanModeBlocksRewindMutation(t *testing.T) {
	store := testSessionStore(t)
	ws := t.TempDir()
	session, err := store.Create(sessions.CreateInput{Title: "plan rewind", Cwd: ws, ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	path := filepath.Join(ws, "a.txt")
	writeTestFile(t, ws, "a.txt", "original")
	if _, err := store.CaptureToolCheckpoint(session.SessionID, ws, "write_file", []string{"a.txt"}); err != nil {
		t.Fatalf("CaptureToolCheckpoint: %v", err)
	}
	writeTestFile(t, ws, "a.txt", "changed while planning")

	m := newModel(context.Background(), Options{SessionStore: store, Cwd: ws, PermissionMode: agent.PermissionModePlan})
	m.activeSession = session

	updated, cmd := m.dispatchCommand(parseCommand("/rewind"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /rewind to be blocked synchronously in plan mode")
	}
	if !transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("expected plan-mode denial, got %#v", next.transcript)
	}
	if transcriptContains(next.transcript, "Rewound") {
		t.Fatalf("rewind should not have run in plan mode, got %#v", next.transcript)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "changed while planning" {
		t.Fatalf("rewind mutated the workspace despite plan mode gate: %q", got)
	}
}

// TestPlanModeBlocksExportMutation guards the same finding for /export, which
// writes a transcript file to disk outside the agent tool gate.
func TestPlanModeBlocksExportMutation(t *testing.T) {
	dir := t.TempDir()
	m := newModel(context.Background(), Options{Cwd: dir, PermissionMode: agent.PermissionModePlan})

	updated, cmd := m.dispatchCommand(parseCommand("/export out.txt"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /export to be blocked synchronously in plan mode")
	}
	if !transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("expected plan-mode denial, got %#v", next.transcript)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("export should not have written a file in plan mode, stat err=%v", err)
	}
}

// TestPlanModeBlocksSandboxSetupProcess guards the same finding for
// /sandbox-setup, which spawns a native host process outside the agent tool
// gate: the injected SandboxSetupCommand must never run while plan mode is
// active, and the command must be handled synchronously (no async tea.Cmd).
func TestPlanModeBlocksSandboxSetupProcess(t *testing.T) {
	called := false
	m := newModel(context.Background(), Options{
		PermissionMode: agent.PermissionModePlan,
		SandboxSetupCommand: func(context.Context) SandboxSetupCommandResult {
			called = true
			return SandboxSetupCommandResult{ExitCode: 0}
		},
	})

	updated, cmd := m.dispatchCommand(parseCommand("/sandbox-setup"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /sandbox-setup to be blocked synchronously in plan mode")
	}
	if called {
		t.Fatal("sandbox setup process must not run in plan mode")
	}
	if !transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("expected plan-mode denial, got %#v", next.transcript)
	}
}

// TestPlanModeBlocksInitCommand: /init's sole job is writing AGENTS.md, which
// plan mode then denies at the tool gate. Block the command up front so the
// operator gets a clear "exit plan mode first" instead of a failed turn.
func TestPlanModeBlocksInitCommand(t *testing.T) {
	m := newModel(context.Background(), Options{Cwd: t.TempDir(), PermissionMode: agent.PermissionModePlan})

	updated, cmd := m.dispatchCommand(parseCommand("/init"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /init to be blocked synchronously in plan mode")
	}
	if !transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("expected plan-mode denial, got %#v", next.transcript)
	}
	if transcriptContains(next.transcript, "Generate an AGENTS.md") {
		t.Fatalf("/init must not launch the bootstrap turn in plan mode, got %#v", next.transcript)
	}
}

// TestPlanModeAllowsBareMCPManagerView: bare /mcp only opens the read-only
// manager overlay and must stay available while planning.
func TestPlanModeAllowsBareMCPManagerView(t *testing.T) {
	m := newModel(context.Background(), Options{Cwd: t.TempDir(), PermissionMode: agent.PermissionModePlan})

	updated, cmd := m.dispatchCommand(parseCommand("/mcp"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected bare /mcp to be synchronous")
	}
	if transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("bare /mcp must not be blocked in plan mode, got %#v", next.transcript)
	}
	if next.mcpManager == nil {
		t.Fatal("expected bare /mcp to open the MCP manager in plan mode")
	}
}

// TestPlanModeBlocksMCPSubcommands: /mcp <sub> mutates server configuration
// and must stay blocked while plan mode is active.
func TestPlanModeBlocksMCPSubcommands(t *testing.T) {
	m := newModel(context.Background(), Options{Cwd: t.TempDir(), PermissionMode: agent.PermissionModePlan})

	updated, cmd := m.dispatchCommand(parseCommand("/mcp disable docs"))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /mcp subcommand to be blocked synchronously in plan mode")
	}
	if !transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("expected plan-mode denial for /mcp subcommand, got %#v", next.transcript)
	}
	if next.mcpManager != nil {
		t.Fatal("/mcp subcommand must not open the manager when blocked")
	}
}

// TestPlanModeCommandGuardDoesNotBlockOutsideMode confirms the guard is
// scoped to plan mode: the same commands must behave normally (not be
// swallowed by the new check) once plan mode is off.
func TestPlanModeCommandGuardDoesNotBlockOutsideMode(t *testing.T) {
	dir := t.TempDir()
	m := newModel(context.Background(), Options{Cwd: dir, PermissionMode: agent.PermissionModeAuto})
	m.transcript = append(m.transcript, transcriptRow{kind: rowUser, text: "hello"})

	updated, _ := m.dispatchCommand(parseCommand("/export out.txt"))
	next := updated.(model)
	if transcriptContains(next.transcript, "unavailable in plan mode") {
		t.Fatalf("export should not be gated outside plan mode, got %#v", next.transcript)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("expected export to write the file outside plan mode: %v", err)
	}
}

func newPlanModeTestModel(root string, provider zeroruntime.Provider) model {
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(root, nil) {
		registry.Register(tool)
	}
	return newModel(context.Background(), Options{
		Cwd:          root,
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     registry,
		// Ask (not Auto) is the base mode here so the "write_file is advertised
		// again after /plan off" check is unambiguous: ToolAdvertised only
		// exposes prompt-permission tools like write_file/bash unconditionally
		// under Ask (Auto hides them from advertisement entirely unless a tool
		// opts into AdvertiseInAuto, which write_file does not).
		PermissionMode: agent.PermissionModeAsk,
	})
}

// TestPlanModeGatesWriteToolAndRestoresOnExit is the end-to-end integration
// test: it drives /plan on, submits a prompt whose (adversarial, since the
// tool isn't even advertised) provider response tries to call write_file
// directly, and confirms the call is denied and nothing is written to disk.
// Then it drives /plan off and confirms write_file is advertised again,
// proving the mode genuinely reverted rather than merely relabeling.
func TestPlanModeGatesWriteToolAndRestoresOnExit(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "notes.txt")
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		// The write attempt is denied before it reaches the sandbox, so the loop
		// makes a second request for the model's next move within THIS run —
		// hence two scripts for run 1, matching the write/deny/react shape used
		// elsewhere (see TestPromptSubmitPersistsPermissionSessionEvents).
		writeFileToolScript("call_write", "notes.txt", "hello from plan mode"),
		textScript("understood, staying read-only"),
		textScript("nothing to report"),
	}}
	m := newPlanModeTestModel(root, provider)

	updated, _ := m.dispatchCommand(parseCommand("/plan on"))
	m = updated.(model)
	if m.permissionMode != agent.PermissionModePlan {
		t.Fatalf("permissionMode after /plan on = %s, want plan", m.permissionMode)
	}

	m.input.SetValue("please write the notes file")
	updatedModel, cmd := m.Update(testKey(tea.KeyEnter))
	next := updatedModel.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}
	updatedModel, _ = next.Update(execCmd(cmd))
	next = updatedModel.(model)

	if len(provider.requests) == 0 {
		t.Fatal("expected at least one provider request")
	}
	if providerRequestIncludesTool(provider.requests[0], "write_file") {
		t.Fatalf("write_file must not be advertised in plan mode: %#v", provider.requests[0].Tools)
	}
	if providerRequestIncludesTool(provider.requests[0], "bash") {
		t.Fatalf("bash must not be advertised in plan mode: %#v", provider.requests[0].Tools)
	}

	result, ok := findTranscriptRow(next.transcript, rowToolResult)
	if !ok || result.tool != "write_file" || result.status != tools.StatusError {
		t.Fatalf("expected a denied write_file tool result, got ok=%v row=%#v", ok, result)
	}
	if !strings.Contains(result.detail, "not available in plan mode") {
		t.Fatalf("expected plan-mode denial message, got %q", result.detail)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("write_file executed despite plan mode gating: stat err=%v", err)
	}

	updated, _ = next.dispatchCommand(parseCommand("/plan off"))
	next = updated.(model)
	if next.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("permissionMode after /plan off = %s, want ask (restored)", next.permissionMode)
	}

	next.input.SetValue("anything else to check?")
	updatedModel, cmd = next.Update(testKey(tea.KeyEnter))
	next = updatedModel.(model)
	if cmd == nil {
		t.Fatal("expected second prompt submit to start an agent run")
	}
	updatedModel, _ = next.Update(execCmd(cmd))
	next = updatedModel.(model)

	if len(provider.requests) != 3 {
		t.Fatalf("expected three provider requests (2 in the plan-mode run, 1 after /plan off), got %d", len(provider.requests))
	}
	if !providerRequestIncludesTool(provider.requests[2], "write_file") {
		t.Fatalf("write_file must be advertised again after /plan off: %#v", provider.requests[2].Tools)
	}
}
