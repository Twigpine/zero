package agentsessions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/sessions"
)

func summaryTexts(t *testing.T, events []sessions.AppendEventInput) []string {
	t.Helper()
	out := []string{}
	for _, event := range events {
		// Activity summaries are now assistant messages carrying the summary
		// marker (see noteEvent), not EventCompaction.
		if !NoteEventIsSummary(event.Payload) {
			continue
		}
		out = append(out, str(t, event, "content"))
	}
	return out
}

func joinedSummary(t *testing.T, events []sessions.AppendEventInput) string {
	t.Helper()
	return strings.Join(summaryTexts(t, events), "\n")
}

// claudeToolLines builds a transcript with one tool_use/tool_result pair.
func claudeToolLines(id, name, arguments, output string, failed bool) []string {
	isError := "false"
	if failed {
		isError = "true"
	}
	return []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id +
			`","name":"` + name + `","input":` + arguments + `}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
			`","is_error":` + isError + `,"content":"` + output + `"}]}}`,
	}
}

func TestTheSummaryNamesFilesCommandsAndSearches(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "Read", `{"file_path":"/w/parser.go"}`, "package main", false)...)
	lines = append(lines, claudeToolLines("t2", "Edit", `{"file_path":"/w/lexer.go"}`, "ok", false)...)
	lines = append(lines, claudeToolLines("t3", "Bash", `{"command":"go test ./..."}`, "PASS", false)...)
	lines = append(lines, claudeToolLines("t4", "Grep", `{"pattern":"handleResume"}`, "3 hits", false)...)

	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	summary := joinedSummary(t, events)

	for _, want := range []string{
		"4 tool calls",
		"Files read: parser.go",   // relative to cwd, not the absolute path
		"Files changed: lexer.go", // Edit is classified as mutating
		"Commands run: go test ./...",
		"Searched for: handleResume",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// TestAFailedCallDoesNotClaimItReadTheFile is the correctness fix the real
// corpus prompted: a Read of a path that does not exist was still listed under
// "Files read", which the next model would take as fact.
func TestAFailedCallDoesNotClaimItReadTheFile(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "Read", `{"file_path":"/home/wrong/parser.go"}`,
		"File does not exist.", true)...)
	lines = append(lines, claudeToolLines("t2", "Read", `{"file_path":"/w/parser.go"}`, "package main", false)...)

	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	summary := joinedSummary(t, events)

	if strings.Contains(summary, "/home/wrong/parser.go") && strings.Contains(summary, "Files read") {
		for _, line := range summaryTexts(t, events) {
			if strings.HasPrefix(line, "Files read") && strings.Contains(line, "/home/wrong/parser.go") {
				t.Errorf("a path whose read FAILED is listed as read:\n%s", line)
			}
		}
	}
	if !strings.Contains(summary, "Files read: parser.go") {
		t.Errorf("the successful read was lost:\n%s", summary)
	}
	// The failure itself must still be reported — that is the most useful line.
	if !strings.Contains(summary, "Failures") || !strings.Contains(summary, "File does not exist") {
		t.Errorf("the failure was not reported:\n%s", summary)
	}
}

// TestEverySummaryEventSurvivesTheDigestIntact is the constraint that decided
// the shape of this feature. sessions.summarizePayload truncates each event at
// 500 bytes, so one combined summary would lose its tail; each event must fit.
func TestEverySummaryEventSurvivesTheDigestIntact(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	for i := 0; i < 40; i++ {
		lines = append(lines, claudeToolLines(
			"t"+itoa(i), "Read",
			`{"file_path":"/w/a/very/long/directory/name/that/eats/budget/file`+itoa(i)+`.go"}`,
			"ok", false)...)
	}
	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	summaries := summaryTexts(t, events)
	if len(summaries) == 0 {
		t.Fatal("no summary events produced")
	}
	for _, summary := range summaries {
		if length := len(summary); length > maxSummaryEventBytes {
			t.Errorf("summary event is %d bytes, over the %d budget — it will be cut "+
				"mid-sentence by the resume digest:\n%s", length, maxSummaryEventBytes, summary)
		}
	}
	// And the overflow must be stated, not silently dropped.
	if !strings.Contains(strings.Join(summaries, "\n"), "more)") {
		t.Errorf("40 files collapsed to a short list with no overflow note:\n%s",
			strings.Join(summaries, "\n"))
	}
}

func TestMultibyteSummarySurvivesFormatExecPromptIntact(t *testing.T) {
	home := t.TempDir()
	lines := []string{`{"type":"user","cwd":"/w","sessionId":"multi","message":{"role":"user","content":"continue"}}`}
	for i := 0; i < 20; i++ {
		name := strings.Repeat("界", 24) + itoa(i) + ".go"
		lines = append(lines, claudeToolLines("t"+itoa(i), "Read", `{"file_path":"/w/`+name+`"}`, "ok", false)...)
	}
	writeFile(t, filepath.Join(home, ".claude", "projects", "-w", "multi.jsonl"), strings.Join(lines, "\n")+"\n")

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	result, err := Import(store, ClaudeCode(testEnv(home, nil)), "multi", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents(result.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var fileSummary string
	for _, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil || !NoteEventIsSummary(payload) {
			continue
		}
		content, _ := payload["content"].(string)
		if strings.HasPrefix(content, "Files read:") {
			fileSummary = content
		}
	}
	if fileSummary == "" || !strings.Contains(fileSummary, "more)") {
		t.Fatalf("multibyte file summary lost its overflow disclosure: %q", fileSummary)
	}
	if len(fileSummary) > maxSummaryEventBytes {
		t.Fatalf("summary is %d bytes, want at most %d: %q", len(fileSummary), maxSummaryEventBytes, fileSummary)
	}
	prepared, err := sessions.PrepareExec(sessions.PrepareExecOptions{Store: store, Resume: result.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	prompt := sessions.FormatExecPrompt("what remains?", prepared)
	if !strings.Contains(prompt, fileSummary) {
		t.Fatalf("FormatExecPrompt truncated a producer-approved summary:\nsummary=%q\nprompt=%s", fileSummary, prompt)
	}
}

func TestSummaryLinePreservesOverflowDisclosureWhenOneItemExhaustsTheBudget(t *testing.T) {
	items := []string{strings.Repeat("界", maxSummaryEventBytes)}
	for i := 0; i < maxSummaryItems+4; i++ {
		items = append(items, "file"+itoa(i)+".go")
	}
	line := summaryLine("Files read", items)
	if len(line) > maxSummaryEventBytes || !utf8.ValidString(line) {
		t.Fatalf("bounded summary is invalid or oversized: bytes=%d valid=%v %q", len(line), utf8.ValidString(line), line)
	}
	if !strings.Contains(line, "more)") {
		t.Fatalf("a long first item erased the overflow disclosure: %q", line)
	}
}

func TestTruncateToBudgetKeepsUTF8ValidAtEverySmallBudget(t *testing.T) {
	for budget := 0; budget <= 4; budget++ {
		got := truncateToBudget("界界", budget)
		if len(got) > budget || !utf8.ValidString(got) {
			t.Errorf("budget %d produced bytes=%d valid=%v %q", budget, len(got), utf8.ValidString(got), got)
		}
	}
}

// TestTheSummaryReachesTheModel is the whole point: these events must survive
// sessions.promptContextEvents, the filter that drops tool events.
func TestTheSummaryReachesTheModel(t *testing.T) {
	home := t.TempDir()
	lines := []string{`{"type":"user","cwd":"/w","sessionId":"s1","message":{"role":"user","content":"fix the parser"}}`}
	lines = append(lines, claudeToolLines("t1", "Read", `{"file_path":"/w/parser.go"}`, "package main", false)...)
	lines = append(lines, claudeToolLines("t2", "Bash", `{"command":"go test ./parser"}`, "FAIL", true)...)
	writeFile(t, filepath.Join(home, ".claude", "projects", "-w", "s1.jsonl"),
		strings.Join(lines, "\n")+"\n")

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	result, err := Import(store, ClaudeCode(testEnv(home, nil)), "s1", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := sessions.PrepareExec(sessions.PrepareExecOptions{Store: store, Resume: result.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	digest := sessions.FormatExecPrompt("what is left?", prepared)

	// Before this feature the digest held only the two prose messages. These
	// facts existed solely in tool events, which never reach the model.
	for _, want := range []string{
		"tool calls",
		"parser.go",
		"go test ./parser",
		"Failures",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("the resume digest is missing %q — the summary is not reaching "+
				"the model:\n%s", want, digest)
		}
	}
}

func TestASessionWithNoToolCallsGetsNoSummary(t *testing.T) {
	events, err := translateFamily1At("", writeTranscript(t,
		`{"type":"user","cwd":"/w","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	if got := summaryTexts(t, events); len(got) != 0 {
		t.Errorf("got %d summary events for a conversation with no tools, want none: %v", len(got), got)
	}
}

// TestAnUnknownToolSchemaDegradesToACount covers the case this design must not
// get wrong: an agent whose arguments use names we have never seen. Naming the
// tool and its count is true; guessing which field held a filename would not be.
func TestAnUnknownToolSchemaDegradesToACount(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "MysteryTool", `{"wibble":"/w/secret.go","flim":3}`, "ok", false)...)
	lines = append(lines, claudeToolLines("t2", "MysteryTool", `{"wibble":"/w/other.go"}`, "ok", false)...)

	events, _ := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	summary := joinedSummary(t, events)

	if !strings.Contains(summary, "MysteryTool x2") {
		t.Errorf("an unrecognised schema should still be counted:\n%s", summary)
	}
	for _, guessed := range []string{"secret.go", "other.go", "Files read"} {
		if strings.Contains(summary, guessed) {
			t.Errorf("summary guessed %q out of an unknown argument schema:\n%s", guessed, summary)
		}
	}
}

func TestRepeatedWorkIsNotListedRepeatedly(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	for i := 0; i < 8; i++ {
		lines = append(lines, claudeToolLines("t"+itoa(i), "Read", `{"file_path":"/w/same.go"}`, "ok", false)...)
	}
	events, _ := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	summary := joinedSummary(t, events)

	if count := strings.Count(summary, "same.go"); count != 1 {
		t.Errorf("same.go appears %d times, want 1 — re-reads must dedupe:\n%s", count, summary)
	}
	// The call count is still the true one.
	if !strings.Contains(summary, "8 tool calls") {
		t.Errorf("dedupe must not change the call count:\n%s", summary)
	}
}

// TestSecretsInToolArgumentsAreRedacted: the summary is built from another
// program's logs, so it goes through the same chokepoint as every other
// imported string.
func TestSecretsInToolArgumentsAreRedacted(t *testing.T) {
	const leaked = "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKKLLLL"
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "Bash", `{"command":"export K=`+leaked+`"}`, "ok", false)...)
	lines = append(lines, claudeToolLines("t2", "Bash", `{"command":"run-command"}`, "stderr: "+leaked, true)...)

	events, _ := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), leaked) {
		t.Errorf("a secret from a tool argument survived into the summary:\n%s", encoded)
	}
}

func TestPathsOutsideTheWorkspaceKeepTheirAbsoluteForm(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "Read", `{"file_path":"/w/inside.go"}`, "ok", false)...)
	lines = append(lines, claudeToolLines("t2", "Read", `{"file_path":"/elsewhere/outside.go"}`, "ok", false)...)

	events, _ := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	summary := joinedSummary(t, events)

	if !strings.Contains(summary, "inside.go") || strings.Contains(summary, "/w/inside.go") {
		t.Errorf("a file in the workspace should be shown relative:\n%s", summary)
	}
	if !strings.Contains(summary, "/elsewhere/outside.go") {
		t.Errorf("a file outside the workspace should keep its absolute path — that "+
			"it is elsewhere is the interesting part:\n%s", summary)
	}
}

func TestSummaryEventsComeLastSoTheySitNearestTheNewRequest(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "Read", `{"file_path":"/w/a.go"}`, "ok", false)...)

	events, _ := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if len(events) < 2 {
		t.Fatal("expected conversation events plus a summary")
	}
	if !NoteEventIsSummary(events[len(events)-1].Payload) {
		t.Errorf("last event is %s, want the summary last so it survives the "+
			"80-event window and reads as a footer", events[len(events)-1].Type)
	}
	// And the conversation itself still comes first.
	if events[0].Type != sessions.EventMessage {
		t.Errorf("first event is %s, want the original conversation to lead", events[0].Type)
	}
}

// TestASuccessfulWriteSurvivesALaterFailedEditOfTheSamePath covers the
// coarse-keying fix: the old code withdrew a failed call's claim BY VALUE, so a
// failed Edit erased the record of an earlier successful Write to the same path
// and the summary reported no files changed although the file was rewritten.
func TestASuccessfulWriteSurvivesALaterFailedEditOfTheSamePath(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	lines = append(lines, claudeToolLines("t1", "Write", `{"file_path":"/w/config.yaml"}`, "wrote 40 lines", false)...)
	lines = append(lines, claudeToolLines("t2", "Edit", `{"file_path":"/w/config.yaml"}`, "string not found", true)...)

	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	summary := joinedSummary(t, events)
	if !strings.Contains(summary, "Files changed: config.yaml") {
		t.Errorf("the successful Write was erased by the later failed Edit of the same path:\n%s", summary)
	}
}

// TestAnInterruptedWriteWithNoResultDoesNotClaimTheFile covers the
// no-result fix: a tool call whose result never arrives (the session stopped
// mid-call) must not report its file as changed — we never learned it ran.
func TestAnInterruptedWriteWithNoResultDoesNotClaimTheFile(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	// The tool_use with no matching tool_result — the transcript ends here.
	lines = append(lines, claudeToolLines("t1", "Write", `{"file_path":"/w/config.yaml"}`, "", false)[0])

	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range summaryTexts(t, events) {
		if strings.HasPrefix(line, "Files changed") && strings.Contains(line, "config.yaml") {
			t.Errorf("an interrupted write with no result is claimed as a file changed:\n%s", line)
		}
	}
}

// THE BUDGET HAS TO BIND ON THE ASSEMBLED LINE. The previous implementation
// capped the tool breakdown and then prepended the call/failure counts, so the
// headline left the budget by exactly the length of that prefix. The existing
// budget test
// never caught it because its tools all carry a recognised "file_path", which
// routes them to the file buckets and leaves the breakdown empty — the overflow
// only appears once the arguments are a schema this package does not know, which
// is the fallback path the breakdown exists for.
func TestTheActivityHeadlineIsTruncatedAfterItIsAssembled(t *testing.T) {
	lines := []string{`{"type":"user","cwd":"/w","message":{"role":"user","content":"go"}}`}
	for i := 0; i < 60; i++ {
		lines = append(lines, claudeToolLines(
			"t"+itoa(i), "unrecognised_tool_"+itoa(i), `{"unknown_argument_name":"x"}`, "ok", false)...)
	}
	events, err := translateFamily1At("", writeTranscript(t, lines...), ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	summaries := summaryTexts(t, events)
	if len(summaries) == 0 {
		t.Fatal("no summary events produced")
	}

	headline := summaries[0]
	// The prefix has to be there, or the breakdown alone would satisfy the budget
	// for the wrong reason — that was the passing state before the fix.
	if !strings.HasPrefix(headline, "Prior session activity: 60 tool calls.") {
		t.Fatalf("the headline is not the count-prefixed line this test is about:\n%s", headline)
	}
	if !strings.Contains(headline, "Also: unrecognised_tool_") {
		t.Fatalf("the breakdown never ran, so nothing could overflow:\n%s", headline)
	}
	if length := len(headline); length != maxSummaryEventBytes {
		t.Errorf("headline is %d bytes, want it cut to exactly the %d budget:\n%s",
			length, maxSummaryEventBytes, headline)
	}
	// Cut, not merely short: the ellipsis is what tells a reader the list goes on.
	if !strings.HasSuffix(headline, "…") {
		t.Errorf("an over-long headline was truncated without saying so:\n%s", headline)
	}
}

func TestActivityItemsRedactCompleteValuesBeforeShortening(t *testing.T) {
	secret := "ghp_" + strings.Repeat("Q", 36)
	for _, presented := range []string{
		secret,
		"ghp_ " + strings.Repeat("Q", 36),
	} {
		log := newActivityLog("/workspace")
		log.observeCall("call", "bash", `{"command":"`+strings.Repeat("x", 105)+` `+presented+`"}`)
		events := log.summaryEvents()
		joined := ""
		for _, event := range events {
			joined += str(t, event, "content") + "\n"
		}
		if strings.Contains(joined, secret) || !strings.Contains(joined, "[REDACTED]") {
			t.Fatalf("activity summary exposed a secret after shortening:\n%s", joined)
		}
	}
}
