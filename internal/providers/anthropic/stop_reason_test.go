package anthropic

import (
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestMapStopReasonRefusal(t *testing.T) {
	t.Parallel()
	if got := mapStopReason("refusal"); got != zeroruntime.FinishReasonContentFilter {
		t.Errorf("refusal → %q, want content_filter (M4)", got)
	}
	if got := mapStopReason("max_tokens"); got != zeroruntime.FinishReasonLength {
		t.Errorf("max_tokens → %q, want length", got)
	}
	for _, normal := range []string{"end_turn", "tool_use", "stop_sequence", "pause_turn", ""} {
		if got := mapStopReason(normal); got != "" {
			t.Errorf("%q should be a normal stop (empty), got %q", normal, got)
		}
	}
}
