package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/terminalpet"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestMouseClickSelectsThenAppliesCommandSuggestionRow(t *testing.T) {
	m := mouseTestModel()
	m = typeRunes(t, m, "/sp")
	if len(m.suggestions) == 0 {
		t.Fatalf("expected command suggestions, got %#v", m.suggestions)
	}

	width := m.chatColumnWidth()
	top := m.overlayMouseTop(len(viewLines(m.suggestionOverlay(width))), width)
	click := testMouseClick(tea.MouseLeft, width/2, top+3)
	updated, cmd := m.Update(click)
	next := updated.(model)
	if cmd != nil {
		t.Fatal("first command click should not return a command")
	}
	if got := next.input.Value(); got != "/sp" {
		t.Fatalf("input after first command click = %q, want /sp", got)
	}
	if !next.suggestionsActive() {
		t.Fatal("suggestions should stay open after first command click")
	}

	updated, cmd = next.Update(click)
	next = updated.(model)
	_ = cmd
	if got := next.input.Value(); got != "/spec" {
		t.Fatalf("input after second command click = %q, want /spec", got)
	}
	if next.suggestionsActive() {
		t.Fatalf("suggestions should close after second command click, got %#v", next.suggestions)
	}
}

func TestMouseClickSelectsThenAppliesPickerRow(t *testing.T) {
	m := mouseTestModel()
	m.modelName = "claude-sonnet-4.5"
	m.picker = &commandPicker{
		kind:  pickerEffort,
		title: "select reasoning effort",
		items: []pickerItem{
			{Label: "auto", Value: "auto"},
			{Label: "high", Value: "high"},
		},
		selected: 0,
	}
	m.picker.allItems = append([]pickerItem{}, m.picker.items...)
	m.mouseCapture = true

	width := m.chatColumnWidth()
	top := m.overlayMouseTop(len(viewLines(m.pickerOverlay(width))), width)
	// Rows: y=0 titled border, y=1 search, y=2 separator, y=3 "auto", y=4 "high".
	click := testMouseClick(tea.MouseLeft, width/2, top+4)
	updated, cmd := m.Update(click)
	next := updated.(model)
	if cmd != nil {
		t.Fatal("first picker click should not return a command")
	}
	if next.picker == nil || next.picker.selected != 1 {
		t.Fatalf("picker after first click = %#v, want selected index 1", next.picker)
	}
	if next.reasoningEffort != "" {
		t.Fatalf("reasoning effort after first picker click = %q, want unchanged", next.reasoningEffort)
	}

	updated, cmd = next.Update(click)
	next = updated.(model)
	_ = cmd
	if next.picker != nil {
		t.Fatalf("picker should close after second click apply, got %#v", next.picker)
	}
	if next.reasoningEffort != "high" {
		t.Fatalf("reasoning effort after second picker click = %q, want high", next.reasoningEffort)
	}
}

func TestMouseClickSelectsProviderWizardRow(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.step = providerWizardStepProvider // skip the new method chooser
	if m.providerWizard == nil || len(m.providerWizard.providers) < 2 {
		t.Fatalf("expected multiple providers, got %#v", m.providerWizard)
	}
	m.mouseCapture = true

	width := m.chatColumnWidth()
	top := m.overlayMouseTop(len(viewLines(m.providerWizardOverlay(width))), width)
	click := testMouseClick(tea.MouseLeft, width/2, top+5)
	updated, cmd := m.Update(click)
	next := updated.(model)
	if cmd != nil {
		t.Fatal("mouse selection should not return a command")
	}
	if next.providerWizard == nil || next.providerWizard.selectedProvider != 1 {
		t.Fatalf("provider selection = %#v, want selected index 1", next.providerWizard)
	}
	if next.providerWizard.step != providerWizardStepProvider {
		t.Fatalf("first provider click should not advance, got step %v", next.providerWizard.step)
	}

	updated, cmd = next.Update(click)
	next = updated.(model)
	_ = cmd
	if next.providerWizard == nil || next.providerWizard.step == providerWizardStepProvider {
		t.Fatalf("second provider click should advance, got %#v", next.providerWizard)
	}
}

func TestMouseWheelMovesProviderWizardRows(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.step = providerWizardStepProvider // skip the new method chooser
	m.mouseCapture = true

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelDown, 0, 0))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("mouse wheel should not return a command")
	}
	if next.providerWizard == nil || next.providerWizard.selectedProvider != 1 {
		t.Fatalf("provider selection after wheel = %#v, want selected index 1", next.providerWizard)
	}
}

func TestMouseClickSelectsThenContinuesSetupProviderRow(t *testing.T) {
	m := setupMouseTestModel()
	m.mouseCapture = true
	m.setup.stage = setupStageProvider

	width := m.chatColumnWidth()
	height := normalizedStartupHeight(m.height)
	rowWidth := setupProviderBlockWidth(width, m.setup.providers)
	top := setupContentTop(height, len(m.setupProviderLines(width, height)), m.setup.err != "")
	click := testMouseClick(tea.MouseLeft, maxInt(0, (width-rowWidth)/2)+2, top+3)
	updated, cmd := m.Update(click)
	next := updated.(model)
	if cmd != nil {
		t.Fatal("first setup provider click should not return a command")
	}
	if next.setup.selected != 1 {
		t.Fatalf("setup provider selection = %d, want 1", next.setup.selected)
	}
	if next.setup.stage != setupStageProvider {
		t.Fatalf("first setup provider click advanced to %v", next.setup.stage)
	}

	updated, cmd = next.Update(click)
	next = updated.(model)
	_ = cmd
	if next.setup.stage != setupStageCredentials {
		t.Fatalf("second setup provider click should advance to credentials, got %v", next.setup.stage)
	}
}

func TestMouseClickSelectsThenContinuesSetupModelRow(t *testing.T) {
	m := setupMouseTestModel()
	m.mouseCapture = true
	m.setup.stage = setupStageModel
	m.setup.models = []providerWizardModel{
		{ID: "alpha"},
		{ID: "beta", Meta: "128K ctx"},
	}
	m.setup.modelForID = m.setupProviderDescriptor().ID

	width := m.chatColumnWidth()
	height := normalizedStartupHeight(m.height)
	rowWidth := setupModelBlockWidth(width, m.setup.models)
	top := setupContentTop(height, len(m.setupModelLines(width, height)), m.setup.err != "")
	click := testMouseClick(tea.MouseLeft, maxInt(0, (width-rowWidth)/2)+2, top+5)
	updated, cmd := m.Update(click)
	next := updated.(model)
	if cmd != nil {
		t.Fatal("first setup model click should not return a command")
	}
	if next.setup.modelIndex != 1 {
		t.Fatalf("setup model selection = %d, want 1", next.setup.modelIndex)
	}
	if next.setup.stage != setupStageModel {
		t.Fatalf("first setup model click advanced to %v", next.setup.stage)
	}

	updated, cmd = next.Update(click)
	next = updated.(model)
	_ = cmd
	if next.setup.stage != setupStageSafety {
		t.Fatalf("second setup model click should advance to safety, got %v", next.setup.stage)
	}
}

func TestMouseCaptureOnlyWhileInteractiveSurfaceOpen(t *testing.T) {
	m := mouseTestModel()
	m.transcript = appendRow(m.transcript, rowUser, "hello")
	if !m.wantsMouseCapture() {
		t.Fatal("chat should capture mouse for Zero-owned transcript selection")
	}

	m = typeRunes(t, m, "/")
	if !m.wantsMouseCapture() || !m.mouseCapture {
		t.Fatalf("open command palette should capture mouse, wants=%v active=%v", m.wantsMouseCapture(), m.mouseCapture)
	}

	updated, cmd := m.Update(testKey(tea.KeyEsc))
	m = updated.(model)
	_ = cmd
	if !m.wantsMouseCapture() || !m.mouseCapture {
		t.Fatalf("closed command palette should keep chat mouse capture, wants=%v active=%v", m.wantsMouseCapture(), m.mouseCapture)
	}
}

func TestMouseCaptureOnEmptyChatSplash(t *testing.T) {
	m := mouseTestModel()
	if !m.wantsMouseCapture() {
		t.Fatal("empty chat splash should capture mouse so only real transcript content becomes selectable")
	}

	m.transcript = appendRow(m.transcript, rowUser, "hello")
	if !m.wantsMouseCapture() {
		t.Fatal("chat with transcript rows should keep mouse capture for Zero-owned selection")
	}
}

func TestComposerMouseClickMovesCursor(t *testing.T) {
	m := mouseTestModel()
	m.input.SetValue("hello world")
	m.input.CursorEnd()
	x, y := composerMousePoint(t, m, 5)

	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, x, y))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("composer click should not return a command")
	}
	if got := next.currentComposerState().cursor; got != 5 {
		t.Fatalf("composer cursor = %d, want 5", got)
	}
	if text := next.selectedComposerText(); text != "" {
		t.Fatalf("composer click should not select text, got %q", text)
	}
}

func TestComposerMouseClickMovesCursorBelowImageThumbnail(t *testing.T) {
	m := mouseTestModel()
	m.attachmentRenderers = []*terminalpet.ImageRenderer{terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})}
	m.pendingImages = []zeroruntime.ImageBlock{previewImageBlock(t)}
	m.refreshPendingImageThumbnail()
	m.input.SetValue("hello world")

	width := m.chatColumnWidth()
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	x := frame.composerRect.x + 2 + lipgloss.Width(composerVisualLinePrefix(m.input, true)) + 5
	y := frame.composerRect.y + 1 + attachmentPreviewRows
	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, x, y))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("composer click should not return a command")
	}
	if got := next.currentComposerState().cursor; got != 5 {
		t.Fatalf("composer cursor below thumbnail = %d, want 5", got)
	}
}

func TestComposerMouseDragSelectsCopiesAndClears(t *testing.T) {
	m := mouseTestModel()
	m.input.SetValue("hello world")
	startX, y := composerMousePoint(t, m, 0)
	endX, _ := composerMousePoint(t, m, 5)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, startX, y))
	next := updated.(model)
	updated, _ = next.Update(testMouseMotion(tea.MouseLeft, endX, y))
	next = updated.(model)
	if got := next.selectedComposerText(); got != "hello" {
		t.Fatalf("selectedComposerText() = %q, want hello", got)
	}

	updated, cmd := next.Update(testMouseRelease(tea.MouseNone, endX, y))
	next = updated.(model)
	if cmd == nil {
		t.Fatal("composer drag release should return copy command")
	}
	if next.composerSelection.active {
		t.Fatal("composer selection should clear automatically after release")
	}
	if got := next.currentComposerState().cursor; got != 5 {
		t.Fatalf("composer cursor after release = %d, want 5", got)
	}
}

func TestComposerMouseSelectionBlockedWhileSuggestionsOpen(t *testing.T) {
	m := mouseTestModel()
	m = typeRunes(t, m, "/sp")
	if !m.suggestionsActive() {
		t.Fatalf("expected suggestions to be open, got %#v", m.suggestions)
	}
	initial := m.currentComposerState()
	startX, y := composerMousePoint(t, m, 0)
	endX, _ := composerMousePoint(t, m, 2)

	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, startX, y))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("composer click behind suggestions should not return a command")
	}
	if next.composerSelection.active {
		t.Fatal("composer selection should not start while suggestions are open")
	}
	if got := next.currentComposerState().cursor; got != initial.cursor {
		t.Fatalf("composer cursor changed behind suggestions: got %d want %d", got, initial.cursor)
	}

	updated, cmd = next.Update(testMouseMotion(tea.MouseLeft, endX, y))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("composer drag behind suggestions should not return a command")
	}
	updated, cmd = next.Update(testMouseRelease(tea.MouseNone, endX, y))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("composer release behind suggestions should not copy")
	}
	if next.composerSelection.active {
		t.Fatal("composer selection should remain inactive while suggestions are open")
	}
}

func TestTranscriptSelectionOnlyStartsOnTranscriptText(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hello world")
	textY := firstTranscriptTextMouseY(t, m)

	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, 40, 20))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("empty-area click should not return a command")
	}
	if next.transcriptSelection.active {
		t.Fatal("empty-area click should not start transcript selection")
	}

	updated, cmd = next.Update(testMouseClick(tea.MouseLeft, 3, textY))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("transcript press should not copy yet")
	}
	if !next.transcriptSelection.active {
		t.Fatal("transcript text click should start transcript selection")
	}
}

func TestTranscriptSelectionExtractsVisibleTextRange(t *testing.T) {
	m := withSidebarContent(mouseTestModel())
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hello world")
	textY := firstTranscriptTextMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 3, textY))
	m = updated.(model)
	updated, _ = m.Update(testMouseMotion(tea.MouseLeft, 8, textY))
	m = updated.(model)

	if got := m.selectedTranscriptText(); got != "hello" {
		t.Fatalf("selectedTranscriptText() = %q, want hello", got)
	}
}

func TestTranscriptSelectionUpdatesOnGenericMotion(t *testing.T) {
	m := withSidebarContent(mouseTestModel())
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hello world")
	textY := firstTranscriptTextMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 3, textY))
	m = updated.(model)
	updated, _ = m.Update(testMouseMotion(tea.MouseNone, 8, textY))
	m = updated.(model)

	if got := m.selectedTranscriptText(); got != "hello" {
		t.Fatalf("selectedTranscriptText() after generic motion = %q, want hello", got)
	}
}

func TestTranscriptSelectionLeftDragDoesNotResetAnchor(t *testing.T) {
	m := withSidebarContent(mouseTestModel())
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hello world")
	textY := firstTranscriptTextMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 3, textY))
	m = updated.(model)
	// A left-button drag is Action==Motion with Button==Left; this must update the
	// cursor without resetting the selection anchor.
	updated, _ = m.Update(testMouseMotion(tea.MouseLeft, 8, textY))
	m = updated.(model)

	if got := m.selectedTranscriptText(); got != "hello" {
		t.Fatalf("selectedTranscriptText() after left-button drag = %q, want hello", got)
	}
}

func TestTranscriptSelectionReleaseExtendsRangeWithoutMotion(t *testing.T) {
	m := withSidebarContent(mouseTestModel())
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hello world")
	textY := firstTranscriptTextMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 3, textY))
	m = updated.(model)
	updated, cmd := m.Update(testMouseRelease(tea.MouseNone, 8, textY))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("release after range selection should return copy command")
	}
	if got := m.selectedTranscriptText(); got != "hello" {
		t.Fatalf("selectedTranscriptText() after release = %q, want hello", got)
	}
}

// A completed selection stays "active" through the async copy-command grace
// window (transcriptCopiedMsg hasn't landed yet), but the drag itself is over.
// A genuine hover motion (no button held) arriving in that window must NOT be
// treated as a drag continuation — it must fall through to hover handling
// instead of silently moving the selection the user just released.
func TestTranscriptHoverAfterReleaseDoesNotMoveSelection(t *testing.T) {
	m := withSidebarContent(mouseTestModel())
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hello world")
	textY := firstTranscriptTextMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 3, textY))
	m = updated.(model)
	updated, cmd := m.Update(testMouseRelease(tea.MouseNone, 8, textY))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("sanity check failed: release should return the copy command")
	}
	if m.transcriptSelection.dragging {
		t.Fatal("dragging must be false immediately after release")
	}
	before := m.selectedTranscriptText()

	// A hover (no button) moves further along the same line, in the window
	// before transcriptCopiedMsg has landed (m.transcriptSelection.active is
	// still true here).
	updated, _ = m.Update(testMouseMotion(tea.MouseNone, 10, textY))
	m = updated.(model)

	if got := m.selectedTranscriptText(); got != before {
		t.Fatalf("a post-release hover changed the selection: got %q, want unchanged %q", got, before)
	}
}

// Dragging a selection past the top edge of the visible transcript must
// auto-scroll toward older content and extend the selection to follow — the
// classic "drag past the viewport edge keeps scrolling" affordance.
func TestTranscriptSelectionDragPastTopEdgeAutoScrolls(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	// This test exercises the animated glide specifically; reducedMotion
	// defaults from TTY detection (see defaultReducedMotion), which differs
	// between a local dev shell and CI's non-TTY runners — set it explicitly
	// so the test is deterministic regardless of environment.
	m.reducedMotion = false
	for i := 0; i < 80; i++ {
		m.transcript = appendRow(m.transcript, rowUser, "line content")
	}
	textY := topmostVisibleTranscriptMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 0, textY))
	m = updated.(model)
	if !m.transcriptSelection.active {
		t.Fatal("selection should be active after a left click on transcript text")
	}
	cursorBefore := m.transcriptSelection.cursor.bodyY
	scrollBefore := m.chatScrollOffset

	frame, _, _ := m.transcriptHitTestLayout()
	aboveBody := frame.bodyRect.y - 1 // one row above the visible transcript body

	// A left-button drag (not a hover) moved past the top edge: the smooth-glide
	// chain must start immediately (first step + a scheduled tick).
	updated, cmd := m.Update(testMouseMotion(tea.MouseLeft, 0, aboveBody))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("crossing the top edge should schedule the glide tick chain")
	}
	if m.edgeScrollDelta <= 0 {
		t.Fatalf("edgeScrollDelta = %d, want > 0 (scrolling toward older content)", m.edgeScrollDelta)
	}
	if m.chatScrollOffset == scrollBefore {
		t.Fatal("dragging past the top edge should auto-scroll toward older content")
	}

	// A single small step may land on a blank spacer row between messages (no
	// text there yet) — drive the tick chain forward like a sustained real hold
	// would, and the cursor must eventually reach a genuinely earlier line.
	for i := 0; i < 10 && m.transcriptSelection.cursor.bodyY >= cursorBefore; i++ {
		updated, _ = m.Update(dragEdgeScrollTickMsg{seq: m.edgeScrollSeq})
		m = updated.(model)
	}

	if !m.transcriptSelection.active {
		t.Fatal("selection must survive an edge auto-scroll, not be cleared")
	}
	if m.transcriptSelection.cursor.bodyY >= cursorBefore {
		t.Fatalf("selection cursor bodyY = %d, want < %d (it must extend upward to follow the drag)", m.transcriptSelection.cursor.bodyY, cursorBefore)
	}
}

// Symmetric to the top-edge case: dragging past the BOTTOM edge scrolls toward
// newer content. Requires scrolling up first so there's somewhere to scroll back
// down to (chatScrollOffset=0 already sits at the bottom).
func TestTranscriptSelectionDragPastBottomEdgeAutoScrolls(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.reducedMotion = false // deterministic across local/CI TTY-detection differences
	for i := 0; i < 80; i++ {
		m.transcript = appendRow(m.transcript, rowUser, "line content")
	}
	m.chatScrollOffset = chatWheelScrollLines * 3 // scrolled up, away from the bottom

	textY := topmostVisibleTranscriptMouseY(t, m)
	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 0, textY))
	m = updated.(model)
	if !m.transcriptSelection.active {
		t.Fatal("selection should be active after a left click on transcript text")
	}
	cursorBefore := m.transcriptSelection.cursor.bodyY
	scrollBefore := m.chatScrollOffset

	frame, _, _ := m.transcriptHitTestLayout()
	belowBody := frame.bodyRect.y + frame.bodyRect.height // one row below the visible transcript body

	updated, cmd := m.Update(testMouseMotion(tea.MouseLeft, 0, belowBody))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("crossing the bottom edge should schedule the glide tick chain")
	}
	if m.edgeScrollDelta >= 0 {
		t.Fatalf("edgeScrollDelta = %d, want < 0 (scrolling toward newer content)", m.edgeScrollDelta)
	}
	if m.chatScrollOffset >= scrollBefore {
		t.Fatal("dragging past the bottom edge should auto-scroll toward newer content (offset decreases)")
	}

	for i := 0; i < 10 && m.transcriptSelection.cursor.bodyY <= cursorBefore; i++ {
		updated, _ = m.Update(dragEdgeScrollTickMsg{seq: m.edgeScrollSeq})
		m = updated.(model)
	}

	if !m.transcriptSelection.active {
		t.Fatal("selection must survive an edge auto-scroll, not be cleared")
	}
	if m.transcriptSelection.cursor.bodyY <= cursorBefore {
		t.Fatalf("selection cursor bodyY = %d, want > %d (it must extend downward to follow the drag)", m.transcriptSelection.cursor.bodyY, cursorBefore)
	}
}

// The glide tick chain must self-terminate the instant the drag moves back into
// the visible body — otherwise a stray scheduled tick could keep scrolling after
// the user has already dragged back to the content they wanted selected.
func TestTranscriptSelectionEdgeGlideStopsWhenDragReturnsToBody(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.reducedMotion = false // deterministic across local/CI TTY-detection differences
	for i := 0; i < 80; i++ {
		m.transcript = appendRow(m.transcript, rowUser, "line content")
	}
	textY := topmostVisibleTranscriptMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 0, textY))
	m = updated.(model)
	frame, _, _ := m.transcriptHitTestLayout()
	updated, _ = m.Update(testMouseMotion(tea.MouseLeft, 0, frame.bodyRect.y-1))
	m = updated.(model)
	if m.edgeScrollDelta == 0 {
		t.Fatal("sanity check failed: the glide chain should be running")
	}
	seqWhileGliding := m.edgeScrollSeq

	updated, _ = m.Update(testMouseMotion(tea.MouseLeft, 0, textY))
	m = updated.(model)
	if m.edgeScrollDelta != 0 {
		t.Fatalf("edgeScrollDelta = %d, want 0 (dragging back into the body must stop the glide)", m.edgeScrollDelta)
	}

	// A tick already in flight from before the drag returned must be a no-op —
	// it carries the OLD seq, which no longer matches.
	scrollBefore := m.chatScrollOffset
	updated, _ = m.Update(dragEdgeScrollTickMsg{seq: seqWhileGliding})
	m = updated.(model)
	if m.chatScrollOffset != scrollBefore {
		t.Fatal("a stale in-flight tick must not scroll after the glide was stopped")
	}
}

// A keypress mid-glide clears m.transcriptSelection.active directly (the
// KeyPressMsg handler in model.go), NOT through stopEdgeScroll — so
// edgeScrollDelta is left stale. If a fresh drag past the SAME edge afterward
// doesn't reset it, startEdgeScroll's "already running" fast path is fooled into
// thinking a chain is already active and silently does nothing: no immediate
// step, no scheduled tick. The press handler must reset glide state so every new
// drag starts clean regardless of how the previous one ended.
func TestTranscriptSelectionEdgeGlideRecoversAfterKeypressInterrupt(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.reducedMotion = false // deterministic across local/CI TTY-detection differences
	for i := 0; i < 80; i++ {
		m.transcript = appendRow(m.transcript, rowUser, "line content")
	}
	textY := topmostVisibleTranscriptMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 0, textY))
	m = updated.(model)
	frame, _, _ := m.transcriptHitTestLayout()
	aboveBody := frame.bodyRect.y - 1

	updated, _ = m.Update(testMouseMotion(tea.MouseLeft, 0, aboveBody))
	m = updated.(model)
	if m.edgeScrollDelta == 0 {
		t.Fatal("sanity check failed: the glide chain should be running")
	}

	// A keypress interrupts the drag WITHOUT releasing the mouse — clears
	// transcriptSelection.active directly, bypassing stopEdgeScroll.
	updated, _ = m.Update(testKey('a'))
	m = updated.(model)
	if m.transcriptSelection.active {
		t.Fatal("sanity check failed: the keypress should have cleared the selection")
	}
	if m.edgeScrollDelta == 0 {
		t.Fatal("sanity check failed: edgeScrollDelta should still be stale here (that's the bug this test guards)")
	}

	// A brand-new press+drag past the SAME edge must glide normally, not be
	// silently swallowed by the stale state from the interrupted drag. The
	// viewport shifted from the first glide's step, so the topmost visible text
	// line's on-screen Y must be recomputed — reusing the ORIGINAL textY would
	// fail for an unrelated reason (it may now land on a blank spacer row).
	textY = topmostVisibleTranscriptMouseY(t, m)
	updated, _ = m.Update(testMouseClick(tea.MouseLeft, 0, textY))
	m = updated.(model)
	if !m.transcriptSelection.active {
		t.Fatal("sanity check failed: the new press should start a fresh selection")
	}
	scrollBefore := m.chatScrollOffset

	updated, cmd := m.Update(testMouseMotion(tea.MouseLeft, 0, aboveBody))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("the new drag should schedule a fresh glide tick chain, not be fooled by stale state")
	}
	if m.chatScrollOffset == scrollBefore {
		t.Fatal("the new drag should have applied its immediate first scroll step")
	}
}

func TestTranscriptSelectionClearsAfterCopy(t *testing.T) {
	m := mouseTestModel()
	m.transcriptSelection = transcriptSelectionState{active: true}

	updated, cmd := m.Update(transcriptCopiedMsg{chars: 5})
	next := updated.(model)
	if cmd == nil {
		t.Fatal("copy feedback should schedule a status clear")
	}
	if next.transcriptSelection.active {
		t.Fatal("selection highlight should clear after copy feedback")
	}
	if next.copyStatus != "Copied!" {
		t.Fatalf("copyStatus = %q, want Copied!", next.copyStatus)
	}
}

func TestTranscriptSelectionIncludesToolResultBody(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowToolCall, id: "call_1", tool: "bash", detail: "go build ./..."})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind:   rowToolResult,
		id:     "call_1",
		tool:   "bash",
		status: tools.StatusError,
		detail: "stdout:\nok build\nstderr:\nwarning: slow\nexit_code: 1",
	})

	line := transcriptSelectableLineContaining(t, m, "warning: slow")
	m.transcriptSelection = transcriptSelectionState{
		active: true,
		anchor: transcriptSelectionPoint{bodyY: line.bodyY, x: line.textStart},
		cursor: transcriptSelectionPoint{bodyY: line.bodyY, x: line.textStart + lipgloss.Width(line.text)},
	}

	if got := m.selectedTranscriptText(); strings.TrimSpace(got) != "warning: slow" {
		t.Fatalf("selectedTranscriptText() = %q, want warning: slow", got)
	}
}

func TestTranscriptSelectionIncludesErrorRows(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowError, text: "provider stream error: connection reset"})

	line := transcriptSelectableLineContaining(t, m, "provider stream error")
	m.transcriptSelection = transcriptSelectionState{
		active: true,
		anchor: transcriptSelectionPoint{bodyY: line.bodyY, x: line.textStart},
		cursor: transcriptSelectionPoint{bodyY: line.bodyY, x: line.textStart + lipgloss.Width(line.text)},
	}

	if got := m.selectedTranscriptText(); got != "provider stream error: connection reset" {
		t.Fatalf("selectedTranscriptText() = %q, want provider stream error: connection reset", got)
	}
}

func TestMouseClickTogglesReasoningRow(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowReasoning, text: "private thought"})

	width := m.chatColumnWidth()
	body, selectable := m.transcriptBody(width, "")
	start, _, top := m.transcriptViewportStart(body, width)
	var target transcriptSelectableLine
	for _, line := range selectable {
		if line.toggle {
			target = line
			break
		}
	}
	if !target.toggle {
		t.Fatalf("expected reasoning header to be clickable, selectable=%#v", selectable)
	}

	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, target.textStart, top+target.bodyY-start))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("reasoning toggle click should not return a command")
	}
	if !next.transcript[len(next.transcript)-1].expanded {
		t.Fatalf("reasoning row should expand after click: %#v", next.transcript[len(next.transcript)-1])
	}
	if next.transcriptSelection.active {
		t.Fatal("reasoning toggle should not start transcript selection")
	}
}

func TestMouseClickTogglesStreamingReasoning(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.pending = true
	m.activeRunID = 1
	m.streamingReasoning = "private **thought**"

	width := m.chatColumnWidth()
	body, selectable := m.transcriptBody(width, "")
	start, _, top := m.transcriptViewportStart(body, width)
	var target transcriptSelectableLine
	for _, line := range selectable {
		if line.toggle && line.live {
			target = line
			break
		}
	}
	if !target.toggle || !target.live {
		t.Fatalf("expected live reasoning header to be clickable, selectable=%#v", selectable)
	}

	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, target.textStart, top+target.bodyY-start))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("streaming reasoning toggle click should not return a command")
	}
	if !next.streamingReasoningExpanded {
		t.Fatal("streaming reasoning should expand after click")
	}
	view := plainRender(t, next.interimBlock(width))
	if !strings.Contains(view, "private thought") || strings.Contains(view, "**") {
		t.Fatalf("expanded streaming reasoning should render markdown-clean text, got:\n%s", view)
	}
}

func TestTranscriptCopyStatusClearsOnlyForLatestCopy(t *testing.T) {
	m := mouseTestModel()

	updated, _ := m.Update(transcriptCopiedMsg{chars: 5})
	m = updated.(model)
	firstSeq := m.copyStatusSeq

	updated, _ = m.Update(transcriptCopiedMsg{chars: 8})
	m = updated.(model)
	secondSeq := m.copyStatusSeq

	updated, _ = m.Update(transcriptCopyStatusExpiredMsg{seq: firstSeq})
	m = updated.(model)
	if m.copyStatus != "Copied!" {
		t.Fatalf("stale expiry cleared status: %q", m.copyStatus)
	}

	updated, _ = m.Update(transcriptCopyStatusExpiredMsg{seq: secondSeq})
	m = updated.(model)
	if m.copyStatus != "" {
		t.Fatalf("latest expiry left status = %q, want empty", m.copyStatus)
	}
}

func TestMCPManagerMouseSelectsFirstItemRow(t *testing.T) {
	m := newModel(context.Background(), Options{
		MCPConfig: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs": {Type: "stdio", Command: "docs-mcp"},
		}},
	})
	m.width = 120
	m.height = 36
	m = m.openMCPManager()
	width := m.chatColumnWidth()
	overlay := m.mcpManagerOverlay(width)
	lines := viewLines(overlay)
	left, _, _ := normalizeOverlayBlock(lines, width)
	y := m.overlayMouseTop(len(lines), width) + mcpManagerFirstItemRow(m.mcpViewState())

	target, ok := m.selectMCPManagerAtMouse(testMouseClick(tea.MouseLeft, left+2, y))
	if !ok {
		t.Fatal("expected click on first manager item row to select")
	}
	if target.Index != 0 || m.mcpManager.selected != 0 || target.Value != "docs" {
		t.Fatalf("selected target = %#v manager=%#v, want first docs item", target, m.mcpManager)
	}
}

func TestMCPAddWizardMouseSelectsAndActivatesType(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 120
	m.height = 36
	m.mcpAddWizard = newMCPAddWizard("http")
	m.mcpAddWizard.step = mcpAddWizardStepType
	width := m.chatColumnWidth()
	overlay := m.mcpAddWizardOverlay(width)
	lines := viewLines(overlay)
	left, _, _ := normalizeOverlayBlock(lines, width)
	y := m.overlayMouseTop(len(lines), width) + 5 // second type row: top border + step + rule + title + first row
	msg := testMouseClick(tea.MouseLeft, left+2, y)

	updated, cmd := m.Update(msg)
	next := updated.(model)
	if cmd != nil {
		t.Fatal("single click should only select the type")
	}
	if next.mcpAddWizard.serverType != "sse" {
		t.Fatalf("serverType after click = %q, want sse", next.mcpAddWizard.serverType)
	}

	updated, cmd = next.Update(msg)
	next = updated.(model)
	if cmd != nil {
		t.Fatal("double-click type activation should advance synchronously")
	}
	if next.mcpAddWizard.step != mcpAddWizardStepEndpoint {
		t.Fatalf("wizard step after double-click = %v, want endpoint", next.mcpAddWizard.step)
	}
}

func TestMCPAddWizardBlocksTranscriptSelectionBehindOverlay(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	m.transcript = appendRow(m.transcript, rowUser, "hidden behind wizard")
	textX, textY := firstTranscriptTextMousePoint(t, m)
	m.mcpAddWizard = newMCPAddWizard("http")
	m.mcpAddWizard.step = mcpAddWizardStepType

	updated, cmd := m.Update(testMouseClick(tea.MouseLeft, textX, textY))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("click behind MCP add wizard should not return a command")
	}
	if next.transcriptSelection.active {
		t.Fatal("MCP add wizard should block transcript selection behind the overlay")
	}
}

func TestTranscriptCopyStatusUsesComposerSpacerWithoutFooterGrowth(t *testing.T) {
	m := mouseTestModel()
	m.providerName = "ollama-cloud"
	m.modelName = "qwen3-coder:480b"

	normalFooterLines := len(viewLines(plainRender(t, m.footerView(80))))
	normalViewLines := len(viewLines(plainRender(t, m.View())))
	m.copyStatus = "Copied!"

	footer := plainRender(t, m.footerView(80))
	if got := len(viewLines(footer)); got != normalFooterLines {
		t.Fatalf("copy status changed footer height from %d to %d:\n%s", normalFooterLines, got, footer)
	}
	view := plainRender(t, m.View())
	if got := len(viewLines(view)); got != normalViewLines {
		t.Fatalf("copy status changed view height from %d to %d:\n%s", normalViewLines, got, view)
	}
	if !strings.Contains(view, "Copied!") {
		t.Fatalf("view should show copy status, got:\n%s", view)
	}
	footerLines := viewLines(footer)
	if len(footerLines) < 2 || !strings.Contains(footerLines[0], "Copied!") || !strings.HasPrefix(footerLines[1], "╭") {
		t.Fatalf("copy status should replace the spacer directly above composer, got:\n%s", footer)
	}
	if strings.Contains(plainRender(t, m.statusLine(80)), "Copied!") {
		t.Fatalf("status line should not contain copy feedback: %q", plainRender(t, m.statusLine(80)))
	}
}

func TestMouseCaptureOnlyDuringInteractiveSetupStages(t *testing.T) {
	m := setupMouseTestModel()
	stages := []struct {
		stage setupStage
		want  bool
	}{
		{setupStageWelcome, false},
		{setupStageProvider, true},
		{setupStageCredentials, false},
		{setupStageModel, true},
		{setupStageSafety, false},
		{setupStageReady, false},
	}

	for _, tt := range stages {
		m.setup.stage = tt.stage
		if got := m.wantsMouseCapture(); got != tt.want {
			t.Fatalf("wantsMouseCapture at setup stage %v = %v, want %v", tt.stage, got, tt.want)
		}
	}
}

// TestComposerMouseSelectionBlockedInDetailedMode: composer mouse hit-testing
// is disabled while the detailed transcript view is active, so wheel and click
// events in the composer area reach the transcript body instead.
func TestComposerMouseSelectionBlockedInDetailedMode(t *testing.T) {
	m := mouseTestModel()
	m.input.SetValue("some text")

	updated, _ := m.Update(testKeyCtrl('o'))
	m = updated.(model)

	if !m.transcriptDetailed {
		t.Fatal("sanity check: Ctrl+O should enter detailed mode")
	}
	if !m.composerMouseSelectionBlocked() {
		t.Fatal("composerMouseSelectionBlocked should be true in detailed mode")
	}

	width := m.chatColumnWidth()
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	if m.mouseOverComposer(testMouseWheel(tea.MouseWheelUp, 0, frame.composerRect.y)) {
		t.Fatal("mouseOverComposer should return false in detailed mode")
	}
}

func firstTranscriptTextMouseY(t *testing.T, m model) int {
	t.Helper()
	_, y := firstTranscriptTextMousePoint(t, m)
	return y
}

func firstTranscriptTextMousePoint(t *testing.T, m model) (int, int) {
	t.Helper()
	width := m.chatColumnWidth()
	body, selectable := m.transcriptBody(width, "")
	start, _, top := m.transcriptViewportStart(body, width)
	for _, line := range selectable {
		if line.text != "" && !line.toggle {
			return line.textStart, top + line.bodyY - start
		}
	}
	t.Fatalf("no selectable transcript text line found: %#v", selectable)
	return 0, 0
}

func transcriptSelectableLineContaining(t *testing.T, m model, text string) transcriptSelectableLine {
	t.Helper()
	width := m.chatColumnWidth()
	layout := m.transcriptBodyLayout(width, "")
	for _, line := range layout.selectable {
		if strings.Contains(line.text, text) {
			return line
		}
	}
	t.Fatalf("no selectable transcript line containing %q found: %#v", text, layout.selectable)
	return transcriptSelectableLine{}
}

func composerMousePoint(t *testing.T, m model, column int) (int, int) {
	t.Helper()
	width := m.chatColumnWidth()
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	if frame.composerRect.height <= 0 {
		t.Fatalf("expected visible composer rect, frame=%#v", frame)
	}
	contentY := 1 + m.attachmentComposerPrefixRows(width)
	x := frame.composerRect.x + 2 + lipgloss.Width(composerVisualLinePrefix(m.input, true)) + column
	y := frame.composerRect.y + contentY
	return x, y
}

func mouseTestModel() model {
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.height = 30
	m.altScreen = true
	m.headerPrinted = true
	return m
}

// withSidebarContent gives the model an active plan so the two-column sidebar
// stays put (an empty panel now auto-hides), for tests that depend on the
// narrower chat-column geometry.
func withSidebarContent(m model) model {
	m.plan.steps = []planStep{{content: "wire it up", status: "in_progress"}}
	return m
}

func TestParseTracerPidMissing(t *testing.T) {
	if got := parseTracerPid([]byte("Pid: 123\nName: foo\n")); got {
		t.Fatal("missing TracerPid should return false")
	}
}

func TestParseTracerPidZero(t *testing.T) {
	if got := parseTracerPid([]byte("TracerPid: 0\nName: foo\n")); got {
		t.Fatal("TracerPid: 0 should return false")
	}
}

func TestParseTracerPidNonZero(t *testing.T) {
	if got := parseTracerPid([]byte("Name: foo\nTracerPid: 42\n")); !got {
		t.Fatal("TracerPid: 42 should return true")
	}
}

func TestParseTracerPidMalformed(t *testing.T) {
	// Non-numeric values are treated as non-zero since any non-"0" string
	// indicates a tracer is attached, matching the function's semantics.
	if got := parseTracerPid([]byte("TracerPid: abc\n")); !got {
		t.Fatal("malformed TracerPid should return true (non-zero string)")
	}
}

func TestParseTracerPidEmpty(t *testing.T) {
	if got := parseTracerPid([]byte{}); got {
		t.Fatal("empty input should return false")
	}
}

func setupMouseTestModel() model {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
				{ID: "anthropic", Name: "Anthropic", DefaultModel: "claude-sonnet-4.5", EnvVar: "ANTHROPIC_API_KEY", RequiresAuth: true},
				{ID: "ollama", Name: "Ollama Local", DefaultModel: "llama3.1", Local: true},
			},
		},
	})
	m.width = 100
	m.height = 30
	m.altScreen = true
	return m
}
