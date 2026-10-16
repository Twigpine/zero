package sessions

import (
	"fmt"
	"strings"
	"testing"
)

// A CALL'S PAYLOAD MUST NOT RIDE INTO A LATER PROMPT ANY MORE THAN A RESULT'S.
//
// toolResultOutcome drops result bodies, and calls were left verbatim, so an
// interrupted write_file put its content into the next turn's prompt while the
// matching result body did not. Same secret, one door left open. The identity
// still has to survive, since a resumed turn knowing WHICH file was written is
// the whole point of admitting calls at all.
//
// Pinned as whole lines, compared as fields, for the same reason the result test
// is: a substring search for one fixture secret passes when a truncated or
// reworded copy leaks, and the renderer walks a map so field order varies.
func TestResumePromptCarriesToolCallIdentityWithoutPayload(t *testing.T) {
	const secret = "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG"
	const token = "Bearer sk-live-4eC39HqLyjWDarjtT1zdp7dc"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "rotate the deploy key"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "write_file", "arguments": `{"path":"deploy/prod.env","content":"` + secret + `"}`})},
		{Sequence: 3, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "write_file", "status": "ok", "output": "written"})},
		{Sequence: 4, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c2", "name": "edit_file", "arguments": `{"path":"deploy/prod.env","old_string":"` + secret + `","new_string":"rotated"}`})},
		{Sequence: 5, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "edit_file", "status": "ok"})},
		{Sequence: 6, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c3", "name": "exec_command", "arguments": `{"cmd":"curl -H '` + token + `' https://api.example/rotate","workdir":"deploy"}`})},
		{Sequence: 7, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "exec_command", "status": "ok"})},
		{Sequence: 8, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c4", "name": "apply_patch", "arguments": `{"patch":"*** Begin Patch\n*** Update File: deploy/prod.env\n-` + secret + `\n+rotated\n*** End Patch"}`})},
		{Sequence: 9, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "apply_patch", "status": "ok"})},
		{Sequence: 10, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c5", "name": "read_file", "arguments": `{"path":"deploy/prod.env","offset":1,"limit":40}`})},
		{Sequence: 11, Type: EventToolResult, Payload: toolContextPayload(t, map[string]any{"name": "read_file", "status": "ok", "output": secret})},
		{Sequence: 12, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error: upstream timeout"})},
	}
	out := resumePrompt(t, events)

	// The payloads: none of them, in any form.
	for _, leaked := range []string{secret, secret[:16], token, token[:14], "rotated", "Begin Patch", "curl -H"} {
		if strings.Contains(out, leaked) {
			t.Errorf("tool call payload %q reached the resume prompt:\n%s", leaked, out)
		}
	}

	// The identities: every one of them, from the call side.
	want := map[int]struct {
		kind   string
		fields []string
	}{
		2:  {"tool_call", []string{"c1", "write_file", `{"path":"deploy/prod.env"}`}},
		4:  {"tool_call", []string{"c2", "edit_file", `{"path":"deploy/prod.env"}`}},
		6:  {"tool_call", []string{"c3", "exec_command", `{"workdir":"deploy"}`}},
		8:  {"tool_call", []string{"c4", "apply_patch"}},
		10: {"tool_call", []string{"c5", "read_file"}},
	}
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "- #") || !strings.Contains(line, "tool_call:") {
			continue
		}
		var sequence int
		var kind string
		rest, _ := strings.CutPrefix(line, "- #")
		if _, err := fmt.Sscanf(rest, "%d %s", &sequence, &kind); err != nil {
			t.Fatalf("unparsable context line %q: %v", line, err)
		}
		expected, wanted := want[sequence]
		if !wanted {
			t.Errorf("unexpected tool_call line %q", line)
			continue
		}
		seen[sequence] = true
		_, payload, _ := strings.Cut(rest, ": ")
		fields := strings.Fields(payload)
		for _, field := range expected.fields {
			if !containsField(fields, field) {
				t.Errorf("line #%d lost identity field %q: %q", sequence, field, line)
			}
		}
	}
	for sequence := range want {
		if !seen[sequence] {
			t.Errorf("tool_call #%d did not reach the resume prompt at all, so the interrupted work is invisible again", sequence)
		}
	}
	// read_file keeps its window too, or a resumed turn re-reads a part it had.
	if !strings.Contains(out, `"offset":1`) || !strings.Contains(out, `"limit":40`) {
		t.Errorf("read_file lost its offset/limit window:\n%s", out)
	}
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	// The reduced arguments object may carry more than one identity key, in map
	// order, so a single-key expectation is also satisfied by an object that
	// contains that key.
	if strings.HasPrefix(want, "{") {
		key := strings.TrimSuffix(strings.TrimPrefix(want, "{"), "}")
		for _, field := range fields {
			if strings.HasPrefix(field, "{") && strings.Contains(field, key) {
				return true
			}
		}
	}
	return false
}

// ARGUMENTS THAT CANNOT BE READ ARE NOT PASSED THROUGH AS TEXT.
//
// An allow-list only protects what it can see. Arguments that do not decode as
// an object are dropped rather than rendered, since text this file could not
// inspect is text it cannot vouch for.
func TestResumePromptDropsUnreadableToolCallArguments(t *testing.T) {
	const secret = "sk-live-unparseable-9f8e7d6c"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "bash", "arguments": `not json at all ` + secret})},
		{Sequence: 3, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)
	if strings.Contains(out, secret) {
		t.Errorf("unparseable arguments were rendered as text:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("the call's identity was lost along with its unreadable arguments:\n%s", out)
	}
}

// AND A TOOL THIS FILE HAS NEVER HEARD OF STILL KEEPS ITS PATH.
//
// The allow-list is by key, not by tool name, so an MCP tool or a future core
// tool whose argument is a path or a url carries it into the resume prompt
// without being enumerated here, while any body field it has is dropped.
func TestResumePromptKeepsIdentityForUnknownTools(t *testing.T) {
	const body = "large opaque payload that must not be replayed"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "mcp_uploader", "arguments": `{"url":"https://files.example/x","blob":"` + body + `"}`})},
		{Sequence: 3, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)
	if strings.Contains(out, body) {
		t.Errorf("an unlisted body field was replayed:\n%s", out)
	}
	if !strings.Contains(out, "https://files.example/x") {
		t.Errorf("an unlisted tool lost its url identity:\n%s", out)
	}
}

// AND AN ALLOW-LISTED KEY IS NOT A SAFE VALUE.
//
// web_fetch accepts a credential in the query string and redacts the URL it
// reports back, so an interrupted fetch had its token dropped from the result
// and kept verbatim in the call. This projection is what admits the call into a
// later turn's prompt, so the token rode into the next resume or fork. The
// identity the resumed turn actually needs is the host and path, and those
// survive.
func TestResumePromptRedactsCredentialsInsideRetainedValues(t *testing.T) {
	const token = "secret-value-9f8e7d6c5b4a"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "check the feed"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "web_fetch", "arguments": `{"url":"https://api.example/data?access_token=` + token + `&page=2"}`})},
		{Sequence: 3, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c2", "name": "web_fetch", "arguments": `{"url":"https://reader:hunter2@api.example/feed"}`})},
		{Sequence: 4, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error: upstream timeout"})},
	}
	out := resumePrompt(t, events)

	for _, leaked := range []string{token, token[:12], "hunter2"} {
		if strings.Contains(out, leaked) {
			t.Errorf("credential %q reached the resume prompt:\n%s", leaked, out)
		}
	}
	// The non-secret identity is the whole reason calls are admitted at all.
	for _, kept := range []string{"api.example", "/data", "page=2", "/feed"} {
		if !strings.Contains(out, kept) {
			t.Errorf("URL identity %q was lost to the scrub:\n%s", kept, out)
		}
	}
}

// The scrub must not eat ordinary identities. A path, a glob and a query are
// what the resumed turn navigates by, and none of them is a credential.
func TestResumePromptKeepsOrdinaryIdentitiesThroughTheScrub(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c1", "name": "read_file", "arguments": `{"path":"internal/sessions/exec_session.go","offset":1,"limit":40}`})},
		{Sequence: 3, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{"id": "c2", "name": "grep", "arguments": `{"pattern":"func toolCallIdentity","glob":"src/**/*.tsx"}`})},
		{Sequence: 4, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)
	for _, kept := range []string{"internal/sessions/exec_session.go", "func toolCallIdentity", "src/**/*.tsx"} {
		if !strings.Contains(out, kept) {
			t.Errorf("identity %q was lost to the scrub:\n%s", kept, out)
		}
	}
	// Numbers are not strings and are nothing to scrub; they must survive whole.
	if !strings.Contains(out, "40") {
		t.Errorf("a numeric read window did not survive the scrub:\n%s", out)
	}
}

// AND A FIELD THIS FILE HAS NEVER HEARD OF DOES NOT RIDE ALONG.
//
// toolResultOutcome builds its output from the fields it keeps, so an unknown
// one cannot reach a prompt through it. The call side edited arguments in place
// and returned every sibling key, which is the opposite contract: a producer
// recording one more top-level field, or the same payload with no arguments at
// all, put that field straight into the next turn. The projection names what
// survives, so both halves now drop what they do not name.
func TestResumePromptProjectsTopLevelToolCallFields(t *testing.T) {
	const body = "opaque top-level payload that must not be replayed"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c1", "name": "write_file", "arguments": `{"path":"deploy/prod.env"}`, "rawInput": body,
		})},
		// The same shape with no arguments at all, which used to return the whole
		// payload untouched.
		{Sequence: 3, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c2", "name": "read_file", "stashedContent": body,
		})},
		{Sequence: 4, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)

	for _, leaked := range []string{body, "rawInput", "stashedContent"} {
		if strings.Contains(out, leaked) {
			t.Errorf("an unnamed top-level field %q reached the resume prompt:\n%s", leaked, out)
		}
	}
	// The identity itself still survives, from both calls.
	for _, kept := range []string{"c1", "write_file", "deploy/prod.env", "c2", "read_file"} {
		if !strings.Contains(out, kept) {
			t.Errorf("call identity %q was lost to the projection:\n%s", kept, out)
		}
	}
}

// AND A PERMITTED KEY IS NOT PERMISSION FOR WHATEVER HANGS UNDER IT.
//
// The projection scrubbed strings and passed everything else through, which
// reads as "a number is nothing to scrub" and is true of a number and not of an
// object. MCP schemas allow object and array properties, so a whole payload can
// arrive under a permitted key. Redacting a serialized object would not help:
// ordinary private text has no credential shape to match.
func TestResumePromptDropsNestedArgumentContainers(t *testing.T) {
	const body = "private body text with no credential shape at all"
	const token = "secret-value-9f8e7d6c5b4a"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c1", "name": "mcp_search",
			"arguments": `{"query":{"api_key":"` + token + `","content":"` + body + `"}}`,
		})},
		{Sequence: 3, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c2", "name": "web_fetch",
			"arguments": `{"url":["https://api.example/data?access_token=` + token + `"]}`,
		})},
		{Sequence: 4, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)

	for _, leaked := range []string{body, body[:20], token, token[:12], "api_key"} {
		if strings.Contains(out, leaked) {
			t.Errorf("nested argument content %q reached the resume prompt:\n%s", leaked, out)
		}
	}
	// The call still says which tool ran, so a resumed turn can ask again.
	for _, kept := range []string{"c1", "mcp_search", "c2", "web_fetch"} {
		if !strings.Contains(out, kept) {
			t.Errorf("call identity %q was lost along with the container:\n%s", kept, out)
		}
	}
}

// THE SEARCH ALIASES ARE TOOL-SPECIFIC, AND ONE OF THEM IS A TRAP.
//
// grep accepts `search` for its pattern and edit_file accepts the same word for
// the text being replaced. Admitting it globally for the sake of the first would
// replay file contents through the second.
func TestResumePromptKeepsGrepSearchAndDropsEditSearch(t *testing.T) {
	const oldText = "OLD FILE CONTENTS THAT MUST NOT BE REPLAYED"
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c1", "name": "grep", "arguments": `{"search":"func toolCallIdentity","path":"internal/sessions"}`,
		})},
		{Sequence: 3, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c2", "name": "edit_file", "arguments": `{"path":"a.go","search":"` + oldText + `","replace":"new"}`,
		})},
		{Sequence: 4, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)

	if !strings.Contains(out, "func toolCallIdentity") {
		t.Errorf("grep lost the pattern it was searching for:\n%s", out)
	}
	if strings.Contains(out, oldText) || strings.Contains(out, oldText[:20]) {
		t.Errorf("edit_file replayed the text it was replacing:\n%s", out)
	}
	// Both calls keep the path, which is identity for either tool.
	for _, kept := range []string{"internal/sessions", "a.go"} {
		if !strings.Contains(out, kept) {
			t.Errorf("path identity %q was lost:\n%s", kept, out)
		}
	}
}

// AND THE SUPPORTED SEARCH AND WINDOW FORMS SURVIVE, not only the canonical
// ones. A resumed turn that knows the path but not which slice was asked for
// has half the identity and will read again.
func TestResumePromptKeepsSupportedAliasesAndWindows(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventMessage, Payload: toolContextPayload(t, map[string]any{"role": "user", "content": "go"})},
		{Sequence: 2, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c1", "name": "glob", "arguments": `{"match":"src/**/*.go"}`,
		})},
		{Sequence: 3, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c2", "name": "read_file", "arguments": `{"path":"large.go","byte_offset":4096,"byte_limit":512}`,
		})},
		{Sequence: 4, Type: EventToolCall, Payload: toolContextPayload(t, map[string]any{
			"id": "c3", "name": "read_file", "arguments": `{"path":"big.go","start_line":40,"end_line":80,"max_lines":41}`,
		})},
		{Sequence: 5, Type: EventError, Payload: toolContextPayload(t, map[string]any{"message": "provider error"})},
	}
	out := resumePrompt(t, events)

	// KEY AND VALUE TOGETHER, NOT THE NUMBER ALONE. Searching for "40" finds
	// it inside "4096", so the start_line assertion passed whether or not
	// start_line survived at all, which is the opposite of what it claims.
	for _, kept := range []string{
		"src/**/*.go",
		`"byte_offset":4096`, `"byte_limit":512`,
		`"start_line":40`, `"end_line":80`, `"max_lines":41`,
	} {
		if !strings.Contains(out, kept) {
			t.Errorf("supported identity or window %s was lost:\n%s", kept, out)
		}
	}
}

// THE SHARED TABLE MUST NEVER NAME A MUTATING TOOL'S BODY ARGUMENT.
//
// This is the rule the grep/edit_file collision taught, kept as a test so the
// next alias added for a search tool cannot quietly open the same door. The
// aliases below are the ones the mutating tools accept for the content they
// write; they belong in the per-tool table or nowhere.
func TestIdentityKeysNeverNameAMutatingToolBodyArgument(t *testing.T) {
	bodyAliases := map[string][]string{
		"edit_file":   {"old_string", "old", "search", "find", "old_str", "new_string", "new", "replace", "replacement", "new_str"},
		"write_file":  {"content", "contents", "text", "body", "data", "file_content"},
		"apply_patch": {"patch"},
	}
	for tool, aliases := range bodyAliases {
		for _, alias := range aliases {
			if toolCallIdentityKeys[alias] {
				t.Errorf("%s accepts %q for content it writes, and the shared identity table admits it: a resumed prompt would replay the body", tool, alias)
			}
		}
	}
	// The per-tool table is where an ambiguous name belongs, and grep's use of
	// `search` is the case that proves the mechanism is wired.
	if !toolCallIdentityKeysByTool["grep"]["search"] {
		t.Error("grep lost its search alias, so an interrupted grep resumes without the pattern")
	}
}

// THROUGH THE STORE, NOT JUST THE RENDERER.
//
// Every test above hands FormatExecPrompt an events slice it built in memory,
// so none of them shows the projection still applies to events that were
// written, reloaded and prepared by the real resume and fork paths. That is
// where it has to hold: the leak this guards against is a later turn reading a
// recorded call, and the recording is what makes it later.
//
// Includes a call the tool would have REJECTED. The agent records OnToolCall
// before execution, so a read_file whose path is an object is persisted even
// though the tool refuses it, and a projection that only considered
// well-formed calls would carry it through.
func TestPersistedToolCallsAreProjectedOnResumeAndFork(t *testing.T) {
	const body = "private body text with no credential shape at all"
	const token = "secret-value-9f8e7d6c5b4a"
	const oldText = "OLD FILE CONTENTS THAT MUST NOT BE REPLAYED"

	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(CreateInput{SessionID: "projection_persisted"})
	if err != nil {
		t.Fatal(err)
	}
	appends := []AppendEventInput{
		{Type: EventMessage, Payload: map[string]any{"role": "user", "content": "go"}},
		{Type: EventToolCall, Payload: map[string]any{
			"id": "c1", "name": "mcp_search",
			"arguments": `{"query":{"api_key":"` + token + `","content":"` + body + `"}}`,
		}},
		{Type: EventToolCall, Payload: map[string]any{
			"id": "c2", "name": "edit_file",
			"arguments": `{"path":"a.go","search":"` + oldText + `","replace":"new"}`,
		}},
		// Rejected by read_file, recorded anyway.
		{Type: EventToolCall, Payload: map[string]any{
			"id": "c3", "name": "read_file",
			"arguments": `{"path":{"nested":"` + body + `"},"start_line":40}`,
		}},
		{Type: EventToolCall, Payload: map[string]any{
			"id": "c4", "name": "grep",
			"arguments": `{"search":"func toolCallIdentity","path":"internal/sessions"}`,
		}},
		{Type: EventError, Payload: map[string]any{"message": "provider error: upstream timeout"}},
	}
	if _, err := store.AppendEvents(session.SessionID, appends); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []struct {
		name    string
		options PrepareExecOptions
	}{
		{"resume", PrepareExecOptions{Store: store, Resume: session.SessionID}},
		{"fork", PrepareExecOptions{Store: store, Fork: session.SessionID, SessionID: "projection_persisted_fork"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			prepared, err := PrepareExec(mode.options)
			if err != nil {
				t.Fatalf("PrepareExec: %v", err)
			}
			prompt := FormatExecPrompt("continue", prepared)

			for _, leaked := range []string{body, body[:20], token, token[:12], oldText, oldText[:20], "api_key"} {
				if strings.Contains(prompt, leaked) {
					t.Errorf("%q survived persistence into the %s prompt:\n%s", leaked, mode.name, prompt)
				}
			}
			// The identities that make the tail worth carrying at all.
			for _, kept := range []string{"mcp_search", "edit_file", "read_file", "grep", "func toolCallIdentity", "internal/sessions", "a.go"} {
				if !strings.Contains(prompt, kept) {
					t.Errorf("identity %q was lost from the %s prompt:\n%s", kept, mode.name, prompt)
				}
			}
		})
	}

	// AND THE STORE STILL HOLDS THE ORIGINALS. The projection is a view for the
	// prompt, not an edit to the record: /rewind and an audit read the events.
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	stored := ""
	for _, event := range events {
		stored += string(event.Payload)
	}
	for _, want := range []string{body, token, oldText} {
		if !strings.Contains(stored, want) {
			t.Errorf("the projection modified the persisted events: %q is no longer in the store", want)
		}
	}
}
