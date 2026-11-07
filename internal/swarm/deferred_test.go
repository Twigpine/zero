package swarm

import (
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// With NO active swarm, all swarm tools must be deferred-eligible so their
// schemas stay out of the eager per-request tool prefix (loaded on demand via
// tool_search), while the core built-ins remain eager. (A zero-value &Swarm{}
// has a nil coordinator, so hasActiveSwarm reports inactive — see nil-safety.)
func TestSwarmToolsAreDeferred(t *testing.T) {
	registry := tools.NewRegistry()
	RegisterTools(registry, &Swarm{})
	for _, tool := range registry.All() {
		if !tools.IsDeferred(tool) {
			t.Errorf("swarm tool %q is NOT deferred-eligible (would bloat the eager prefix)", tool.Name())
		}
	}
	if len(registry.All()) != 7 {
		t.Fatalf("expected 7 swarm tools registered, got %d", len(registry.All()))
	}
	// Every swarm tool counts toward the deferral threshold (deferral-eligible),
	// cold start and always.
	for _, tool := range registry.All() {
		if !tools.IsDeferralEligible(tool) {
			t.Errorf("swarm tool %q should be deferral-eligible", tool.Name())
		}
	}
}

// Once a swarm is active, the COORDINATION tools un-defer (IsDeferred==false) so a
// coordinating model sees them eagerly and doesn't misroute the calls to the
// specialist tool. The entry-point/handoff tools stay deferred for cold-start
// discovery. Critically, the un-deferred tools STAY deferral-eligible, so the
// global deferral count is unchanged and other deferred tools (MCP) are never
// force-exposed — in any threshold/filter config.
func TestSwarmCoordinationToolsUndeferWhenActive(t *testing.T) {
	reg, sw := newToolSwarm(t, newLauncher(okFor))

	// Cold start: every swarm tool is deferred.
	for _, tool := range reg.All() {
		if !tools.IsDeferred(tool) {
			t.Errorf("with no active swarm, %q should be deferred", tool.Name())
		}
	}

	// Activate the swarm by registering a task in the coordinator.
	if _, err := sw.Coordinator().Register("t1", "teammate-1", "default", "do work"); err != nil {
		t.Fatalf("register task: %v", err)
	}

	undeferred := map[string]bool{SendToolName: true, StatusToolName: true, InboxToolName: true, CollectToolName: true}
	stillDeferred := map[string]bool{SpawnToolName: true, ScheduleToolName: true, HandoffToolName: true}
	eligibleCount := 0
	for _, tool := range reg.All() {
		name := tool.Name()
		got := tools.IsDeferred(tool)
		// The active-gate counts deferral-ELIGIBLE tools; every swarm tool must
		// stay eligible even when un-deferred, so the count never drops.
		if !tools.IsDeferralEligible(tool) {
			t.Errorf("swarm tool %q must stay deferral-eligible even when un-deferred", name)
		} else {
			eligibleCount++
		}
		switch {
		case undeferred[name] && got:
			t.Errorf("with an active swarm, coordination tool %q should NOT be deferred", name)
		case stillDeferred[name] && !got:
			t.Errorf("entry/handoff tool %q must stay deferred", name)
		}
	}
	// All 7 stay deferral-eligible (the count partitionTools' active-gate uses),
	// so un-deferring the 4 coordination tools cannot deactivate deferral.
	if eligibleCount != 7 {
		t.Fatalf("expected all 7 swarm tools deferral-eligible while active, got %d", eligibleCount)
	}
}

// hasActiveSwarm must be nil-safe: a nil *Swarm or a zero-value Swarm (nil
// coordinator) reports inactive, so the coordination tools stay deferred rather
// than panicking — this is what keeps TestSwarmToolsAreDeferred (&Swarm{}) green.
func TestHasActiveSwarmNilSafe(t *testing.T) {
	var nilSwarm *Swarm
	if nilSwarm.hasActiveSwarm() {
		t.Error("nil swarm should report inactive")
	}
	if (&Swarm{}).hasActiveSwarm() {
		t.Error("zero-value swarm (nil coord) should report inactive")
	}
}
