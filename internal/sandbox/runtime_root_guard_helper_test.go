package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// digestFor recomputes the fallback leaf name the way fallbackSandboxRuntimeRoot
// does, so the test moves with the implementation rather than pinning a literal.
func digestFor(workspaceRoot string, scope string) string {
	digest := sha256.Sum256([]byte(canonicalSandboxWorkspaceRoot(workspaceRoot) + "\x00" + scope))
	return hex.EncodeToString(digest[:8])
}

var _ = testing.Verbose
