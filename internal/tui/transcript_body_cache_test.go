package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
// tail (frontier blocked by an unresolved tool call) holds more stable rows
// than the STATIC height/render cache caps, so a growth-disabled cache must
// evict rows the very next frame still needs. The working set below exceeds
// defaultTranscriptBodyHeightCacheMaxEntries (2048) and
// defaultTranscriptBodyRenderCacheMaxEntries (256); before auto-growth every
// spinner frame evicted rows the next frame still needed, so
// measureTranscriptBodyItems re-rendered the whole tail (~78% of session CPU).
// A steady-state frame must now pay zero build() calls.
//
// Provenance: verified to FAIL when transcriptBodyHeightCache.growLocked and
// transcriptBodyRenderCache.growLocked are neutered (steady-frame height misses
// appear and the render rebuild count jumps into the hundreds), and to pass
// with growth restored.
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
	// Collapsed reasoning rows are the cheapest per-row render the pipeline
	// has (a one-line header), and each unique text yields a unique
	// content-addressed key — so the row count, not the row cost, drives the
	// caches past their static caps.
	const tailRows = defaultTranscriptBodyHeightCacheMaxEntries + 256
	for i := 0; i < tailRows; i++ {
		m.transcript = append(m.transcript,
			transcriptRow{kind: rowReasoning, text: fmt.Sprintf("thinking step %d", i), runID: runID},
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

// TestTranscriptBodyRenderCacheChargesKeys verifies the byte budget accounts
// for the retained key, not just the rendered value. Keys are content-
// addressed (they embed the row text) and are held by the map and the LRU
// entry for the entry's lifetime, so an uncharged key makes the "8MB retained"
// ceiling a floor instead: a long tool card pays its key once on insert and
// evicts with the same entry.chars, and a budget of exactly one entry's key
// must reject that entry outright.
func TestTranscriptBodyRenderCacheChargesKeys(t *testing.T) {
	cache := newTranscriptBodyRenderCache(1024, 64)
	key := strings.Repeat("k", 60)
	value := "short"
	cache.render(key, true, 0, func() (string, []transcriptSelectableLine) { return value, nil })
	if len(cache.items) != 0 {
		t.Fatalf("entry with %d-byte key + %d-byte value stored under a %d-byte budget; keys are not charged", len(key), len(value), 64)
	}
	// The same value under a budget that covers value+key must store, and the
	// retained total must equal value+key so eviction subtracts consistently.
	cache = newTranscriptBodyRenderCache(1024, 70)
	cache.render(key, true, 0, func() (string, []transcriptSelectableLine) { return value, nil })
	if cache.retained != len(key)+len(value) {
		t.Fatalf("retained = %d, want %d (key+value)", cache.retained, len(key)+len(value))
	}
}

// duplicateReasoningTestModel builds an alt-screen model with two
// byte-identical collapsed reasoning rows (empty ids, distinct indices) at a
// height where BOTH headers are visible without scrolling. It returns the
// transcript indices of the first and second reasoning rows.
func duplicateReasoningTestModel(t *testing.T) (model, int, int) {
	t.Helper()
	m := transcriptViewTestModel()
	m.altScreen = true
	m.height = 24
	text := strings.Repeat("same thought. ", 40)
	// Both reasoning rows share the active run's runID so their content-
	// addressed cache keys are identical — that collision is the premise.
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowUser, text: "first question", final: true, runID: 1},
		transcriptRow{kind: rowReasoning, text: text, runID: 1},
		transcriptRow{kind: rowUser, text: "second question", final: true, runID: 1},
		transcriptRow{kind: rowReasoning, text: text, runID: 1},
	)
	first, second := -1, -1
	for i, row := range m.transcript {
		if row.kind != rowReasoning {
			continue
		}
		if first < 0 {
			first = i
		} else {
			second = i
		}
	}
	if first < 0 || second < 0 {
		t.Fatalf("reasoning rows missing from transcript: %#v", m.transcript)
	}
	width := m.chatColumnWidth()
	items := m.transcriptBodyItems(width, "", false)
	// Sanity: the two rows must be distinct live-tail items whose height-cache
	// keys collide only through identical content, not through index.
	keyFirst, keySecond := "", ""
	for _, item := range items {
		switch item.rowIndex {
		case first:
			keyFirst = item.heightCacheKey
		case second:
			keySecond = item.heightCacheKey
		}
	}
	if keyFirst == "" || keySecond == "" {
		t.Fatalf("reasoning rows missing from body items: %#v", items)
	}
	if keyFirst != keySecond {
		t.Fatalf("test fixture lost its premise: identical rows must share a content key\n first=%s\n second=%s", keyFirst, keySecond)
	}
	_ = m.scrollableTranscriptItemsView("", items, "", width, "")
	return m, first, second
}

// TestDuplicateReasoningHeadersToggleOwnRow is the regression test for the
// review finding that the render cache can make a click on one "Thought"
// header expand another row. Two reasoning rows with identical text (and
// empty ids) produce identical content-addressed cache keys; the cached
// selectable lines carry the FIRST row's index. A click through the live
// hit-test path must always toggle the row under the cursor.
func TestDuplicateReasoningHeadersToggleOwnRow(t *testing.T) {
	old := defaultTranscriptBodyRenders
	defaultTranscriptBodyRenders = newTranscriptBodyRenderCache(
		defaultTranscriptBodyRenderCacheMaxEntries,
		defaultTranscriptBodyRenderCacheMaxCharacters,
	)
	t.Cleanup(func() { defaultTranscriptBodyRenders = old })

	m, first, second := duplicateReasoningTestModel(t)
	// Prime the shared cache, then click the SECOND row's header: the hit must
	// carry the second row's index, not the first row's index that primed the
	// shared entry.
	frame, window, layout := m.transcriptHitTestLayout()
	var headers []transcriptSelectableLine
	for _, line := range layout.selectable {
		if line.toggle && !line.live && line.bodyY >= window.start && line.bodyY < window.end {
			headers = append(headers, line)
		}
	}
	if len(headers) < 2 {
		t.Fatalf("expected two visible reasoning headers, got %d", len(headers))
	}
	// Collapsed reasoning rows render one toggle line each, in transcript
	// order: the second row's header is the last toggle line on screen.
	target := headers[len(headers)-1]
	if target.rowIndex != second {
		t.Fatalf("second reasoning header carries rowIndex %d, want %d (cached identity leaked)", target.rowIndex, second)
	}
	click := testMouseClick(tea.MouseLeft, target.textStart, frame.bodyRect.y+target.bodyY-window.start)
	updated, _, handled := m.handleTranscriptSelectionMouse(click)
	if !handled {
		t.Fatal("click on reasoning header was not handled")
	}
	if !updated.transcript[second].expanded {
		t.Fatalf("clicking the second reasoning header did not expand the second row (%d)", second)
	}
	if updated.transcript[first].expanded {
		t.Fatalf("clicking the second reasoning header expanded the FIRST row (%d; cached rowIndex leaked)", first)
	}
}

// TestExploreGroupClickTargetsOwnRow is the regression test for the grouped
// explore path, whose render-cache key originally covered only the block text
// while the cached lines baked each member row's index. Two byte-identical
// explore groups at different transcript indices must not share a render
// entry: clicking a card in the second group toggles a row of the second
// group.
func TestExploreGroupClickTargetsOwnRow(t *testing.T) {
	old := defaultTranscriptBodyRenders
	defaultTranscriptBodyRenders = newTranscriptBodyRenderCache(
		defaultTranscriptBodyRenderCacheMaxEntries,
		defaultTranscriptBodyRenderCacheMaxCharacters,
	)
	t.Cleanup(func() { defaultTranscriptBodyRenders = old })

	m := transcriptViewTestModel()
	m.altScreen = true
	m.height = 24
	m.transcriptDetailed = false
	// Two identical single-row explore groups separated by a user message
	// (which breaks group contiguity and gives distinct group indices).
	explore := func(run int) []transcriptRow {
		return []transcriptRow{
			{kind: rowUser, text: fmt.Sprintf("question %d", run), final: true, runID: run},
			// The call's detail becomes the card's path hint for read_file.
			{kind: rowToolCall, id: "unused", tool: "read_file", detail: "a.go", runID: run},
			{kind: rowToolResult, id: "unused", tool: "read_file", status: "completed", runID: run},
		}
	}
	m.transcript = append(m.transcript, explore(1)...)
	m.transcript = append(m.transcript, explore(2)...)
	// Locate the two group cards by transcript index (the seeded welcome row
	// offsets everything): each successful explore result is one group.
	groupRows := []int{}
	for i, row := range m.transcript {
		if isSuccessfulExploreResult(row) {
			groupRows = append(groupRows, i)
		}
	}
	if len(groupRows) != 2 || groupRows[0] == groupRows[1] {
		t.Fatalf("expected two explore group rows, got %#v of %#v", groupRows, m.transcript)
	}

	width := m.chatColumnWidth()
	// Prime the render cache through the real path so the SECOND group is the
	// one that would receive a poisoned hit from the FIRST group's entry.
	_ = m.scrollableTranscriptItemsView("", m.transcriptBodyItems(width, "", false), "", width, "")

	_, window, layout := m.transcriptHitTestLayout()
	var cardLines []transcriptSelectableLine
	for _, line := range layout.selectable {
		if strings.Contains(line.text, "Read") && line.bodyY >= window.start && line.bodyY < window.end {
			cardLines = append(cardLines, line)
		}
	}
	if len(cardLines) != 2 {
		t.Fatalf("expected two visible explore card lines, got %d: %#v", len(cardLines), cardLines)
	}
	// Layout order follows bodyY; the last card line sits in the second
	// group's block. Without the identity-specialised key it is a poisoned
	// cache hit carrying the first group's row index.
	second := cardLines[len(cardLines)-1]
	if second.rowIndex != groupRows[1] {
		t.Fatalf("second explore group's card line carries rowIndex %d, want %d (cached identity leaked from the first group)", second.rowIndex, groupRows[1])
	}
	if cardLines[0].rowIndex != groupRows[0] {
		t.Fatalf("first explore group's card line carries rowIndex %d, want %d", cardLines[0].rowIndex, groupRows[0])
	}
}
