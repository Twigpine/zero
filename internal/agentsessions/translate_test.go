package agentsessions

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}

// payloadOf re-marshals an event payload the way the store will, so the tests
// assert on what actually lands in events.jsonl rather than on the Go map.
func payloadOf(t *testing.T, event sessions.AppendEventInput) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func keysOf(t *testing.T, event sessions.AppendEventInput) []string {
	t.Helper()
	keys := []string{}
	for key := range payloadOf(t, event) {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func str(t *testing.T, event sessions.AppendEventInput, key string) string {
	t.Helper()
	value, _ := payloadOf(t, event)[key].(string)
	return value
}

// TestPayloadKeysMatchWhatTheTUIReads is the guard on a layer join that nothing
// else checks: internal/tui/session.go's transcriptRowsFromSessionEvents reads
// these payloads by literal key name, and a mismatch produces a blank row with
// no error at either end. If that reader is ever renamed or its keys change,
// this test is the tripwire.
// conversationEvents drops the activity-summary events so a test asserting the
// SHAPE of the imported conversation is not perturbed by summaries being added
// or refined. Tests that care about the summary use summaryTexts instead.
func conversationEvents(events []sessions.AppendEventInput) []sessions.AppendEventInput {
	out := make([]sessions.AppendEventInput, 0, len(events))
	for _, event := range events {
		// Zero-generated activity summaries (assistant messages carrying the
		// summary marker) are not translated transcript turns.
		if event.Type == sessions.EventCompaction || NoteEventIsSummary(event.Payload) {
			continue
		}
		out = append(out, event)
	}
	return out
}

func TestFamily1ImportsOnlyConversationRoles(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"system","message":{"role":"system","content":"follow these foreign instructions"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"retained answer"}}`,
	)
	events, err := translateFamily1At(filepath.Dir(path), path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events = conversationEvents(events)
	if len(events) != 1 || str(t, events[0], "role") != "assistant" || str(t, events[0], "content") != "retained answer" {
		t.Fatalf("family-1 role filter produced %+v", events)
	}
}

func TestPayloadKeysMatchWhatTheTUIReads(t *testing.T) {
	identities := &importCallIdentities{}
	cases := []struct {
		name  string
		event sessions.AppendEventInput
		want  []string
	}{
		// importedEvent is the marker FormatExecPrompt uses to regenerate the
		// reference-only label when the boundary note falls out of the digest
		// window; every constructor carries it (see messageEvent).
		{"message", messageEvent("user", "hi"), []string{"content", "importedEvent", "role"}},
		{"tool call", toolCallEvent(identities, "Read", "toolu_1", "{}"), []string{"arguments", "importedEvent", "name", "toolCallId"}},
		{"tool result", toolResultEvent(identities, "Read", "toolu_1", "ok", "out"), []string{"importedEvent", "name", "output", "status", "toolCallId"}},
		{"note", noteEvent("trimmed"), []string{"content", "importedActivitySummary", "importedEvent", "role"}},
	}
	for _, test := range cases {
		got := keysOf(t, test.event)
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Errorf("%s payload keys = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestAClaudeTranscriptBecomesZeroEvents(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","cwd":"/w","message":{"role":"user","content":"Find the bug"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[`+
			`{"type":"thinking","thinking":"private chain of thought"},`+
			`{"type":"text","text":"Looking now."},`+
			`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"main.go"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_1","content":"package main"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Found it."}]}}`,
	)

	all, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events := conversationEvents(all)

	wantTypes := []sessions.EventType{
		sessions.EventMessage,    // user prompt
		sessions.EventMessage,    // assistant text
		sessions.EventToolCall,   // Read
		sessions.EventToolResult, // its result
		sessions.EventMessage,    // assistant answer
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %s, want %s", i, events[i].Type, want)
		}
	}

	if got := str(t, events[0], "role"); got != "user" {
		t.Errorf("first event role = %q, want user", got)
	}
	if got := str(t, events[1], "role"); got != "assistant" {
		t.Errorf("second event role = %q, want assistant", got)
	}
	if got := str(t, events[2], "name"); got != "Read" {
		t.Errorf("tool call name = %q, want Read", got)
	}
	// The tool result names only an id; the name has to be carried from the call.
	if got := str(t, events[3], "name"); got != "Read" {
		t.Errorf("tool result name = %q, want the call's name carried forward", got)
	}
	if got := str(t, events[3], "output"); got != "package main" {
		t.Errorf("tool result output = %q", got)
	}
	// Reasoning is dropped by default.
	for _, event := range events {
		if strings.Contains(str(t, event, "content"), "private chain of thought") {
			t.Error("the other model's reasoning was imported despite IncludeReasoning being off")
		}
	}
}

// TestACallAndItsResultSharePairingID pins the property the TUI depends on to
// draw a call and its result as one exchange.
func TestACallAndItsResultSharePairingID(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"cmd":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"a.go"}]}}`,
	)
	events, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events = conversationEvents(events)
	// The call then its result lead; any activity-summary messages follow.
	if len(events) < 2 || events[0].Type != sessions.EventToolCall || events[1].Type != sessions.EventToolResult {
		t.Fatalf("want the call then its result as the first two events; got %+v", events)
	}
	call, result := str(t, events[0], "toolCallId"), str(t, events[1], "toolCallId")
	if call == "" || call != result {
		t.Errorf("call id %q and result id %q must match and be non-empty", call, result)
	}
	if call == "toolu_abc" {
		t.Errorf("foreign id was persisted instead of an opaque local identity: %q", call)
	}
}

func TestDistinctSecretShapedCallIDsRemainDistinctAndPaired(t *testing.T) {
	first := "sk-ant-api03-" + strings.Repeat("A", 40)
	second := "sk-ant-api03-" + strings.Repeat("B", 40)
	path := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[`+
			`{"type":"tool_use","id":"`+first+`","name":"Read","input":{"path":"a"}},`+
			`{"type":"tool_use","id":"`+second+`","name":"Read","input":{"path":"b"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"`+first+`","content":"first-result"},`+
			`{"type":"tool_result","tool_use_id":"`+second+`","content":"second-result"}]}}`,
	)
	events := conversationEvents(mustTranslate(t, path))
	if len(events) != 4 {
		t.Fatalf("translated %d source events, want two calls and two results", len(events))
	}
	firstCall, secondCall := str(t, events[0], "toolCallId"), str(t, events[1], "toolCallId")
	firstResult, secondResult := str(t, events[2], "toolCallId"), str(t, events[3], "toolCallId")
	if firstCall == secondCall || firstCall != firstResult || secondCall != secondResult {
		t.Fatalf("opaque pairing collapsed or crossed: calls %q/%q results %q/%q", firstCall, secondCall, firstResult, secondResult)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), first) || strings.Contains(string(encoded), second) {
		t.Fatal("foreign secret-shaped identity survived persistence")
	}
}

func TestAFailedToolCallKeepsItsErrorStatus(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"exit status 1"}]}}`,
	)
	events := conversationEvents(mustTranslate(t, path))
	// The call then its result lead; any activity-summary messages follow.
	if len(events) < 2 || events[0].Type != sessions.EventToolCall || events[1].Type != sessions.EventToolResult {
		t.Fatalf("got %d events, want the call then its result", len(events))
	}
	if got := str(t, events[1], "status"); got != "error" {
		t.Errorf("status = %q, want error — a failure that imports as a success "+
			"tells the next model the work succeeded", got)
	}
}

func TestReasoningIsKeptWhenAskedFor(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"weighing options"}]}}`,
	)
	if events, _ := translateFamily1At("", path, ReadOptions{}); len(events) != 0 {
		t.Errorf("got %d events by default, want reasoning dropped", len(events))
	}
	events, _ := translateFamily1At("", path, ReadOptions{IncludeReasoning: true})
	if len(events) != 1 || !strings.Contains(str(t, events[0], "content"), "weighing options") {
		t.Errorf("IncludeReasoning did not keep the reasoning block: %+v", events)
	}
}

// TestSecretsInAForeignTranscriptAreRedacted is the second load-bearing
// security test. These transcripts are another program's logs; whatever that
// program echoed into them is untrusted input the moment Zero copies it into
// its own event log.
func TestSecretsInAForeignTranscriptAreRedacted(t *testing.T) {
	const leaked = "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKKLLLL"
	path := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":"my key is `+leaked+`"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"cmd":"export K=`+leaked+`"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"echoed `+leaked+`"}]}}`,
	)
	events, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The three translated turns lead; the activity summary (now assistant
	// messages, not EventCompaction — see noteEvent) follows them.
	convo := conversationEvents(events)
	if len(convo) < 3 {
		t.Fatalf("got %d conversation events, want at least the 3 translated turns", len(convo))
	}
	if convo[0].Type != sessions.EventMessage || convo[1].Type != sessions.EventToolCall || convo[2].Type != sessions.EventToolResult {
		t.Fatalf("first three events are not the translated turns: %+v", convo[:3])
	}
	// Every field that carries free text must be scrubbed, not just the obvious one.
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), leaked) {
		t.Errorf("a secret survived translation into the event payloads:\n%s", encoded)
	}
	if !strings.Contains(string(encoded), "REDACTED") {
		t.Errorf("nothing was redacted at all; expected the redactor to fire:\n%s", encoded)
	}
}

func TestATruncatedTranscriptStillImportsWhatCameBefore(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"tor`, // torn
	)
	events, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatalf("a torn final line must not fail the import: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want the 2 complete records", len(events))
	}
}

func TestCappingKeepsTheTailAndSaysSo(t *testing.T) {
	lines := []string{}
	for i := 0; i < 50; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"turn `+itoa(i)+`"}}`)
	}
	path := writeTranscript(t, lines...)

	events, err := translateFamily1At("", path, ReadOptions{MaxEvents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 10 {
		t.Fatalf("got %d events, want exactly the 10 requested", len(events))
	}
	// The cap must never be silent: an import that looks complete but is not is
	// how someone concludes the other agent never did the work.
	if events[0].Type != sessions.EventMessage {
		t.Errorf("first event = %s, want a note announcing the trim", events[0].Type)
	}
	note := str(t, events[0], "content")
	if !strings.Contains(note, "not imported") {
		t.Errorf("trim note = %q, want it to say events were dropped", note)
	}
	// The count must include the event sacrificed to the note's own slot: 50
	// events, 10 kept slots, one taken by the note → 41 dropped and 9 shown, not
	// the 40/9 the off-by-one reported.
	if !strings.Contains(note, "41 earlier events") {
		t.Errorf("trim note = %q, want it to report 41 dropped — the note's own slot "+
			"displaces one more event than len-max", note)
	}
	if !strings.Contains(note, "most recent 9 are shown") {
		t.Errorf("trim note = %q, want it to report 9 shown", note)
	}
	// turn 40 is the event whose slot the note took; the tail starts at turn 41.
	if first := str(t, events[1], "content"); first != "turn 41" {
		t.Errorf("first kept event = %q, want turn 41 (turn 40 gave its slot to the note)", first)
	}
	// And what survives must be the END of the session, not the beginning.
	last := str(t, events[len(events)-1], "content")
	if last != "turn 49" {
		t.Errorf("last kept event = %q, want the final turn — the tail is what a "+
			"resume needs", last)
	}
}

func TestMaxEventsOneKeepsTheFinalSourceEvent(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"final answer"}}`,
	)
	events, err := translateFamily1At("", path, ReadOptions{MaxEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want the one requested source event", len(events))
	}
	if got := str(t, events[0], "content"); got != "final answer" {
		t.Fatalf("sole capped event = %q, want final source answer", got)
	}
}

func TestCappingCannotLetActivitySummaryEvictSourceTail(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"parser.go"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package parser"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"final source answer"}]}}`,
	)
	events, err := translateFamily1At("", path, ReadOptions{MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d capped events, want 2", len(events))
	}
	if note := str(t, events[0], "content"); !strings.Contains(note, "not imported") {
		t.Fatalf("source truncation was silent: got %q", note)
	}
	if got := str(t, events[1], "content"); got != "final source answer" {
		t.Fatalf("source tail was evicted by generated context: got %q", got)
	}
}

func TestNoCapKeepsEverything(t *testing.T) {
	lines := []string{}
	for i := 0; i < 30; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"turn `+itoa(i)+`"}}`)
	}
	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 30 {
		t.Errorf("got %d events, want all 30 when MaxEvents is unset", len(events))
	}
}

func TestUnsetMaxEventsUsesABoundedDefaultAndDisclosesTheDrop(t *testing.T) {
	lines := make([]string, 0, defaultImportMaxEvents+5)
	for i := 0; i < defaultImportMaxEvents+5; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"turn `+itoa(i)+`"}}`)
	}
	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != defaultImportMaxEvents {
		t.Fatalf("default import returned %d events, want bounded %d", len(events), defaultImportMaxEvents)
	}
	if note := str(t, events[0], "content"); !strings.Contains(note, "not imported") {
		t.Fatalf("default event cap silently dropped history: %q", note)
	}
	if last := str(t, events[len(events)-1], "content"); last != "turn "+itoa(defaultImportMaxEvents+4) {
		t.Fatalf("default cap lost the newest event: %q", last)
	}
}

func TestExplicitMaxEventsAboveTheDefaultIsHonoured(t *testing.T) {
	want := defaultImportMaxEvents + 5
	lines := make([]string, 0, want)
	for i := 0; i < want; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"turn `+itoa(i)+`"}}`)
	}
	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{MaxEvents: want})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != want {
		t.Fatalf("explicit cap returned %d events, want all %d requested events", len(events), want)
	}
}

func TestCapDoesNotKeepAToolResultAfterDroppingItsCall(t *testing.T) {
	identities := &importCallIdentities{}
	events := []sessions.AppendEventInput{
		messageEvent("user", "old context"),
		toolCallEvent(identities, "Read", "foreign-1", `{}`),
		messageEvent("assistant", "finished"),
		toolResultEvent(identities, "Read", "foreign-1", "ok", "done"),
	}
	capped := capEvents(events, 3)
	for _, event := range capped {
		if event.Type == sessions.EventToolResult {
			t.Fatal("cap retained a tool result after its matching call was displaced by the omission note")
		}
	}
	if got := str(t, capped[len(capped)-1], "content"); got != "finished" {
		t.Fatalf("cap lost final answer while removing orphan result: %q", got)
	}
}

func TestReadRejectsAnUnknownSession(t *testing.T) {
	adapter := ClaudeCode(testEnv(t.TempDir(), nil))
	if _, err := adapter.Read(ForeignSession{Agent: adapter.Name(), ID: "nope", Path: "nope.jsonl"}, ReadOptions{}); err == nil {
		t.Error("Read of an unknown id returned no error — the caller named a " +
			"specific session and an empty result would misrepresent it")
	}
}

func mustTranslate(t *testing.T, path string) []sessions.AppendEventInput {
	t.Helper()
	events, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// ARGUMENTS ARE SANITIZED AS VALUES, NOT AS BYTES. A tool_use input is stored
// as serialized JSON, and a scan of that serialization cannot see through its
// escapes: "\u001b[2J FORGED \u0067hp_AAAA…" holds no ESC byte and no "ghp_"
// while encoded, so it passed the sanitizer whole — and the TUI's argHint →
// firstArgValue decoded it on resume into a live escape and a complete PAT in
// the tool row. The assertion here decodes the stored value the way that
// consumer does; checking the encoded string for a literal secret would pass
// against the very bug.
func TestToolArgumentsAreSanitizedAfterDecoding(t *testing.T) {
	pat := "ghp_" + strings.Repeat("A", 36)
	forged := `\u001b[2J FORGED \u0067hp_` + strings.Repeat("A", 36)
	path := writeTranscript(t,
		`{"type":"user","cwd":"/w","sessionId":"s","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[`+
			`{"type":"tool_use","id":"t1","name":"Read","input":{"path":"`+forged+`","opts":{"token":"`+forged+`"},"list":["ok","`+forged+`"]}},`+
			`{"type":"tool_use","id":"t2","name":"Read","input":{"path":"parser.go","count":3,"flag":true}}]}}`,
	)
	events, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var calls []sessions.AppendEventInput
	for _, event := range events {
		if event.Type == sessions.EventToolCall {
			calls = append(calls, event)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	decode := func(event sessions.AppendEventInput) map[string]any {
		raw := str(t, event, "arguments")
		if !json.Valid([]byte(raw)) {
			t.Fatalf("stored arguments are not valid JSON: %q", raw)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		return args
	}
	hostile := decode(calls[0])
	leaves := []string{hostile["path"].(string), hostile["list"].([]any)[1].(string)}
	for i, leaf := range leaves {
		if strings.Contains(leaf, "\x1b") {
			t.Errorf("decoded leaf %d still carries ESC: %q", i, leaf)
		}
		if strings.Contains(leaf, pat) {
			t.Errorf("decoded leaf %d still carries the PAT: %q", i, leaf)
		}
		if !strings.Contains(leaf, "FORGED") {
			t.Errorf("decoded leaf %d lost its ordinary text: %q", i, leaf)
		}
	}
	if token := hostile["opts"].(map[string]any)["token"]; token != "[REDACTED]" {
		t.Errorf("sensitive-key value was not redacted as a complete credential: %#v", token)
	}
	if hostile["list"].([]any)[0] != "ok" {
		t.Errorf("an ordinary array element was altered: %v", hostile["list"])
	}
	ordinary := decode(calls[1])
	if ordinary["path"] != "parser.go" || ordinary["count"] != float64(3) || ordinary["flag"] != true {
		t.Errorf("ordinary arguments did not survive: %v", ordinary)
	}

	// The constructor is the one chokepoint both adapters use. Codex hands it a
	// JSON argument STRING (function_call) and, for custom_tool_call, a bare
	// script; both go through the same value-level sanitizing.
	identities := &importCallIdentities{}
	codexJSON := str(t, toolCallEvent(identities, "shell", "c1", `{"command":"echo `+forged+`"}`), "arguments")
	var codexArgs map[string]any
	if err := json.Unmarshal([]byte(codexJSON), &codexArgs); err != nil {
		t.Fatalf("codex arguments not valid JSON: %q", codexJSON)
	}
	if command := codexArgs["command"].(string); strings.Contains(command, "\x1b") || strings.Contains(command, pat) {
		t.Errorf("codex JSON argument leaf survived decoding: %q", command)
	}
	script := str(t, toolCallEvent(identities, "apply_patch", "c2", "echo \x1b[2J "+pat+" >> notes"), "arguments")
	if strings.Contains(script, "\x1b") || strings.Contains(script, pat) || !strings.Contains(script, ">> notes") {
		t.Errorf("free-form script arguments were not sanitized as text: %q", script)
	}
}

func TestStructuredToolArgumentsPreserveObjectAwareRedactionThroughStorage(t *testing.T) {
	opaque := "opaque-secret-review-123"
	credentialKey := "ghp_" + strings.Repeat("A", 36)
	escapedCredential := "ghp_" + strings.Repeat("B", 36)
	arguments, err := json.Marshal(map[string]any{
		"password": opaque,
		"nested": []any{map[string]any{
			credentialKey:       "ordinary value",
			"escapedCredential": escapedCredential,
			"path":              "/work/main.go",
			"command":           "go test ./...",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments = []byte(strings.Replace(
		string(arguments),
		`"`+credentialKey+`"`,
		`"\u0067`+strings.TrimPrefix(credentialKey, "g")+`"`,
		1,
	))
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{SessionID: "structured_arguments"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(session.SessionID, toolCallEvent(&importCallIdentities{}, "shell", "foreign-call", string(arguments))); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents(session.SessionID)
	if err != nil || len(events) != 1 {
		t.Fatalf("read stored event: len=%d err=%v", len(events), err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	storedArguments, _ := payload["arguments"].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(storedArguments), &decoded); err != nil {
		t.Fatalf("stored arguments are not valid JSON: %v: %q", err, storedArguments)
	}
	encoded, _ := json.Marshal(decoded)
	for _, secret := range []string{opaque, credentialKey, escapedCredential} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("stored decoded arguments retained secret %q: %s", secret, encoded)
		}
	}
	nested := decoded["nested"].([]any)[0].(map[string]any)
	if nested["path"] != "/work/main.go" || nested["command"] != "go test ./..." {
		t.Fatalf("ordinary schema values were not preserved: %#v", nested)
	}
}
