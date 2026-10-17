package tools

import "github.com/Twigpine/zero/internal/execution"

// markStructuredSandboxDenial mirrors typed adapter facts into legacy metadata
// for presentation and backward-compatible session readers. Classification is
// never inferred from stdout or stderr.
func markStructuredSandboxDenial(meta map[string]string, denial execution.Denial) {
	if meta == nil {
		return
	}
	meta[SandboxLikelyDeniedMeta] = "true"
	meta[SandboxDenialKindMeta] = SandboxDenialKindSandbox
	meta[SandboxDenialReasonMeta] = denial.Reason
	meta["sandbox_denial_capability"] = string(denial.Capability.Kind)
	if denial.Capability.Scope != "" {
		meta["sandbox_denial_scope"] = denial.Capability.Scope
	}
}
