package agentsessions

import (
	"slices"
	"sync"
	"time"
)

// discoveryTTL is how long a workspace's discovery result is reused.
//
// Discovery costs 50-300ms on a working machine — a bounded head read of every
// transcript across four stores. That is affordable once, but /resume is opened,
// dismissed and reopened constantly, and paying it on each keypress makes the
// TUI hitch every time.
//
// Ten seconds is chosen to be shorter than a person can act on the staleness. A
// session finished in another terminal appears on the next open rather than
// this one; the alternative — no cache — makes every open hitch to avoid a case
// nobody notices.
const discoveryTTL = 10 * time.Second

type discoveryEntry struct {
	sessions []ForeignSession
	problems []error
	at       time.Time
}

var (
	discoveryMu    sync.Mutex
	discoveryCache = map[string]discoveryEntry{}
	discoveryEpoch uint64
	// discoveryNow is the clock, swapped in tests so TTL expiry is exercised
	// without sleeping.
	discoveryNow = time.Now
	discoverAll  = DiscoverAll
)

// DiscoverAllCached is DiscoverAll with a short per-workspace memo. Callers on a
// UI path should prefer it; a one-shot CLI command should not, since a fresh
// process has nothing cached and the result would only ever be stale.
func DiscoverAllCached(env Env, cwd string) ([]ForeignSession, []error) {
	discoveryMu.Lock()
	now := discoveryNow()
	if entry, ok := discoveryCache[cwd]; ok && now.Sub(entry.at) < discoveryTTL {
		// Copy: callers sort and filter the slice they are handed, and a shared
		// backing array would let one caller reorder another's results.
		discoveryMu.Unlock()
		return append([]ForeignSession{}, entry.sessions...), slices.Clone(entry.problems)
	}
	epoch := discoveryEpoch
	discoveryMu.Unlock()

	// Discovery reads several external stores and can take hundreds of
	// milliseconds. It must not hold the global cache lock: a miss for one
	// workspace cannot stall a valid hit for another workspace.
	found, problems := discoverAll(env, cwd)
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	// Prefer a value another concurrent miss already published. If the cache was
	// invalidated while discovery ran, return this result to the current caller
	// but do not repopulate the cache with a pre-invalidation snapshot.
	now = discoveryNow()
	if entry, ok := discoveryCache[cwd]; ok && now.Sub(entry.at) < discoveryTTL {
		return append([]ForeignSession{}, entry.sessions...), slices.Clone(entry.problems)
	}
	if epoch == discoveryEpoch {
		discoveryCache[cwd] = discoveryEntry{sessions: found, problems: problems, at: now}
	}
	return append([]ForeignSession{}, found...), slices.Clone(problems)
}

// InvalidateDiscovery drops the memo so the next discovery re-reads the stores.
// Called after an import, which changes which sessions are still un-imported.
func InvalidateDiscovery() {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	discoveryEpoch++
	discoveryCache = map[string]discoveryEntry{}
}
