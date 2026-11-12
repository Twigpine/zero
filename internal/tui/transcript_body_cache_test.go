package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestTranscriptBodyHeightCacheGrowsBeyondInitialCap(t *testing.T) {
	cache := newTranscriptBodyHeightCache(4)
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("row-%d", i)
		cache.set(key, i)
		if _, ok := cache.get(key); !ok {
			t.Fatalf("entry %d evicted despite auto-growth (cap now %d)", i, cache.maxEntries)
		}
	}
	if cache.maxEntries <= 4 {
		t.Fatalf("maxEntries = %d, want auto-growth past the initial cap", cache.maxEntries)
	}
	if cache.maxEntries > transcriptBodyHeightCacheHardCap {
		t.Fatalf("maxEntries = %d, want at most hard cap %d", cache.maxEntries, transcriptBodyHeightCacheHardCap)
	}
}

func TestTranscriptBodyHeightCacheEvictsAtHardCap(t *testing.T) {
	cache := newTranscriptBodyHeightCache(4)
	for i := 0; i < transcriptBodyHeightCacheHardCap*2+8; i++ {
		cache.set(fmt.Sprintf("row-%d", i), i)
	}
	if cache.maxEntries != transcriptBodyHeightCacheHardCap {
		t.Fatalf("maxEntries = %d, want exactly the hard cap", cache.maxEntries)
	}
	if len(cache.items) > transcriptBodyHeightCacheHardCap {
		t.Fatalf("items = %d, want at most the hard cap", len(cache.items))
	}
	// The oldest entries must be evicted once the hard cap binds.
	if _, ok := cache.get("row-0"); ok {
		t.Fatal("row-0 still cached past the hard cap; LRU eviction never engaged")
	}
	// The newest entry stays.
	if _, ok := cache.get(fmt.Sprintf("row-%d", transcriptBodyHeightCacheHardCap*2+7)); !ok {
		t.Fatal("newest entry missing; eviction loop damaged fresh inserts")
	}
}

func TestTranscriptBodyHeightCacheResetShrinksCeiling(t *testing.T) {
	cache := newTranscriptBodyHeightCache(4)
	for i := 0; i < 40; i++ {
		cache.set(fmt.Sprintf("row-%d", i), i)
	}
	cache.reset()
	if cache.maxEntries != defaultTranscriptBodyHeightCacheMaxEntries {
		t.Fatalf("maxEntries = %d after reset, want default %d", cache.maxEntries, defaultTranscriptBodyHeightCacheMaxEntries)
	}
	if len(cache.items) != 0 {
		t.Fatalf("items = %d after reset, want empty", len(cache.items))
	}
}

func TestTranscriptBodyRenderCacheGrowsBeyondInitialCap(t *testing.T) {
	cache := newTranscriptBodyRenderCache(4, 512)
	build := func(value string) func() (string, []transcriptSelectableLine) {
		return func() (string, []transcriptSelectableLine) {
			return value, []transcriptSelectableLine{{bodyY: 0, text: value}}
		}
	}
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("row-%d", i)
		want := fmt.Sprintf("rendered %d", i)
		got, _ := cache.render(key, true, 0, build(want))
		if got != want {
			t.Fatalf("entry %d = %q, want %q", i, got, want)
		}
		// Re-render must reuse the stored value even after later insertions,
		// which is the whole point of auto-growth.
		second, _ := cache.render(key, true, 0, build("should not render"))
		if second != want {
			t.Fatalf("entry %d rebuilt on second frame (%q); LRU evicted it despite auto-growth", i, second)
		}
	}
	if cache.maxEntries <= 4 {
		t.Fatalf("maxEntries = %d, want auto-growth past the initial cap", cache.maxEntries)
	}
}

func TestTranscriptBodyRenderCacheResetRestoresDefaults(t *testing.T) {
	cache := newTranscriptBodyRenderCache(4, 512)
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("row-%d", i)
		cache.render(key, true, 0, func() (string, []transcriptSelectableLine) {
			return strings.Repeat("x", 32), nil
		})
	}
	cache.reset()
	if cache.maxEntries != defaultTranscriptBodyRenderCacheMaxEntries || cache.maxCharacters != defaultTranscriptBodyRenderCacheMaxCharacters {
		t.Fatalf("ceilings after reset = %d/%d, want defaults %d/%d",
			cache.maxEntries, cache.maxCharacters,
			defaultTranscriptBodyRenderCacheMaxEntries, defaultTranscriptBodyRenderCacheMaxCharacters)
	}
	if len(cache.items) != 0 || cache.retained != 0 {
		t.Fatalf("items=%d retained=%d after reset, want empty", len(cache.items), cache.retained)
	}
}

// TestStreamingTailFramesHitCachesPastStaticCaps is the regression test for the
// pathological streaming state captured in cpu4/mem4: a long run whose live
// tail (frontier blocked by an unresolved tool call) holds more rendered rows
// than the static height/render cache caps. Before auto-growth every spinner
// frame evicted rows the next frame still needed, so measureTranscriptBodyItems
// re-rendered the whole tail (~78% of session CPU). A steady-state frame must
// now pay zero build() calls.
func TestStreamingTailFramesHitCachesPastStaticCaps(t *testing.T) {
	m := transcriptViewTestModel()
	m.altScreen = true
	m.height = 40
	for i := 0; i < 5; i++ {
		m.transcript = append(m.transcript,
			transcriptRow{kind: rowUser, text: fmt.Sprintf("question %d", i), runID: i},
			transcriptRow{kind: rowAssistant, text: strings.Repeat("answer ", 60), final: true, runID: i},
		)
	}
	m, _ = m.settleTranscript()

	// Active run: the frontier blocks on the unresolved live call, leaving a
	// settled tail far larger than the static height-cache cap behind it.
	const runID = 99
	m.pending = true
	m.activeRunID = runID
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, id: "live_call", tool: "exec_command", arg: "long-running", detail: "running", status: "running", runID: runID},
	)
	for i := 0; i < 400; i++ {
		m.transcript = append(m.transcript,
			transcriptRow{kind: rowToolCall, id: fmt.Sprintf("call_%d", i), tool: "exec_command", arg: "ls", detail: "hint", status: "completed", runID: runID},
			transcriptRow{kind: rowToolResult, id: fmt.Sprintf("call_%d", i), tool: "exec_command", detail: strings.Repeat(fmt.Sprintf("output %d ", i), 200), status: "completed", runID: runID},
		)
	}
	m.streamingText = []byte("partial streaming text")
	m, _ = m.settleTranscript()
	if m.flushed >= len(m.transcript) {
		t.Fatalf("frontier flushed=%d/%d, want it blocked by the live call", m.flushed, len(m.transcript))
	}

	width := m.chatColumnWidth()

	// Count rebuilds across frames via a fresh render cache installed before
	// priming, so the primed frame is its first.
	renders := newTranscriptBodyRenderCache(
		defaultTranscriptBodyRenderCacheMaxEntries,
		defaultTranscriptBodyRenderCacheMaxCharacters,
	)
	old := defaultTranscriptBodyRenders
	defaultTranscriptBodyRenders = renders
	t.Cleanup(func() { defaultTranscriptBodyRenders = old })

	// Prime the caches with one full frame.
	_ = m.scrollableTranscriptItemsView("", m.transcriptBodyItems(width, "", false), "", width, "")
	missesAfterPrime := renders.misses

	// Height pass on a steady frame must fully hit the height cache: any miss
	// renders the entire row just to count lines.
	items := m.transcriptBodyItems(width, "", false)
	misses := 0
	for _, item := range items {
		if !item.heightCacheStable || item.heightCacheKey == "" {
			continue
		}
		if _, ok := m.transcriptBodyHeights.get(item.heightCacheKey); !ok {
			misses++
		}
	}
	if misses > 0 {
		t.Fatalf("%d/%d stable items missed the height cache on a steady frame; caches thrash again", misses, len(items))
	}

	// And the render pass must reuse cached renders for all but live items.
	_ = m.scrollableTranscriptItemsView("", m.transcriptBodyItems(width, "", false), "", width, "")
	missDelta := renders.misses - missesAfterPrime
	if missDelta > 8 {
		t.Fatalf("second steady frame rebuilt %d items, want only the handful of live/unstable ones", missDelta)
	}
}
