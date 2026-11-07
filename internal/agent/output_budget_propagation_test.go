package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/trace"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

type propagationOutputTool struct {
	output string
}

func TestOutputBudgetHookHelperProcess(t *testing.T) {
	for index, arg := range os.Args {
		if arg == "--zero-output-budget-hook-repeat" && index+1 < len(os.Args) {
			repeats, err := strconv.Atoi(os.Args[index+1])
			if err != nil {
				os.Exit(2)
			}
			if _, err := os.Stdout.WriteString(strings.Repeat("hook feedback ", repeats)); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		}
		if arg != "--zero-output-budget-hook" || index+1 >= len(os.Args) {
			continue
		}
		if _, err := os.Stdout.WriteString(os.Args[index+1]); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func largeOutputBudgetHookDispatcher() *hooks.Dispatcher {
	return outputBudgetHookDispatcherFor("propagation_output", 200)
}

func outputBudgetHookDispatcherFor(toolName string, repeats int) *hooks.Dispatcher {
	feedback := strings.Repeat("hook feedback ", repeats)
	hookArgs := []string{
		"-test.run=TestOutputBudgetHookHelperProcess",
		"--",
		"--zero-output-budget-hook",
		feedback,
	}
	if repeats > 1_000 {
		hookArgs = []string{
			"-test.run=TestOutputBudgetHookHelperProcess",
			"--",
			"--zero-output-budget-hook-repeat",
			strconv.Itoa(repeats),
		}
	}
	return hooks.NewDispatcher(hooks.DispatcherOptions{Config: hooks.Config{
		Enabled: true,
		Hooks: []hooks.Definition{{
			ID:      "large-feedback",
			Event:   hooks.EventAfterTool,
			Matcher: toolName,
			Command: os.Args[0],
			Args:    hookArgs,
			Enabled: true,
		}},
	}})
}

func TestExecuteToolCallCommitsReadObservationAfterFinalBoundary(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.txt")
	if err := os.WriteFile(targetPath, []byte("known content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "small.txt")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	tracker := tools.NewFileTracker()

	result, abortErr := executeToolCall(context.Background(), registry, ToolCall{
		ID: "read-small", Name: "read_file", Arguments: `{"path":"small.txt"}`,
	}, PermissionModeAuto, Options{Cwd: root, FileTracker: tracker})
	if abortErr != nil || result.Status != tools.StatusOK {
		t.Fatalf("executeToolCall: result=%#v err=%v", result, abortErr)
	}
	if !tracker.SeenWhole(resolvedPath) {
		t.Fatal("unmodified final read output was not committed as observed")
	}
}

func TestExecuteToolCallDoesNotCommitReadObservationBeforeHookBudget(t *testing.T) {
	t.Setenv("ZERO_TOOL_OUTPUT_CEILING_TOKENS", "80")
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.txt")
	if err := os.WriteFile(targetPath, []byte("known content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "small.txt")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	tracker := tools.NewFileTracker()

	result, abortErr := executeToolCall(context.Background(), registry, ToolCall{
		ID: "read-hook", Name: "read_file", Arguments: `{"path":"small.txt"}`,
	}, PermissionModeAuto, Options{
		Cwd: root, FileTracker: tracker, Hooks: outputBudgetHookDispatcherFor("read_file", 20_000),
	})
	if abortErr != nil {
		t.Fatalf("executeToolCall: %v", abortErr)
	}
	if !result.Truncated {
		t.Fatalf("precondition: hook feedback was not truncated: %#v", result)
	}
	if tracker.SeenWhole(resolvedPath) {
		t.Fatal("read observation committed before hook output was budgeted")
	}
}

func (tool propagationOutputTool) Name() string        { return "propagation_output" }
func (tool propagationOutputTool) Description() string { return "returns output for propagation tests" }
func (tool propagationOutputTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (tool propagationOutputTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "test read"}
}
func (tool propagationOutputTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: tool.output}
}

func TestExecuteToolCallPropagatesOutputTruncation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ZERO_TOOL_OUTPUT_CEILING_TOKENS", "80")
	registry := tools.NewRegistry()
	registry.Register(propagationOutputTool{output: strings.Repeat("large output\n", 1000)})

	result, abortErr := executeToolCall(context.Background(), registry, ToolCall{
		ID:        "call-budget",
		Name:      "propagation_output",
		Arguments: `{}`,
	}, PermissionModeAuto, Options{Cwd: t.TempDir()})
	if abortErr != nil {
		t.Fatalf("executeToolCall abort error: %v", abortErr)
	}
	if !result.Truncated {
		t.Fatalf("agent ToolResult lost tools.Result.Truncated: %#v", result)
	}
	if result.Meta["spill_path"] == "" {
		t.Fatalf("agent ToolResult lost spill metadata: %#v", result.Meta)
	}
}

func TestRecordOutputBudgetTraceUsesOnlyCompactMetadata(t *testing.T) {
	recorder := trace.NewRecorder("session", "run", "")
	recorder.Start()
	recordOutputBudgetTrace(recorder, ToolResult{
		Name:      "grep",
		Truncated: true,
		Output:    "SECRET OUTPUT MUST NOT ENTER TRACE",
		Meta: map[string]string{
			"output_budget_category":                  "search",
			"output_budget_original_bytes":            "1000",
			"output_budget_retained_bytes":            "100",
			"output_budget_estimated_original_tokens": "250",
			"output_budget_estimated_retained_tokens": "25",
			"output_budget_reason":                    "semantic_search_budget",
			"output_budget_spill_created":             "true",
			"spill_path":                              "/secret/path/not-for-trace",
		},
	})
	events := recorder.Finish().OutputBudgets
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	event := events[0]
	if event.Tool != "grep" || event.Category != "search" || event.OriginalBytes != 1000 || event.RetainedBytes != 100 || !event.SpillCreated {
		t.Fatalf("unexpected trace event: %#v", event)
	}
}

func TestExecuteToolCallRebudgetsOversizedAfterToolFeedback(t *testing.T) {
	t.Setenv("ZERO_TOOL_OUTPUT_CEILING_TOKENS", "80")
	registry := tools.NewRegistry()
	registry.Register(propagationOutputTool{output: "tool output"})
	dispatcher := largeOutputBudgetHookDispatcher()

	result, abortErr := executeToolCall(context.Background(), registry, ToolCall{
		ID:        "call-hook-budget",
		Name:      "propagation_output",
		Arguments: `{}`,
	}, PermissionModeAuto, Options{Hooks: dispatcher})
	if abortErr != nil {
		t.Fatalf("executeToolCall abort error: %v", abortErr)
	}
	if !result.Truncated || len(result.Output) > 80*4 {
		t.Fatalf("afterTool feedback bypassed output budget: truncated=%t bytes=%d meta=%#v", result.Truncated, len(result.Output), result.Meta)
	}
	if result.Meta["output_budget_category"] == "" || result.Meta["output_budget_retained_bytes"] != strconv.Itoa(len(result.Output)) {
		t.Fatalf("post-hook budget metadata does not describe final output: %#v", result.Meta)
	}
}

func TestRunTraceReflectsPostHookBudget(t *testing.T) {
	t.Setenv("ZERO_TOOL_OUTPUT_CEILING_TOKENS", "80")
	registry := tools.NewRegistry()
	registry.Register(propagationOutputTool{output: "tool output"})
	dispatcher := largeOutputBudgetHookDispatcher()
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-hook-trace", ToolName: "propagation_output"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-hook-trace", ArgumentsFragment: `{}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-hook-trace"},
			{Type: zeroruntime.StreamEventDone},
		},
		{{Type: zeroruntime.StreamEventText, Content: "done"}, {Type: zeroruntime.StreamEventDone}},
	}}
	recorder := trace.NewRecorder("session", "run", "")
	var toolResults []ToolResult
	if _, err := Run(context.Background(), "budget hook", provider, Options{
		Registry:     registry,
		Hooks:        dispatcher,
		Trace:        recorder,
		OnToolResult: func(result ToolResult) { toolResults = append(toolResults, result) },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(toolResults) != 1 || !toolResults[0].Truncated {
		t.Fatalf("tool result = %#v, want one truncated post-hook result", toolResults)
	}
	events := recorder.Finish().OutputBudgets
	if len(events) != 1 || !events[0].Truncated || events[0].RetainedBytes != len(toolResults[0].Output) {
		t.Fatalf("trace does not describe final post-hook output: events=%#v result=%#v", events, toolResults[0])
	}
}
