package sessions

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// MessageAttachments is the count-only summary stored on user message events.
// It matches the TUI's attachments payload; image data is never persisted here.
type MessageAttachments struct {
	Images    int `json:"images"`
	Documents int `json:"documents"`
}

// Match the TUI's bound when reading untrusted persisted attachment counts.
const maxMessageAttachments = 64

// UnmarshalJSON ignores malformed counts without dropping the message's text.
func (a *MessageAttachments) UnmarshalJSON(data []byte) error {
	var raw struct {
		Images    json.RawMessage `json:"images"`
		Documents json.RawMessage `json:"documents"`
	}
	*a = MessageAttachments{}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	a.Images = messageAttachmentCount(raw.Images)
	a.Documents = messageAttachmentCount(raw.Documents)
	return nil
}

func messageAttachmentCount(raw json.RawMessage) int {
	var count float64
	if json.Unmarshal(raw, &count) != nil || count <= 0 || count != math.Trunc(count) {
		return 0
	}
	return int(min(count, maxMessageAttachments))
}

// WithContent renders an attachment marker beside the original message text for
// history and replay. The stored content itself stays unchanged.
func (a MessageAttachments) WithContent(content string) string {
	parts := []string{}
	for _, item := range []struct {
		count int
		name  string
	}{{a.Images, "image"}, {a.Documents, "document"}} {
		count := min(max(item.count, 0), maxMessageAttachments)
		if count == 0 {
			continue
		}
		name := item.name
		if count != 1 {
			name += "s"
		}
		parts = append(parts, fmt.Sprintf("[%d %s]", count, name))
	}
	if len(parts) == 0 {
		return content
	}
	if content != "" {
		return content + " " + strings.Join(parts, " ")
	}
	return strings.Join(parts, " ")
}
