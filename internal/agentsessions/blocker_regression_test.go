package agentsessions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
)

// TestImportedControlBytesAreStripped pins the terminal-injection fix: a foreign
// transcript is attacker-influenced, and an ESC or NUL in a message must not
// survive into a transcript line or picker row (the #835/#876 class). The
// content is built with Go escapes and JSON-encoded so the transcript carries
// the real control bytes.
func TestImportedControlBytesAreStripped(t *testing.T) {
	malicious := "before\x1b[2J\x1b[1;1H FORGED \x00\x07\r after"
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": malicious},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, string(line))

	events, err := translateFamily1At("", path, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Inspect the payload strings DIRECTLY, not a json.Marshal of the events —
	// JSON encoding would escape a surviving control byte to "" and hide it.
	var contentSeen string
	for _, event := range events {
		payload, ok := event.Payload.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range payload {
			s, ok := field.(string)
			if !ok {
				continue
			}
			if strings.ContainsAny(s, "\x1b\x00\x07\r") {
				t.Errorf("a control byte survived translation into a payload string: %q", s)
			}
			if strings.Contains(s, "before") {
				contentSeen = s
			}
		}
	}
	if contentSeen == "" || !strings.Contains(contentSeen, "after") {
		t.Errorf("stripping removed visible text, not just control bytes: %q", contentSeen)
	}
}

// TestStripControlKeepsTabAndNewline guards the one carve-out: transcripts
// legitimately carry tab and newline, and dropping them would mangle real text.
func TestStripControlKeepsTabAndNewline(t *testing.T) {
	got, _ := stripControlWithBoundaries("a\tb\nc\rd\x1be\x00f")
	if got != "a\tb\ncdef" {
		t.Errorf("stripControl = %q, want tab and newline kept and ESC/NUL dropped", got)
	}
}

// TestActivitySummaryIsAMessageNotACompaction pins the replay-contract fix.
// EventCompaction drives RehydrateEvents, which hoists a bookkeeping-less summary
// to the front of the transcript; the activity summary must not be that type.
func TestActivitySummaryIsAMessageNotACompaction(t *testing.T) {
	note := noteEvent("Prior session activity: 1 tool call.")
	if note.Type == sessions.EventCompaction {
		t.Fatal("activity summary is EventCompaction — RehydrateEvents will hoist it to the transcript front")
	}
	if note.Type != sessions.EventMessage {
		t.Fatalf("activity summary type = %s, want EventMessage", note.Type)
	}
	if !NoteEventIsSummary(note.Payload) {
		t.Fatal("activity summary carries no marker, so nothing can tell it from a real assistant turn")
	}
}

// TestStructuralFieldsAreRedacted covers CodeRabbit's finding: role, name, and
// toolCallId come from the foreign transcript too, so a credential hidden in any
// of them must be redacted, not merely stripped of control bytes.
func TestStructuralFieldsAreRedacted(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("A", 40)
	identities := &importCallIdentities{}
	events := []sessions.AppendEventInput{
		messageEvent(secret, "hi"),                               // malicious role
		toolCallEvent(identities, secret, secret, "{}"),          // malicious tool name + call id
		toolResultEvent(identities, secret, secret, "ok", "out"), // malicious tool name + result id
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Errorf("a secret in a structural field (role/name/toolCallId) survived translation:\n%s", encoded)
	}
	// Redaction is deterministic, so the call and its result must still pair up.
	call := events[1].Payload.(map[string]any)["toolCallId"].(string)
	result := events[2].Payload.(map[string]any)["toolCallId"].(string)
	if call == "" || call != result {
		t.Errorf("redacted call/result ids diverged and broke pairing: %q vs %q", call, result)
	}
}
