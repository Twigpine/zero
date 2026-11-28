package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/tools"
	zeroruntime "github.com/Twigpine/zero/internal/zeroruntime"
)

// editMatchingDispatcher is beforeToolDispatcher for the edit tool, so the same
// notice-and-chatter hook runs ahead of a call whose result carries a diff.
func editMatchingDispatcher(t *testing.T) *hooks.Dispatcher {
	t.Helper()
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	return hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.before-tool", Event: hooks.EventBeforeTool, Matcher: "edit_file", Command: "hook", Enabled: true},
			},
		},
		Audit:     audit,
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(noticeHookPreparer{}),
	})
}

// editFileRunOptions is readFileRunOptions for a tool whose result carries a
// rich preview, which is the case the disclosure went missing on.
func editFileRunOptions(t *testing.T, dispatcher *hooks.Dispatcher, onResult func(ToolResult)) (Options, *mockProvider) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package main\n\nconst answer = 41\n"), 0o644); err != nil {
		t.Fatalf("write x.go: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedEditFileTool(root, nil))
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "edit_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"x.go","old_string":"41","new_string":"42"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "edited it"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	return Options{
		SessionID:      "session-hook-edit",
		Cwd:            root,
		Registry:       registry,
		ProviderName:   "test-provider",
		Model:          "test-model",
		Hooks:          dispatcher,
		OnToolResult:   onResult,
		PermissionMode: PermissionModeUnsafe,
		MaxTurns:       2,
	}, provider
}

// THE NOTICE HAS TO SURVIVE A RESULT WHOSE BODY IS A DIFF.
//
// This is the boundary the component suites left between them. The delivery test
// above proves a beforeTool notice reaches the provider with read_file, whose
// body is its output. The card tests prove a card renders a notice when the
// result already carries one typed. Neither composed the two, and in between them
// the notice was being folded into result.Output as hook prose.
//
// For an edit or a write that is exactly where it disappears: the card builds its
// body from Display.Preview rather than Output, and draws enforcement furniture
// only from the typed slice, so the operator saw the diff and no disclosure at
// all, live and restored, while the model saw the disclosure. A notice about a
// weakened token was hidden on precisely the results where something was written.
func TestABeforeToolNoticeSurvivesARichPreviewResult(t *testing.T) {
	dispatcher := editMatchingDispatcher(t)

	var results []ToolResult
	options, provider := editFileRunOptions(t, dispatcher, func(result ToolResult) {
		results = append(results, result)
	})
	if _, err := Run(context.Background(), "bump the answer", provider, options); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("SETUP INVALID: %d tool results, want the one edit", len(results))
	}
	result := results[0]
	if result.Status != tools.StatusOK {
		t.Fatalf("SETUP INVALID: the edit failed, so this is not the successful rich-preview path: %s", result.Output)
	}
	preview := result.BaseDisplay().Preview
	if !strings.Contains(preview, "42") {
		t.Fatalf("SETUP INVALID: the result carries no diff preview, which is the whole case under test: %q", preview)
	}

	// TYPED, which is what every interactive surface reads.
	if len(result.EnforcementNotices) != 1 || result.EnforcementNotices[0] != beforeToolNotice {
		t.Errorf("the hook's disclosure did not reach the typed field, so the card and the restored card show the diff and no disclosure: %v", result.EnforcementNotices)
	}
	// AND NOT IN THE BODY AS WELL. Decoration has one owner per surface; carrying
	// it in both places renders it twice wherever the surface draws the slice.
	if strings.Contains(result.BaseModelOutput(), beforeToolNotice) {
		t.Errorf("the disclosure is in the result body as well as the typed field:\n%s", result.BaseModelOutput())
	}
	// The diff itself stays a parseable diff.
	if strings.Contains(preview, beforeToolNotice) {
		t.Errorf("the disclosure was glued onto the diff:\n%s", preview)
	}

	// And the model still sees it, exactly once, with none of the hook's chatter.
	if got := countRequestsContaining(provider.requests, beforeToolNotice); got != 1 {
		t.Errorf("the notice reached the model %d times, want exactly once", got)
	}
	if someRequestContains(provider.requests, beforeToolChatter) {
		t.Error("the hook's ordinary output reached the model")
	}
}
