package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/testutil"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestAsyncDiagnosticsNilCollectorNoOps(t *testing.T) {
	var diagnostics *asyncDiagnostics
	if diagnostics != newAsyncDiagnostics(nil, "/ws") {
		t.Fatal("nil check must yield a nil collector")
	}
	diagnostics.enqueue(context.Background(), []string{"a.go"})
	if nudge := diagnostics.drain(context.Background()); nudge != "" {
		t.Fatalf("nil collector drain = %q, want empty", nudge)
	}
}

func TestAsyncDiagnosticsCollectsAndDrainsOnce(t *testing.T) {
	// A real absolute root: a literal "/ws" is not filepath.IsAbs on Windows
	// (no drive letter), which is exactly what the assertions below check.
	root := t.TempDir()
	var mu sync.Mutex
	var checked []string
	check := func(_ context.Context, absPath string) string {
		mu.Lock()
		checked = append(checked, absPath)
		mu.Unlock()
		if filepath.Base(absPath) == "clean.go" {
			return ""
		}
		return "ERR " + filepath.Base(absPath)
	}
	diagnostics := newAsyncDiagnostics(check, root)
	diagnostics.enqueue(context.Background(), []string{"broken.go", "clean.go"})

	nudge := diagnostics.drain(context.Background())
	if !strings.HasPrefix(nudge, asyncDiagnosticsNudge) {
		t.Fatalf("nudge missing prefix: %q", nudge)
	}
	if !strings.Contains(nudge, "ERR broken.go") || strings.Contains(nudge, "clean.go") {
		t.Fatalf("nudge = %q, want broken.go errors only", nudge)
	}
	mu.Lock()
	for _, path := range checked {
		if !filepath.IsAbs(path) || !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Fatalf("check received %q, want path under workspace root %q", path, root)
		}
	}
	mu.Unlock()
	if second := diagnostics.drain(context.Background()); second != "" {
		t.Fatalf("second drain = %q, want empty (results are consumed)", second)
	}
}

// A check still running at drain time defers delivery to the next drain
// instead of blocking the turn.
func TestAsyncDiagnosticsSlowCheckDefersToNextDrain(t *testing.T) {
	previous := asyncDiagnosticsDrainTimeout
	asyncDiagnosticsDrainTimeout = 20 * time.Millisecond
	defer func() { asyncDiagnosticsDrainTimeout = previous }()

	release := make(chan struct{})
	check := func(_ context.Context, absPath string) string {
		<-release
		return "ERR " + filepath.Base(absPath)
	}
	diagnostics := newAsyncDiagnostics(check, "/ws")
	diagnostics.enqueue(context.Background(), []string{"slow.go"})

	start := time.Now()
	if nudge := diagnostics.drain(context.Background()); nudge != "" {
		t.Fatalf("drain during in-flight check = %q, want empty", nudge)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("drain blocked %v, want bounded by the (shortened) timeout", elapsed)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if nudge := diagnostics.drain(context.Background()); nudge != "" {
			if !strings.Contains(nudge, "ERR slow.go") {
				t.Fatalf("deferred nudge = %q", nudge)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("deferred diagnostics never delivered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Re-enqueueing a file after its check completed replaces the stale result.
func TestAsyncDiagnosticsReEditReplacesResult(t *testing.T) {
	var mu sync.Mutex
	response := "ERR old"
	check := func(context.Context, string) string {
		mu.Lock()
		defer mu.Unlock()
		return response
	}
	diagnostics := newAsyncDiagnostics(check, "/ws")

	diagnostics.enqueue(context.Background(), []string{"a.go"})
	testutil.WaitFor(t, "worker idle", func() bool {
		diagnostics.mu.Lock()
		defer diagnostics.mu.Unlock()
		return diagnostics.working == nil
	})
	mu.Lock()
	response = "ERR new"
	mu.Unlock()
	diagnostics.enqueue(context.Background(), []string{"a.go"})

	nudge := diagnostics.drain(context.Background())
	if !strings.Contains(nudge, "ERR new") || strings.Contains(nudge, "ERR old") {
		t.Fatalf("nudge = %q, want only the re-check result", nudge)
	}
}

// changedFilesTool is a fake mutating tool reporting a changed file without
// touching the filesystem.
type changedFilesTool struct{}

func (changedFilesTool) Name() string        { return "fake_edit" }
func (changedFilesTool) Description() string { return "test mutating tool" }
func (changedFilesTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{}}
}
func (changedFilesTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionAllow, Reason: "test"}
}
func (changedFilesTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "edited", ChangedFiles: []string{"main.go"}}
}

// End-to-end through Run: an edit turn does NOT carry diagnostics in the tool
// result (the old inline path), but the next request contains the background
// diagnostics nudge, before the model finalizes.
func TestRunDeliversAsyncDiagnosticsNudgeNextTurn(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(changedFilesTool{})
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "fake_edit"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	root := t.TempDir()
	var checkedPath string
	var mu sync.Mutex

	result, err := Run(context.Background(), "fix main.go", provider, Options{
		Registry: registry,
		Cwd:      root,
		FileDiagnostics: func(_ context.Context, absPath string) string {
			mu.Lock()
			checkedPath = absPath
			mu.Unlock()
			return "main.go:1:1 error: boom"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected 2 provider turns, got %d", len(provider.requests))
	}

	// The tool result itself must NOT block on / embed diagnostics.
	for _, message := range provider.requests[1].Messages {
		if message.Role == zeroruntime.MessageRoleTool && strings.Contains(message.Content, "boom") {
			t.Fatalf("diagnostics leaked into the tool result: %q", message.Content)
		}
	}
	// The nudge must be present in the second request as a user message.
	found := false
	for _, message := range provider.requests[1].Messages {
		if message.Role == zeroruntime.MessageRoleUser && strings.HasPrefix(message.Content, asyncDiagnosticsNudge) {
			if !strings.Contains(message.Content, "boom") {
				t.Fatalf("nudge missing diagnostics: %q", message.Content)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no async diagnostics nudge in the follow-up request")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := filepath.Join(root, "main.go"); checkedPath != want {
		t.Fatalf("checked path = %q, want %q", checkedPath, want)
	}
}

// When the run hits the maxTurns ceiling right after an edit turn, the
// final-answer request must still carry the diagnostics nudge — otherwise an
// error introduced by the last edit would go unreported in the summary.
func TestRunDrainsAsyncDiagnosticsBeforeMaxTurnsFinalAnswer(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(changedFilesTool{})
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "fake_edit"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "summary"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "fix main.go", provider, Options{
		Registry: registry,
		Cwd:      t.TempDir(),
		MaxTurns: 1,
		FileDiagnostics: func(context.Context, string) string {
			return "main.go:1:1 error: boom"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "summary" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected the max-turns final-answer request, got %d requests", len(provider.requests))
	}
	found := false
	for _, message := range provider.requests[1].Messages {
		if message.Role == zeroruntime.MessageRoleUser && strings.HasPrefix(message.Content, asyncDiagnosticsNudge) {
			found = true
		}
	}
	if !found {
		t.Fatal("max-turns final-answer request missing the diagnostics nudge")
	}
}

// When the model finalizes while a slow check is still in flight (the
// per-turn drain missed it), the finalization gate must wait it out and give
// the model one more turn with the nudge instead of dropping the errors.
func TestRunFinalizationGateDeliversLateDiagnostics(t *testing.T) {
	previous := asyncDiagnosticsDrainTimeout
	asyncDiagnosticsDrainTimeout = time.Millisecond
	defer func() { asyncDiagnosticsDrainTimeout = previous }()

	registry := tools.NewRegistry()
	registry.Register(changedFilesTool{})
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "fake_edit"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done after fix"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "fix main.go", provider, Options{
		Registry: registry,
		Cwd:      t.TempDir(),
		FileDiagnostics: func(context.Context, string) string {
			// Slower than the (shortened) per-turn drain, well under the
			// finalization budget.
			time.Sleep(100 * time.Millisecond)
			return "main.go:1:1 error: boom"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done after fix" {
		t.Fatalf("final answer = %q, want the post-nudge answer", result.FinalAnswer)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("expected a third request carrying the late nudge, got %d requests", len(provider.requests))
	}
	found := false
	for _, message := range provider.requests[2].Messages {
		if message.Role == zeroruntime.MessageRoleUser && strings.HasPrefix(message.Content, asyncDiagnosticsNudge) {
			if !strings.Contains(message.Content, "boom") {
				t.Fatalf("nudge missing diagnostics: %q", message.Content)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("finalization gate did not deliver the late diagnostics nudge")
	}
}
