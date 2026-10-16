package sessions

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func toolContextPayload(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(raw)
}

// interruptedTurnEvents is a turn that read two files and then died on a
// provider error, which is the shape #913 reports: no assistant answer was ever
// produced, so nothing in the conversation describes the work.
func interruptedTurnEvents(t *testing.T) []Event {
	t.Helper()
	return []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "add retries to the http client"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "read_file", "arguments": `{"path":"internal/http/client.go"}`})},
		{Sequence: 3, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "content": "package http"})},
		{Sequence: 4, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c2", "name": "read_file", "arguments": `{"path":"internal/http/retry.go"}`})},
		{Sequence: 5, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "content": "var defaultBackoff = 250ms"})},
		{Sequence: 6, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error: upstream timeout"})},
	}
}

func resumePrompt(t *testing.T, events []Event) string {
	t.Helper()
	return FormatExecPrompt("continue", PreparedExec{
		Mode:          ModeResume,
		Session:       Metadata{SessionID: "s1"},
		ContextEvents: events,
	})
}

// AN INTERRUPTED TURN HAS TO SAY WHAT IT DID.
//
// A turn that ends normally describes its own work in the assistant's answer,
// so the next turn inherits a prose record. A turn killed by a provider error
// produces no answer, and the tool events that were the only record of the work
// were filtered out of the resume context. The next turn was told a request had
// been made and an error had happened, and nothing else, so it re-read from
// scratch (#913).
func TestResumePromptCarriesInterruptedToolWork(t *testing.T) {
	out := resumePrompt(t, interruptedTurnEvents(t))

	for _, want := range []string{"internal/http/client.go", "internal/http/retry.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("the resume prompt does not name %s, so the next turn cannot know it was already read:\n%s", want, out)
		}
	}
	// The conversation spine is still there.
	for _, want := range []string{"add retries to the http client", "upstream timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("the resume prompt lost conversation context %q:\n%s", want, out)
		}
	}
}

// AND THE ORDER STAYS THE ORDER IT HAPPENED IN.
//
// The sequence number is rendered into each line, so a list that jumps
// backwards reads as a corrupted history rather than a merge artifact.
func TestResumePromptKeepsEventsInSequenceOrder(t *testing.T) {
	out := resumePrompt(t, interruptedTurnEvents(t))

	last := -1
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "- #") {
			continue
		}
		var seq int
		if _, err := fmt.Sscanf(line, "- #%d", &seq); err != nil {
			t.Fatalf("unparsable context line %q", line)
		}
		if seq <= last {
			t.Fatalf("context is out of order at %q (previous #%d):\n%s", line, last, out)
		}
		last = seq
	}
	if last < 0 {
		t.Fatal("SETUP INVALID: the prompt rendered no context lines")
	}
}

// TOOL EVENTS MUST NOT EVICT CONVERSATION.
//
// This is what the original filter was added for (#460): tool events vastly
// outnumber messages, so admitting them into the same trailing budget would let
// one tool-heavy turn push every earlier message out of the context. A separate
// allowance is what keeps both properties at once.
func TestToolEventsNeverEvictConversation(t *testing.T) {
	var events []Event
	seq := 0
	next := func() int { seq++; return seq }
	// An early message that must survive, then a flood of tool work.
	events = append(events, Event{Sequence: next(), Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "EARLIEST-REQUEST-MARKER"})})
	for i := 0; i < 500; i++ {
		arguments := fmt.Sprintf(`{"path":"noise/%03d.go"}`, i)
		events = append(events, Event{Sequence: next(), Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "arguments": arguments})})
		events = append(events, Event{Sequence: next(), Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "content": "noise"})})
	}
	events = append(events, Event{Sequence: next(), Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "LATEST-REQUEST-MARKER"})})

	out := resumePrompt(t, events)
	for _, want := range []string{"EARLIEST-REQUEST-MARKER", "LATEST-REQUEST-MARKER"} {
		if !strings.Contains(out, want) {
			t.Errorf("a tool-heavy turn evicted conversation event %q, which is the regression the filter exists to prevent", want)
		}
	}

	selected := promptContextEvents(events)
	tools := 0
	for _, event := range selected {
		if event.Type == EventToolCall || event.Type == EventToolResult {
			tools++
		}
	}
	if tools == 0 {
		t.Error("no tool work survived at all, so an interrupted tool-heavy turn still says nothing about what it did")
	}
	if len(selected) > 80 {
		t.Errorf("selected %d events, over the 80 the prompt budget allows", len(selected))
	}
}

// A session with no tool work renders exactly as it did before.
func TestResumePromptUnchangedWithoutToolEvents(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "hello"})},
		{Sequence: 2, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "assistant", "content": "hi"})},
	}
	out := resumePrompt(t, events)
	if strings.Count(out, "- #") != 2 {
		t.Fatalf("a tool-free session rendered %d context lines, want 2:\n%s", strings.Count(out, "- #"), out)
	}
}

// TOOL OUTPUT MUST NOT RIDE ALONG INTO A LATER PROMPT.
//
// The tail is there to say what the interrupted turn did, and the call already
// says that. Carrying the result body would put up to 500 bytes of raw tool
// output into a prompt on a later turn, and nothing redacts on the way in.
func TestResumePromptCarriesToolOutcomeWithoutOutput(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE-and-more-file-contents"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "look at the config"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "read_file", "arguments": `{"path":"deploy/prod.env"}`})},
		{Sequence: 3, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "status": "ok", "output": secret})},
		{Sequence: 4, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c2", "name": "read_file", "arguments": `{"path":"deploy/missing.env"}`})},
		{Sequence: 5, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "status": "error", "output": "no such file"})},
		{Sequence: 6, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error: upstream timeout"})},
	}
	out := resumePrompt(t, events)

	// THE TOOL LINES ARE PINNED WHOLE, NOT SEARCHED.
	//
	// Two reasons the obvious assertions are too weak to say what the trim
	// promises. Searching the prompt for the fixture's full secret only rejects
	// that exact string, so a change that carried a truncated or partly
	// redacted prefix of the output would pass it. And asking whether "ok" and
	// "error" each appear somewhere among the result lines passes just as well
	// when the two statuses are swapped, or both attached to the wrong call.
	//
	// Pinning the whole of every tool line covers both: nothing but the tool
	// name and the outcome survives a result, each outcome sits on its own
	// line, and the line order pairs each one with the call above it.
	//
	// Compared as a set of fields per line rather than as a string, because the
	// renderer walks the payload map and Go randomizes that order, so the same
	// events render their fields in a different order on every run.
	want := []struct {
		sequence int
		kind     string
		fields   []string
	}{
		{2, "tool_call", []string{"c1", "read_file", `{"path":"deploy/prod.env"}`}},
		{3, "tool_result", []string{"read_file", "ok"}},
		{4, "tool_call", []string{"c2", "read_file", `{"path":"deploy/missing.env"}`}},
		{5, "tool_result", []string{"read_file", "error"}},
	}
	var toolLines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- #") && (strings.Contains(line, "tool_call:") || strings.Contains(line, "tool_result:")) {
			toolLines = append(toolLines, line)
		}
	}
	if len(toolLines) != len(want) {
		t.Fatalf("the resume prompt carries %d tool lines, want %d:\n%s", len(toolLines), len(want), out)
	}
	for index, line := range toolLines {
		var sequence int
		var kind string
		rest, found := strings.CutPrefix(line, "- #")
		if !found {
			t.Fatalf("unparsable context line %q", line)
		}
		if _, err := fmt.Sscanf(rest, "%d %s", &sequence, &kind); err != nil {
			t.Fatalf("unparsable context line %q: %v", line, err)
		}
		kind = strings.TrimSuffix(kind, ":")
		_, payload, _ := strings.Cut(rest, ": ")
		fields := strings.Fields(payload)
		slices.Sort(fields)
		expected := slices.Clone(want[index].fields)
		slices.Sort(expected)
		if sequence != want[index].sequence || kind != want[index].kind || !slices.Equal(fields, expected) {
			t.Errorf("tool line %d is not what the trim promises.\ngot:  #%d %s %v\nwant: #%d %s %v\nfull prompt:\n%s",
				index, sequence, kind, fields, want[index].sequence, want[index].kind, expected, out)
		}
	}
	// Named on its own so a leak reports as a leak rather than as the
	// formatting drift the comparison above would also catch. A PREFIX, because
	// the danger is output reaching a later prompt at all, not this exact
	// fixture string reaching it.
	if strings.Contains(out, secret[:16]) {
		t.Errorf("tool output reached the resume prompt:\n%s", out)
	}
}
