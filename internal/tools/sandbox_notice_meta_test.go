package tools

import (
	"strings"
	"testing"

	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

// THE DISCLOSURE HAS TO REACH THE OPERATOR, not just exist on the plan.
//
// The DenyRead write-jail trade was reachable only from BackendPlan, which
// `zero sandbox policy` and `zero sandbox check` render. Someone approving
// file_system.deny_read for a single command never runs those, so they lost the
// write jail silently. addSandboxMeta is the boundary where a tool call's
// sandbox facts become visible, alongside the downgrade reason that already
// travels this way.
func TestSandboxMetaCarriesLeastPrivilegeNotices(t *testing.T) {
	meta := map[string]string{}
	addSandboxMeta(meta, zeroSandbox.CommandPlan{
		Backend: zeroSandbox.Backend{Name: zeroSandbox.BackendWindowsRestrictedToken},
		Notes: []string{
			"denyRead is set, so the restricted token drops WRITE_RESTRICTED and the workspace write jail no longer confines writes outside it (#869).",
		},
	})

	notices, ok := meta["sandbox_notices"]
	if !ok {
		t.Fatalf("no sandbox_notices in the tool result metadata, so the trade stays invisible to whoever approved it: %#v", meta)
	}
	for _, want := range []string{"denyRead", "#869"} {
		if !strings.Contains(notices, want) {
			t.Errorf("notice does not mention %q: %q", want, notices)
		}
	}
}

// A plan with nothing to disclose must not add the key, or every command grows
// an empty field and the presence of one stops meaning anything.
func TestSandboxMetaOmitsNoticesWhenThereAreNone(t *testing.T) {
	meta := map[string]string{}
	addSandboxMeta(meta, zeroSandbox.CommandPlan{
		Backend: zeroSandbox.Backend{Name: zeroSandbox.BackendWindowsRestrictedToken},
	})
	if value, ok := meta["sandbox_notices"]; ok {
		t.Errorf("sandbox_notices present with nothing to say: %q", value)
	}
}
