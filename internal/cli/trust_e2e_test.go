package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/workspacetrust"
	"github.com/Twigpine/zero/internal/worktrees"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// These tests close the end-to-end gap the chokepoint unit tests leave open:
// they drive the REAL agent loop (and, for the worktree cases, the real exec
// entry point) with a fake provider that emits a tool call, so a project
// beforeTool hook actually fires (or is gated away) through production code, not
// a direct dispatcher call.

// markerTool is a minimal, always-allowed tool the fake provider "calls" so the
// agent loop reaches dispatchBeforeTool (which only fires for a registered,
// permitted tool). Its own Run is a no-op; the observable effect is the project
// beforeTool hook the gate did or did not load.
type markerTool struct{}

func (markerTool) Name() string        { return "marker_tool" }
func (markerTool) Description() string { return "test-only no-op tool" }
func (markerTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (markerTool) Safety() tools.Safety { return tools.Safety{Permission: tools.PermissionAllow} }
func (markerTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}

// toolThenTextProvider calls toolName on the first turn, then answers with text so
// the loop terminates. It detects "first turn" by the absence of a prior tool
// result in the message history.
type toolThenTextProvider struct{ toolName string }

func (p toolThenTextProvider) StreamCompletion(_ context.Context, req zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	toolAlreadyCalled := false
	for _, m := range req.Messages {
		if m.Role == zeroruntime.MessageRoleTool {
			toolAlreadyCalled = true
			break
		}
	}
	ch := make(chan zeroruntime.StreamEvent, 8)
	go func() {
		defer close(ch)
		if toolAlreadyCalled {
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
			return
		}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: p.toolName}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "c1", ArgumentsFragment: `{"pattern":"*"}`}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	}()
	return ch, nil
}

// writeMarkerHook writes a ./.zero/hooks.json under dir whose enabled beforeTool
// hook touches markerPath when it fires. The hook shells out to /bin/sh, so the
// trusted-case assertion (the marker must appear) is meaningless on Windows, where
// that interpreter does not exist; skip there, matching writeMarkerHookScript.
func writeMarkerHook(t *testing.T, dir, markerPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("marker hook is POSIX-shell based (/bin/sh)")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".zero"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"enabled":true,"hooks":[{"id":"m","event":"beforeTool","command":"/bin/sh","args":["-c","touch '` + markerPath + `'"],"enabled":true}]}`
	if err := os.WriteFile(filepath.Join(dir, ".zero", "hooks.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTrustGateBlocksToolHookThroughAgentRun closes gap #2: the gate blocks a
// real tool call's beforeTool hook through the production agent.Run loop, not
// just a direct dispatcher call. Untrusted => the project hook is not in the
// dispatcher, so the tool call fires nothing. Trusted => it fires.
func TestTrustGateBlocksToolHookThroughAgentRun(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	writeMarkerHook(t, repo, marker)

	runOnce := func() {
		reg := tools.NewRegistry()
		reg.Register(markerTool{})
		disp, _ := newHookDispatcherWithExtra(repo, nil, repo)
		if _, err := agent.Run(context.Background(), "go", toolThenTextProvider{toolName: "marker_tool"}, agent.Options{
			Registry:       reg,
			Hooks:          disp,
			PermissionMode: agent.PermissionModeUnsafe,
			MaxTurns:       3,
		}); err != nil {
			t.Fatalf("agent.Run: %v", err)
		}
	}

	// Untrusted: gate excludes the project layer; the beforeTool hook must not run.
	_ = os.Remove(marker)
	runOnce()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("UNTRUSTED: project beforeTool hook ran through agent.Run (marker exists) -- gate failed OPEN")
	}

	// Trusted: the hook is in the dispatcher; the tool call fires it.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	runOnce()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TRUSTED: project beforeTool hook did NOT run (marker absent): %v", err)
	}
}

// TestTrustGateFiresInDefaultAutoMode is the in-scope proof for the security
// report: the beforeTool hook fires in the DEFAULT permission mode (auto), NOT
// only under --skip-permissions-unsafe. marker_tool has PermissionAllow safety,
// so effectivePermission grants it at loop.go without a prompt regardless of
// mode; dispatchBeforeTool then runs the project hook. This means the
// vulnerability is reachable in normal operation with the sandbox ON, so it is
// not the "requires the user to disable the sandbox" out-of-scope case.
//
// Trusted => the hook fires in auto mode without any unsafe flag; untrusted =>
// the gate blocks it, also in auto mode. No OnPermissionRequest is wired: an
// auto-allowed tool needs no approval callback, which is the whole point.
func TestTrustGateFiresInDefaultAutoMode(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	writeMarkerHook(t, repo, marker)

	runOnce := func(mode agent.PermissionMode) {
		reg := tools.NewRegistry()
		reg.Register(markerTool{})
		disp, _ := newHookDispatcherWithExtra(repo, nil, repo)
		if _, err := agent.Run(context.Background(), "go", toolThenTextProvider{toolName: "marker_tool"}, agent.Options{
			Registry:       reg,
			Hooks:          disp,
			PermissionMode: mode,
			MaxTurns:       3,
		}); err != nil {
			t.Fatalf("agent.Run (%s): %v", mode, err)
		}
	}

	// Untrusted, default auto mode: the gate excludes the project layer, so the
	// hook must NOT fire even though the tool call is permitted.
	_ = os.Remove(marker)
	runOnce(agent.PermissionModeAuto)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("UNTRUSTED auto mode: project beforeTool hook ran -- gate failed OPEN in the default mode")
	}

	// Trusted, default auto mode (sandbox on, NO --skip-permissions-unsafe): the
	// auto-allowed tool is granted with no prompt and its beforeTool hook fires.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	runOnce(agent.PermissionModeAuto)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TRUSTED auto mode: project beforeTool hook did NOT fire without an unsafe flag (marker absent): %v", err)
	}

	// And it is not an auto-only quirk: ask mode (also sandboxed, non-unsafe)
	// fires the same way for an auto-allowed tool.
	_ = os.Remove(marker)
	runOnce(agent.PermissionModeAsk)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TRUSTED ask mode: project beforeTool hook did NOT fire (marker absent): %v", err)
	}
}

// TestExecSurfacesMCPTrustNotice proves the exec path actually threads the MCP trust
// skip into the one-line notice (the CodeRabbit finding), not just that the unit
// pieces work in isolation: an untrusted repo whose only project config is MCP prints
// the notice through the real runExec wiring; trusting it silences it. resolveMCPConfig
// is stubbed to return no servers so nothing spawns -- the notice depends only on the
// trust verdict and the real ./.zero/config.json that projectMCPConfigExists reads.
func TestExecSurfacesMCPTrustNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec fake-provider harness assumes a POSIX process environment")
	}
	setTrustConfigRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func() string {
		var out, errBuf bytes.Buffer
		code := runWithDeps([]string{"exec", "--skip-permissions-unsafe", "--max-turns", "3", "go"}, &out, &errBuf, appDeps{
			getwd:         func() (string, error) { return repo, nil },
			resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) { return execResolvedConfig(), nil },
			newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return toolThenTextProvider{toolName: "glob"}, nil
			},
			resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
		})
		if code != exitSuccess {
			t.Fatalf("exec exit = %d, stderr=%q", code, errBuf.String())
		}
		return errBuf.String()
	}

	// Untrusted: the project MCP layer is dropped, so the notice must name it. The repo
	// has no project hooks/plugins, so the MCP skip is the ONLY thing that can fire it.
	untrusted := run()
	if !strings.Contains(untrusted, "MCP servers") || !strings.Contains(untrusted, "zero trust") {
		t.Fatalf("untrusted exec must surface the MCP trust notice, stderr=%q", untrusted)
	}

	// Trusted: nothing is skipped, so no notice at all.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	if trusted := run(); strings.Contains(trusted, "ignoring project") {
		t.Fatalf("trusted exec must not emit a trust notice, stderr=%q", trusted)
	}
}

// TestExecSpecSurfacesMCPTrustNotice is the --use-spec analogue of the test above:
// the spec-draft path (exec_spec.go) must also thread the MCP skip into its notice.
// The fake provider never submits a spec, so the run exits non-zero; that is
// orthogonal to trust, so we assert only the notice, not the exit code.
func TestExecSpecSurfacesMCPTrustNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec fake-provider harness assumes a POSIX process environment")
	}
	setTrustConfigRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	_ = runWithDeps([]string{"exec", "--use-spec", "--skip-permissions-unsafe", "--max-turns", "3", "go"}, &out, &errBuf, appDeps{
		getwd:         func() (string, error) { return repo, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) { return execResolvedConfig(), nil },
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return toolThenTextProvider{toolName: "glob"}, nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
	})
	if !strings.Contains(errBuf.String(), "MCP servers") || !strings.Contains(errBuf.String(), "zero trust") {
		t.Fatalf("untrusted --use-spec exec must surface the MCP trust notice, stderr=%q", errBuf.String())
	}
}

// runExecTrust drives the full exec entry point with a fake worktree and a
// tool-calling provider, returning the exit code. The provider calls the core
// "glob" tool so dispatchBeforeTool fires inside the real exec-built registry.
func runExecTrust(t *testing.T, extraArgs []string, launchDir, worktreeDir string) int {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	args := append([]string{"exec", "--worktree", "--skip-permissions-unsafe", "--max-turns", "3"}, extraArgs...)
	args = append(args, "go")
	var out, errBuf bytes.Buffer
	return runWithDeps(args, &out, &errBuf, appDeps{
		getwd: func() (string, error) { return launchDir, nil },
		prepareWorktree: func(context.Context, worktrees.Options) (worktrees.Result, error) {
			return worktrees.Result{Path: worktreeDir}, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return toolThenTextProvider{toolName: "glob"}, nil
		},
	})
}

// TestExecWorktreeInheritsTrustEndToEnd closes gap #1 for the exec path: trust is
// keyed on the ORIGINAL launch dir, not the generated worktree path. The worktree
// checkout carries the committed .zero/hooks.json; the gate must load it only when
// the SOURCE repo (launch dir) is trusted, proving exec.go captures trustRoot
// before the --worktree reassignment and threads it into the chokepoint.
func TestExecWorktreeInheritsTrustEndToEnd(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()     // original launch dir -- the trust key
	worktree := t.TempDir() // simulated worktree checkout (workspaceRoot after reassignment)
	marker := filepath.Join(t.TempDir(), "marker")
	writeMarkerHook(t, worktree, marker) // the checkout carries the committed hook

	// Untrusted source repo: worktree hooks must NOT run.
	_ = os.Remove(marker)
	if code := runExecTrust(t, nil, repo, worktree); code != exitSuccess {
		t.Fatalf("exec --worktree (untrusted) exit = %d", code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("UNTRUSTED worktree: project hook ran -- exec keyed trust on the worktree path or failed open")
	}

	// Trusted source repo: the worktree inherits its trust, so the hook runs.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	if code := runExecTrust(t, nil, repo, worktree); code != exitSuccess {
		t.Fatalf("exec --worktree (trusted) exit = %d", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TRUSTED worktree: project hook did NOT run (marker absent) -- worktree trust inheritance broken: %v", err)
	}
}

// TestExecSpecWorktreeInheritsTrustEndToEnd closes gap #1 for the spec-draft path
// (the exact --use-spec --worktree combination the review fix addressed): the
// spec-draft chokepoint must also key trust on the original launch dir.
func TestExecSpecWorktreeInheritsTrustEndToEnd(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	worktree := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	writeMarkerHook(t, worktree, marker)

	// The spec-draft flow itself exits non-zero here (the fake provider does not
	// submit a real spec), which is orthogonal to trust: the hook dispatcher is
	// built (keyed on run.trustRoot) before the agent runs, and glob (a read-only
	// allow tool) is advertised in spec-draft, so beforeTool still fires. We assert
	// only the marker, the trust behavior, not the spec-flow exit code.

	// Untrusted: spec-draft in a worktree of an untrusted repo runs no project hook.
	_ = os.Remove(marker)
	_ = runExecTrust(t, []string{"--use-spec"}, repo, worktree)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("UNTRUSTED spec-draft worktree: project hook ran -- spec-draft keyed trust on the worktree path")
	}

	// Trusted: the spec-draft path inherits the source repo's trust.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	_ = runExecTrust(t, []string{"--use-spec"}, repo, worktree)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TRUSTED spec-draft worktree: project hook did NOT run -- spec-draft trust inheritance broken: %v", err)
	}
}
