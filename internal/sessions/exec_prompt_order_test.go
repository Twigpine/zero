package sessions

import (
	"encoding/json"
	"strings"
	"testing"
)

// ONE SESSION HAS TO RENDER ONE PROMPT.
//
// extractText walked a map[string]any directly, and Go randomizes map
// iteration, so the resume context put the same fields in a different order in
// every process: `c1 read_file {"path":...}` one run, `{"path":...} c1
// read_file` the next. That block is part of the user prompt, so the prompt
// itself changed run to run, which leaves a provider nothing stable to
// prefix-cache and makes two resumes of one session impossible to diff.
//
// PINNED AS AN EXACT STRING, NOT AS TWO RUNS AGREEING. A test that renders the
// same payload twice in one process and compares the results passes whenever
// the randomized order happens to repeat, which for a small map is often. The
// only assertion that cannot pass by luck names the order it expects.
func TestSummarizePayloadRendersFieldsInKeyOrder(t *testing.T) {
	payload := map[string]any{
		"id":        "c1",
		"name":      "read_file",
		"arguments": `{"path":"deploy/prod.env"}`,
		"status":    "ok",
	}

	// Keys sorted: arguments, id, name, status.
	const want = `{"path":"deploy/prod.env"} c1 read_file ok`
	for attempt := range 50 {
		if got := summarizePayload(payload); got != want {
			t.Fatalf("attempt %d rendered %q, want %q", attempt, got, want)
		}
	}
}

// The same for a payload that arrives as raw JSON, which is the shape the store
// hands back, since that decodes into a map and takes the same path.
func TestSummarizePayloadRendersRawJSONInKeyOrder(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"zulu":  "last",
		"alpha": "first",
		"mike":  "middle",
	})
	if err != nil {
		t.Fatal(err)
	}

	const want = "first middle last"
	for attempt := range 50 {
		if got := summarizePayload(json.RawMessage(raw)); got != want {
			t.Fatalf("attempt %d rendered %q, want %q", attempt, got, want)
		}
	}
}

// The summary short-circuit is untouched: a payload carrying one still answers
// with it alone, rather than with every field in key order.
func TestSummarizePayloadStillPrefersASummary(t *testing.T) {
	payload := map[string]any{
		"alpha":   "not this",
		"summary": "this one",
		"zulu":    "nor this",
	}
	if got := summarizePayload(payload); got != "this one" {
		t.Fatalf("rendered %q, want the summary alone", got)
	}
}

// And a list keeps the order it was written in, which is the caller's and not
// this function's to sort.
func TestSummarizePayloadKeepsListOrder(t *testing.T) {
	payload := map[string]any{"items": []any{"third", "first", "second"}}
	if got := summarizePayload(payload); got != "third first second" {
		t.Fatalf("rendered %q, want the list in its written order", got)
	}
}

// THE WHOLE BLOCK IS STABLE, not just one payload. Rendered through the
// prompt-context path a resume actually uses, so a future renderer that walks a
// map of its own is caught here too.
//
// THE EXPECTED BLOCK IS WRITTEN OUT, NOT TAKEN FROM THE FIRST RENDER. Comparing
// later renders against the first only asks whether the renderer is stable, and
// a renderer that is stably wrong passes: any fixed field order satisfies it,
// including the one this change replaces. It also hid something. The first
// version of this test used tool events, which promptContextEvents filters out
// of the resume context on this branch, so the block being compared was a
// single line and nobody could tell. Naming the expected block makes both
// failures visible.
func TestResumeContextBlockIsIdenticalAcrossRenders(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: mustOrderPayload(t, map[string]any{"role": "user", "content": "rotate the deploy key"})},
		{Sequence: 2, Type: EventMessage, Payload: mustOrderPayload(t, map[string]any{"role": "assistant", "content": "reading the env file"})},
		{Sequence: 3, Type: EventMessage, Payload: mustOrderPayload(t, map[string]any{"role": "user", "content": "and the hooks"})},
	}

	// Keys sorted, so content comes before role in every line.
	want := strings.Join([]string{
		"message: rotate the deploy key user",
		"message: reading the env file assistant",
		"message: and the hooks user",
	}, "\n")

	for attempt := range 50 {
		if got := renderOrderContext(t, events); got != want {
			t.Fatalf("attempt %d rendered:\n%s\nwant:\n%s", attempt, got, want)
		}
	}
}

func renderOrderContext(t *testing.T, events []Event) string {
	t.Helper()
	lines := []string{}
	for _, event := range promptContextEvents(events) {
		var decoded any
		if len(event.Payload) > 0 {
			if err := json.Unmarshal(event.Payload, &decoded); err != nil {
				t.Fatal(err)
			}
		}
		lines = append(lines, string(event.Type)+": "+summarizePayload(decoded))
	}
	return strings.Join(lines, "\n")
}

func mustOrderPayload(t *testing.T, payload map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
