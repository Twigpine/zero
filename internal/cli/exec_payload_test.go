package cli

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/tools"
)

// THE HEADLESS WRITER PERSISTS THE SAME SHAPE THE TUI DOES.
//
// The tui-side restore test proves the shared payload restores a disclosure
// exactly once; this proves the CLI actually WRITES that shared payload rather
// than its own. The two used to be spelled separately and the headless one had
// already drifted to decorated output only. Reverting the delegation leaves
// the tui test green and fails this one, which is the point of having both.
func TestHeadlessPayloadCarriesTypedNoticesAndUndecoratedBody(t *testing.T) {
	const notice = "denyRead is configured, so the Windows sandbox uses the token shape without WRITE_RESTRICTED (#869)"
	result := agent.ToolResult{
		ToolCallID:         "call-cli",
		Name:               "bash",
		Status:             tools.StatusOK,
		Output:             "PROBE-BODY",
		Truncated:          true,
		EnforcementNotices: []string{notice},
	}
	payload := persistedToolResultPayload(result)

	notices, _ := payload["enforcementNotices"].([]string)
	if len(notices) != 1 || notices[0] != notice {
		t.Errorf("headless payload does not carry the typed notice: %#v", payload["enforcementNotices"])
	}
	preview, _ := payload["displayPreview"].(string)
	if preview != "PROBE-BODY" {
		t.Errorf("headless payload does not carry the undecorated body as displayPreview: %q", preview)
	}
	output, _ := payload["output"].(string)
	if !strings.Contains(output, notice) || !strings.Contains(output, "PROBE-BODY") {
		t.Errorf("provider-facing output is no longer the decorated text: %q", output)
	}
	// The one field the headless writer added on its own must survive the
	// delegation, or a truncation marker silently stops being persisted.
	if truncated, _ := payload["truncated"].(bool); !truncated {
		t.Errorf("truncated flag was lost in the shared payload: %#v", payload["truncated"])
	}
}
