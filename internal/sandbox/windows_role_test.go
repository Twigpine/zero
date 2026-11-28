package sandbox

import "testing"

// Doctor and the runtime must name the SAME principal.
//
// The helper this replaces existed to be that single rule and said so, and the
// drift it was meant to prevent happened anyway: dual-role provisioning made the
// runtime use an offline principal under NetworkDeny while doctor still reported
// the old restricted-token standdown, so an operator was told their reads were
// unconfined while they were confined. Both sides now call this, and the test
// pins the mapping rather than either caller's copy of it.
func TestPrincipalRoleForNetwork(t *testing.T) {
	for mode, want := range map[NetworkMode]string{
		NetworkAllow: "online",
		NetworkDeny:  "offline",
		// Unset and unrecognized lose the network rather than keeping it, matching
		// what the runtime does with a mode it does not know.
		"":           "offline",
		"not-a-mode": "offline",
	} {
		if got := WindowsSandboxPrincipalRoleForNetwork(mode); got != want {
			t.Errorf("role for %q = %q, want %q", mode, got, want)
		}
	}
}
