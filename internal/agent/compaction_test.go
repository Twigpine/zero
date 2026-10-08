package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// --- Pure Compact() tests -------------------------------------------------

func TestCompactKeepsSystemAndPreservedSuffix(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system prompt"},
		{Role: zeroruntime.MessageRoleUser, Content: "first question"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "first answer"},
		{Role: zeroruntime.MessageRoleUser, Content: "second question"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "second answer"},
		{Role: zeroruntime.MessageRoleUser, Content: "most recent question"},
	}

	var captured []zeroruntime.Message
	summarizeCalls := 0
	result, err := Compact(messages, CompactionOptions{
		PreserveLast: 2,
		Summarize: func(toSummarize []zeroruntime.Message) (string, error) {
			summarizeCalls++
			captured = toSummarize
			return "DENSE SUMMARY", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summarizeCalls != 1 {
		t.Fatalf("expected Summarize to be called once, got %d", summarizeCalls)
	}

	// Head is the original system message.
	if result[0].Role != zeroruntime.MessageRoleSystem || result[0].Content != "system prompt" {
		t.Fatalf("expected system message preserved at head, got %#v", result[0])
	}
	// Next is the injected summary as a single user message.
	if result[1].Role != zeroruntime.MessageRoleUser {
		t.Fatalf("expected summary injected as user role, got %#v", result[1])
	}
	if !strings.Contains(result[1].Content, "[Summary of earlier conversation]") {
		t.Fatalf("expected summary label, got %q", result[1].Content)
	}
	if !strings.Contains(result[1].Content, "DENSE SUMMARY") {
		t.Fatalf("expected summary body, got %q", result[1].Content)
	}
	// Tail is the preserved suffix, verbatim.
	last := result[len(result)-1]
	if last.Content != "most recent question" {
		t.Fatalf("expected most recent message preserved, got %q", last.Content)
	}
	// The summarizer receives a compact semantic projection of the middle rather
	// than reconstructible raw tool output.
	if len(captured) != 1 || captured[0].Role != zeroruntime.MessageRoleUser {
		t.Fatalf("expected one projected summary input, got %#v", captured)
	}
	for _, want := range []string{"[user #0]", "first question", "[assistant #1]", "first answer", "second question"} {
		if !strings.Contains(captured[0].Content, want) {
			t.Fatalf("projected summary input missing %q: %q", want, captured[0].Content)
		}
	}
	// Compaction must shrink the conversation.
	if estimateTokens(result) >= estimateTokens(messages) {
		t.Fatalf("expected compaction to reduce estimated tokens")
	}
}

func TestCompactSuffixNeverStartsWithToolResult(t *testing.T) {
	// An assistant tool-call followed by its tool result sits exactly on the
	// naive preserve boundary. Compact must walk back so the suffix begins at a
	// safe user/assistant boundary, never a dangling tool result.
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system prompt"},
		{Role: zeroruntime.MessageRoleUser, Content: "do the thing"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "ok", ToolCalls: []zeroruntime.ToolCall{{ID: "1", Name: "read_file"}}},
		{Role: zeroruntime.MessageRoleTool, Content: "file contents", ToolCallID: "1"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "here is the result", ToolCalls: []zeroruntime.ToolCall{{ID: "2", Name: "read_file"}}},
		{Role: zeroruntime.MessageRoleTool, Content: "more contents", ToolCallID: "2"},
	}

	// PreserveLast=1 would naively keep only the trailing tool result — illegal.
	result, err := Compact(messages, CompactionOptions{
		PreserveLast: 1,
		Summarize: func(toSummarize []zeroruntime.Message) (string, error) {
			return "SUMMARY", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Find the first message after the summary (index 2 onward) and ensure it
	// is not a tool/tool_result message.
	for index, message := range result {
		if index <= 1 {
			continue // system + summary
		}
		if message.Role == zeroruntime.MessageRoleTool {
			t.Fatalf("preserved suffix begins with a tool result at index %d: %#v", index, result)
		}
		break
	}
}

func TestCompactNoopWhenTooFewMessages(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system"},
		{Role: zeroruntime.MessageRoleUser, Content: "hi"},
	}
	called := false
	result, err := Compact(messages, CompactionOptions{
		PreserveLast: 8,
		Summarize: func([]zeroruntime.Message) (string, error) {
			called = true
			return "x", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("Summarize should not be called when there is nothing to summarize")
	}
	if len(result) != len(messages) {
		t.Fatalf("expected input returned unchanged, got %#v", result)
	}
}

func TestCompactPropagatesSummarizeError(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system"},
		{Role: zeroruntime.MessageRoleUser, Content: "a"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "b"},
		{Role: zeroruntime.MessageRoleUser, Content: "c"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "d"},
		{Role: zeroruntime.MessageRoleUser, Content: "e"},
	}
	_, err := Compact(messages, CompactionOptions{
		PreserveLast: 2,
		Summarize: func([]zeroruntime.Message) (string, error) {
			return "", errors.New("summarizer down")
		},
	})
	if err == nil {
		t.Fatal("expected error from Compact when Summarize fails")
	}
}

// scriptedSummaryProvider returns, per StreamCompletion call, either an error
// event (when the script entry is non-empty) or a success text, so a test can
// drive the recursive summarizeWithFallback split/reduce path deterministically.
type scriptedSummaryProvider struct {
	calls   int
	scripts []string
}

func (p *scriptedSummaryProvider) StreamCompletion(_ context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	i := p.calls
	p.calls++
	if i < len(p.scripts) && p.scripts[i] != "" {
		return streamEvents([]zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventError, Error: p.scripts[i]}}), nil
	}
	return streamEvents([]zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "PARTIAL"},
		{Type: zeroruntime.StreamEventDone},
	}), nil
}

func TestSummarizeWithFallbackPropagatesNonContextReduceError(t *testing.T) {
	msgs := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: "a"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "b"},
	}
	ctxLimit := "This model's maximum context length is 1000 tokens. Please reduce the length of the messages."
	// call 1 (full slice): context-limit → split; calls 2,3 (halves): succeed;
	// call 4 (reduce of the two partials): a NON-context provider error that must
	// surface unchanged rather than be masked by the joined-text fallback.
	provider := &scriptedSummaryProvider{scripts: []string{ctxLimit, "", "", "provider request failed: 401 unauthorized"}}
	if _, err := summarizeWithFallback(context.Background(), provider, msgs, func(Usage) {}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a non-context reduce error must surface, got %v", err)
	}
}

func TestSummarizeWithFallbackUsesJoinedTextOnContextLimitReduce(t *testing.T) {
	msgs := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: "a"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "b"},
	}
	ctxLimit := "This model's maximum context length is 1000 tokens. Please reduce the length of the messages."
	// Same split, but the reduce also hits a context-limit: fall back to the joined
	// partial summaries instead of failing.
	provider := &scriptedSummaryProvider{scripts: []string{ctxLimit, "", "", ctxLimit}}
	got, err := summarizeWithFallback(context.Background(), provider, msgs, func(Usage) {})
	if err != nil {
		t.Fatalf("a context-limit reduce must fall back to joined text, got error %v", err)
	}
	if !strings.Contains(got, "PARTIAL") {
		t.Fatalf("joined fallback should contain the partial summaries, got %q", got)
	}
}

// --- estimateTokens tests -------------------------------------------------

func TestEstimateTokensMonotonic(t *testing.T) {
	small := []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "short"}}
	large := []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: strings.Repeat("x", 4000)}}
	if estimateTokens(large) <= estimateTokens(small) {
		t.Fatalf("expected larger content to estimate more tokens: small=%d large=%d", estimateTokens(small), estimateTokens(large))
	}
	// Adding a message must not decrease the estimate.
	grown := append([]zeroruntime.Message{}, large...)
	grown = append(grown, zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: "more"})
	if estimateTokens(grown) < estimateTokens(large) {
		t.Fatal("expected estimate to be monotonic when appending messages")
	}
}

func TestEstimateTokensCountsToolCallsAndResults(t *testing.T) {
	plain := []zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, Content: "hi"}}
	withCall := []zeroruntime.Message{{
		Role:      zeroruntime.MessageRoleAssistant,
		Content:   "hi",
		ToolCalls: []zeroruntime.ToolCall{{ID: "1", Name: "read_file", Arguments: strings.Repeat("a", 400)}},
	}}
	if estimateTokens(withCall) <= estimateTokens(plain) {
		t.Fatal("expected tool-call arguments to increase the token estimate")
	}
}

func TestEstimateTokensCountsImages(t *testing.T) {
	// A tiny image carries far more model tokens than its text content suggests;
	// the estimate must rise with each image so an image-heavy context still
	// trends toward compaction instead of reading as ~0.
	plain := []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "look"}}
	withImage := []zeroruntime.Message{{
		Role:    zeroruntime.MessageRoleUser,
		Content: "look",
		Images:  []zeroruntime.ImageBlock{{MediaType: "image/png", Data: []byte("tiny")}},
	}}
	twoImages := []zeroruntime.Message{{
		Role:    zeroruntime.MessageRoleUser,
		Content: "look",
		Images: []zeroruntime.ImageBlock{
			{MediaType: "image/png", Data: []byte("tiny")},
			{MediaType: "image/png", Data: []byte("tiny")},
		},
	}}
	if estimateTokens(withImage) <= estimateTokens(plain) {
		t.Fatal("expected an image to increase the token estimate")
	}
	if estimateTokens(twoImages) <= estimateTokens(withImage) {
		t.Fatal("expected the estimate to grow with image count")
	}
}

func TestEstimateToolDefTokensCountsDefinitions(t *testing.T) {
	if got := estimateToolDefTokens(nil); got != 0 {
		t.Fatalf("no tools should estimate 0 tokens, got %d", got)
	}
	one := []zeroruntime.ToolDefinition{{
		Name:        "read_file",
		Description: "Read a file from the workspace and return its contents.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}}
	if estimateToolDefTokens(one) <= 0 {
		t.Fatal("a tool definition (name + description + schema) should estimate > 0 tokens")
	}
	typed := append([]zeroruntime.ToolDefinition(nil), one...)
	typed[0].Type = zeroruntime.ToolDefinitionFreeform
	if estimateToolDefTokens(typed) <= estimateToolDefTokens(one) {
		t.Fatal("a non-empty tool type must increase the estimated request cost")
	}
	for _, test := range []struct {
		name   string
		format zeroruntime.ToolDefinitionFormat
	}{
		{name: "type", format: zeroruntime.ToolDefinitionFormat{Type: "grammar"}},
		{name: "syntax", format: zeroruntime.ToolDefinitionFormat{Syntax: "lark"}},
		{name: "definition", format: zeroruntime.ToolDefinitionFormat{Definition: "start: WORD"}},
	} {
		t.Run("format "+test.name, func(t *testing.T) {
			formatted := append([]zeroruntime.ToolDefinition(nil), one...)
			formatted[0].Format = &test.format
			if estimateToolDefTokens(formatted) <= estimateToolDefTokens(one) {
				t.Fatalf("a non-empty format %s must increase the estimated request cost", test.name)
			}
		})
	}
	two := append(append([]zeroruntime.ToolDefinition{}, one...), zeroruntime.ToolDefinition{
		Name:        "write_file",
		Description: "Write contents to a file in the workspace.",
		Parameters:  map[string]any{"type": "object"},
	})
	if estimateToolDefTokens(two) <= estimateToolDefTokens(one) {
		t.Fatal("the estimate must grow with more tool definitions")
	}
}

// --- Loop integration: provider mocks -------------------------------------

// summarizeRecordingProvider returns scripted turns for the main agent loop
// and records the message count of any request that carries no tools (the
// summary request issued by the compaction Summarize closure).
type summarizeRecordingProvider struct {
	turns          [][]zeroruntime.StreamEvent
	requests       []zeroruntime.CompletionRequest
	summarizeCalls int
}

func (provider *summarizeRecordingProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)

	// A summary request advertises no tools and is issued out-of-band by the
	// compaction closure; respond with summary text and don't consume a turn.
	if len(request.Tools) == 0 {
		provider.summarizeCalls++
		return streamEvents([]zeroruntime.StreamEvent{
			{Type: zeroruntime.StreamEventText, Content: "COMPACTED SUMMARY"},
			{Type: zeroruntime.StreamEventDone},
		}), nil
	}

	turnIndex := len(provider.requests) - 1 - provider.summarizeCalls
	events := []zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventDone}}
	if turnIndex >= 0 && turnIndex < len(provider.turns) {
		events = provider.turns[turnIndex]
	}
	return streamEvents(events), nil
}

func streamEvents(events []zeroruntime.StreamEvent) <-chan zeroruntime.StreamEvent {
	ch := make(chan zeroruntime.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func TestRunProactiveCompactionTriggers(t *testing.T) {
	bigText := strings.Repeat("x", 8000) // ~2000 estimated tokens

	// Turn 1 emits a big response AND a tool call so the loop keeps going (a
	// turn with tool calls is never final). The bloated history then trips the
	// proactive top-of-turn check before turn 2 builds its request.
	provider := &summarizeRecordingProvider{
		turns: [][]zeroruntime.StreamEvent{
			toolTurnWithText(bigText, "1", "read_file", `{"path":"x"}`),
			textTurn("done"),
		},
	}

	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(t.TempDir(), nil))

	result, err := Run(context.Background(), strings.Repeat("y", 8000), provider, Options{
		Registry:               registry,
		PermissionMode:         PermissionModeUnsafe,
		ContextWindow:          1000, // ~250 token 80% threshold; easily exceeded
		CompactionPreserveLast: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected run to complete with 'done', got %q", result.FinalAnswer)
	}
	if provider.summarizeCalls == 0 {
		t.Fatal("expected proactive compaction to invoke the summarizer at least once")
	}
}

func TestRunNoCompactionWhenContextWindowZero(t *testing.T) {
	bigText := strings.Repeat("x", 8000)
	provider := &summarizeRecordingProvider{
		turns: [][]zeroruntime.StreamEvent{
			toolTurnWithText(bigText, "1", "read_file", `{"path":"x"}`),
			textTurn("done"),
		},
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(t.TempDir(), nil))

	_, err := Run(context.Background(), strings.Repeat("y", 8000), provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeUnsafe,
		ContextWindow:  0, // disabled
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.summarizeCalls != 0 {
		t.Fatalf("expected no compaction when ContextWindow==0, got %d summarize calls", provider.summarizeCalls)
	}
}

func TestMaybeCompactStopsAfterSupersededReadsReachThreshold(t *testing.T) {
	body := strings.Repeat("same source line\n", 1000)
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system"},
		toolCallWithArgs("old", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "old", Content: body},
		toolCallWithArgs("new", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "new", Content: body},
	}
	pruned, reclaimed := pruneSupersededReadResults(messages)
	if reclaimed <= 0 {
		t.Fatal("test setup did not reclaim superseded read output")
	}
	prunedSize := estimateTokens(pruned)
	if estimateTokens(messages) <= prunedSize {
		t.Fatal("test setup does not cross the compaction threshold")
	}

	provider := &summarizeRecordingProvider{}
	state := &compactionState{
		enabled:      true,
		threshold:    prunedSize,
		preserveLast: 2,
	}
	got := state.maybeCompact(context.Background(), provider, messages, nil)

	if provider.summarizeCalls != 0 {
		t.Fatalf("cheap pruning should avoid the summarizer, got %d calls", provider.summarizeCalls)
	}
	if state.lowWaterMark != prunedSize {
		t.Fatalf("lowWaterMark = %d, want %d", state.lowWaterMark, prunedSize)
	}
	if !strings.Contains(got[2].Content, "superseded identical") || got[4].Content != body {
		t.Fatalf("unexpected pruned messages: %#v", got)
	}
}

// reactiveProvider builds up history with a tool call on turn 1, then errors
// with a context-limit message on turn 2 (once the history is large enough that
// compaction can shrink it), then succeeds on the same-turn retry. Summary
// requests (no tools) always succeed and are counted.
type reactiveProvider struct {
	requests       []zeroruntime.CompletionRequest
	summarizeCalls int
	turnRequests   int
	failedOnce     bool
	connectError   bool
	bigText        string
	finalText      string
}

func (provider *reactiveProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)
	if len(request.Tools) == 0 {
		provider.summarizeCalls++
		return streamEvents([]zeroruntime.StreamEvent{
			{Type: zeroruntime.StreamEventText, Content: "SUMMARY"},
			{Type: zeroruntime.StreamEventDone},
		}), nil
	}
	provider.turnRequests++
	switch {
	case provider.turnRequests == 1:
		// Turn 1: a tool call whose big result bloats the history so a later
		// context-limit error has something to compact.
		return streamEvents(toolTurnWithText(provider.bigText, "1", "read_file", `{"path":"x"}`)), nil
	case provider.turnRequests == 2 && !provider.failedOnce:
		provider.failedOnce = true
		if provider.connectError {
			return nil, errors.New("context_length_exceeded")
		}
		return streamEvents([]zeroruntime.StreamEvent{
			{Type: zeroruntime.StreamEventError, Error: "This model's maximum context length is 1000 tokens. Please reduce the length of the messages."},
		}), nil
	default:
		return streamEvents(textTurn(provider.finalText)), nil
	}
}

func TestRunConnectErrorCompactionRecordsReplacementPlan(t *testing.T) {
	provider := &reactiveProvider{
		connectError: true,
		bigText:      strings.Repeat("b", 6000),
		finalText:    "recovered",
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(t.TempDir(), nil))
	recorder := trace.NewRecorder("connect-compaction", "run-1", "test")
	var contextPlans []ContextBreakdown

	result, err := Run(context.Background(), strings.Repeat("z", 6000), provider, Options{
		Registry:               registry,
		PermissionMode:         PermissionModeUnsafe,
		ContextWindow:          10_000_000,
		CompactionPreserveLast: 2,
		Trace:                  recorder,
		OnContext: func(breakdown ContextBreakdown) {
			contextPlans = append(contextPlans, breakdown)
		},
	})
	if err != nil || result.FinalAnswer != "recovered" {
		t.Fatalf("connect-error compaction failed: result=%#v err=%v", result, err)
	}
	if len(contextPlans) != 3 || contextPlans[2].MessageTokens >= contextPlans[1].MessageTokens {
		t.Fatalf("replacement request context not reported: %#v", contextPlans)
	}
	gotTrace := recorder.Finish()
	if len(gotTrace.PrefixHashes) != 4 || gotTrace.PrefixHashes[3].InvalidationReason != "unchanged" {
		t.Fatalf("replacement request trace not recorded: %#v", gotTrace.PrefixHashes)
	}
}

func TestRunReactiveCompactionRecovers(t *testing.T) {
	provider := &reactiveProvider{
		bigText:   strings.Repeat("b", 6000),
		finalText: "recovered",
	}
	// A registered tool makes the main turn requests advertise tools, so the
	// provider can distinguish them from the tool-less summary request issued by
	// the compaction closure. read_file also returns a sizeable result that
	// bloats the history.
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(t.TempDir(), nil))
	recorder := trace.NewRecorder("reactive-compaction", "run-1", "test")
	var contextPlans []ContextBreakdown

	// ContextWindow large enough that proactive compaction never triggers, so
	// only the reactive path can save the run.
	result, err := Run(context.Background(), strings.Repeat("z", 6000), provider, Options{
		Registry:               registry,
		PermissionMode:         PermissionModeUnsafe,
		ContextWindow:          10_000_000,
		CompactionPreserveLast: 2,
		Trace:                  recorder,
		OnContext: func(breakdown ContextBreakdown) {
			contextPlans = append(contextPlans, breakdown)
		},
	})
	if err != nil {
		t.Fatalf("expected reactive compaction to recover the run, got error: %v", err)
	}
	if result.FinalAnswer != "recovered" {
		t.Fatalf("expected recovered answer, got %q", result.FinalAnswer)
	}
	if provider.summarizeCalls == 0 {
		t.Fatal("expected reactive compaction to invoke the summarizer")
	}
	if len(contextPlans) != 3 {
		t.Fatalf("main request context callbacks = %d, want initial turns plus compacted retry", len(contextPlans))
	}
	if contextPlans[2].MessageTokens >= contextPlans[1].MessageTokens {
		t.Fatalf("retry context was not updated after compaction: before=%d after=%d", contextPlans[1].MessageTokens, contextPlans[2].MessageTokens)
	}
	gotTrace := recorder.Finish()
	if len(gotTrace.PrefixHashes) != 4 {
		t.Fatalf("prefix evidence count = %d, want two turns, compaction, and retry: %#v", len(gotTrace.PrefixHashes), gotTrace.PrefixHashes)
	}
	if gotTrace.PrefixHashes[0].InvalidationReason != "initial" ||
		gotTrace.PrefixHashes[1].InvalidationReason != "unchanged" ||
		gotTrace.PrefixHashes[2].InvalidationReason != "initial" ||
		gotTrace.PrefixHashes[3].InvalidationReason != "unchanged" {
		t.Fatalf("unexpected request-specific invalidation evidence: %#v", gotTrace.PrefixHashes)
	}
	if gotTrace.PrefixHashes[2].CompletePrefixHash == gotTrace.PrefixHashes[1].CompletePrefixHash {
		t.Fatalf("compaction fingerprint reused the main request prefix: %#v", gotTrace.PrefixHashes)
	}
}

// midStreamReactiveProvider forwards some text BEFORE surfacing a context-limit
// error mid-stream, then succeeds on the same-turn retry. It exists to prove the
// reactive retry collect does not re-stream OnText/OnUsage (double output).
type midStreamReactiveProvider struct {
	summarizeCalls int
	turnRequests   int
	failedOnce     bool
	bigText        string
	partialText    string
	finalText      string
}

func (provider *midStreamReactiveProvider) StreamCompletion(_ context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	if len(request.Tools) == 0 {
		provider.summarizeCalls++
		return streamEvents([]zeroruntime.StreamEvent{
			{Type: zeroruntime.StreamEventText, Content: "SUMMARY"},
			{Type: zeroruntime.StreamEventDone},
		}), nil
	}
	provider.turnRequests++
	switch {
	case provider.turnRequests == 1:
		return streamEvents(toolTurnWithText(provider.bigText, "1", "read_file", `{"path":"x"}`)), nil
	case provider.turnRequests == 2 && !provider.failedOnce:
		provider.failedOnce = true
		// Some text is forwarded to OnText BEFORE the mid-stream error.
		return streamEvents([]zeroruntime.StreamEvent{
			{Type: zeroruntime.StreamEventText, Content: provider.partialText},
			{Type: zeroruntime.StreamEventError, Error: "This model's maximum context length is 1000 tokens. Please reduce the length of the messages."},
		}), nil
	default:
		return streamEvents(textTurn(provider.finalText)), nil
	}
}

func TestRunReactiveRetryDoesNotDoubleEmitText(t *testing.T) {
	provider := &midStreamReactiveProvider{
		bigText:     strings.Repeat("b", 6000),
		partialText: "partial-output ",
		finalText:   "recovered",
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(t.TempDir(), nil))

	var deltas []string
	result, err := Run(context.Background(), strings.Repeat("z", 6000), provider, Options{
		Registry:               registry,
		PermissionMode:         PermissionModeUnsafe,
		ContextWindow:          10_000_000,
		CompactionPreserveLast: 2,
		OnText:                 func(delta string) { deltas = append(deltas, delta) },
	})
	if err != nil {
		t.Fatalf("expected reactive compaction to recover, got error: %v", err)
	}
	if result.FinalAnswer != "recovered" {
		t.Fatalf("expected recovered answer, got %q", result.FinalAnswer)
	}
	if provider.summarizeCalls == 0 {
		t.Fatal("expected reactive compaction to run")
	}
	// The retried turn's text must NOT be re-streamed to OnText. Before the fix it
	// was emitted on both the original (mid-stream) collect AND the retry collect,
	// double-emitting the retried response.
	joined := strings.Join(deltas, "")
	if got := strings.Count(joined, "recovered"); got != 0 {
		t.Fatalf("retried-turn text must NOT be re-streamed to OnText, saw %d occurrences in %q", got, joined)
	}
}

func TestIsContextLimitError(t *testing.T) {
	positives := []string{
		"This model's maximum context length is 8192 tokens",
		"context_length_exceeded",
		"prompt is too long: 250000 tokens > 200000 maximum",
		"Please reduce the length of the messages",
		"input length and `max_tokens` exceed context limit",
	}
	for _, message := range positives {
		if !isContextLimitError(message) {
			t.Fatalf("expected %q to be detected as a context-limit error", message)
		}
	}
	negatives := []string{
		"",
		"connection reset by peer",
		"401 unauthorized",
		"rate limit exceeded",
	}
	for _, message := range negatives {
		if isContextLimitError(message) {
			t.Fatalf("did not expect %q to be detected as a context-limit error", message)
		}
	}
}

// toolTurnWithText produces a turn that emits visible text AND a tool call so
// the loop keeps going (a turn with tool calls is not final).
func toolTurnWithText(text string, callID string, toolName string, args string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: text},
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: callID, ToolName: toolName},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: callID, ArgumentsFragment: args},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: callID},
		{Type: zeroruntime.StreamEventDone},
	}
}

func TestCompactNeverProducesConsecutiveUserMessages(t *testing.T) {
	msgs := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "sys"},
		{Role: zeroruntime.MessageRoleUser, Content: "u1"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "a1"},
		{Role: zeroruntime.MessageRoleUser, Content: "u2"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "a2"},
		{Role: zeroruntime.MessageRoleUser, Content: "u3-latest"},
	}
	out, err := Compact(msgs, CompactionOptions{
		PreserveLast: 1, // suffix would naively start at u3-latest (user)
		Summarize:    func([]zeroruntime.Message) (string, error) { return "summary", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(out); i++ {
		if out[i].Role == zeroruntime.MessageRoleUser && out[i-1].Role == zeroruntime.MessageRoleUser {
			t.Fatalf("consecutive user messages at %d in %+v", i, out)
		}
	}
}

func TestRecoverNoopDoesNotConsumeReactiveBudget(t *testing.T) {
	st := newCompactionState(Options{ContextWindow: 1000, CompactionPreserveLast: 2}, nil)
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "SUMMARY"}, {Type: zeroruntime.StreamEventDone},
	}}}

	// First recover: history is too small to compact, so it is a no-op (not
	// retried). This must NOT consume the one-shot reactive budget.
	tiny := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "sys"},
		{Role: zeroruntime.MessageRoleUser, Content: "hi"},
	}
	_, retried, err := st.recover(context.Background(), provider, tiny, nil, "context length exceeded")
	if err != nil {
		t.Fatalf("unexpected error from no-op recover: %v", err)
	}
	if retried {
		t.Fatal("expected the too-small recover to be a no-op (not retried)")
	}
	if st.reactiveAttempted {
		t.Fatal("a no-op recover must not consume the one-shot reactive budget")
	}

	// Second recover: now there is a compactible middle, so it must still fire.
	big := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "sys"},
		{Role: zeroruntime.MessageRoleUser, Content: strings.Repeat("u", 4000)},
		{Role: zeroruntime.MessageRoleAssistant, Content: strings.Repeat("a", 4000)},
		{Role: zeroruntime.MessageRoleUser, Content: "u2"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "a2"},
		{Role: zeroruntime.MessageRoleUser, Content: "u3"},
	}
	compacted, retried, err := st.recover(context.Background(), provider, big, nil, "context length exceeded")
	if err != nil {
		t.Fatalf("unexpected error from second recover: %v", err)
	}
	if !retried {
		t.Fatal("expected the second recover to compact and retry")
	}
	if estimateTokens(compacted) >= estimateTokens(big) {
		t.Fatal("expected the second recover to actually shrink the history")
	}
	if !st.reactiveAttempted {
		t.Fatal("a successful recover must consume the reactive budget")
	}
}

func TestRecoverDisabledIsNoop(t *testing.T) {
	st := newCompactionState(Options{ContextWindow: 0}, nil)
	msgs := []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "x"}}
	called := false
	// recover must not invoke the provider/summarize when disabled, even on a
	// context-limit error string.
	got, retried, err := st.recover(context.Background(), &mockProvider{turns: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "should not be called"}, {Type: zeroruntime.StreamEventDone},
	}}}, msgs, nil, "context length exceeded")
	_ = called
	if retried || err != nil || len(got) != 1 {
		t.Fatalf("disabled recover must be a no-op, got retried=%v err=%v len=%d", retried, err, len(got))
	}
}
