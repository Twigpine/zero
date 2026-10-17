package sessions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFormatExecPromptAttachmentSummary(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"image only", `{"role":"user","content":"","attachments":{"images":1,"documents":0}}`, "[1 image] user"},
		{"text and images", `{"role":"user","content":"compare","attachments":{"images":2}}`, "compare [2 images] user"},
		{"TUI documents", `{"role":"user","content":"read","attachments":{"documents":1}}`, "read [1 document] user"},
		{"old text", `{"role":"user","content":"old"}`, "old user"},
		{"malformed counts", `{"role":"user","content":"text","attachments":{"images":-2,"documents":1.5}}`, "text user"},
		{"wrong attachment type", `{"role":"user","content":"text","attachments":"wrong"}`, "text user"},
		{"wrong count type", `{"role":"user","content":"text","attachments":{"images":"1","documents":true}}`, "text user"},
		{"bounded counts", `{"role":"user","content":"","attachments":{"images":1e100,"documents":64.5}}`, "[64 images] user"},
		{"preserve metadata", `{"role":"user","content":"read","origin":"cross_session","from":"peer","attachments":{"images":1}}`, "read [1 image] peer cross_session user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatExecPrompt("continue", PreparedExec{
				Mode:          ModeResume,
				ContextEvents: []Event{{Sequence: 1, Type: EventMessage, Payload: json.RawMessage(tc.payload)}},
			})
			if !strings.Contains(got, "- #1 message: "+tc.want+"\n") {
				t.Errorf("resume context = %q, want message %q", got, tc.want)
			}
		})
	}
}
