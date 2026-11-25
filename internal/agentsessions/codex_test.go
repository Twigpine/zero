package agentsessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCodexStore(t *testing.T, lines ...string) (Env, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "sessions", "2026", "08", "01",
		"rollout-2026-08-01T10-00-00-019f73d7-e215-7ce0-ab38-d9e6db354717.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return testEnv(home, nil), path
}

// TestATurnContextDoesNotBreakOnAFieldItSharesByNameOnly is a regression test
// for a defect the real corpus exposed.
//
// "summary" is an array of blocks on a reasoning item and the bare string "auto"
// on turn_context. Typing it as []codexBlock made encoding/json reject the whole
// turn_context record, so the model id vanished from every Codex session — with
// no error at any layer, because a record that fails to decode is simply skipped.
//
// Nothing about the model id itself is special here; the point is that ONE
// mistyped field in a union struct silently discards every other field on that
// record.
func TestATurnContextDoesNotBreakOnAFieldItSharesByNameOnly(t *testing.T) {
	env, _ := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"019f73d7-e215-7ce0-ab38-d9e6db354717","cwd":"/Users/someone/proj"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-5.6-sol","effort":"high","summary":"auto"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the parser"}]}}`,
	)

	found, err := Codex(env).Discover("")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d sessions, want 1", len(found))
	}
	if found[0].ModelID != "gpt-5.6-sol" {
		t.Errorf("ModelID = %q, want gpt-5.6-sol — a string-valued \"summary\" on "+
			"turn_context must not discard the rest of the record", found[0].ModelID)
	}
	if found[0].Cwd != "/Users/someone/proj" {
		t.Errorf("Cwd = %q", found[0].Cwd)
	}
	if found[0].ID != "019f73d7-e215-7ce0-ab38-d9e6db354717" {
		t.Errorf("ID = %q, want the uuid from the filename tail", found[0].ID)
	}
}

// TestCodexHarnessChatterIsNotTheConversation covers the two ways Codex speaks
// to itself through channels that look like conversation: "developer" messages
// carrying its system prompt, and <environment_context> injected as a user turn.
// Neither is the user's work, and both would otherwise dominate a title and the
// imported transcript.
func TestCodexHarnessChatterIsNotTheConversation(t *testing.T) {
	env, path := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"s","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex. Follow the permissions policy."}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"install hermes on this pc"}]}}`,
	)

	found, _ := Codex(env).Discover("")
	if len(found) != 1 {
		t.Fatalf("got %d sessions, want 1", len(found))
	}
	if found[0].Title != "install hermes on this pc" {
		t.Errorf("Title = %q, want the first real human turn", found[0].Title)
	}

	events, err := translateCodexAt("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want only the human turn: %+v", len(events), events)
	}
	encoded, _ := json.Marshal(events)
	for _, unwanted := range []string{"You are Codex", "environment_context"} {
		if strings.Contains(string(encoded), unwanted) {
			t.Errorf("harness chatter %q was imported as conversation", unwanted)
		}
	}
}

func TestCodexToolCallsPairUpAcrossBothCallShapes(t *testing.T) {
	_, path := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"s","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"wait","call_id":"call_1","arguments":"{\"ms\":10}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"done"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_2","input":"ls -la"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"[{\"type\":\"input_text\",\"text\":\"a.go\"}]"}}`,
	)
	all, err := translateCodexAt("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events := conversationEvents(all)
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	// Both call shapes must carry their name through to their result.
	for _, pair := range [][2]int{{0, 1}, {2, 3}} {
		call, result := events[pair[0]], events[pair[1]]
		if str(t, call, "toolCallId") != str(t, result, "toolCallId") {
			t.Errorf("call/result ids differ: %q vs %q", str(t, call, "toolCallId"), str(t, result, "toolCallId"))
		}
		if str(t, call, "name") != str(t, result, "name") {
			t.Errorf("result name %q does not match its call %q", str(t, result, "name"), str(t, call, "name"))
		}
		if status := str(t, result, "status"); status != "unknown" {
			t.Errorf("Codex result status = %q, want unknown without structured outcome evidence", status)
		}
	}
	// An output stored as an escaped JSON array must render as its text, not as
	// raw JSON the reader has to decode by eye.
	if got := str(t, events[3], "output"); got != "a.go" {
		t.Errorf("escaped-array output = %q, want the flattened text", got)
	}
}

func TestCodexUnknownToolOutcomePreservesOutputWithoutClaimingAFileChange(t *testing.T) {
	_, path := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"s","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"write_file","call_id":"call_1","arguments":"{\"path\":\"/w/config.go\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"permission denied"}}`,
	)
	events, err := translateCodexAt("", path, ReadOptions{Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	conversation := conversationEvents(events)
	if len(conversation) != 2 {
		t.Fatalf("conversation events = %d, want call/result: %+v", len(conversation), conversation)
	}
	result := conversation[1]
	if got := str(t, result, "status"); got != "unknown" {
		t.Fatalf("result status = %q, want unknown", got)
	}
	if got := str(t, result, "output"); got != "permission denied" {
		t.Fatalf("raw result output = %q", got)
	}
	for _, summary := range summaryTexts(t, events) {
		if strings.Contains(summary, "Files changed") || strings.Contains(summary, "config.go") {
			t.Fatalf("unverified Codex output created a factual file claim: %q", summary)
		}
	}
}

func TestCodexByteTailDropsOrphanResultButKeepsLaterPairAndDisclosure(t *testing.T) {
	const testByteLimit = 4 << 10
	padding := strings.Repeat("x", testByteLimit)
	_, path := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"s","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"old_call","call_id":"old","arguments":"{}"}}`,
		`{"type":"padding","payload":"`+padding+`"}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"old","output":"orphaned output"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"new_call","call_id":"new","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"new","output":"kept output"}}`,
	)
	events, err := translateCodexAtWithByteLimit("", path, ReadOptions{}, testByteLimit)
	if err != nil {
		t.Fatal(err)
	}
	conversation := conversationEvents(events)
	if len(conversation) != 2 {
		t.Fatalf("byte-tail conversation = %d events, want only later pair: %+v", len(conversation), conversation)
	}
	if str(t, conversation[0], "name") != "new_call" ||
		str(t, conversation[1], "name") != "new_call" ||
		str(t, conversation[0], "toolCallId") != str(t, conversation[1], "toolCallId") {
		t.Fatalf("later valid pair was not retained: %+v", conversation)
	}
	for _, event := range conversation {
		if str(t, event, "name") == "old_call" || str(t, event, "output") == "orphaned output" {
			t.Fatalf("orphan result survived byte-tail normalization: %+v", event)
		}
	}
	joined := strings.Join(summaryTexts(t, events), "\n")
	if !strings.Contains(joined, "Older transcript records were not imported") {
		t.Fatalf("byte-tail loss was not disclosed: %q", joined)
	}
}

func TestCodexCapCannotLetActivitySummaryEvictSourceTail(t *testing.T) {
	_, path := writeCodexStore(t,
		`{"type":"session_meta","timestamp":"2026-08-01T10:00:00.000Z","payload":{"session_id":"s","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"read_file","call_id":"call_1","arguments":"{\"path\":\"parser.go\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"package parser"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final codex answer"}]}}`,
	)
	events, err := translateCodexAt("", path, ReadOptions{MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d capped events, want 2", len(events))
	}
	if note := str(t, events[0], "content"); !strings.Contains(note, "not imported") {
		t.Fatalf("source truncation was silent: got %q", note)
	}
	if got := str(t, events[1], "content"); got != "final codex answer" {
		t.Fatalf("source tail was evicted by generated context: got %q", got)
	}
}

func TestCodexDiscoveryIsFixedDepth(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions")
	good := filepath.Join(root, "2026", "08", "01", "rollout-a-019f73d7-e215-7ce0-ab38-d9e6db354717.jsonl")
	writeFile(t, good, `{"type":"session_meta","payload":{"session_id":"s","cwd":"/w"}}`+"\n")

	// A live OPENAI_API_KEY lives at ~/.codex/auth.json, one level above the
	// sessions root, plus decoy transcripts a walk would reach.
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), `{"OPENAI_API_KEY":"sk-MUST_NEVER_BE_READ"}`)
	for _, stray := range []string{
		filepath.Join(root, "stray.jsonl"),
		filepath.Join(root, "2026", "deep.jsonl"),
		filepath.Join(root, "2026", "08", "01", "nested", "deeper.jsonl"),
		filepath.Join(home, ".codex", "leaked.jsonl"),
	} {
		writeFile(t, stray, `{"type":"session_meta","payload":{"session_id":"x","cwd":"/w"}}`+"\n")
	}

	paths := Codex(testEnv(home, nil)).(codex).transcripts()
	if len(paths) != 1 || paths[0] != good {
		t.Fatalf("transcripts = %v, want exactly [%s]", paths, good)
	}
}

func TestTheRealCodexCorpusStillParses(t *testing.T) {
	if os.Getenv("ZERO_TEST_LIVE_AGENT_SESSIONS") != "1" {
		t.Skip("set ZERO_TEST_LIVE_AGENT_SESSIONS=1 to inspect the local Codex store")
	}
	env := OSEnv()
	root := codexRoot(env)
	if root == "" {
		t.Skip("no home directory")
	}
	if _, err := os.Stat(root); err != nil {
		t.Skip("no Codex store on this machine")
	}
	adapter := Codex(env)
	found, err := adapter.Discover("")
	if err != nil {
		t.Fatal(err)
	}
	total := len(adapter.(codex).transcripts())
	if total == 0 {
		t.Skip("store exists but holds no rollouts")
	}
	titled, modelled := 0, 0
	for _, session := range found {
		if session.Title != "" && session.Title != "untitled" && !isCodexContextInjection(session.Title) {
			titled++
		}
		if session.ModelID != "" {
			modelled++
		}
	}
	t.Logf("indexed %d of %d rollouts; %d titled, %d with a model", len(found), total, titled, modelled)
	if len(found) == 0 {
		t.Fatal("no Codex sessions indexed from a non-empty store")
	}
	// REPORTED, NOT ASSERTED. What these counts measure is the shape of the store
	// on the machine running them, which is not a property of this package: the
	// same code reported "44 of 44 rollouts, 43 with a model" on one machine and
	// "2 of 2, 0 with a model" on a reviewer's. Failing on the second is a red
	// build for the contributor and a green one for CI, which is backwards — CI
	// has no store and skips, so the assertion never ran where a regression would
	// actually land.
	//
	// The shapes behind both numbers are pinned by construction in
	// fixture_corpus_test.go, which runs everywhere and cannot skip. What this
	// test is still good for is noticing a shape the fixtures do not cover, so it
	// prints its counts and leaves the judgement to the reader.
	if titled == 0 {
		t.Logf("no session in the live store got a real title; if that is unexpected here, "+
			"the context-injection filter may have drifted (%d rollouts)", total)
	}
	if modelled == 0 {
		t.Logf("no session in the live store carries a model: every rollout here has its "+
			"turn_context outside the head budget. TestARolloutWithALateTurnContextIndexesWithoutAModel "+
			"pins that shape deterministically (%d rollouts)", total)
	}
}
