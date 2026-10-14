package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// captureAgentOptions makes every turn record the agent.Options it was built
// with, so a test can assert the permission mode the next prompt actually runs
// with rather than only what the wire reports.
func captureAgentOptions(deps *Deps) *agent.Options {
	var captured agent.Options
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		captured = opts
		return agent.Result{FinalAnswer: "ok"}, nil
	}
	return &captured
}

func newHarnessT(t *testing.T, deps Deps) *clientHarness {
	t.Helper()
	h := newHarness(t, deps)
	t.Cleanup(h.stop)
	return h
}

func mustCall(t *testing.T, ctx context.Context, h *clientHarness, method string, params any, result any) {
	t.Helper()
	if err := h.client.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// restartHarness replaces the live agent with a fresh one over the same store,
// which is the process restart the persisted mode has to survive.
func restartHarness(t *testing.T, h *clientHarness, deps Deps) *clientHarness {
	t.Helper()
	h.stop()
	return newHarnessT(t, deps)
}

func modeOptionValue(t *testing.T, options []SessionConfigOption) string {
	t.Helper()
	for _, option := range options {
		if option.ID == configIDMode {
			return option.CurrentValue
		}
	}
	t.Fatalf("no mode config option in %+v", options)
	return ""
}

func assertModeReported(t *testing.T, result LoadSessionResult, want agent.PermissionMode) {
	t.Helper()
	if result.Modes == nil || result.Modes.CurrentModeID != string(want) {
		t.Fatalf("modes = %+v, want currentModeId %q", result.Modes, want)
	}
	if got := modeOptionValue(t, result.ConfigOptions); got != string(want) {
		t.Fatalf("mode config option = %q, want %q", got, want)
	}
}

func TestACPModePersistsAcrossRestartLoadAndResume(t *testing.T) {
	deps := testDeps(t)
	captured := captureAgentOptions(&deps)
	h := newHarnessT(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := t.TempDir()
	var created NewSessionResult
	mustCall(t, ctx, h, MethodSessionNew, NewSessionParams{Cwd: workspace, McpServers: []McpServer{}}, &created)

	mustCall(t, ctx, h, MethodSessionSetMode, SetSessionModeParams{SessionID: created.SessionID, ModeID: string(agent.PermissionModePlan)}, &SetSessionModeResult{})
	meta, err := deps.Store.Get(created.SessionID)
	if err != nil || meta == nil {
		t.Fatalf("store.Get: %v", err)
	}
	if meta.PermissionMode != string(agent.PermissionModePlan) {
		t.Fatalf("stored mode = %q, want plan", meta.PermissionMode)
	}

	// LOAD after a restart: the response must report the stored mode, and the
	// next prompt must actually run with it.
	h = restartHarness(t, h, deps)
	var loaded LoadSessionResult
	mustCall(t, ctx, h, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &loaded)
	assertModeReported(t, loaded, agent.PermissionModePlan)
	mustCall(t, ctx, h, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("continue")}}, &PromptResult{})
	if captured.PermissionMode != agent.PermissionModePlan {
		t.Fatalf("after load, agent.Options.PermissionMode = %q, want plan", captured.PermissionMode)
	}

	// RESUME after another restart reports the same.
	h = restartHarness(t, h, deps)
	var resumed ResumeSessionResult
	mustCall(t, ctx, h, MethodSessionResume, ResumeSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &resumed)
	assertModeReported(t, resumed, agent.PermissionModePlan)
}

func TestACPModePersistsViaSetConfigOption(t *testing.T) {
	deps := testDeps(t)
	h := newHarnessT(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := t.TempDir()
	var created NewSessionResult
	mustCall(t, ctx, h, MethodSessionNew, NewSessionParams{Cwd: workspace, McpServers: []McpServer{}}, &created)

	var configured SetSessionConfigOptionResult
	mustCall(t, ctx, h, MethodSessionSetConfigOption, SetSessionConfigOptionParams{
		SessionID: created.SessionID, ConfigID: configIDMode, Value: string(agent.PermissionModeAsk),
	}, &configured)
	if got := modeOptionValue(t, configured.ConfigOptions); got != string(agent.PermissionModeAsk) {
		t.Fatalf("configured mode = %q, want ask", got)
	}
	meta, err := deps.Store.Get(created.SessionID)
	if err != nil || meta == nil {
		t.Fatalf("store.Get: %v", err)
	}
	if meta.PermissionMode != string(agent.PermissionModeAsk) {
		t.Fatalf("stored mode = %q, want ask", meta.PermissionMode)
	}

	h = restartHarness(t, h, deps)
	var loaded LoadSessionResult
	mustCall(t, ctx, h, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &loaded)
	assertModeReported(t, loaded, agent.PermissionModeAsk)
}

// TestACPModeRestoreFallback pins the restore boundary: no stored value keeps
// the historical auto default, and a stored value outside the three offered
// modes must never come back weaker than ask (and never unsafe).
func TestACPModeRestoreFallback(t *testing.T) {
	cases := []struct {
		name string
		seed string // "" means metadata written before modes were persisted
		want agent.PermissionMode
	}{
		{name: "unset keeps auto", want: agent.PermissionModeAuto},
		{name: "ask preserved", seed: string(agent.PermissionModeAsk), want: agent.PermissionModeAsk},
		{name: "unknown falls back to ask", seed: "bogus", want: agent.PermissionModeAsk},
		{name: "unsafe never restored", seed: string(agent.PermissionModeUnsafe), want: agent.PermissionModeAsk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			captured := captureAgentOptions(&deps)
			h := newHarnessT(t, deps)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			workspace := t.TempDir()
			var created NewSessionResult
			mustCall(t, ctx, h, MethodSessionNew, NewSessionParams{Cwd: workspace, McpServers: []McpServer{}}, &created)
			if tc.seed != "" {
				if _, err := deps.Store.UpdateMode(created.SessionID, tc.seed); err != nil {
					t.Fatalf("seed mode: %v", err)
				}
			}

			h = restartHarness(t, h, deps)
			var loaded LoadSessionResult
			mustCall(t, ctx, h, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &loaded)
			assertModeReported(t, loaded, tc.want)

			mustCall(t, ctx, h, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("go")}}, &PromptResult{})
			if captured.PermissionMode != tc.want {
				t.Fatalf("next prompt PermissionMode = %q, want %q", captured.PermissionMode, tc.want)
			}
		})
	}
}

// TestACPSetModeWriteFailureKeepsLiveMode pins the store-first order: when the
// mode cannot be saved the request fails and the live session is untouched.
func TestACPSetModeWriteFailureKeepsLiveMode(t *testing.T) {
	deps := testDeps(t)
	captured := captureAgentOptions(&deps)
	h := newHarnessT(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var created NewSessionResult
	mustCall(t, ctx, h, MethodSessionNew, NewSessionParams{Cwd: t.TempDir(), McpServers: []McpServer{}}, &created)

	// Remove the metadata file so UpdateMode fails but the live session stays
	// promptable (turn persistence only warns when the file is gone).
	metadataPath := filepath.Join(deps.Store.RootDir, created.SessionID, sessions.MetadataFile)
	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}

	err := h.client.Call(ctx, MethodSessionSetMode, SetSessionModeParams{SessionID: created.SessionID, ModeID: string(agent.PermissionModePlan)}, &SetSessionModeResult{})
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != codeInternalError {
		t.Fatalf("set_mode with an unsavable mode = %v, want internal error", err)
	}

	mustCall(t, ctx, h, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("still auto")}}, &PromptResult{})
	if captured.PermissionMode != agent.PermissionModeAuto {
		t.Fatalf("failed set_mode changed the live mode: PermissionMode = %q, want auto", captured.PermissionMode)
	}
}
