package tui

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

func TestStaticRenderCacheHitMiss(t *testing.T) {
	cache := newStaticRenderCache(4, 100)
	renders := 0

	first := cache.render("row:1", true, func() string {
		renders++
		return "rendered"
	})
	second := cache.render("row:1", true, func() string {
		renders++
		return "changed"
	})

	if first != "rendered" || second != "rendered" {
		t.Fatalf("cached values = %q, %q; want first render reused", first, second)
	}
	if renders != 1 {
		t.Fatalf("render func called %d times, want 1", renders)
	}
	stats := cache.stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("stats = %#v, want 1 hit and 1 miss", stats)
	}
}

func TestStaticRenderCacheEvictsLeastRecentlyUsedEntry(t *testing.T) {
	cache := newStaticRenderCache(2, 100)

	cache.render("a", true, func() string { return "A" })
	cache.render("b", true, func() string { return "B" })
	cache.render("a", true, func() string { return "A2" })
	cache.render("c", true, func() string { return "C" })

	renders := 0
	got := cache.render("b", true, func() string {
		renders++
		return "B2"
	})

	if got != "B2" || renders != 1 {
		t.Fatalf("b render = %q with %d calls, want evicted entry to rerender", got, renders)
	}
	stats := cache.stats()
	if stats.Evictions != 2 {
		t.Fatalf("stats = %#v, want 2 evictions (c insertion, b reinsertion)", stats)
	}
}

func TestStaticRenderCacheEvictsToRetainedCharacterLimit(t *testing.T) {
	cache := newStaticRenderCache(10, 6)

	cache.render("a", true, func() string { return "aaa" })
	cache.render("b", true, func() string { return "bbb" })
	cache.render("a", true, func() string { return "aaa" })
	cache.render("c", true, func() string { return "cc" })

	if got := cache.retainedCharacters(); got != 5 {
		t.Fatalf("retained characters = %d, want 5", got)
	}

	renders := 0
	got := cache.render("b", true, func() string {
		renders++
		return "bbb"
	})
	if got != "bbb" || renders != 1 {
		t.Fatalf("b render = %q with %d calls, want retained-char eviction to rerender", got, renders)
	}
	if stats := cache.stats(); stats.Evictions != 2 {
		t.Fatalf("stats = %#v, want 2 evictions (char-limit insertion, b reinsertion)", stats)
	}
}

func TestStaticRenderCacheSkipsOversizedOutputs(t *testing.T) {
	cache := newStaticRenderCache(4, 5)
	renders := 0

	for range 2 {
		got := cache.render("huge", true, func() string {
			renders++
			return "123456"
		})
		if got != "123456" {
			t.Fatalf("render = %q, want oversized value returned without caching", got)
		}
	}

	stats := cache.stats()
	if renders != 2 || stats.SkippedOversized != 2 || stats.Hits != 0 {
		t.Fatalf("renders=%d stats=%#v, want rerendered oversized output with no hits", renders, stats)
	}
	if cache.retainedCharacters() != 0 {
		t.Fatalf("retained characters = %d, want 0", cache.retainedCharacters())
	}
}

func TestStaticRenderCacheSkipsUnstableRequests(t *testing.T) {
	cache := newStaticRenderCache(4, 100)
	renders := 0

	for range 2 {
		cache.render("spinner", false, func() string {
			renders++
			return "tick"
		})
	}

	if renders != 2 {
		t.Fatalf("render func called %d times, want 2 for unstable requests", renders)
	}
	if stats := cache.stats(); stats != (renderCacheStats{}) {
		t.Fatalf("stats = %#v, want unstable requests to bypass cache accounting", stats)
	}
}

func TestRenderRowCacheSkipsRunningToolRows(t *testing.T) {
	cache := replaceDefaultRenderCache(t, newStaticRenderCache(8, 1000))
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	row := transcriptRow{kind: rowToolCall, id: "call_1", runID: 7, tool: "grep", detail: "internal/tui"}

	for range 2 {
		_ = m.renderRow(row, 80, buildRowContext([]transcriptRow{row}))
	}

	if stats := cache.stats(); stats != (renderCacheStats{}) {
		t.Fatalf("stats = %#v, want running tool rows to bypass cache", stats)
	}
}

func TestRenderRowCacheSkipsPendingPermissionPromptRows(t *testing.T) {
	cache := replaceDefaultRenderCache(t, newStaticRenderCache(8, 1000))
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 9
	row := transcriptRow{kind: rowPermission, id: "call_1", runID: 9, permission: &agent.PermissionEvent{
		ToolCallID: "call_1",
		ToolName:   "bash",
		Action:     agent.PermissionActionPrompt,
	}}

	for range 2 {
		_ = m.renderRow(row, 80, buildRowContext([]transcriptRow{row}))
	}

	if stats := cache.stats(); stats != (renderCacheStats{}) {
		t.Fatalf("stats = %#v, want pending permission prompts to bypass cache", stats)
	}
}

func TestSelectableAssistantRowsUseRenderCacheWhenSelectionInactive(t *testing.T) {
	cache := replaceDefaultRenderCache(t, newStaticRenderCache(8, 1000))
	m := newModel(context.Background(), Options{})
	row := transcriptRow{kind: rowAssistant, text: "Done with **markdown**.", final: true, turnTools: 1}

	first, firstSelectable := m.renderSelectableAssistantRow(0, row, 80, 0)
	second, secondSelectable := m.renderSelectableAssistantRow(0, row, 80, 0)

	if first == "" || first != second {
		t.Fatalf("rendered rows = %q and %q, want stable cached output", first, second)
	}
	if len(firstSelectable) == 0 || len(secondSelectable) != len(firstSelectable) {
		t.Fatalf("selectable metadata = %#v then %#v, want stable selectable lines", firstSelectable, secondSelectable)
	}
	stats := cache.stats()
	if stats.Misses != 1 || stats.Hits != 1 {
		t.Fatalf("cache stats = %#v, want one miss and one hit", stats)
	}
}

func replaceDefaultRenderCache(t *testing.T, cache *staticRenderCache) *staticRenderCache {
	t.Helper()
	previous := defaultRenderCache
	defaultRenderCache = cache
	t.Cleanup(func() {
		defaultRenderCache = previous
	})
	return cache
}
