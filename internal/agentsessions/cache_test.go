package agentsessions

import (
	"path/filepath"
	"testing"
	"time"
)

func withFakeClock(t *testing.T) *time.Time {
	t.Helper()
	clock := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	previous := discoveryNow
	discoveryNow = func() time.Time { return clock }
	InvalidateDiscovery()
	t.Cleanup(func() {
		discoveryNow = previous
		InvalidateDiscovery()
	})
	return &clock
}

func storeWithOneSession(t *testing.T) Env {
	t.Helper()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", "projects", "-w", "a.jsonl"),
		`{"type":"user","cwd":"/w","sessionId":"a","message":{"role":"user","content":"hi"}}`+"\n")
	return testEnv(home, nil)
}

func TestASecondDiscoveryInsideTheWindowDoesNotReReadTheStores(t *testing.T) {
	clock := withFakeClock(t)
	env := storeWithOneSession(t)

	first, _ := DiscoverAllCached(env, "/w")
	if len(first) != 1 {
		t.Fatalf("got %d sessions, want 1", len(first))
	}

	// Delete the store outright. A cached answer still returns the session;
	// a re-read could not.
	env2 := testEnv(t.TempDir(), nil)
	*clock = clock.Add(discoveryTTL - time.Second)
	cached, _ := DiscoverAllCached(env2, "/w")
	if len(cached) != 1 {
		t.Errorf("got %d sessions inside the TTL, want the memoised 1", len(cached))
	}

	*clock = clock.Add(2 * time.Second) // now past the TTL
	fresh, _ := DiscoverAllCached(env2, "/w")
	if len(fresh) != 0 {
		t.Errorf("got %d sessions after the TTL, want a fresh (empty) read", len(fresh))
	}
}

func TestEachWorkspaceIsMemoisedSeparately(t *testing.T) {
	withFakeClock(t)
	env := storeWithOneSession(t)

	if got, _ := DiscoverAllCached(env, "/w"); len(got) != 1 {
		t.Fatalf("/w got %d, want 1", len(got))
	}
	// A different workspace must not be served /w's answer.
	if got, _ := DiscoverAllCached(env, "/elsewhere"); len(got) != 0 {
		t.Errorf("/elsewhere got %d sessions, want 0 — the memo is keyed by workspace", len(got))
	}
}

func TestInvalidatingForcesAReRead(t *testing.T) {
	withFakeClock(t)
	env := storeWithOneSession(t)
	if got, _ := DiscoverAllCached(env, "/w"); len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	InvalidateDiscovery()
	empty := testEnv(t.TempDir(), nil)
	if got, _ := DiscoverAllCached(empty, "/w"); len(got) != 0 {
		t.Errorf("got %d after invalidation, want a fresh read", len(got))
	}
}

// TestCallersCannotReorderEachOthersResults covers the aliasing bug a memo
// invites: handing every caller the same backing array lets one caller's sort
// or filter mutate what the next one sees.
func TestCallersCannotReorderEachOthersResults(t *testing.T) {
	withFakeClock(t)
	home := t.TempDir()
	for _, id := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(home, ".claude", "projects", "-w", id+".jsonl"),
			`{"type":"user","cwd":"/w","sessionId":"`+id+`","message":{"role":"user","content":"hi"}}`+"\n")
	}
	env := testEnv(home, nil)

	first, _ := DiscoverAllCached(env, "/w")
	if len(first) != 3 {
		t.Fatalf("got %d, want 3", len(first))
	}
	for i := range first {
		first[i].ID = "clobbered"
	}

	second, _ := DiscoverAllCached(env, "/w")
	for _, session := range second {
		if session.ID == "clobbered" {
			t.Fatal("one caller's mutation reached the next caller's results")
		}
	}
}

func TestSlowMissDoesNotBlockAnotherWorkspacesCacheHit(t *testing.T) {
	withFakeClock(t)
	discoveryMu.Lock()
	discoveryCache["/cached"] = discoveryEntry{
		sessions: []ForeignSession{{ID: "cached"}},
		at:       discoveryNow(),
	}
	discoveryMu.Unlock()

	previous := discoverAll
	started := make(chan struct{})
	release := make(chan struct{})
	discoverAll = func(Env, string) ([]ForeignSession, []error) {
		close(started)
		<-release
		return []ForeignSession{{ID: "slow"}}, nil
	}
	t.Cleanup(func() {
		discoverAll = previous
		select {
		case <-release:
		default:
			close(release)
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		DiscoverAllCached(Env{}, "/slow")
	}()
	<-started

	hit := make(chan []ForeignSession, 1)
	go func() {
		found, _ := DiscoverAllCached(Env{}, "/cached")
		hit <- found
	}()
	select {
	case found := <-hit:
		if len(found) != 1 || found[0].ID != "cached" {
			t.Fatalf("cache hit = %+v, want cached session", found)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("a slow miss held the global cache lock and blocked an unrelated hit")
	}
	close(release)
	<-done
}
