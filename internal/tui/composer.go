package tui

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type composerState struct {
	text   string
	cursor int
}

type composerSelectionState struct {
	active bool
	anchor int
	cursor int
}

type composerPastePreview struct {
	active bool
	start  int
	end    int
	label  string
}

func insertComposerText(state composerState, text string) composerState {
	state = normalizeComposerState(state)
	if text == "" {
		return state
	}
	runes := []rune(state.text)
	insert := []rune(text)
	out := make([]rune, 0, len(runes)+len(insert))
	out = append(out, runes[:state.cursor]...)
	out = append(out, insert...)
	out = append(out, runes[state.cursor:]...)
	return composerState{text: string(out), cursor: state.cursor + len(insert)}
}

func deleteComposerWordBefore(state composerState) composerState {
	state = normalizeComposerState(state)
	if state.cursor == 0 {
		return state
	}
	runes := []rune(state.text)
	start := state.cursor
	for start > 0 && unicode.IsSpace(runes[start-1]) {
		start--
	}
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	return deleteComposerRange(state, start, state.cursor)
}

func deleteComposerWordAfter(state composerState) composerState {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	if state.cursor >= len(runes) {
		return state
	}
	end := state.cursor
	for end < len(runes) && unicode.IsSpace(runes[end]) {
		end++
	}
	for end < len(runes) && !unicode.IsSpace(runes[end]) {
		end++
	}
	for end < len(runes) && runes[end] != '\n' && unicode.IsSpace(runes[end]) {
		end++
	}
	return deleteComposerRange(state, state.cursor, end)
}

func moveComposerWordBefore(state composerState) composerState {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	pos := state.cursor
	for pos > 0 && unicode.IsSpace(runes[pos-1]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(runes[pos-1]) {
		pos--
	}
	state.cursor = pos
	return state
}

func moveComposerWordAfter(state composerState) composerState {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	pos := state.cursor
	for pos < len(runes) && unicode.IsSpace(runes[pos]) {
		pos++
	}
	for pos < len(runes) && !unicode.IsSpace(runes[pos]) {
		pos++
	}
	state.cursor = pos
	return state
}

func sanitizeComposerPaste(text string) string {
	var out strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '\r':
			out.WriteRune('\n')
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
		case '\n':
			out.WriteRune('\n')
		case '\t':
			out.WriteString("    ")
		default:
			if !unicode.IsControl(r) {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func sanitizeComposerInput(text string) string {
	return sanitizeComposerPaste(strings.ReplaceAll(text, "\n", ""))
}

func (m model) composerValue() string {
	if m.composerActive {
		return m.composer.text
	}
	return m.input.Value()
}

func (m model) currentComposerState() composerState {
	if m.composerActive {
		return normalizeComposerState(m.composer)
	}
	return composerState{text: m.input.Value(), cursor: m.input.Position()}
}

func (m *model) setComposerState(state composerState) {
	m.composer = normalizeComposerState(state)
	m.composerActive = true
	m.syncInputFromComposer()
}

func (m *model) clearComposer() {
	m.composer = composerState{}
	m.composerActive = false
	m.composerPastePreviews = nil
	m.composerSelection = composerSelectionState{}
	m.input.SetValue("")
}

func (m *model) resetComposerFromInput() {
	m.composer = composerState{}
	m.composerActive = false
	m.composerPastePreviews = nil
	m.composerSelection = composerSelectionState{}
}

func (m *model) syncInputFromComposer() {
	display := strings.ReplaceAll(m.composer.text, "\n", " ")
	m.input.SetValue(display)
	m.input.SetCursor(composerDisplayCursor(m.composer))
}

func composerDisplayCursor(state composerState) int {
	state = normalizeComposerState(state)
	count := 0
	for range []rune(state.text)[:state.cursor] {
		count++
	}
	return count
}

func (selection composerSelectionState) rangeFor(state composerState) (int, int, bool) {
	if !selection.active {
		return 0, 0, false
	}
	state = normalizeComposerState(state)
	start := clamp(selection.anchor, 0, len([]rune(state.text)))
	end := clamp(selection.cursor, 0, len([]rune(state.text)))
	if start > end {
		start, end = end, start
	}
	return start, end, start != end
}

func (m model) selectedComposerText() string {
	state := m.currentComposerState()
	start, end, ok := m.composerSelection.rangeFor(state)
	if !ok {
		return ""
	}
	return string([]rune(state.text)[start:end])
}

func (m model) handleComposerSelectionMouse(msg tea.MouseMsg) (model, tea.Cmd, bool) {
	switch {
	case mouseLeftPress(msg):
		pos, ok := m.composerPositionAtMouse(msg)
		if !ok {
			if m.composerSelection.active {
				m.composerSelection = composerSelectionState{}
				return m, nil, true
			}
			return m, nil, false
		}
		state := m.currentComposerState()
		state.cursor = pos
		m.setComposerState(state)
		m.copyStatus = ""
		m.composerSelection = composerSelectionState{active: true, anchor: pos, cursor: pos}
		return m, nil, true
	case mouseMotion(msg):
		if !m.composerSelection.active {
			return m, nil, false
		}
		if pos, ok := m.composerPositionAtMouse(msg); ok {
			state := m.currentComposerState()
			state.cursor = pos
			m.setComposerState(state)
			m.composerSelection.active = true
			m.composerSelection.cursor = pos
		}
		return m, nil, true
	case mouseRelease(msg):
		if !m.composerSelection.active {
			return m, nil, false
		}
		if pos, ok := m.composerPositionAtMouse(msg); ok {
			state := m.currentComposerState()
			state.cursor = pos
			m.setComposerState(state)
			m.composerSelection.active = true
			m.composerSelection.cursor = pos
		}
		text := m.selectedComposerText()
		m.composerSelection = composerSelectionState{}
		if strings.TrimSpace(text) == "" {
			return m, nil, true
		}
		return m, copyTranscriptSelectionCmd(text), true
	default:
		return m, nil, false
	}
}

func (m model) composerPositionAtMouse(msg tea.MouseMsg) (int, bool) {
	if !m.altScreen || m.height <= 0 || m.composerMouseSelectionBlocked() {
		return 0, false
	}
	width := m.chatColumnWidth()
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	localX, localY, ok := frame.composerRect.local(mouseX(msg), mouseY(msg))
	if !ok {
		return 0, false
	}
	if width < 8 {
		return m.composerPositionAtVisualCell(localX, localY, width)
	}
	contentY := localY - 1 - m.attachmentComposerPrefixRows(width)
	if contentY < 0 {
		return 0, false
	}
	return m.composerPositionAtVisualCell(localX-2, contentY, maxInt(1, width-4))
}

func (m model) composerMouseSelectionBlocked() bool {
	return m.transcriptDetailed || m.setup.visible || m.providerWizard != nil || m.mcpAddWizard != nil ||
		m.mcpManager != nil || m.picker != nil || m.renamePrompt != nil || m.suggestionsActive()
}

func (m model) composerPositionAtVisualCell(x int, y int, width int) (int, bool) {
	input := m.input
	state := m.currentComposerState()
	previews := validComposerPastePreviews(state, m.composerPastePreviews)
	displayState := composerDisplayStateForPastePreviews(state, previews)
	segments, _ := composerVisibleVisualLines(input, displayState, width)
	if len(segments) == 0 {
		return 0, y == 0
	}
	if y < 0 || y >= len(segments) {
		return 0, false
	}
	segment := segments[y]
	prefixWidth := lipgloss.Width(composerVisualLinePrefix(input, segment.first))
	column := maxInt(0, x-prefixWidth)
	displayCursor := composerCursorForVisualColumn(displayState, segment, column)
	return clamp(composerOriginalCursorForPastePreviews(displayCursor, previews), 0, len([]rune(state.text))), true
}

func (m model) applyComposerKey(msg tea.KeyMsg) (model, bool) {
	state := m.currentComposerState()
	switch {
	case keyIs(msg, tea.KeyEnter) && (keyAlt(msg) || keyShift(msg)):
		m = m.insertComposerTextWithPastePreview(state, "\n", "")
	case keyCtrl(msg, 'j'):
		m = m.insertComposerTextWithPastePreview(state, "\n", "")
	case keyAlt(msg) && keyText(msg) == "d":
		end := deleteComposerWordAfter(state).cursor
		nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, state.cursor, end)
		m.setComposerState(nextState)
		m.composerPastePreviews = nextPreviews
	case keyAlt(msg) && keyIs(msg, tea.KeyLeft):
		m.setComposerState(moveComposerWordBefore(state))
	case keyAlt(msg) && keyIs(msg, tea.KeyRight):
		m.setComposerState(moveComposerWordAfter(state))
	case keyCtrlArrow(msg, tea.KeyLeft):
		m.setComposerState(moveComposerWordBefore(state))
	case keyCtrlArrow(msg, tea.KeyRight):
		m.setComposerState(moveComposerWordAfter(state))
	case keyAlt(msg) && keyText(msg) == "b":
		m.setComposerState(moveComposerWordBefore(state))
	case keyAlt(msg) && keyText(msg) == "f":
		m.setComposerState(moveComposerWordAfter(state))
	case keyIs(msg, tea.KeySpace):
		m = m.insertComposerTextWithPastePreview(state, " ", "")
	case keyPrintable(msg):
		m = m.applyComposerText(state, keyText(msg), false)
	case keyIs(msg, tea.KeyLeft) || keyCtrl(msg, 'b'):
		if nextState, ok := moveComposerPastePreviewBoundary(state, m.composerPastePreviews, -1); ok {
			m.setComposerState(nextState)
			break
		}
		state.cursor--
		m.setComposerState(state)
	case keyIs(msg, tea.KeyRight) || keyCtrl(msg, 'f'):
		if nextState, ok := moveComposerPastePreviewBoundary(state, m.composerPastePreviews, 1); ok {
			m.setComposerState(nextState)
			break
		}
		state.cursor++
		m.setComposerState(state)
	case keyIs(msg, tea.KeyHome) || keyCtrl(msg, 'a'):
		state.cursor = composerLineStart(state)
		m.setComposerState(state)
	case keyIs(msg, tea.KeyEnd) || keyCtrl(msg, 'e'):
		state.cursor = composerLineEnd(state)
		m.setComposerState(state)
	case keyCtrl(msg, 'u'):
		nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, composerLineStart(state), state.cursor)
		m.setComposerState(nextState)
		m.composerPastePreviews = nextPreviews
	case keyCtrl(msg, 'k'):
		nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, state.cursor, composerLineEnd(state))
		m.setComposerState(nextState)
		m.composerPastePreviews = nextPreviews
	case keyCtrl(msg, 'w') || (keyAlt(msg) && keyBackspace(msg)):
		start := deleteComposerWordBefore(state).cursor
		nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, start, state.cursor)
		m.setComposerState(nextState)
		m.composerPastePreviews = nextPreviews
	case keyAlt(msg) && keyIs(msg, tea.KeyDelete):
		end := deleteComposerWordAfter(state).cursor
		nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, state.cursor, end)
		m.setComposerState(nextState)
		m.composerPastePreviews = nextPreviews
	case keyBackspace(msg):
		if nextState, nextPreviews, ok := deleteComposerPastePreviewBefore(state, m.composerPastePreviews); ok && !m.suggestionsActive() {
			m.setComposerState(nextState)
			m.composerPastePreviews = nextPreviews
		} else if start, end, ok := completedFileMentionRangeBefore(state); ok && !m.suggestionsActive() {
			nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, start, end)
			m.setComposerState(nextState)
			m.composerPastePreviews = nextPreviews
		} else {
			nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, state.cursor-1, state.cursor)
			m.setComposerState(nextState)
			m.composerPastePreviews = nextPreviews
		}
	case keyIs(msg, tea.KeyDelete) || keyCtrl(msg, 'd'):
		if nextState, nextPreviews, ok := deleteComposerPastePreviewAfter(state, m.composerPastePreviews); ok {
			m.setComposerState(nextState)
			m.composerPastePreviews = nextPreviews
		} else {
			nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, state.cursor, state.cursor+1)
			m.setComposerState(nextState)
			m.composerPastePreviews = nextPreviews
		}
	default:
		return m, false
	}

	if strings.Contains(m.composer.text, "\n") {
		m.clearSuggestions()
	} else {
		m.recomputeSuggestions()
	}
	return m, true
}

func (m model) applyComposerText(state composerState, text string, paste bool) model {
	previewLabel := ""
	if paste {
		text = sanitizeComposerPaste(text)
		previewLabel, _ = composerPastePreviewLabel(text, m.composerPastePreviewWrapWidth())
	} else {
		text = sanitizeComposerInput(text)
	}
	if shouldInsertCommandArgumentSpace(state, text) {
		text = " " + text
		if previewLabel != "" {
			previewLabel = " " + previewLabel
		}
	}
	return m.insertComposerTextWithPastePreview(state, text, previewLabel)
}

func (m model) insertComposerTextWithPastePreview(state composerState, text string, previewLabel string) model {
	state = normalizeComposerState(state)
	insertStart := state.cursor
	insertedRunes := len([]rune(text))
	nextPreviews := composerPastePreviewsAfterInsert(m.composerPastePreviews, insertStart, insertedRunes)
	m.setComposerState(insertComposerText(state, text))
	if previewLabel != "" && insertedRunes > 0 {
		previewLabel = composerPastePreviewLabelWithIndex(previewLabel, len(nextPreviews)+1)
		nextPreviews = append(nextPreviews, composerPastePreview{
			active: true,
			start:  insertStart,
			end:    insertStart + insertedRunes,
			label:  previewLabel,
		})
		sortComposerPastePreviews(nextPreviews)
		m.composerPastePreviews = nextPreviews
		return m
	}
	m.composerPastePreviews = nextPreviews
	return m
}

func (m model) replaceComposerRangeWithPastePreviews(state composerState, start int, end int, replacement string) model {
	nextState, nextPreviews := deleteComposerRangeWithPastePreviews(state, m.composerPastePreviews, start, end)
	m.setComposerState(nextState)
	m.composerPastePreviews = nextPreviews
	if replacement == "" {
		return m
	}
	return m.insertComposerTextWithPastePreview(m.currentComposerState(), replacement, "")
}

func (m model) composerPastePreviewWrapWidth() int {
	width := chatWidth(m.width)
	if width < 8 {
		width = defaultStartupWidth
	}
	return maxInt(1, width-4-lipgloss.Width(m.input.Prompt))
}

func composerPastePreviewLabel(text string, wrapWidth int) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	runeCount := len([]rune(text))
	lineCount := composerPastePreviewVisualLineCount(text, wrapWidth)
	if lineCount < 3 && runeCount < 220 {
		return "", false
	}
	snippet := text
	if newline := strings.IndexRune(snippet, '\n'); newline >= 0 {
		snippet = snippet[:newline]
	}
	snippet = strings.Join(strings.Fields(snippet), " ")
	if snippet == "" {
		snippet = "pasted text"
	}
	snippet = truncateComposerPasteSnippet(snippet, 48)
	label := "lines"
	if lineCount == 1 {
		label = "line"
	}
	return "[" + snippet + " · " + strconv.Itoa(lineCount) + " " + label + "]", true
}

func composerPastePreviewLabelWithIndex(label string, pasteNumber int) string {
	if pasteNumber <= 1 || !strings.HasSuffix(label, "]") {
		return label
	}
	return strings.TrimSuffix(label, "]") + ", paste " + strconv.Itoa(pasteNumber) + "]"
}

func composerPastePreviewVisualLineCount(text string, wrapWidth int) int {
	wrapWidth = maxInt(1, wrapWidth)
	total := 0
	for _, line := range strings.Split(text, "\n") {
		width := lipgloss.Width(line)
		total += maxInt(1, (width+wrapWidth-1)/wrapWidth)
	}
	return total
}

func truncateComposerPasteSnippet(text string, limit int) string {
	runes := []rune(text)
	if limit <= 3 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit-3]) + "..."
}

func composerPastePreviewsAfterInsert(previews []composerPastePreview, pos int, length int) []composerPastePreview {
	if length <= 0 || len(previews) == 0 {
		return previews
	}
	next := make([]composerPastePreview, 0, len(previews))
	for _, preview := range previews {
		if !preview.active {
			continue
		}
		switch {
		case pos <= preview.start:
			preview.start += length
			preview.end += length
		case pos < preview.end:
			continue
		}
		next = append(next, preview)
	}
	sortComposerPastePreviews(next)
	return next
}

func moveComposerPastePreviewBoundary(state composerState, previews []composerPastePreview, direction int) (composerState, bool) {
	state = normalizeComposerState(state)
	for _, preview := range validComposerPastePreviews(state, previews) {
		switch {
		case direction < 0 && state.cursor > preview.start && state.cursor <= preview.end:
			state.cursor = preview.start
			return state, true
		case direction > 0 && state.cursor >= preview.start && state.cursor < preview.end:
			state.cursor = preview.end
			return state, true
		}
	}
	return state, false
}

func deleteComposerPastePreviewBefore(state composerState, previews []composerPastePreview) (composerState, []composerPastePreview, bool) {
	state = normalizeComposerState(state)
	valid := validComposerPastePreviews(state, previews)
	for index, preview := range valid {
		if state.cursor != preview.end {
			continue
		}
		return deleteComposerRange(state, preview.start, preview.end), composerPastePreviewsAfterDelete(valid, index), true
	}
	return state, previews, false
}

func deleteComposerPastePreviewAfter(state composerState, previews []composerPastePreview) (composerState, []composerPastePreview, bool) {
	state = normalizeComposerState(state)
	valid := validComposerPastePreviews(state, previews)
	for index, preview := range valid {
		if state.cursor != preview.start {
			continue
		}
		return deleteComposerRange(state, preview.start, preview.end), composerPastePreviewsAfterDelete(valid, index), true
	}
	return state, previews, false
}

func deleteComposerRangeWithPastePreviews(state composerState, previews []composerPastePreview, start int, end int) (composerState, []composerPastePreview) {
	state = normalizeComposerState(state)
	runeCount := len([]rune(state.text))
	start = clamp(start, 0, runeCount)
	end = clamp(end, 0, runeCount)
	if end < start {
		start, end = end, start
	}
	if start == end {
		return state, validComposerPastePreviews(state, previews)
	}

	valid := validComposerPastePreviews(state, previews)
	deleteStart, deleteEnd := start, end
	for {
		expanded := false
		for _, preview := range valid {
			if !composerRangesOverlap(deleteStart, deleteEnd, preview.start, preview.end) {
				continue
			}
			if preview.start < deleteStart {
				deleteStart = preview.start
				expanded = true
			}
			if preview.end > deleteEnd {
				deleteEnd = preview.end
				expanded = true
			}
		}
		if !expanded {
			break
		}
	}

	nextState := deleteComposerRange(state, deleteStart, deleteEnd)
	delta := deleteEnd - deleteStart
	nextPreviews := make([]composerPastePreview, 0, len(valid))
	for _, preview := range valid {
		switch {
		case composerRangesOverlap(deleteStart, deleteEnd, preview.start, preview.end):
			continue
		case preview.start >= deleteEnd:
			preview.start -= delta
			preview.end -= delta
		}
		nextPreviews = append(nextPreviews, preview)
	}
	sortComposerPastePreviews(nextPreviews)
	return nextState, nextPreviews
}

func composerRangesOverlap(leftStart int, leftEnd int, rightStart int, rightEnd int) bool {
	return leftStart < rightEnd && rightStart < leftEnd
}

func validComposerPastePreviews(state composerState, previews []composerPastePreview) []composerPastePreview {
	state = normalizeComposerState(state)
	if len(previews) == 0 {
		return nil
	}
	runeCount := len([]rune(state.text))
	valid := make([]composerPastePreview, 0, len(previews))
	for _, preview := range previews {
		if !preview.active || preview.label == "" || preview.start < 0 || preview.start >= preview.end || preview.end > runeCount {
			continue
		}
		valid = append(valid, preview)
	}
	sortComposerPastePreviews(valid)
	out := valid[:0]
	lastEnd := -1
	for _, preview := range valid {
		if preview.start < lastEnd {
			continue
		}
		out = append(out, preview)
		lastEnd = preview.end
	}
	return out
}

func sortComposerPastePreviews(previews []composerPastePreview) {
	sort.SliceStable(previews, func(i, j int) bool {
		if previews[i].start == previews[j].start {
			return previews[i].end < previews[j].end
		}
		return previews[i].start < previews[j].start
	})
}

func composerPastePreviewsAfterDelete(previews []composerPastePreview, deleteIndex int) []composerPastePreview {
	if deleteIndex < 0 || deleteIndex >= len(previews) {
		return previews
	}
	deleted := previews[deleteIndex]
	delta := deleted.end - deleted.start
	next := make([]composerPastePreview, 0, len(previews)-1)
	for index, preview := range previews {
		if index == deleteIndex {
			continue
		}
		if preview.start >= deleted.end {
			preview.start -= delta
			preview.end -= delta
		}
		next = append(next, preview)
	}
	sortComposerPastePreviews(next)
	return next
}

func shouldInsertCommandArgumentSpace(state composerState, text string) bool {
	if text == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(text)
	if unicode.IsSpace(first) {
		return false
	}
	state = normalizeComposerState(state)
	if state.cursor != len([]rune(state.text)) {
		return false
	}
	if strings.TrimRightFunc(state.text, unicode.IsSpace) != state.text {
		return false
	}
	return commandArgumentHintForInput(state.text) != ""
}

func completedFileMentionRangeBefore(state composerState) (int, int, bool) {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	if state.cursor <= 0 || state.cursor > len(runes) || !isPathQueryBoundary(runes[state.cursor-1]) {
		return 0, 0, false
	}
	tokenEnd := state.cursor
	for tokenEnd > 0 && isPathQueryBoundary(runes[tokenEnd-1]) {
		tokenEnd--
	}
	tokenStart := tokenEnd
	for tokenStart > 0 && !isPathQueryBoundary(runes[tokenStart-1]) {
		tokenStart--
	}
	if tokenStart >= tokenEnd || runes[tokenStart] != '@' || tokenEnd-tokenStart <= 1 {
		return 0, 0, false
	}
	return tokenStart, state.cursor, true
}

func deleteComposerRange(state composerState, start int, end int) composerState {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	start = clamp(start, 0, len(runes))
	end = clamp(end, 0, len(runes))
	if end < start {
		start, end = end, start
	}
	if start == end {
		return state
	}
	out := make([]rune, 0, len(runes)-(end-start))
	out = append(out, runes[:start]...)
	out = append(out, runes[end:]...)
	return composerState{text: string(out), cursor: start}
}

func normalizeComposerState(state composerState) composerState {
	runes := []rune(state.text)
	state.cursor = clamp(state.cursor, 0, len(runes))
	return state
}

func composerLineStart(state composerState) int {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	pos := state.cursor
	for pos > 0 && runes[pos-1] != '\n' {
		pos--
	}
	return pos
}

func composerLineEnd(state composerState) int {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	pos := state.cursor
	for pos < len(runes) && runes[pos] != '\n' {
		pos++
	}
	return pos
}
