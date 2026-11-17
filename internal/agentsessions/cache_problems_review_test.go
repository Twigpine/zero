package agentsessions

import (
	"errors"
	"testing"
)

func TestDiscoveryProblemsBelongToEachCaller(t *testing.T) {
	withFakeClock(t)
	firstProblem, secondProblem := errors.New("first discovery failure"), errors.New("second discovery failure")
	previous := discoverAll
	discoverAll = func(Env, string) ([]ForeignSession, []error) {
		return nil, []error{firstProblem, secondProblem}
	}
	t.Cleanup(func() { discoverAll = previous })
	env := testEnv(t.TempDir(), nil)
	_, first := DiscoverAllCached(env, "/problems")
	first[0] = errors.New("caller changed initial result")
	_, second := DiscoverAllCached(env, "/problems")
	if second[0] != firstProblem {
		t.Error("first caller changed the memoized problem")
	}
	second[1] = errors.New("caller changed cache-hit result")
	_, third := DiscoverAllCached(env, "/problems")
	if third[1] != secondProblem {
		t.Error("cache-hit caller changed the next result")
	}
}
