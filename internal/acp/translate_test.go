package acp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
)

func browserDescriptor(t *testing.T, update ToolCallUpdate) BrowserToolDetails {
	t.Helper()
	raw, ok := update.Meta[zeroBrowserMetaKey]
	if !ok {
		t.Fatalf("browser metadata = %#v, want %q", update.Meta, zeroBrowserMetaKey)
	}
	var details BrowserToolDetails
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatalf("decode browser metadata: %v", err)
	}
	return details
}

func TestAgentMessageAndThoughtChunks(t *testing.T) {
	m := agentMessageChunk("hello")
	if m.SessionUpdate != UpdateAgentMessageChunk || m.Content.Type != "text" || m.Content.Text != "hello" {
		t.Fatalf("unexpected message chunk: %+v", m)
	}
	th := agentThoughtChunk("thinking")
	if th.SessionUpdate != UpdateAgentThoughtChunk || th.Content.Text != "thinking" {
		t.Fatalf("unexpected thought chunk: %+v", th)
	}
}

func TestToolKindFor(t *testing.T) {
	cases := map[string]string{
		"read_file":      ToolKindRead,
		"list_directory": ToolKindRead,
		"grep":           ToolKindSearch,
		"glob":           ToolKindSearch,
		"edit_file":      ToolKindEdit,
		"apply_patch":    ToolKindEdit,
		"bash":           ToolKindExecute,
		"exec_command":   ToolKindExecute,
		"web_fetch":      ToolKindFetch,
		"update_plan":    ToolKindThink,
		"some_mcp_tool":  ToolKindOther,
	}
	for name, want := range cases {
		if got := toolKindFor(name); got != want {
			t.Errorf("toolKindFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestToolTitleAndHint(t *testing.T) {
	if got := toolTitle("read_file", `{"path":"src/main.go"}`); got != "read_file src/main.go" {
		t.Errorf("title = %q", got)
	}
	if got := toolTitle("bash", `{"command":"go test ./..."}`); got != "bash go test ./..." {
		t.Errorf("title = %q", got)
	}
	if got := toolTitle("mystery", `not json`); got != "mystery" {
		t.Errorf("malformed args should yield bare name, got %q", got)
	}
	if got := toolTitle("noargs", ``); got != "noargs" {
		t.Errorf("empty args should yield bare name, got %q", got)
	}
}

func TestBrowserToolUpdatesAreStructuredAndPresentationSafe(t *testing.T) {
	start := toolCallStart(agent.ToolCall{
		ID:        "browser-1",
		Name:      "browser_open",
		Arguments: `{"url":"https://example.com/settings?token=not-for-a-title#account"}`,
	})
	if got := browserDescriptor(t, start); got != (BrowserToolDetails{Version: 1, Command: "open"}) {
		t.Fatalf("browser descriptor = %#v, want open", got)
	}
	if start.Title != "browser open https://example.com" {
		t.Fatalf("browser title = %q", start.Title)
	}
	if strings.Contains(start.Title, "token=") || strings.Contains(start.Title, "#account") {
		t.Fatalf("browser title leaked URL-sensitive data: %q", start.Title)
	}
	encoded, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire.Meta[zeroBrowserMetaKey]; !ok {
		t.Fatalf("browser wire metadata = %#v", wire.Meta)
	}

	typed := toolCallStart(agent.ToolCall{
		ID:        "browser-2",
		Name:      "browser_type",
		Arguments: `{"ref":"email","text":"secret@example.test"}`,
	})
	if got := browserDescriptor(t, typed); got.Command != "type" {
		t.Fatalf("browser type descriptor = %#v", got)
	}
	if typed.Title != "browser type" || strings.Contains(typed.Title, "secret@example.test") {
		t.Fatalf("browser type title = %q", typed.Title)
	}

	action := toolCallStart(agent.ToolCall{
		ID:        "browser-3",
		Name:      "browser_action",
		Arguments: `{"command":"keyboard_insert_text","args":["secret@example.test"]}`,
	})
	if action.Title != "browser action keyboard_insert_text" {
		t.Fatalf("browser action title = %q", action.Title)
	}

	result := toolCallResult(agent.ToolResult{
		ToolCallID: "browser-2",
		Name:       "browser_type",
		Status:     tools.StatusOK,
	})
	if got := browserDescriptor(t, result); got.Command != "type" {
		t.Fatalf("browser result descriptor = %#v", got)
	}
}

func TestBrowserDescriptorSurvivesProtocolShapedRoundTrip(t *testing.T) {
	updates := []ToolCallUpdate{
		toolCallStart(agent.ToolCall{
			ID:        "start",
			Name:      "browser_open",
			Arguments: `{"url":"https://user:password@example.test/private?token=secret#fragment"}`,
		}),
		toolCallResult(agent.ToolResult{
			ToolCallID: "result",
			Name:       "browser_type",
			Status:     tools.StatusOK,
		}),
		permissionToolCall(agent.PermissionRequest{
			ToolCallID: "permission",
			ToolName:   "browser_connect",
			Args:       map[string]any{"target": "127.0.0.1:9222"},
		}),
	}

	type protocolToolCallUpdate struct {
		SessionUpdate string                     `json:"sessionUpdate,omitempty"`
		ToolCallID    string                     `json:"toolCallId"`
		Title         string                     `json:"title,omitempty"`
		Kind          string                     `json:"kind,omitempty"`
		Status        string                     `json:"status,omitempty"`
		RawInput      json.RawMessage            `json:"rawInput,omitempty"`
		Content       []ToolCallContent          `json:"content,omitempty"`
		Locations     []ToolCallLocation         `json:"locations,omitempty"`
		Meta          map[string]json.RawMessage `json:"_meta,omitempty"`
	}

	for _, update := range updates {
		encoded, err := json.Marshal(update)
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &root); err != nil {
			t.Fatal(err)
		}
		if _, ok := root["browser"]; ok {
			t.Fatalf("browser descriptor escaped ACP _meta: %s", encoded)
		}

		var protocol protocolToolCallUpdate
		if err := json.Unmarshal(encoded, &protocol); err != nil {
			t.Fatal(err)
		}
		forwarded, err := json.Marshal(protocol)
		if err != nil {
			t.Fatal(err)
		}
		var roundTripped ToolCallUpdate
		if err := json.Unmarshal(forwarded, &roundTripped); err != nil {
			t.Fatal(err)
		}
		details := browserDescriptor(t, roundTripped)
		if details.Version != 1 || details.Command == "" {
			t.Fatalf("round-tripped browser descriptor = %#v", details)
		}
		descriptorJSON := string(roundTripped.Meta[zeroBrowserMetaKey])
		for _, secret := range []string{"password", "private", "token", "fragment", "127.0.0.1", "9222"} {
			if strings.Contains(descriptorJSON, secret) {
				t.Fatalf("browser descriptor leaked %q: %s", secret, descriptorJSON)
			}
		}
	}
}

func TestBrowserPermissionTitlesMirrorSafeToolArguments(t *testing.T) {
	if got := browserToolTitle("open", `{"url":"evil.example.test/pay?token=hidden#fragment"}`); got != "browser open https://evil.example.test" {
		t.Fatalf("bare-host title = %q", got)
	}
	if got := browserToolTitle("open", `{"URL":"https://different.example.test"}`); got != "browser open" {
		t.Fatalf("case-variant URL title = %q", got)
	}
	if got := browserToolTitle("open", `{"URL":"https://different.example.test","url":"https://actual.example.test/path"}`); got != "browser open https://actual.example.test" {
		t.Fatalf("exact URL key title = %q", got)
	}
	if got := browserToolTitle("action", `{"command":"not an action"}`); got != "browser action" {
		t.Fatalf("unknown browser action title = %q", got)
	}

	longHost := "https://" + strings.Repeat("a", 200) + ".example.test/path?token=hidden"
	title := browserToolTitle("open", `{"url":"`+longHost+`"}`)
	if !utf8.ValidString(title) || utf8.RuneCountInString(title) > len("browser open ")+61 || strings.Contains(title, "token=") {
		t.Fatalf("bounded browser origin title = %q", title)
	}
}

func TestBrowserOpenTitlesRejectDecodedUnicodePresentationControls(t *testing.T) {
	for _, rawURL := range []string{
		"https://safe.example%E2%80%AEevil.test/path",
		"https://safe.example%E2%81%A6evil.test/path",
		"https://safe.example%C2%85evil.test/path",
		"https://safe.example%E2%80%A8evil.test/path",
		"https://safe.example%E2%80%A9evil.test/path",
	} {
		t.Run(rawURL, func(t *testing.T) {
			normalized, err := tools.NormalizeBrowserOpenURL(rawURL)
			if err != nil {
				t.Fatalf("execution URL rejected: %v", err)
			}
			if normalized != rawURL {
				t.Fatalf("execution URL = %q, want unchanged %q", normalized, rawURL)
			}

			args, err := json.Marshal(map[string]any{"url": rawURL})
			if err != nil {
				t.Fatal(err)
			}
			updates := []ToolCallUpdate{
				toolCallStart(agent.ToolCall{ID: "start", Name: "browser_open", Arguments: string(args)}),
				permissionToolCall(agent.PermissionRequest{ToolCallID: "permission", ToolName: "browser_open", Args: map[string]any{"url": rawURL}}),
			}
			for _, update := range updates {
				encoded, err := json.Marshal(update)
				if err != nil {
					t.Fatal(err)
				}
				var decoded ToolCallUpdate
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.Title != "browser open" {
					t.Fatalf("unsafe browser title survived wire round trip: %q", decoded.Title)
				}
				for _, r := range decoded.Title {
					if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
						t.Fatalf("browser title contains unsafe presentation rune %U: %q", r, decoded.Title)
					}
				}
			}
		})
	}
}

func TestBrowserDescriptorDoesNotClaimSimilarlyNamedMCPTools(t *testing.T) {
	start := toolCallStart(agent.ToolCall{ID: "mcp-1", Name: "browser_plugin_open", Arguments: `{}`})
	if len(start.Meta) != 0 {
		t.Fatalf("MCP-like tool received built-in browser metadata: %#v", start.Meta)
	}
	encoded, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"browser"`) {
		t.Fatalf("non-browser tool encoded browser field: %s", encoded)
	}
}

func TestToolCallStart(t *testing.T) {
	upd := toolCallStart(agent.ToolCall{ID: "tc1", Name: "read_file", Arguments: `{"path":"a.go"}`})
	if upd.SessionUpdate != UpdateToolCall {
		t.Fatalf("sessionUpdate = %q", upd.SessionUpdate)
	}
	if upd.ToolCallID != "tc1" || upd.Status != ToolStatusInProgress || upd.Kind != ToolKindRead {
		t.Fatalf("unexpected start: %+v", upd)
	}
	if string(upd.RawInput) != `{"path":"a.go"}` {
		t.Fatalf("rawInput = %s", upd.RawInput)
	}
	// Malformed args must not produce invalid JSON on the wire.
	if got := toolCallStart(agent.ToolCall{ID: "x", Name: "bash", Arguments: "broken"}); got.RawInput != nil {
		t.Fatalf("malformed args should drop rawInput, got %s", got.RawInput)
	}
}

func TestToolCallResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	ok := toolCallResult(agent.ToolResult{
		ToolCallID:   "tc1",
		Name:         "edit_file",
		Status:       tools.StatusOK,
		Output:       "applied\n",
		ChangedFiles: []string{"a.go", ""},
		FileDiffs:    []tools.FileDiff{{Path: path, OldExists: true, NewExists: true, OldText: "before\n", NewText: "after\n"}},
	})
	if ok.SessionUpdate != UpdateToolCallUpdate || ok.Status != ToolStatusCompleted {
		t.Fatalf("unexpected ok result: %+v", ok)
	}
	if len(ok.Content) != 2 || ok.Content[0].Type != "content" || ok.Content[0].Content.Text != "applied" {
		t.Fatalf("unexpected content: %+v", ok.Content)
	}
	if diff := ok.Content[1]; diff.Type != "diff" || diff.Path != path || diff.OldText == nil || *diff.OldText != "before\n" || diff.NewText == nil || *diff.NewText != "after\n" {
		t.Fatalf("unexpected diff content: %+v", diff)
	}
	if len(ok.Locations) != 2 || ok.Locations[0].Path != path || ok.Locations[1].Path != "a.go" {
		t.Fatalf("unproven absolute/relative aliases must both remain visible, got %+v", ok.Locations)
	}

	failed := toolCallResult(agent.ToolResult{ToolCallID: "tc2", Status: tools.StatusError, Output: "boom"})
	if failed.Status != ToolStatusFailed {
		t.Fatalf("error result should be failed, got %q", failed.Status)
	}
}

func TestToolCallDiffJSONDistinguishesEmptyFilesAndDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	content := appendToolResultDiffs(nil, []tools.FileDiff{
		{Path: path, OldExists: false, NewExists: true, NewText: ""},
		{Path: path, OldExists: true, NewExists: true, OldText: "before", NewText: ""},
		{Path: path, OldExists: true, NewExists: false, OldText: "before"},
	})
	if len(content) != 3 {
		t.Fatalf("diff content = %#v", content)
	}
	for index, diff := range content {
		encoded, err := json.Marshal(diff)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if wire["path"] != path {
			t.Fatalf("wire diff %d = %s", index, encoded)
		}
		switch index {
		case 0:
			oldText, present := wire["oldText"]
			if !present || oldText != nil || wire["newText"] != "" {
				t.Fatalf("create diff = %s, want null oldText and empty newText", encoded)
			}
		case 1:
			if wire["oldText"] != "before" || wire["newText"] != "" {
				t.Fatalf("empty replacement diff = %s", encoded)
			}
		case 2:
			newText, present := wire["newText"]
			if wire["oldText"] != "before" || !present || newText != nil {
				t.Fatalf("deletion diff = %s, want oldText and null newText", encoded)
			}
		}
	}
}

func TestToolResultLocationsPreserveDistinctPathIdentities(t *testing.T) {
	root := t.TempDir()
	rootPath := filepath.Join(root, "a.go")
	nestedPath := filepath.Join(root, "sub", "a.go")
	diff := func(path string) tools.FileDiff {
		return tools.FileDiff{Path: path, OldExists: true, NewExists: true, OldText: "before", NewText: "after"}
	}
	for _, tc := range []struct {
		name  string
		diffs []tools.FileDiff
		want  []string
	}{
		{name: "both rich", diffs: []tools.FileDiff{diff(rootPath), diff(nestedPath)}, want: []string{rootPath, nestedPath, "a.go", filepath.Join("sub", "a.go")}},
		{name: "root rich", diffs: []tools.FileDiff{diff(rootPath)}, want: []string{rootPath, "a.go", filepath.Join("sub", "a.go")}},
		{name: "nested rich", diffs: []tools.FileDiff{diff(nestedPath)}, want: []string{nestedPath, "a.go", filepath.Join("sub", "a.go")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locations := toolResultLocations(agent.ToolResult{
				ChangedFiles: []string{"a.go", filepath.Join("sub", "a.go")},
				FileDiffs:    tc.diffs,
			})
			if len(locations) != len(tc.want) {
				t.Fatalf("locations = %#v, want %#v", locations, tc.want)
			}
			for index := range tc.want {
				if locations[index].Path != tc.want[index] {
					t.Fatalf("locations = %#v, want %#v", locations, tc.want)
				}
			}
		})
	}
}

func TestToolCallResultPreservesWhitespaceInFilePaths(t *testing.T) {
	relativePath := " report.txt "
	absolutePath := filepath.Join(t.TempDir(), relativePath)
	update := toolCallResult(agent.ToolResult{
		ChangedFiles: []string{relativePath},
		FileDiffs: []tools.FileDiff{{
			Path: absolutePath, OldExists: true, NewExists: true, OldText: "before", NewText: "after",
		}},
	})
	if len(update.Content) != 1 || update.Content[0].Path != absolutePath {
		t.Fatalf("diff content path = %#v, want %q", update.Content, absolutePath)
	}
	if len(update.Locations) != 2 || update.Locations[0].Path != absolutePath || update.Locations[1].Path != relativePath {
		t.Fatalf("locations = %#v, want exact paths %q and %q", update.Locations, absolutePath, relativePath)
	}
}

func TestToolResultLocationsDeduplicateOnlyExactPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	locations := toolResultLocations(agent.ToolResult{
		ChangedFiles: []string{path, path},
		FileDiffs:    []tools.FileDiff{{Path: path, OldExists: true, NewExists: true, OldText: "before", NewText: "after"}},
	})
	if len(locations) != 1 || locations[0].Path != path {
		t.Fatalf("exact duplicate locations = %#v", locations)
	}
}

func TestDeletedFileEmitsDiffAndKeepsLocations(t *testing.T) {
	relativePath := "deleted.go"
	absolutePath := filepath.Join(t.TempDir(), relativePath)
	update := toolCallResult(agent.ToolResult{
		ChangedFiles: []string{relativePath},
		FileDiffs: []tools.FileDiff{{
			Path: absolutePath, OldExists: true, NewExists: false, OldText: "before",
		}},
	})
	if len(update.Content) != 1 || update.Content[0].OldText == nil || *update.Content[0].OldText != "before" || update.Content[0].NewText != nil {
		t.Fatalf("deleted file diff = %#v, want oldText with null newText", update.Content)
	}
	if len(update.Locations) != 2 || update.Locations[0].Path != absolutePath || update.Locations[1].Path != relativePath {
		t.Fatalf("deleted file locations = %#v", update.Locations)
	}
}

func TestToolCallResultEmitsOnlyRedactedFileDiffs(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz"
	path := filepath.Join(t.TempDir(), "secret.txt")
	scrubbed := tools.ScrubResultSecrets(tools.Result{FileDiffs: []tools.FileDiff{{
		Path: path, OldExists: true, NewExists: true, OldText: "token=" + secret, NewText: "safe",
	}}})
	update := toolCallResult(agent.ToolResult{ToolCallID: "call", Status: tools.StatusError, FileDiffs: scrubbed.FileDiffs})
	if len(update.Content) != 1 || update.Content[0].OldText == nil || strings.Contains(*update.Content[0].OldText, secret) {
		t.Fatalf("ACP content leaked unredacted diff: %#v", update.Content)
	}
}

func TestToolCallResultOmitsSemanticallyUnchangedRedactedDiff(t *testing.T) {
	oldSecret := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	newSecret := "ghp_9876543210ZYXWVUTSRQPONMLKJIHGFEDCBA"
	scrubbed := tools.ScrubResultSecrets(tools.Result{
		ChangedFiles: []string{"credentials.txt"},
		FileDiffs: []tools.FileDiff{{
			Path: filepath.Join(t.TempDir(), "credentials.txt"), OldExists: true, NewExists: true,
			OldText: "token=" + oldSecret, NewText: "token=" + newSecret,
		}},
	})
	update := toolCallResult(agent.ToolResult{
		ToolCallID: "call", Status: tools.StatusOK,
		ChangedFiles: scrubbed.ChangedFiles, FileDiffs: scrubbed.FileDiffs,
	})
	if len(update.Content) != 0 {
		t.Fatalf("ACP emitted semantically unchanged redacted diff: %#v", update.Content)
	}
	if len(update.Locations) != 1 || update.Locations[0].Path != "credentials.txt" {
		t.Fatalf("ACP path fallback = %#v", update.Locations)
	}
}

func TestToolCallResultOmitsDefaultIgnorableSplitSecretsOnEitherSide(t *testing.T) {
	secret := "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGG"
	for name, separator := range map[string]string{
		"combining grapheme joiner": "\u034f",
		"variation selector":        "\ufe0f",
	} {
		for _, side := range []string{"old", "new"} {
			t.Run(name+" "+side, func(t *testing.T) {
				obfuscated := secret[:20] + separator + secret[20:]
				diff := tools.FileDiff{
					Path: filepath.Join(t.TempDir(), "secret.txt"), OldExists: true, NewExists: true,
					OldText: "safe old", NewText: "safe new",
				}
				if side == "old" {
					diff.OldText = obfuscated
				} else {
					diff.NewText = obfuscated
				}
				scrubbed := tools.ScrubResultSecrets(tools.Result{FileDiffs: []tools.FileDiff{diff}})
				if !scrubbed.Redacted || len(scrubbed.FileDiffs) != 0 {
					t.Fatalf("registry boundary retained an obfuscated secret: %#v", scrubbed)
				}
				update := toolCallResult(agent.ToolResult{ToolCallID: "call", Status: tools.StatusOK, FileDiffs: scrubbed.FileDiffs})
				if len(update.Content) != 0 {
					t.Fatalf("ACP content retained an obfuscated secret: %#v", update.Content)
				}
			})
		}
	}
}

func TestPlanUpdateAndStatus(t *testing.T) {
	upd := planUpdate([]tools.PlanItem{
		{Content: "step a", Status: "completed"},
		{Content: "step b", Status: "in_progress"},
		{Content: "step c", Status: "failed"},
		{Content: "step d", Status: "weird"},
	})
	if upd.SessionUpdate != UpdatePlan || len(upd.Entries) != 4 {
		t.Fatalf("unexpected plan: %+v", upd)
	}
	want := []string{PlanStatusCompleted, PlanStatusInProgress, PlanStatusCompleted, PlanStatusPending}
	for i, w := range want {
		if upd.Entries[i].Status != w {
			t.Errorf("entry %d status = %q, want %q", i, upd.Entries[i].Status, w)
		}
		if upd.Entries[i].Priority != PlanPriorityMedium {
			t.Errorf("entry %d priority = %q", i, upd.Entries[i].Priority)
		}
	}
}

func TestPromptText(t *testing.T) {
	got := promptText([]ContentBlock{
		TextBlock("hello "),
		ImageBlock("base64", "image/png"),
		TextBlock("world"),
	})
	if got != "hello world" {
		t.Fatalf("promptText = %q", got)
	}
}

func TestToolTitleTruncateHintRuneSafe(t *testing.T) {
	// A 61-character string containing multi-byte UTF-8 runes (emojis / CJK characters).
	// We want to verify that it is truncated without cutting any runes or producing invalid UTF-8.
	longPath := "📁/项目/非常长的路径名称/测试/🚀/emoji-and-cjk-characters-which-are-very-long-and-exceed-sixty-characters"

	// Create JSON args for read_file
	rawArgs := `{"path":"` + longPath + `"}`
	got := toolTitle("read_file", rawArgs)

	expectedPrefix := "read_file "
	if !strings.HasPrefix(got, expectedPrefix) {
		t.Fatalf("expected title to start with %q, got %q", expectedPrefix, got)
	}

	hint := strings.TrimPrefix(got, expectedPrefix)
	// Hint should end with the ellipsis character
	if !strings.HasSuffix(hint, "…") {
		t.Fatalf("expected truncated hint to end with ellipsis, got %q", hint)
	}

	// Check that we don't have invalid UTF-8 runes
	if !utf8.ValidString(hint) {
		t.Fatalf("truncated hint is not a valid UTF-8 string: %q", hint)
	}

	// The rune count of the hint (excluding ellipsis) should be exactly 60
	runes := []rune(strings.TrimSuffix(hint, "…"))
	if len(runes) != 60 {
		t.Fatalf("expected exactly 60 runes before ellipsis, got %d (hint: %q)", len(runes), hint)
	}
}
