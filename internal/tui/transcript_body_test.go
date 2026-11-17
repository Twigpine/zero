package tui

import "testing"

func TestTranscriptBodyItemsRepresentEmptyState(t *testing.T) {
	m := mouseTestModel()
	width := m.chatColumnWidth()

	items := m.transcriptBodyItems(width, "", false)
	layout := layoutTranscriptBodyItems(items)

	if len(layout.spans) != 1 || layout.spans[0].kind != transcriptBodyItemEmpty {
		t.Fatalf("spans = %#v, want one empty-state item", layout.spans)
	}
	if layout.spans[0].height != len(layout.lines) || layout.spans[0].height == 0 {
		t.Fatalf("empty-state span = %#v lines=%d, want positive span covering all lines", layout.spans[0], len(layout.lines))
	}
	if len(layout.selectable) != 0 {
		t.Fatalf("empty state should not expose selectable transcript text: %#v", layout.selectable)
	}
}

func TestTranscriptBodyItemsShiftSelectableLinesByItemStart(t *testing.T) {
	m := mouseTestModel()
	m.transcript = appendRow(m.transcript, rowUser, "hello")
	width := m.chatColumnWidth()

	layout := layoutTranscriptBodyItems(m.transcriptBodyItems(width, "", false))

	if len(layout.spans) != 1 || layout.spans[0].kind != transcriptBodyItemRow {
		t.Fatalf("spans = %#v, want one transcript row item", layout.spans)
	}
	rowSpan := layout.spans[0]
	if rowSpan.height != 2 {
		t.Fatalf("user row height = %d, want blank-delimiter + text", rowSpan.height)
	}
	if len(layout.selectable) != 1 {
		t.Fatalf("selectable lines = %#v, want one user text line", layout.selectable)
	}
	if got, want := layout.selectable[0].bodyY, rowSpan.startY+1; got != want {
		t.Fatalf("selectable bodyY = %d, want item start + user padding = %d", got, want)
	}
	if layout.selectable[0].rowIndex != len(m.transcript)-1 || layout.selectable[0].text != "hello" {
		t.Fatalf("selectable line = %#v, want user row text", layout.selectable[0])
	}
}

func TestTranscriptBodyItemsKeepPendingInterimSelectableLocal(t *testing.T) {
	m := mouseTestModel()
	m.pending = true
	m.streamingReasoning = []byte("private thought")
	width := m.chatColumnWidth()

	layout := layoutTranscriptBodyItems(m.transcriptBodyItems(width, "", false))

	if len(layout.spans) != 2 {
		t.Fatalf("spans = %#v, want separator plus pending interim", layout.spans)
	}
	if layout.spans[0].kind != transcriptBodyItemSeparator || layout.spans[0].height != 1 {
		t.Fatalf("first span = %#v, want one-line separator", layout.spans[0])
	}
	pendingSpan := layout.spans[1]
	if pendingSpan.kind != transcriptBodyItemPendingInterim || pendingSpan.height == 0 {
		t.Fatalf("pending span = %#v, want rendered interim item", pendingSpan)
	}
	if len(layout.selectable) != 1 || !layout.selectable[0].live || !layout.selectable[0].toggle {
		t.Fatalf("selectable lines = %#v, want live streaming reasoning toggle", layout.selectable)
	}
	if layout.selectable[0].bodyY != pendingSpan.startY {
		t.Fatalf("streaming selectable bodyY = %d, want pending item start %d", layout.selectable[0].bodyY, pendingSpan.startY)
	}
}

func TestTranscriptBodyLayoutVisibleLinesUsesViewportWindow(t *testing.T) {
	layout := transcriptBodyLayout{lines: []string{"zero", "one", "two", "three"}}
	window := newTranscriptViewport(layout.totalLines(), 2, 1).window()

	got := layout.visibleLines(window)
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("visible lines = %#v, want [one two]", got)
	}
	got[0] = "mutated"
	if layout.lines[1] != "one" {
		t.Fatalf("visibleLines should return a copy, layout lines = %#v", layout.lines)
	}
}

func TestMeasureTranscriptBodyItemsUsesHeightCache(t *testing.T) {
	cache := newTranscriptBodyHeightCache(8)
	renders := 0
	item := transcriptBodyItem{
		kind:              transcriptBodyItemRow,
		rowIndex:          7,
		heightCacheKey:    "stable-row",
		heightCacheStable: true,
		render: func(int) transcriptBodyRenderedItem {
			renders++
			return transcriptBodyRenderedItem{lines: []string{"one", "two"}}
		},
	}

	first := measureTranscriptBodyItems([]transcriptBodyItem{item}, cache)
	second := measureTranscriptBodyItems([]transcriptBodyItem{item}, cache)

	if renders != 1 {
		t.Fatalf("renders = %d, want first measure only", renders)
	}
	if first.totalLines() != 2 || second.totalLines() != 2 {
		t.Fatalf("total lines = %d/%d, want cached height 2", first.totalLines(), second.totalLines())
	}
}

func TestVisibleTranscriptBodyLayoutRendersOnlyIntersectingItems(t *testing.T) {
	cache := newTranscriptBodyHeightCache(8)
	renders := [3]int{}
	items := []transcriptBodyItem{
		countingTranscriptBodyItem("a", []string{"a0", "a1"}, &renders[0]),
		countingTranscriptBodyItem("b", []string{"b0", "b1"}, &renders[1]),
		countingTranscriptBodyItem("c", []string{"c0", "c1"}, &renders[2]),
	}

	metrics := measureTranscriptBodyItems(items, cache)
	window := transcriptViewportWindow{start: 2, end: 4, height: 2}
	layout := layoutVisibleTranscriptBodyItems(items, metrics, window)

	if got, want := renders, [3]int{1, 2, 1}; got != want {
		t.Fatalf("renders after first visible layout = %#v, want %#v", got, want)
	}
	if len(layout.lines) != 2 || layout.lines[0] != "b0" || layout.lines[1] != "b1" {
		t.Fatalf("visible lines = %#v, want b item only", layout.lines)
	}

	metrics = measureTranscriptBodyItems(items, cache)
	_ = layoutVisibleTranscriptBodyItems(items, metrics, window)

	if got, want := renders, [3]int{1, 3, 1}; got != want {
		t.Fatalf("renders after cached measure = %#v, want only visible item rerendered %#v", got, want)
	}
}

func TestVisibleTranscriptBodyLayoutKeepsSelectableBodyYAbsolute(t *testing.T) {
	items := []transcriptBodyItem{
		transcriptBlankBodyItem(),
		{
			kind:              transcriptBodyItemRow,
			rowIndex:          3,
			heightCacheKey:    "selectable-row",
			heightCacheStable: true,
			render: func(startBodyY int) transcriptBodyRenderedItem {
				return transcriptBodyRenderedItem{
					lines: []string{"top", "hit", "bottom"},
					selectable: []transcriptSelectableLine{{
						bodyY:    startBodyY + 1,
						rowIndex: 3,
						text:     "hit",
					}},
				}
			},
		},
	}
	metrics := measureTranscriptBodyItems(items, newTranscriptBodyHeightCache(8))
	window := transcriptViewportWindow{start: 2, end: 3, height: 1}

	layout := layoutVisibleTranscriptBodyItems(items, metrics, window)

	if len(layout.lines) != 1 || layout.lines[0] != "hit" {
		t.Fatalf("visible lines = %#v, want selected item line", layout.lines)
	}
	if len(layout.selectable) != 1 || layout.selectable[0].bodyY != 2 {
		t.Fatalf("selectable = %#v, want absolute bodyY 2", layout.selectable)
	}
}

func TestScrollableTranscriptItemsViewMatchesFullLayout(t *testing.T) {
	m := mouseTestModel()
	m.height = 14
	m.chatScrollOffset = 2
	m.transcript = appendRow(m.transcript, rowUser, "please inspect this request")
	m.transcript = appendRow(m.transcript, rowAssistant, "first response line\nsecond response line\nthird response line")
	m.transcript = appendRow(m.transcript, rowUser, "follow up")
	width := m.chatColumnWidth()
	header := m.pinnedTitleBar(width)
	footer := m.footerView(width)
	items := m.transcriptBodyItems(width, "", false)
	full := layoutTranscriptBodyItems(items)

	got := m.scrollableTranscriptItemsView(header, items, footer, width, "")
	want := m.scrollableTranscriptLayoutView(header, full, footer, width, "")

	if got != want {
		t.Fatalf("visible item view changed output\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func countingTranscriptBodyItem(key string, lines []string, renders *int) transcriptBodyItem {
	return transcriptBodyItem{
		kind:              transcriptBodyItemRow,
		heightCacheKey:    key,
		heightCacheStable: true,
		render: func(int) transcriptBodyRenderedItem {
			(*renders)++
			return transcriptBodyRenderedItem{lines: append([]string(nil), lines...)}
		},
	}
}
