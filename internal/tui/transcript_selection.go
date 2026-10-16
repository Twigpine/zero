package tui

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/tools"
)

type transcriptSelectionPoint struct {
	bodyY int
	x     int
}

type transcriptSelectionState struct {
	active bool
	// dragging is true from press until release — narrower than active, which
	// stays true through the async copy-command grace window after release (a
	// non-empty selection isn't reset to {} until transcriptCopiedMsg lands).
	// mouseMotion gates on THIS, not active, so a genuine hover motion arriving
	// in that post-release window doesn't get misrouted as a drag continuation.
	// It also deliberately does NOT require msg's own Button field to be
	// non-None on each motion event: some terminals don't restate the button on
	// every motion report during a real drag (see
	// TestTranscriptSelectionUpdatesOnGenericMotion), so trusting the
	// per-event button field would break those terminals — dragging tracks the
	// app's own press/release bracket instead.
	dragging bool
	anchor   transcriptSelectionPoint
	cursor   transcriptSelectionPoint
}

// transcriptRenderInteraction lets settled-item render closures read the
// current selection and hover state without rebuilding the cached descriptors.
// The mutex keeps the shared cache state safe even if a renderer inspects a
// model copy concurrently with an update.
type transcriptRenderInteraction struct {
	mu        sync.Mutex
	selection transcriptSelectionState
	hover     hoverTarget
}

func (s *transcriptRenderInteraction) set(selection transcriptSelectionState, hover hoverTarget) {
	s.mu.Lock()
	s.selection = selection
	s.hover = hover
	s.mu.Unlock()
}

func (s *transcriptRenderInteraction) get() (transcriptSelectionState, hoverTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selection, s.hover
}

type transcriptSelectableLine struct {
	bodyY     int
	rowIndex  int
	textStart int
	text      string
	toggle    bool
	live      bool
	// permOption marks a clickable permission-popup choice; permChoice is the
	// decision a left-click on this row resolves. These rows carry no selectable
	// text (they are buttons, not content).
	permOption bool
	permChoice permissionDecision
	// specialistCard marks a clickable specialist card row. specialistID is
	// the childSessionID to drill into on click or Enter.
	specialistCard bool
	specialistID   string
}

type transcriptCopiedMsg struct {
	chars int
	// err is set when neither the native clipboard nor the OSC52 fallback landed
	// the copy, so the status line can report the failure instead of "Copied!".
	err error
}

type transcriptCopyStatusExpiredMsg struct {
	seq int
}

type transcriptBodyItemKind int

const (
	transcriptBodyItemTitle transcriptBodyItemKind = iota
	transcriptBodyItemEmpty
	transcriptBodyItemSeparator
	transcriptBodyItemRule
	transcriptBodyItemRow
	transcriptBodyItemPendingPrompt
	transcriptBodyItemPendingInterim
	transcriptBodyItemSpecReview
)

type transcriptBodyItem struct {
	kind              transcriptBodyItemKind
	rowIndex          int
	heightCacheKey    string
	heightCacheStable bool
	render            func(startBodyY int) transcriptBodyRenderedItem
}

type transcriptBodyRenderedItem struct {
	lines      []string
	selectable []transcriptSelectableLine
}

type transcriptBodyItemSpan struct {
	kind     transcriptBodyItemKind
	rowIndex int
	startY   int
	height   int
}

type transcriptBodyLayout struct {
	lines      []string
	selectable []transcriptSelectableLine
	spans      []transcriptBodyItemSpan
}

func (l transcriptBodyLayout) String() string {
	return strings.Join(l.lines, "\n")
}

func (l transcriptBodyLayout) totalLines() int {
	if len(l.spans) > 0 {
		last := l.spans[len(l.spans)-1]
		return last.startY + last.height
	}
	return len(l.lines)
}

// padTranscriptBodyLines left-indents transcript body rows when a non-zero
// gutter is configured. It is horizontal only — it never changes the line count,
// so the width-keyed height cache stays valid. Two-column mode right-pads the
// chat block to the column width in joinColumns; single-column leaves any
// remaining cells blank.
func padTranscriptBodyLines(lines []string, gutter int) []string {
	if gutter <= 0 {
		return lines
	}
	pad := strings.Repeat(" ", gutter)
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return lines
}

// shiftSelectableX bumps each selectable line's textStart by the gutter so
// click-to-select and the highlight stay aligned with the indented glyphs (the
// text now begins gutter cells further right on screen).
func shiftSelectableX(lines []transcriptSelectableLine, gutter int) []transcriptSelectableLine {
	if gutter <= 0 {
		return lines
	}
	for i := range lines {
		lines[i].textStart += gutter
	}
	return lines
}

// finalizeTranscriptBodyRow indents a rendered row by the reading-column gutter,
// shifts its selectable spans to match, and THEN paints the selection highlight —
// so the highlight is computed in the same shifted coordinate the mouse maps to
// and lands exactly where the user selected (instead of gutter cells off).
func (m model) finalizeTranscriptBodyRow(rendered string, selectable []transcriptSelectableLine, gutter int, startBodyY int) transcriptBodyRenderedItem {
	if m.transcriptInteraction != nil {
		m.transcriptSelection, m.hover = m.transcriptInteraction.get()
	}
	lines := padTranscriptBodyLines(viewLines(rendered), gutter)
	shifted := shiftSelectableX(selectable, gutter)
	if m.transcriptSelection.active {
		lines = viewLines(m.renderRenderedSelection(strings.Join(lines, "\n"), shifted, startBodyY))
	} else if m.hover.kind == hoverTranscript {
		// Mutually exclusive with the selection highlight above: transcriptSelection
		// stays active for the lifetime of a persisted (copied) selection, not just
		// during the drag — so hover correctly stays suppressed over old selected
		// text too, not only mid-drag.
		lines = viewLines(m.renderHoverHighlight(strings.Join(lines, "\n"), shifted, startBodyY))
	}
	return transcriptBodyRenderedItem{lines: lines, selectable: shifted}
}

// renderHoverHighlight re-styles the WHOLE line at m.hover.bodyY in the hover
// accent (see renderRenderedSelection for the sibling selection-highlight logic,
// which this mirrors). Only a clickable line (a specialist card or a
// collapse/expand toggle header) is eligible — if the hovered bodyY no longer
// matches one (content shifted beneath a stale hover target), this is a no-op,
// which self-heals a hover left over from before a transcript update.
func (m model) renderHoverHighlight(rendered string, selectable []transcriptSelectableLine, startBodyY int) string {
	index := m.hover.bodyY - startBodyY
	if index < 0 {
		return rendered
	}
	lines := viewLines(rendered)
	if index >= len(lines) {
		return rendered
	}
	matched := false
	for _, line := range selectable {
		if line.bodyY == m.hover.bodyY && (line.specialistCard || line.toggle) {
			matched = true
			break
		}
	}
	if !matched {
		return rendered
	}
	lines[index] = zeroTheme.hover.Render(ansi.Strip(lines[index]))
	return strings.Join(lines, "\n")
}

func (m model) transcriptBodyItems(width int, emptyOverlay string, detailed bool) []transcriptBodyItem {
	if m.transcriptInteraction != nil {
		m.transcriptInteraction.set(m.transcriptSelection, m.hover)
	}
	// Steady-state alt-screen frames (notably the idle cursor blink) can reuse
	// the settled list directly. Keeping this fast path outside the closure-heavy
	// builder also prevents the large model receiver from escaping to the heap.
	if m.altScreen && !detailed && !m.fileView.active && !m.pending && m.pendingSpecReview == nil &&
		m.flushedAny && m.flushed == len(m.transcript) &&
		m.altScreenSettledWidth == width && m.altScreenSettledFrontier == m.flushed {
		return m.altScreenSettledItems
	}
	return m.buildTranscriptBodyItems(width, emptyOverlay, detailed)
}

func (m model) buildTranscriptBodyItems(width int, emptyOverlay string, detailed bool) []transcriptBodyItem {
	// File drill-in: the chat column's body swaps to the viewed file's
	// diff/content. Swapping HERE (the single source every consumer reads) keeps
	// the viewport, scroll engine, renderer, and mouse hit-tests consistent.
	if m.fileView.active {
		return m.fileViewBodyItems(width)
	}
	items := []transcriptBodyItem{}
	// Transcript ROWS render at the full chat width; row/status glyphs provide
	// structure without adding another body margin. Block items (title bar, empty
	// state, prompts) keep the full column width below.
	contentWidth := transcriptContentWidth(width)
	gutter := transcriptGutter(width)

	// The inline title bar prints once into scrollback on the first WindowSizeMsg;
	// until then it renders managed so the surface never appears headless.
	if m.titleBarInTranscriptBody() {
		items = append(items, transcriptBlockBodyItem(transcriptBodyItemTitle, -1, m.titleBar(width)))
	}

	previousKind := rowWelcome
	havePreviousKind := false
	if m.transcriptEmpty() && !m.pending {
		if emptyOverlay != "" {
			items = append(items, transcriptBlockBodyItem(transcriptBodyItemEmpty, -1, m.emptyStateWithOverlay(width, emptyOverlay)))
		} else {
			items = append(items, transcriptBlockBodyItem(transcriptBodyItemEmpty, -1, m.emptyState(width)))
		}
	} else {
		shownAny := false
		// The detailed view shows the full transcript from index 0, not
		// the managed region after m.flushed.
		startIdx := m.flushed
		if detailed {
			startIdx = 0
		}
		useSettledCache := m.altScreen && !detailed &&
			m.altScreenSettledWidth == width &&
			m.altScreenSettledFrontier == m.flushed
		if useSettledCache {
			// The cache reserves scratch capacity for live-tail descriptors, avoiding
			// an O(history) slice copy on every streaming/spinner frame.
			items = m.altScreenSettledItems[:len(m.altScreenSettledItems)]
		} else if m.altScreen && !detailed {
			// A directly-constructed model (not yet passed through Update), or an
			// explicitly invalidated cache, must still render complete history.
			// The next settle rebuilds the cache for subsequent frames.
			startIdx = 0
		}
		// Dependencies for an unflushed row cannot live wholly before the
		// frontier: such a row would have blocked the frontier. Build context only
		// for the live tail instead of rescanning settled history each frame.
		rc := buildRowContext(m.transcript[startIdx:])
		renderRowFn := transcriptRowDispatchFn(m.renderTranscriptRow)
		if detailed {
			renderRowFn = transcriptRowDispatchFn(m.renderTranscriptDetailedRow)
		}
		if startIdx > 0 {
			previousKind, havePreviousKind = m.flushedPreviousKind, m.flushedHavePreviousKind
		} else {
			previousKind, havePreviousKind = previousVisibleTranscriptKind(m.transcript, startIdx, rc)
		}
		specialistSummaryEmitted := false
		for index := startIdx; index < len(m.transcript); index++ {
			row := m.transcript[index]
			// A welcome row carries no Lime visual (the empty state replaced it)
			// and a resolved tool call collapses into its result's card.
			if row.kind == rowWelcome || rc.skip(row) {
				continue
			}
			if isSuccessfulExploreResult(row) {
				groupRows, groupIndices, nextIndex := collectExploreResultGroup(m.transcript, index, rc)
				if len(groupRows) > 0 {
					if (shownAny || m.flushedAny) && startsTurn(row.kind) {
						items = append(items, transcriptBlankBodyItem())
					}
					if shownAny && havePreviousKind && needsSeparatorBeforeToolCard(previousKind, row.kind) {
						items = append(items, transcriptBlankBodyItem())
					}
					firstRowIndex := groupIndices[0]
					groupRowsCopy := append([]transcriptRow(nil), groupRows...)
					groupIndicesCopy := append([]int(nil), groupIndices...)
					block := m.renderExploreResultGroup(groupRowsCopy, contentWidth, rc)
					items = append(items, transcriptBodyItem{
						kind:              transcriptBodyItemRow,
						rowIndex:          firstRowIndex,
						heightCacheKey:    transcriptBlockBodyHeightCacheKey(transcriptBodyItemRow, block),
						heightCacheStable: true,
						render: func(startBodyY int) transcriptBodyRenderedItem {
							rendered := m.renderExploreResultGroup(groupRowsCopy, contentWidth, rc)
							selectable := make([]transcriptSelectableLine, 0, len(groupIndicesCopy)+1)
							for offset, line := range viewLines(rendered) {
								rowIndex := firstRowIndex
								if offset > 0 && offset-1 < len(groupIndicesCopy) {
									rowIndex = groupIndicesCopy[offset-1]
								}
								if meta, ok := selectableLineFromRenderedLine(rowIndex, startBodyY+offset, line, false); ok {
									selectable = append(selectable, meta)
								}
							}
							return m.finalizeTranscriptBodyRow(rendered, selectable, gutter, startBodyY)
						},
					})
					shownAny = true
					previousKind = row.kind
					havePreviousKind = true
					index = nextIndex - 1
					continue
				}
			}
			// Blank-line separation before turns, including between flushed
			// history and the first live row.
			if (shownAny || m.flushedAny) && startsTurn(row.kind) {
				if havePreviousKind && shouldRuleBeforeTurn(previousKind, row.kind) {
					items = append(items, transcriptRuleBodyItem(contentWidth, gutter))
				} else {
					items = append(items, transcriptBlankBodyItem())
				}
			}
			if (shownAny || (m.flushedAny && havePreviousKind)) && previousKind == rowUser && row.kind == rowReasoning {
				items = append(items, transcriptBlankBodyItem())
			}
			// Breathing room between back-to-back tool cards in the same turn: a
			// tool result collapses its call into one card, so consecutive cards
			// would otherwise stack with no gap (the dense "wall" look). One blank
			// line between them matches the reference agents. Turn-starters are
			// separated above, so this only fires tool-card -> tool-card.
			if shownAny && havePreviousKind && needsSeparatorBeforeToolCard(previousKind, row.kind) {
				items = append(items, transcriptBlankBodyItem())
			}
			// A "Thought for X" reasoning header opens the next think→act group, so
			// give it a blank above when it follows a tool card. Reasoning interleaves
			// the tool cards (tool → thought → tool …), which breaks the
			// tool-card→tool-card rule above; without this the groups pack into a wall.
			if shownAny && havePreviousKind && row.kind == rowReasoning && isToolCardKind(previousKind) {
				items = append(items, transcriptBlankBodyItem())
			}
			// The plan panel is no longer injected inline here — it is pinned
			// above the composer (see footerView) so a streaming turn can't push
			// it off-screen.
			// Inject the specialist summary line once, before the first
			// specialist card in this turn's contiguous group.
			if row.kind == rowSpecialist && !specialistSummaryEmitted {
				specialistSummaryEmitted = true
				specialists := m.specialists.all()
				if len(specialists) == 0 {
					// Fall back to the specialist info carried by the
					// transcript rows themselves (covers tests and any path
					// where the tracker has been cleared).
					for j := index; j < len(m.transcript); j++ {
						r := m.transcript[j]
						if r.kind != rowSpecialist || r.specialistInfo == nil {
							break
						}
						specialists = append(specialists, *r.specialistInfo)
					}
				}
				summary := renderSpecialistSummary(specialists, m.spinnerGlyph())
				if summary != "" {
					items = append(items, transcriptBlockBodyItem(transcriptBodyItemRow, -1, summary))
					items = append(items, transcriptBlankBodyItem())
				}
			}
			rowIndex, transcriptRow := index, row
			bodyCap := cardBodyMaxLines
			if detailed {
				bodyCap = 0
			}
			heightCacheKey, heightCacheStable := m.transcriptRowBodyHeightCacheKeyOpts(transcriptRow, contentWidth, rc, bodyCap)
			items = append(items, transcriptBodyItem{
				kind:              transcriptBodyItemRow,
				rowIndex:          rowIndex,
				heightCacheKey:    heightCacheKey,
				heightCacheStable: heightCacheStable,
				render: func(startBodyY int) transcriptBodyRenderedItem {
					rendered, selectable := renderRowFn(rowIndex, transcriptRow, contentWidth, rc, startBodyY)
					return m.finalizeTranscriptBodyRow(rendered, selectable, gutter, startBodyY)
				},
			})
			shownAny = true
			previousKind = row.kind
			havePreviousKind = true
		}
		// The plan panel is pinned above the composer (footerView), not injected
		// into the scrolling body, so there is nothing to emit here anymore.
	}

	if m.pending {
		pendingShowsAssistantText := m.pendingPermission == nil && m.pendingAskUser == nil && strings.TrimSpace(m.streamingTextString()) != ""
		if pendingShowsAssistantText && havePreviousKind && shouldRuleBeforeTurn(previousKind, rowAssistant) {
			items = append(items, transcriptRuleBodyItem(contentWidth, gutter))
		} else {
			items = append(items, transcriptBlankBodyItem())
		}
		switch {
		case m.pendingPermission != nil:
			perm := m.pendingPermission
			items = append(items, transcriptBodyItem{
				kind:              transcriptBodyItemPendingPrompt,
				rowIndex:          -1,
				heightCacheStable: false, // the highlight changes with the cursor
				render: func(startBodyY int) transcriptBodyRenderedItem {
					block, offsets := renderFocusedPermissionPrompt(perm.request, perm.cursor, perm.typing, m.input.Value(), width)
					options := permissionOptions(perm.request)
					selectable := make([]transcriptSelectableLine, 0, len(offsets))
					for index, offset := range offsets {
						if index >= len(options) {
							break
						}
						selectable = append(selectable, transcriptSelectableLine{
							bodyY:      startBodyY + offset,
							rowIndex:   -1,
							permOption: true,
							permChoice: options[index].choice,
						})
					}
					return transcriptBodyRenderedItem{lines: viewLines(block), selectable: selectable}
				},
			})
		case m.pendingAskUser != nil:
			// The ask-user questionnaire renders in the composer/footer region
			// (footerView), not as a scrolling transcript card — nothing to emit here.
		default:
			items = append(items, transcriptBodyItem{
				kind:     transcriptBodyItemPendingInterim,
				rowIndex: -1,
				render: func(startBodyY int) transcriptBodyRenderedItem {
					// Streaming text shares the reading column so it doesn't snap
					// width when the turn finalizes into a row.
					return m.finalizeTranscriptBodyRow(m.interimBlock(contentWidth), m.renderSelectableStreamingReasoning(contentWidth, startBodyY), gutter, startBodyY)
				},
			})
		}
	}
	if m.pendingSpecReview != nil {
		items = append(items, transcriptBlankBodyItem())
		items = append(items, transcriptBlockBodyItem(transcriptBodyItemSpecReview, -1, renderFocusedSpecReviewPrompt(*m.pendingSpecReview, width)))
	}

	return items
}

func transcriptBlockBodyItem(kind transcriptBodyItemKind, rowIndex int, block string) transcriptBodyItem {
	return transcriptBodyItem{
		kind:              kind,
		rowIndex:          rowIndex,
		heightCacheKey:    transcriptBlockBodyHeightCacheKey(kind, block),
		heightCacheStable: true,
		render: func(int) transcriptBodyRenderedItem {
			return transcriptBodyRenderedItem{lines: viewLines(block)}
		},
	}
}

func transcriptBlankBodyItem() transcriptBodyItem {
	return transcriptBodyItem{
		kind:              transcriptBodyItemSeparator,
		rowIndex:          -1,
		heightCacheKey:    "transcript-body-height:v1:separator",
		heightCacheStable: true,
		render: func(int) transcriptBodyRenderedItem {
			return transcriptBodyRenderedItem{lines: []string{""}}
		},
	}
}

func transcriptRuleBodyItem(width int, gutter int) transcriptBodyItem {
	if width < 1 {
		width = 1
	}
	return transcriptBodyItem{
		kind:              transcriptBodyItemRule,
		rowIndex:          -1,
		heightCacheKey:    "transcript-body-height:v1:rule:" + strconv.Itoa(width) + ":" + strconv.Itoa(gutter),
		heightCacheStable: true,
		render: func(int) transcriptBodyRenderedItem {
			rule := zeroTheme.line.Render(strings.Repeat("─", width))
			return transcriptBodyRenderedItem{lines: padTranscriptBodyLines([]string{rule, ""}, gutter)}
		},
	}
}

func isSuccessfulExploreResult(row transcriptRow) bool {
	return row.kind == rowToolResult && !row.expanded && row.status != tools.StatusError && isExploreTool(toolRowName(row))
}

func collectExploreResultGroup(rows []transcriptRow, start int, rc rowContext) ([]transcriptRow, []int, int) {
	groupRows := []transcriptRow{}
	groupIndices := []int{}
	for index := start; index < len(rows); index++ {
		row := rows[index]
		if row.kind == rowWelcome || rc.skip(row) {
			continue
		}
		if !isSuccessfulExploreResult(row) {
			return groupRows, groupIndices, index
		}
		groupRows = append(groupRows, row)
		groupIndices = append(groupIndices, index)
	}
	return groupRows, groupIndices, len(rows)
}

func (m model) renderExploreResultGroup(rows []transcriptRow, width int, rc rowContext) string {
	if len(rows) == 0 {
		return ""
	}
	opts := cardRenderOptions{bodyCap: cardBodyMaxLines, cwd: m.cwd}
	body := make([]string, 0, len(rows))
	for index, row := range rows {
		marker := "├"
		if index == len(rows)-1 {
			marker = "└"
		}
		key := rcKey(row.runID, row.id)
		body = append(body, exploreCardLine(toolRowName(row), rc.hints[key], rc.args[key], row.detail, width, opts, marker))
	}
	head := zeroTheme.green.Bold(true).Render("Explored")
	return toolCard(head, zeroTheme.green.Render("•"), body, "", zeroTheme.line, width)
}

// transcriptBodyItemsFromRows builds body items from an arbitrary set of
// transcript rows (used by the subchat drill-in view to render a child
// session's events). It mirrors the main loop's logic but operates on the
// provided rows instead of m.transcript, and skips pending/permission state.
func (m model) transcriptBodyItemsFromRows(rows []transcriptRow, width int) []transcriptBodyItem {
	items := []transcriptBodyItem{}
	if len(rows) == 0 {
		items = append(items, transcriptBlockBodyItem(transcriptBodyItemEmpty, -1, "No events in this subagent session."))
		return items
	}
	contentWidth := transcriptContentWidth(width)
	gutter := transcriptGutter(width)
	rc := buildRowContext(rows)
	shownAny := false
	var previousKind rowKind
	havePreviousKind := false
	for index, row := range rows {
		if row.kind == rowWelcome || rc.skip(row) {
			continue
		}
		if shownAny && startsTurn(row.kind) {
			if havePreviousKind && shouldRuleBeforeTurn(previousKind, row.kind) {
				items = append(items, transcriptRuleBodyItem(contentWidth, gutter))
			} else {
				items = append(items, transcriptBlankBodyItem())
			}
		}
		if shownAny && havePreviousKind && previousKind == rowUser && row.kind == rowReasoning {
			items = append(items, transcriptBlankBodyItem())
		}
		rowIndex, transcriptRow := index, row
		items = append(items, transcriptBodyItem{
			kind:           transcriptBodyItemRow,
			rowIndex:       rowIndex,
			heightCacheKey: "subchat:" + strconv.Itoa(index) + ":" + strconv.Itoa(contentWidth),
			render: func(startBodyY int) transcriptBodyRenderedItem {
				rendered, selectable := m.renderTranscriptRow(rowIndex, transcriptRow, contentWidth, rc, startBodyY)
				return m.finalizeTranscriptBodyRow(rendered, selectable, gutter, startBodyY)
			},
		})
		shownAny = true
		previousKind = row.kind
		havePreviousKind = true
	}
	return items
}

func layoutTranscriptBodyItems(items []transcriptBodyItem) transcriptBodyLayout {
	layout := transcriptBodyLayout{}
	for _, item := range items {
		startY := len(layout.lines)
		rendered := renderTranscriptBodyItem(item, startY)
		layout.lines = append(layout.lines, rendered.lines...)
		layout.selectable = append(layout.selectable, rendered.selectable...)
		layout.spans = append(layout.spans, transcriptBodyItemSpan{
			kind:     item.kind,
			rowIndex: item.rowIndex,
			startY:   startY,
			height:   len(rendered.lines),
		})
	}
	return layout
}

func measureTranscriptBodyItems(items []transcriptBodyItem, cache *transcriptBodyHeightCache) transcriptBodyLayout {
	layout := transcriptBodyLayout{}
	startY := 0
	for _, item := range items {
		height := transcriptBodyItemHeight(item, cache)
		layout.spans = append(layout.spans, transcriptBodyItemSpan{
			kind:     item.kind,
			rowIndex: item.rowIndex,
			startY:   startY,
			height:   height,
		})
		startY += height
	}
	return layout
}

func layoutVisibleTranscriptBodyItems(items []transcriptBodyItem, metrics transcriptBodyLayout, window transcriptViewportWindow) transcriptBodyLayout {
	layout := transcriptBodyLayout{spans: append([]transcriptBodyItemSpan(nil), metrics.spans...)}
	if window.end <= window.start {
		return layout
	}
	for index, item := range items {
		if index >= len(metrics.spans) {
			break
		}
		span := metrics.spans[index]
		spanEnd := span.startY + span.height
		if spanEnd <= window.start || span.startY >= window.end {
			continue
		}
		rendered := renderTranscriptBodyItem(item, span.startY)
		localStart := clampInt(window.start-span.startY, 0, len(rendered.lines))
		localEnd := clampInt(window.end-span.startY, localStart, len(rendered.lines))
		layout.lines = append(layout.lines, rendered.lines[localStart:localEnd]...)
		for _, line := range rendered.selectable {
			if line.bodyY >= window.start && line.bodyY < window.end {
				layout.selectable = append(layout.selectable, line)
			}
		}
	}
	return layout
}

func transcriptBodyItemHeight(item transcriptBodyItem, cache *transcriptBodyHeightCache) int {
	if item.heightCacheStable {
		if height, ok := cache.get(item.heightCacheKey); ok {
			return height
		}
	}
	rendered := renderTranscriptBodyItem(item, 0)
	height := len(rendered.lines)
	if item.heightCacheStable {
		cache.set(item.heightCacheKey, height)
	}
	return height
}

func renderTranscriptBodyItem(item transcriptBodyItem, startBodyY int) transcriptBodyRenderedItem {
	if item.render == nil {
		return transcriptBodyRenderedItem{}
	}
	return item.render(startBodyY)
}

func transcriptBlockBodyHeightCacheKey(kind transcriptBodyItemKind, block string) string {
	var b strings.Builder
	appendRenderCacheField(&b, "transcript-body-block-height-v1")
	appendRenderCacheField(&b, strconv.Itoa(int(kind)))
	appendRenderCacheField(&b, block)
	return b.String()
}

// rowRenderFn abstracts row rendering for normal vs detailed mode:
// m.renderRow for normal (bodyCap: cardBodyMaxLines) and m.renderRowDetailed
// for detailed (bodyCap: 0).
type rowRenderFn func(transcriptRow, int, rowContext) string

// transcriptRowDispatchFn dispatches row-kind rendering for the transcript body
// pipeline, producing rendered text and selectable-line geometry.
type transcriptRowDispatchFn func(int, transcriptRow, int, rowContext, int) (string, []transcriptSelectableLine)

func (m model) transcriptRowBodyHeightCacheKeyOpts(row transcriptRow, width int, rc rowContext, bodyCap int) (string, bool) {
	opts := cardRenderOptions{bodyCap: bodyCap, cwd: m.cwd}
	rowKey, stable := m.renderRowCacheKey(row, width, rc, opts, false)
	if rowKey == "" {
		return "", false
	}
	var b strings.Builder
	appendRenderCacheField(&b, "transcript-body-row-height-v1")
	appendRenderCacheField(&b, rowKey)
	return b.String(), stable
}

func (m model) renderTranscriptRow(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int) (string, []transcriptSelectableLine) {
	opts := cardRenderOptions{bodyCap: cardBodyMaxLines, cwd: m.cwd}
	return m.renderTranscriptRowFn(rowIndex, row, width, rc, startBodyY, m.renderRow, opts)
}

// renderTranscriptDetailedRow routes through renderTranscriptRowFn with
// renderRowDetailed (bodyCap: 0) so tool output appears uncapped.
func (m model) renderTranscriptDetailedRow(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int) (string, []transcriptSelectableLine) {
	opts := cardRenderOptions{bodyCap: 0, cwd: m.cwd}
	return m.renderTranscriptRowFn(rowIndex, row, width, rc, startBodyY, m.renderRowDetailed, opts)
}

// renderTranscriptRowFn dispatches row-kind rendering using the provided renderFn.
func (m model) renderTranscriptRowFn(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int, renderFn rowRenderFn, toolOpts cardRenderOptions) (string, []transcriptSelectableLine) {
	switch row.kind {
	case rowUser:
		return m.renderSelectableUserRow(rowIndex, row, width, startBodyY)
	case rowAssistant:
		return m.renderSelectableAssistantRow(rowIndex, row, width, startBodyY)
	case rowReasoning:
		return m.renderSelectableReasoningRow(rowIndex, row, width, startBodyY)
	case rowSystem, rowError, rowToolCall, rowPermission, rowAskUser:
		return m.renderSelectableRenderedRowFn(rowIndex, row, width, rc, startBodyY, renderFn)
	case rowToolResult:
		return m.renderSelectableToolResultRowFn(rowIndex, row, width, rc, startBodyY, renderFn, toolOpts)
	case rowSpecialist:
		return m.renderSelectableSpecialistRowFn(rowIndex, row, width, rc, startBodyY, renderFn)
	default:
		rendered := renderFn(row, width, rc)
		if rendered == "" {
			return "", nil
		}
		selectable := selectableLinesFromRendered(rowIndex, rendered, startBodyY, 0)
		return rendered, selectable
	}
}

// renderSelectableToolResultRow renders the tool result card. Only cards that
// can actually collapse expose a clickable header; always-visible diff cards
// retain normal text selection and never imply an unavailable action.
func (m model) renderSelectableToolResultRowFn(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int, renderFn rowRenderFn, opts cardRenderOptions) (string, []transcriptSelectableLine) {
	rendered := renderFn(row, width, rc)
	if rendered == "" {
		return "", nil
	}
	// Carry the first line's text so a selection dragged through the card copies
	// its label too. It receives a toggle only when this card exposes a collapse
	// affordance in the current transcript mode.
	allLines := viewLines(rendered)
	header := transcriptSelectableLine{bodyY: startBodyY, rowIndex: rowIndex, toggle: toolResultCanToggle(row, width, rc, opts)}
	if len(allLines) > 0 {
		if meta, ok := selectableLineFromRenderedLine(rowIndex, startBodyY, allLines[0], false); ok {
			header.text = meta.text
			header.textStart = meta.textStart
		}
	}
	selectable := []transcriptSelectableLine{header}
	selectable = append(selectable, selectableLinesFromRendered(rowIndex, rendered, startBodyY, 1)...)
	// The selection highlight is painted once, at the body-item level, AFTER the
	// reading-column gutter shift (finalizeTranscriptBodyRow) — in the same shifted
	// coordinate the mouse maps to. Painting it here (unshifted) made the highlight
	// land gutter cells off from where the user clicked.
	return rendered, selectable
}

func toolResultCanToggle(row transcriptRow, width int, rc rowContext, opts cardRenderOptions) bool {
	name := toolRowName(row)
	if opts.bodyCap <= 0 || toolCardAlwaysExpands(name) {
		return false
	}
	failed := row.status == tools.StatusError
	if !failed && looksLikeRedundantConfirmation(row.detail) {
		return false
	}
	collapsedFooter := ""
	if failed || (!isExploreTool(name) && !isLocalControlTool(name)) {
		collapsedFooter = collapsedToolFooter(row.detail)
	}
	if collapsedFooter != "" && !row.expanded {
		return true
	}
	// Explore cards deliberately use their own compact "details" affordance
	// instead of the generic long-output footer. Ask the body renderer whether
	// it exposed that affordance so selection behavior stays coupled to the
	// rendered card rather than matching display text.
	if collapsedFooter == "" && !isExploreTool(name) {
		return false
	}
	bodyOpts := opts
	bodyOpts.expanded = row.expanded
	body := toolCardBody(name, rc.hints[rcKey(row.runID, row.id)], rc.args[rcKey(row.runID, row.id)], row.detail, width, bodyOpts, failed)
	if collapsedFooter != "" && row.expanded && body.footer == "" {
		return true
	}
	return body.canToggle
}

func (m model) renderSelectableRenderedRowFn(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int, renderFn rowRenderFn) (string, []transcriptSelectableLine) {
	rendered := renderFn(row, width, rc)
	if rendered == "" {
		return "", nil
	}
	selectable := selectableLinesFromRendered(rowIndex, rendered, startBodyY, 0)
	// The selection highlight is painted once, at the body-item level, AFTER the
	// reading-column gutter shift (finalizeTranscriptBodyRow) — in the same shifted
	// coordinate the mouse maps to. Painting it here (unshifted) made the highlight
	// land gutter cells off from where the user clicked.
	return rendered, selectable
}

func selectableLinesFromRendered(rowIndex int, rendered string, startBodyY int, skipLeading int) []transcriptSelectableLine {
	lines := viewLines(rendered)
	boxed := renderedLinesHaveBoxBorder(lines)
	selectable := make([]transcriptSelectableLine, 0, len(lines))
	for index, line := range lines {
		if index < skipLeading {
			continue
		}
		meta, ok := selectableLineFromRenderedLine(rowIndex, startBodyY+index, line, boxed)
		if ok {
			selectable = append(selectable, meta)
		}
	}
	return selectable
}

func selectableLineFromRenderedLine(rowIndex int, bodyY int, rendered string, boxed bool) (transcriptSelectableLine, bool) {
	text := ansi.Strip(rendered)
	if isTranscriptStructuralLine(text) {
		return transcriptSelectableLine{}, false
	}

	textStart := 0
	if strings.HasPrefix(text, "│ ") {
		textStart = lipgloss.Width("│ ")
		text = strings.TrimPrefix(text, "│ ")
		if boxed && strings.HasSuffix(text, " │") {
			text = strings.TrimSuffix(text, " │")
		}
	}
	for _, prefix := range []string{"  ├ ", "  └ ", "  │ "} {
		if strings.HasPrefix(text, prefix) {
			textStart += lipgloss.Width(prefix)
			text = strings.TrimPrefix(text, prefix)
			break
		}
	}
	text = strings.TrimRight(text, " ")
	if strings.TrimSpace(text) == "" {
		return transcriptSelectableLine{}, false
	}
	return transcriptSelectableLine{
		bodyY:     bodyY,
		rowIndex:  rowIndex,
		textStart: textStart,
		text:      text,
	}, true
}

func renderedLinesHaveBoxBorder(lines []string) bool {
	hasTop := false
	hasBottom := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(ansi.Strip(line))
		if strings.HasPrefix(trimmed, "╭") && strings.HasSuffix(trimmed, "╮") {
			hasTop = true
		}
		if strings.HasPrefix(trimmed, "╰") && strings.HasSuffix(trimmed, "╯") {
			hasBottom = true
		}
	}
	return hasTop && hasBottom
}

func isTranscriptStructuralLine(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	for _, r := range trimmed {
		switch r {
		case '╭', '╮', '╰', '╯', '┌', '┐', '└', '┘', '─', '━':
			continue
		default:
			return false
		}
	}
	return true
}

func (m model) renderRenderedSelection(rendered string, selectable []transcriptSelectableLine, startBodyY int) string {
	if len(selectable) == 0 {
		return rendered
	}
	byY := make(map[int]transcriptSelectableLine, len(selectable))
	for _, line := range selectable {
		// Toggle headers and specialist cards now carry text, so a selection
		// dragged through them highlights and copies their content. Empty lines and
		// permission buttons (no copyable content) stay excluded.
		if line.text == "" || line.permOption {
			continue
		}
		byY[line.bodyY] = line
	}
	lines := viewLines(rendered)
	for index, line := range lines {
		meta, ok := byY[startBodyY+index]
		if !ok {
			continue
		}
		lines[index] = m.renderTranscriptSelectableStyledLine(meta, line)
	}
	return strings.Join(lines, "\n")
}

func (m model) renderTranscriptSelectableStyledLine(line transcriptSelectableLine, styledLine string) string {
	start, end, ok := m.selectedColumnsForTranscriptLine(line)
	if !ok {
		return styledLine
	}
	absoluteStart := line.textStart + start
	absoluteEnd := line.textStart + end
	lineWidth := ansi.StringWidth(styledLine)
	absoluteStart = clampInt(absoluteStart, 0, lineWidth)
	absoluteEnd = clampInt(absoluteEnd, absoluteStart, lineWidth)
	prefix := ansi.Cut(styledLine, 0, absoluteStart)
	selected := ansi.Strip(ansi.Cut(styledLine, absoluteStart, absoluteEnd))
	suffix := ansi.Cut(styledLine, absoluteEnd, lineWidth)
	return prefix + zeroTheme.selection.Render(selected) + suffix
}

func (m model) renderSelectableSpecialistRowFn(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int, renderFn rowRenderFn) (string, []transcriptSelectableLine) {
	rendered := renderFn(row, width, rc)
	if rendered == "" || row.specialistInfo == nil {
		return "", nil
	}
	lines := viewLines(rendered)
	boxed := renderedLinesHaveBoxBorder(lines)
	selectable := make([]transcriptSelectableLine, len(lines))
	for i := range lines {
		sl := transcriptSelectableLine{
			bodyY:          startBodyY + i,
			rowIndex:       rowIndex,
			specialistCard: true,
			specialistID:   row.specialistInfo.childSessionID,
		}
		if meta, ok := selectableLineFromRenderedLine(rowIndex, startBodyY+i, lines[i], boxed); ok {
			sl.text = meta.text
			sl.textStart = meta.textStart
		}
		selectable[i] = sl
	}
	return rendered, selectable
}

func (m model) renderSelectableUserRow(rowIndex int, row transcriptRow, width int, startBodyY int) (string, []transcriptSelectableLine) {
	contentWidth := userPromptContentWidth(width)
	wrapped := wrapPlainText(row.text, maxInt(1, contentWidth))
	selectable := make([]transcriptSelectableLine, 0, len(wrapped))
	for index, line := range wrapped {
		meta := transcriptSelectableLine{
			bodyY:     startBodyY + index + 1,
			rowIndex:  rowIndex,
			textStart: lipgloss.Width(userPromptPrefix),
			text:      line,
		}
		selectable = append(selectable, meta)
	}
	// The selection highlight is painted once, at the body-item level, AFTER the
	// reading-column gutter shift (finalizeTranscriptBodyRow) — in the same shifted
	// coordinate the mouse maps to. Self-painting here (unshifted) double-painted the
	// highlight gutter cells off from the finalize pass (the "two highlights" bug).
	return m.renderRow(row, width, rowContext{}), selectable
}

func (m model) renderSelectableAssistantRow(rowIndex int, row transcriptRow, width int, startBodyY int) (string, []transcriptSelectableLine) {
	tableMeasure := width
	measure := assistantMeasure(width)
	textStart := 0
	// Committed/selectable row: highlight (the result is cached per row).
	wrapped := renderAssistantMarkdownText(row.text, measure, tableMeasure, true)
	selectable := make([]transcriptSelectableLine, 0, len(wrapped))
	for index, line := range wrapped {
		plainLine := stripMarkdownRenderControls(line)
		meta := transcriptSelectableLine{
			bodyY:     startBodyY + index,
			rowIndex:  rowIndex,
			textStart: textStart,
			text:      plainLine,
		}
		selectable = append(selectable, meta)
	}
	// The selection highlight is painted once, at the body-item level, AFTER the
	// reading-column gutter shift (finalizeTranscriptBodyRow) — in the same shifted
	// coordinate the mouse maps to. Self-painting here (unshifted) double-painted the
	// highlight gutter cells off from the finalize pass (the "two highlights" bug).
	return m.renderRow(row, width, rowContext{}), selectable
}

func (m model) renderSelectableReasoningRow(rowIndex int, row transcriptRow, width int, startBodyY int) (string, []transcriptSelectableLine) {
	lines, selectable := m.renderSelectableReasoningBlock(rowIndex, row.text, row.expanded, false, row.turnElapsed, width, startBodyY)
	return strings.Join(lines, "\n"), selectable
}

func (m model) renderSelectableStreamingReasoning(width int, startBodyY int) []transcriptSelectableLine {
	_, selectable := m.renderSelectableReasoningBlock(-1, m.streamingReasoning, m.streamingReasoningExpanded, true, 0, width, startBodyY)
	for index := range selectable {
		selectable[index].live = true
	}
	return selectable
}

func (m model) renderSelectableReasoningBlock(rowIndex int, text string, expanded bool, running bool, elapsed time.Duration, width int, startBodyY int) ([]string, []transcriptSelectableLine) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	headerPlain := reasoningHeaderText(text, expanded, running, elapsed)
	header := reasoningHeaderLine(text, expanded, running, elapsed)
	headerMeta := transcriptSelectableLine{
		bodyY:     startBodyY,
		rowIndex:  rowIndex,
		textStart: 0,
		text:      headerPlain,
		toggle:    true,
	}
	// Selection highlight is painted once at the body-item level (finalizeTranscriptBodyRow),
	// in the gutter-shifted coordinate the mouse maps to — never self-painted here.
	headerRendered := header
	lines := []string{headerRendered}
	selectable := []transcriptSelectableLine{headerMeta}
	if expanded {
		renderedLines := renderReasoningBodyLines(text, width)
		plainLines := renderAssistantMarkdownPlainText(text, maxInt(16, sayMeasure(width)-2), maxInt(16, sayMeasure(width)-2))
		// A LIVE reasoning block is capped to ~half the screen so its toggle header
		// stays on-screen. This MUST mirror renderReasoningBlock exactly (same cap,
		// same marker line) so the selectable spans stay line-aligned with the
		// displayed lines that finalizeTranscriptBodyRow highlights.
		bodyOffset := 1
		if running {
			if bodyCap := m.liveReasoningBodyCap(); bodyCap > 0 && len(renderedLines) > bodyCap {
				hidden := len(renderedLines) - bodyCap
				selectable = append(selectable, transcriptSelectableLine{bodyY: startBodyY + 1, rowIndex: rowIndex, textStart: 2})
				lines = append(lines, fitStyledLine("  "+reasoningHiddenMarker(hidden), width))
				renderedLines = renderedLines[hidden:]
				if hidden < len(plainLines) {
					plainLines = plainLines[hidden:]
				} else {
					plainLines = nil
				}
				bodyOffset = 2
			}
		}
		for index, line := range renderedLines {
			plainLine := ""
			if index < len(plainLines) {
				plainLine = plainLines[index]
			}
			meta := transcriptSelectableLine{
				bodyY:     startBodyY + index + bodyOffset,
				rowIndex:  rowIndex,
				textStart: 2,
				text:      plainLine,
			}
			selectable = append(selectable, meta)
			rendered := styleAssistantMarkdownLine(line, zeroTheme.sayText)
			lines = append(lines, fitStyledLine("  "+rendered, width))
		}
	}
	return lines, selectable
}

func (m model) selectedColumnsForTranscriptLine(line transcriptSelectableLine) (int, int, bool) {
	if !m.transcriptSelection.active {
		return 0, 0, false
	}
	startPoint, endPoint := orderedTranscriptSelectionPoints(m.transcriptSelection.anchor, m.transcriptSelection.cursor)
	if line.bodyY < startPoint.bodyY || line.bodyY > endPoint.bodyY {
		return 0, 0, false
	}
	lineStart := line.textStart
	lineEnd := line.textStart + lipgloss.Width(line.text)
	start := lineStart
	end := lineEnd
	if line.bodyY == startPoint.bodyY {
		start = clampInt(startPoint.x, lineStart, lineEnd)
	}
	if line.bodyY == endPoint.bodyY {
		end = clampInt(endPoint.x, lineStart, lineEnd)
	}
	if end <= start {
		return 0, 0, false
	}
	return start - line.textStart, end - line.textStart, true
}

func orderedTranscriptSelectionPoints(a transcriptSelectionPoint, b transcriptSelectionPoint) (transcriptSelectionPoint, transcriptSelectionPoint) {
	if a.bodyY < b.bodyY || a.bodyY == b.bodyY && a.x <= b.x {
		return a, b
	}
	return b, a
}

func splitPlainAtDisplayWidth(text string, width int) (string, string) {
	if width <= 0 {
		return "", text
	}
	used := 0
	for index, glyph := range text {
		glyphWidth := lipgloss.Width(string(glyph))
		if used+glyphWidth > width {
			return text[:index], text[index:]
		}
		used += glyphWidth
	}
	return text, ""
}

// transcriptHitTestSource returns the header/body-items/width mouse hit-testing
// must use to match what's actually on screen. It mirrors transcriptView's own
// branching exactly: the subchat drill-in swaps BOTH the header (nav bar instead
// of the pinned title bar) and the row source (the child session's rows instead
// of the parent transcript) — and drops the sidebar-reserved width, since subchat
// is always single-column. Hit-testing against the wrong source is why mouse
// selection previously resolved against transcript rows that weren't even
// visible while viewing a subagent/swarm child session.
func (m model) transcriptHitTestSource() (header string, items []transcriptBodyItem, width int) {
	if m.transcriptDetailed {
		width = chatWidth(m.width)
		header = detailedTranscriptHeader(width) + "\n" + zeroTheme.line.Render(strings.Repeat("-", width))
		items = m.transcriptBodyItems(width, "", true)
		return
	}
	if m.subchat.active {
		width = chatWidth(m.width)
		return renderSubchatNavBar(m.subchat.childSessionTitle, width), m.transcriptBodyItemsFromRows(m.subchat.childRows, width), width
	}
	width = m.chatColumnWidth()
	return m.pinnedTitleBar(width), m.transcriptBodyItems(width, "", false), width
}

// transcriptHitTestBlocked reports whether mouse hit-testing must be skipped
// outright — a modal/overlay is up, or there's no alt-screen viewport at all.
func (m model) transcriptHitTestBlocked() bool {
	return !m.altScreen || m.height <= 0 || m.setup.visible || m.providerWizard != nil || m.mcpAddWizard != nil ||
		m.mcpManager != nil || m.picker != nil || m.renamePrompt != nil || m.terminalAttach != nil || m.suggestionsActive()
}

// transcriptHitTestLayout computes the frame/window/layout mouse hit-testing needs,
// shared by transcriptLineAtMouse (exact match) and nearestTranscriptLineAtMouse
// (nearest-line fallback for scroll-driven selection extension).
func (m model) transcriptHitTestLayout() (frame transcriptFrameLayout, window transcriptViewportWindow, layout transcriptBodyLayout) {
	header, items, width := m.transcriptHitTestSource()
	footer := m.footerView(width)
	if m.transcriptDetailed {
		footer = m.detailedTranscriptFooter(width)
	}
	frame = m.scrollableTranscriptFrame(header, footer)
	metrics := measureTranscriptBodyItems(items, m.transcriptBodyHeights)
	window = transcriptViewportForLayout(metrics, frame, m.chatScrollOffset).window()
	layout = layoutVisibleTranscriptBodyItems(items, metrics, window)
	return frame, window, layout
}

func (m model) transcriptLineAtMouse(msg tea.MouseMsg) (transcriptSelectableLine, bool) {
	if m.transcriptHitTestBlocked() {
		return transcriptSelectableLine{}, false
	}
	frame, window, layout := m.transcriptHitTestLayout()
	_, localY, ok := frame.bodyRect.local(mouseX(msg), mouseY(msg))
	if !ok {
		return transcriptSelectableLine{}, false
	}
	bodyY := window.start + localY
	for _, line := range layout.selectable {
		if line.bodyY != bodyY {
			continue
		}
		if mouseX(msg) < 0 {
			return transcriptSelectableLine{}, false
		}
		return line, true
	}
	return transcriptSelectableLine{}, false
}

// nearestTranscriptLineAtMouse is transcriptLineAtMouse with a fallback for
// scroll-driven selection extension: the recomputed on-screen position can land
// exactly on a non-selectable spacer row between messages (blank lines aren't in
// layout.selectable; a scroll can shift which physical row is text vs spacer), where
// an EXACT match finds nothing even though real text sits one row away. Falls back
// to the closest visible selectable line by bodyY, so the selection still extends
// instead of silently freezing for that scroll tick. Only used for scroll
// extension — a plain click keeps using transcriptLineAtMouse's exact match, since
// "nothing there" is a meaningful, correct result for an intentional click below
// the last line or in dead space.
func (m model) nearestTranscriptLineAtMouse(msg tea.MouseMsg) (transcriptSelectableLine, bool) {
	if line, ok := m.transcriptLineAtMouse(msg); ok {
		return line, true
	}
	if m.transcriptHitTestBlocked() {
		return transcriptSelectableLine{}, false
	}
	frame, window, layout := m.transcriptHitTestLayout()
	_, localY, ok := frame.bodyRect.local(mouseX(msg), mouseY(msg))
	if !ok || mouseX(msg) < 0 {
		return transcriptSelectableLine{}, false
	}
	return nearestTranscriptSelectableAt(layout, window.start+localY)
}

// nearestTranscriptSelectableAt returns the selectable line in layout closest to
// targetBodyY (ties go to whichever is found first) — the fallback core shared by
// nearestTranscriptLineAtMouse (target derived from a mouse position that may
// have shifted onto a non-selectable spacer row) and the edge-auto-scroll drag
// case (target derived directly from the new viewport's top/bottom bound, which
// has no "mouse position" to speak of at all).
func nearestTranscriptSelectableAt(layout transcriptBodyLayout, targetBodyY int) (transcriptSelectableLine, bool) {
	best := transcriptSelectableLine{}
	bestDist := -1
	found := false
	for _, line := range layout.selectable {
		dist := line.bodyY - targetBodyY
		if dist < 0 {
			dist = -dist
		}
		if !found || dist < bestDist {
			best, bestDist, found = line, dist, true
		}
	}
	return best, found
}

// transcriptEdgeScrollDelta reports the scroll delta to apply when a drag has
// moved past the top or bottom edge of the visible transcript body while
// staying within its horizontal span — the classic "drag past the viewport
// edge auto-scrolls" affordance from browsers/editors. Positive reveals OLDER
// content (dragged above the top edge, same sign convention as wheel-up);
// negative reveals NEWER content (dragged below the bottom edge). ok is false
// when the point isn't a vertical-edge case at all (e.g. off to the side, over
// the sidebar) — that's not an edge-scroll, just outside the column.
func (m model) transcriptEdgeScrollDelta(msg tea.MouseMsg) (int, bool) {
	if m.transcriptHitTestBlocked() {
		return 0, false
	}
	frame, _, _ := m.transcriptHitTestLayout()
	x, y := mouseX(msg), mouseY(msg)
	if x < frame.bodyRect.x || x >= frame.bodyRect.x+frame.bodyRect.width {
		return 0, false
	}
	if y < frame.bodyRect.y {
		return chatWheelScrollLines, true
	}
	if y >= frame.bodyRect.y+frame.bodyRect.height {
		return -chatWheelScrollLines, true
	}
	return 0, false
}

// dragToEdgeScroll scrolls one step toward the edge the drag moved past, then
// extends the selection cursor to whichever line now sits at THAT edge of the
// new viewport, at column x. It deliberately does NOT try to re-resolve the
// drag's (still off-screen) physical mouse position — after scrolling, that
// position is still outside frame.bodyRect (scrolling changes what content
// occupies a screen row, not the mouse's screen position), so
// nearestTranscriptLineAtMouse would keep finding nothing. Anchoring to the
// viewport edge instead is what makes the selection visibly keep pace with the
// drag while the mouse holds past the edge. Takes a plain column rather than a
// tea.MouseMsg because the smooth-glide tick chain (dragEdgeScrollTickMsg) calls
// this repeatedly with no real mouse event of its own — see edgeScrollMouseX.
func (m model) dragToEdgeScroll(delta int, x int) model {
	m = m.scrollChat(delta)
	_, window, layout := m.transcriptHitTestLayout()
	target := window.start
	if delta < 0 {
		target = window.start + window.height - 1
	}
	if line, ok := nearestTranscriptSelectableAt(layout, target); ok {
		m.transcriptSelection.cursor = transcriptSelectionPointForMouse(line, x)
	}
	return m
}

// dragEdgeScrollTickCmd schedules the next step of the smooth-glide edge-scroll,
// gated by seq (see dragEdgeScrollTickMsg / edgeScrollSeq).
func dragEdgeScrollTickCmd(seq int) tea.Cmd {
	return tea.Tick(dragEdgeScrollInterval, func(time.Time) tea.Msg {
		return dragEdgeScrollTickMsg{seq: seq}
	})
}

// startEdgeScroll (re)starts the smooth-glide tick chain in the given direction —
// a no-op if it's already running the SAME direction (a fresh raw motion event
// arriving mid-glide must not reset the cadence, or repeated events faster than
// dragEdgeScrollInterval would make it jerky again, defeating the point). x is
// remembered for the ticks, which carry no mouse position of their own. Applies
// one immediate step so there's no perceptible delay between crossing the edge
// and the first visible movement.
func (m model) startEdgeScroll(direction int, x int) (model, tea.Cmd) {
	step := dragEdgeScrollStep
	if direction < 0 {
		step = -step
	}
	if m.edgeScrollDelta == step {
		m.edgeScrollMouseX = x
		return m, nil
	}
	m = m.dragToEdgeScroll(step, x)
	m.edgeScrollDelta = step
	m.edgeScrollMouseX = x
	m.edgeScrollSeq++
	return m, dragEdgeScrollTickCmd(m.edgeScrollSeq)
}

// stopEdgeScroll ends the smooth-glide tick chain (the drag moved back into the
// body, off to the side, or released). Bumping edgeScrollSeq invalidates any
// tick already in flight, so it lands as a no-op and doesn't reschedule itself.
func (m model) stopEdgeScroll() model {
	if m.edgeScrollDelta == 0 {
		return m
	}
	m.edgeScrollDelta = 0
	m.edgeScrollSeq++
	return m
}

func transcriptSelectionPointForMouse(line transcriptSelectableLine, x int) transcriptSelectionPoint {
	lineEnd := line.textStart + lipgloss.Width(line.text)
	return transcriptSelectionPoint{
		bodyY: line.bodyY,
		x:     clampInt(x, line.textStart, lineEnd),
	}
}

func (m model) handleTranscriptSelectionMouse(msg tea.MouseMsg) (model, tea.Cmd, bool) {
	switch {
	case mouseLeftPress(msg):
		// Context data is available through the run-details overlay. The former
		// sidebar is no longer rendered, so its legacy hit targets must never
		// intercept clicks in the full-width transcript.
		line, ok := m.transcriptLineAtMouse(msg)
		if !ok {
			if m.transcriptSelection.active {
				m.transcriptSelection = transcriptSelectionState{}
				return m, nil, true
			}
			return m, nil, false
		}
		if line.permOption && (m.pendingPermission == nil || !m.pendingPermission.typing) {
			// A left-click on a permission-popup option resolves it directly. The
			// typing guard is defence-in-depth: renderFocusedPermissionPrompt already
			// returns nil offsets in feedback mode, so no option row is registered as
			// clickable then — but that single early-return is the only thing keeping
			// a stray click (Allow included) off the decision path, and it lives in a
			// function other PRs also edit. Guarding here makes the safety explicit
			// rather than emergent.
			next, cmd := m.resolvePermission(line.permChoice)
			return next.(model), cmd, true
		}
		if line.specialistCard {
			// Click on a specialist card drills into its child session.
			title := m.specialistTitleFor(line.specialistID)
			if errMsg := m.subchat.enter(m.sessionStore, line.specialistID, title, m.chatScrollOffset); errMsg != "" {
				m = m.appendSystemNotice(errMsg)
			}
			m.chatScrollOffset = 0
			m = m.clearHover() // bodyY numbering differs between subchat and the parent transcript
			return m, nil, true
		}
		if line.toggle {
			if line.live {
				m.streamingReasoningExpanded = !m.streamingReasoningExpanded
			} else {
				m = m.toggleTranscriptRow(line.rowIndex)
			}
			return m, nil, true
		}
		point := transcriptSelectionPointForMouse(line, mouseX(msg))
		m.copyStatus = ""
		m.transcriptSelection = transcriptSelectionState{active: true, dragging: true, anchor: point, cursor: point}
		// A fresh press always starts a brand-new drag: reset any glide state left
		// over from a PREVIOUS selection that ended without going through
		// stopEdgeScroll (e.g. a keypress mid-drag clears transcriptSelection.active
		// directly, model.go's KeyPressMsg handler). Without this, a stale
		// edgeScrollDelta can coincidentally match this new drag's direction and
		// fool startEdgeScroll's "already running" fast path into silently doing
		// nothing for the whole hold.
		m = m.stopEdgeScroll()
		return m, nil, true
	case mouseMotion(msg):
		// Gate on dragging (the app's own press/release bracket), not active
		// alone: active stays true through the async copy-command grace window
		// after release, so without this a genuine hover motion arriving in
		// that window would be misrouted here and silently move the
		// just-released selection instead of falling through to hover
		// handling. Deliberately NOT gated on msg's own Button field — see
		// transcriptSelectionState.dragging's doc comment.
		if !m.transcriptSelection.dragging {
			return m, nil, false
		}
		if line, ok := m.transcriptLineAtMouse(msg); ok {
			m.transcriptSelection.cursor = transcriptSelectionPointForMouse(line, mouseX(msg))
			m = m.stopEdgeScroll() // back inside the body: any running glide ends
			return m, nil, true
		}
		// The drag moved past the top/bottom edge of the visible transcript:
		// auto-scroll toward that side and extend the selection to follow.
		delta, ok := m.transcriptEdgeScrollDelta(msg)
		if !ok {
			m = m.stopEdgeScroll() // off to the side (e.g. over the sidebar), not an edge case
			return m, nil, true
		}
		if m.reducedMotion {
			// No animated glide: a single, larger step per raw motion event,
			// matching the app's reduced-motion convention elsewhere.
			m = m.dragToEdgeScroll(delta, mouseX(msg))
			return m, nil, true
		}
		direction := 1
		if delta < 0 {
			direction = -1
		}
		var cmd tea.Cmd
		m, cmd = m.startEdgeScroll(direction, mouseX(msg))
		return m, cmd, true
	case mouseRelease(msg):
		if !m.transcriptSelection.active {
			return m, nil, false
		}
		m = m.stopEdgeScroll()
		if line, ok := m.transcriptLineAtMouse(msg); ok {
			m.transcriptSelection.cursor = transcriptSelectionPointForMouse(line, mouseX(msg))
		}
		text := m.selectedTranscriptText()
		if strings.TrimSpace(text) == "" {
			m.transcriptSelection = transcriptSelectionState{}
			return m, nil, true
		}
		// The drag itself is over even though the selection stays active for
		// the async copy-command grace window below — see dragging's doc
		// comment on why mouseMotion must stop treating events as a drag now.
		m.transcriptSelection.dragging = false
		return m, copyTranscriptSelectionCmd(text), true
	default:
		return m, nil, false
	}
}

// toggleTranscriptRow flips the collapse state of a collapsible row (a provider
// thought or a tool result card).
func (m model) toggleTranscriptRow(rowIndex int) model {
	if rowIndex < 0 || rowIndex >= len(m.transcript) {
		return m
	}
	switch m.transcript[rowIndex].kind {
	case rowReasoning, rowToolResult:
		m.transcript[rowIndex].expanded = !m.transcript[rowIndex].expanded
		// Settled alt-screen rows are cached across frames; force a rebuild so an
		// expand/collapse interaction is reflected immediately.
		if rowIndex < m.flushed {
			m.altScreenSettledWidth = 0
		}
	}
	return m
}

func (m model) selectedTranscriptText() string {
	// Must read from the SAME row source transcriptLineAtMouse hit-tested against
	// (subchat vs parent transcript) — otherwise the selection's bodyY range is
	// matched against the wrong layout here and copy silently returns nothing.
	_, items, _ := m.transcriptHitTestSource()
	layout := layoutTranscriptBodyItems(items)
	parts := []string{}
	for _, line := range layout.selectable {
		start, end, ok := m.selectedColumnsForTranscriptLine(line)
		if !ok {
			continue
		}
		before, rest := splitPlainAtDisplayWidth(line.text, start)
		_ = before
		selected, _ := splitPlainAtDisplayWidth(rest, end-start)
		parts = append(parts, selected)
	}
	return strings.Join(parts, "\n")
}

// clipboardSafeText removes the control characters a clipboard write cannot
// carry, keeping newlines and tabs.
//
// A NUL PANICS THE WHOLE PROGRAM ON WINDOWS, and that is not a theoretical
// concern: syscall.StringToUTF16 — which github.com/atotto/clipboard calls on
// Windows — PANICS rather than erroring when the string contains one, so a
// single copied NUL killed the TUI with "program experienced a panic". It
// reached the clipboard because ansi.Strip removes escape SEQUENCES and leaves
// C0 bytes untouched, and the transcript legitimately carries them from two
// directions: tool output (a read of a binary file, git's -z NUL-separated
// listings) and this package's own card protocol, whose row prefixes are
// literally "\x00command-card\x00".
//
// Stripped at the WRITE, not at each source, because the sources are unbounded:
// any tool result a user can select from is a source, and a fix per source is
// one that the next tool re-breaks. The same pass protects the OSC52 fallback,
// where a stray control byte would terminate the escape sequence early and
// corrupt the copy rather than crash.
//
// \n and \t survive: a multi-line selection is the normal case, and both are
// legal clipboard content.
func clipboardSafeText(text string) string {
	if !strings.ContainsFunc(text, unsafeClipboardRune) {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		if unsafeClipboardRune(r) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func unsafeClipboardRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r < 0x20 || r == 0x7f
}

func copyTranscriptSelectionCmd(text string) tea.Cmd {
	// SANITISED ONCE, HERE, before either destination sees it — the native
	// clipboard would panic on a NUL and the OSC52 fallback would be corrupted
	// by one.
	text = clipboardSafeText(text)
	return func() tea.Msg {
		// Prefer the native OS clipboard (pbcopy / clip.exe / xclip): it works on
		// local terminals — including macOS Terminal.app, which has no OSC52 support
		// at all — so the auto-copy-on-select actually lands on the clipboard. Fall
		// back to OSC52 (forwarded by the terminal) for remote/SSH sessions where no
		// local clipboard utility is reachable.
		if err := clipboard.WriteAll(text); err != nil {
			if _, oscErr := os.Stdout.WriteString(ansi.SetSystemClipboard(text)); oscErr != nil {
				// Both paths failed; report it rather than claiming a copy that
				// never reached any clipboard.
				return transcriptCopiedMsg{err: err}
			}
		}
		return transcriptCopiedMsg{chars: utf8.RuneCountInString(text)}
	}
}
