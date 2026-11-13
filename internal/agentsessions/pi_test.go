package agentsessions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

// A source-shaped Pi session: the real outer wrapper (a "session" header, then
// "message" entries carrying id/parentId), a visible user prompt, an assistant
// toolCall with object arguments, a matching toolResult with isError false, a
// second call whose result reports isError true, and an unrelated entry type.
func writePiSession(t *testing.T, home string) string {
	t.Helper()
	const ts = "2026-09-01T10:00:00.000Z"
	lines := []string{
		`{"type":"session","version":3,"id":"abc","timestamp":"` + ts + `","cwd":"/w"}`,
		`{"type":"message","id":"m1","parentId":null,"timestamp":"` + ts + `","message":{"role":"user","content":"Fix the parser","timestamp":1}}`,
		`{"type":"message","id":"m2","parentId":"m1","timestamp":"` + ts + `","message":{"role":"assistant","model":"gpt-x","api":"a","provider":"p","content":[{"type":"thinking","thinking":"private chain"},{"type":"text","text":"Reading it."},{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"parser.go"}}],"stopReason":"toolUse","timestamp":2}}`,
		`{"type":"message","id":"m3","parentId":"m2","timestamp":"` + ts + `","message":{"role":"toolResult","toolCallId":"tc1","toolName":"read","content":[{"type":"text","text":"func parse() {}"}],"isError":false,"timestamp":3}}`,
		`{"type":"message","id":"m4","parentId":"m3","timestamp":"` + ts + `","message":{"role":"assistant","model":"gpt-x","content":[{"type":"toolCall","id":"tc2","name":"write","arguments":{"path":"parser.go","content":"new"}}],"stopReason":"toolUse","timestamp":4}}`,
		`{"type":"message","id":"m5","parentId":"m4","timestamp":"` + ts + `","message":{"role":"toolResult","toolCallId":"tc2","toolName":"write","content":[{"type":"text","text":"permission denied"}],"isError":true,"timestamp":5}}`,
		`{"type":"message","id":"m6","parentId":"m5","timestamp":"` + ts + `","message":{"role":"assistant","model":"gpt-x","content":[{"type":"text","text":"The write failed."}],"stopReason":"stop","timestamp":6}}`,
		`{"type":"model_change","id":"m7","parentId":"m6","timestamp":"` + ts + `","provider":"p","modelId":"gpt-y"}`,
	}
	path := filepath.Join(home, ".pi", "agent", "sessions", "-w", "2026-09-01T10-00-00_abc.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}

// PI'S SCHEMA IS ITS OWN, AND ITS TOOL WORK HAS TO SURVIVE TRANSLATION. Routed
// through the Claude parser, a Pi session with a request and a completed tool
// call imported "successfully" with no call, no result and no activity summary,
// and every Pi session was "untitled" because the title check looked for an
// outer type of "user" that Pi never writes. This pins the vendor's real shapes
// end to end: index, import, stored pairing and outcome, activity claims, and
// the resume digest.
func TestPiSessionsImportTheirRealToolSchema(t *testing.T) {
	home := t.TempDir()
	path := writePiSession(t, home)
	adapter := Pi(testEnv(home, nil))

	found, err := adapter.Discover("/w")
	if err != nil || len(found) != 1 {
		t.Fatalf("discover: %v (%d results)", err, len(found))
	}
	if found[0].Title != "Fix the parser" || found[0].Cwd != "/w" || found[0].ModelID != "gpt-x" {
		t.Fatalf("index = %+v, want title from the first prompt, cwd from the header, model from the assistant", found[0])
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	result, err := Import(store, adapter, transcriptID(path), ReadOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	events, err := store.ReadEvents(result.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	type call struct{ name, id string }
	type outcome struct{ status, output string }
	calls := map[string]call{}
	results := map[string]outcome{}
	var texts []string
	var summaries []string
	for _, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		get := func(key string) string { value, _ := payload[key].(string); return value }
		switch event.Type {
		case sessions.EventToolCall:
			calls[get("toolCallId")] = call{name: get("name"), id: get("toolCallId")}
		case sessions.EventToolResult:
			results[get("toolCallId")] = outcome{status: get("status"), output: get("output")}
		case sessions.EventMessage:
			if NoteEventIsSummary(payload) {
				summaries = append(summaries, get("content"))
				continue
			}
			texts = append(texts, get("role")+": "+get("content"))
		}
	}
	if len(calls) != 2 || len(results) != 2 {
		t.Fatalf("calls=%d results=%d, want both tool calls with their results: %v / %v", len(calls), len(results), calls, results)
	}
	var okRead, failedWrite bool
	for id, c := range calls {
		r, paired := results[id]
		if !paired {
			t.Fatalf("call %s (%s) has no paired result", id, c.name)
		}
		switch c.name {
		case "read":
			okRead = r.status == string(tools.StatusOK) && r.output == "func parse() {}"
		case "write":
			failedWrite = r.status == string(tools.StatusError) && r.output == "permission denied"
		}
	}
	if !okRead || !failedWrite {
		t.Fatalf("outcomes were not carried: calls=%v results=%v", calls, results)
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"user: Fix the parser", "assistant: Reading it.", "assistant: The write failed."} {
		if !strings.Contains(joined, want) {
			t.Errorf("conversation is missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "private chain") {
		t.Errorf("reasoning was imported without being asked for:\n%s", joined)
	}
	summary := strings.Join(summaries, "\n")
	// Only the confirmed failure is a factual claim; the failed write must not
	// be reported as a change to parser.go.
	if !strings.Contains(summary, "permission denied") {
		t.Errorf("activity summary does not carry the failed call:\n%s", summary)
	}
	if strings.Contains(strings.ToLower(summary), "changed") && strings.Contains(summary, "parser.go") {
		t.Errorf("a failed write was claimed as a change:\n%s", summary)
	}

	// The reasoning opt-in still works for Pi's "thinking" blocks.
	withReasoning, err := adapter.Read(found[0], ReadOptions{IncludeReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	sawReasoning := false
	for _, event := range withReasoning {
		payload, _ := event.Payload.(map[string]any)
		if payload["role"] == "reasoning" && strings.Contains(payload["content"].(string), "private chain") {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Error("IncludeReasoning did not surface Pi's thinking block")
	}

	// And the real resume path sees the work.
	prepared, err := sessions.PrepareExec(sessions.PrepareExecOptions{Store: store, Resume: result.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	prompt := sessions.FormatExecPrompt("continue", prepared)
	for _, want := range []string{"Fix the parser", "The write failed.", "reference context only"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("resume prompt is missing %q:\n%s", want, prompt)
		}
	}
}
