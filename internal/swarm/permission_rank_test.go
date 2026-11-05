package swarm

import "testing"

// permissionRank decides how hard a member is clamped against its parent, and
// an unranked mode scores 0, the strictest tier. That default is deliberately
// fail-safe, which is also what makes a missing mode invisible: nothing errors,
// members just quietly lose authority the parent meant them to have.
//
// The mode was renamed from "unsafe" to "full-auto". These values cross into
// this package as raw strings on tools.RunOptions.PermissionMode, so the agent
// package's deprecated Go alias does not apply, and ranking only the old
// spelling means a full-auto parent clamps its members hardest of all.
func TestFullAutoRanksAsTheMostPermissiveMode(t *testing.T) {
	fullAuto := permissionRank("full-auto")

	for _, mode := range []string{"", "spec-draft", "ask", "auto", "unknown-mode"} {
		if permissionRank(mode) >= fullAuto {
			t.Errorf("mode %q ranks %d, at or above full-auto's %d, so full-auto is not the most permissive tier",
				mode, permissionRank(mode), fullAuto)
		}
	}
}

// The legacy spelling must keep ranking identically, or a caller that has not
// been updated is clamped differently from one that has, for the same intent.
func TestLegacyUnsafeRanksTheSameAsFullAuto(t *testing.T) {
	if permissionRank("unsafe") != permissionRank("full-auto") {
		t.Fatalf("unsafe ranks %d but full-auto ranks %d; the same mode under two names must clamp members the same way",
			permissionRank("unsafe"), permissionRank("full-auto"))
	}
}

// The clamp is only meaningful if the order is strict, so pin the whole ladder
// rather than one rung. A rename that collapsed two tiers would otherwise pass
// the checks above.
func TestPermissionRankOrdersModesStrictly(t *testing.T) {
	ascending := []string{"unknown-mode", "spec-draft", "ask", "auto", "full-auto"}
	for i := 1; i < len(ascending); i++ {
		previous, current := permissionRank(ascending[i-1]), permissionRank(ascending[i])
		if current <= previous {
			t.Errorf("%q ranks %d and %q ranks %d, so the ladder is not strictly increasing",
				ascending[i-1], previous, ascending[i], current)
		}
	}
}
