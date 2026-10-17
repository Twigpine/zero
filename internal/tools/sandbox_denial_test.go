package tools

import (
	"testing"

	"github.com/Twigpine/zero/internal/execution"
)

func TestStructuredSandboxDenialMetadata(t *testing.T) {
	meta := map[string]string{}
	markStructuredSandboxDenial(meta, execution.Denial{
		Capability: execution.Capability{Kind: execution.CapabilityProtectedMetadata, Scope: "/workspace/.zero"},
		Reason:     "protected metadata is denied",
	})
	if meta["sandbox_denial_capability"] != string(execution.CapabilityProtectedMetadata) || meta["sandbox_denial_scope"] != "/workspace/.zero" {
		t.Fatalf("structured metadata = %#v", meta)
	}
}
