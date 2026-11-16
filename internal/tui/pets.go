package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/terminalpet"
)

const (
	petAmbientImageID        uint32 = 0xC0DE
	petPreviewImageID        uint32 = 0xC0DF
	petPreviewDelay                 = 140 * time.Millisecond
	petFrameDelay                   = 180 * time.Millisecond
	petImageColumns                 = 9
	petImageRows                    = 5
	petWrapGapColumns               = 2
	petReservedColumns              = petImageColumns + petWrapGapColumns
	petOutcomeHold                  = 2200 * time.Millisecond
	petDoubleClickWindow            = 350 * time.Millisecond
	petDockSnapColumns              = 2
	petResizeEdgeAnchorCells        = 2
	petPickerMaxWidth               = 58
	petSidePreviewMin               = 50
	petPreviewPaneGap               = 2
	// petTextMinWidth keeps the composer usable beside a text companion,
	// which is twice as wide as an image one.
	petTextMinWidth = 60
	// petSleepAfter is how long the companion waits, with no turn running, no
	// key pressed and no reaction playing, before it falls asleep.
	// model.petLastActiveAt records the last key press or turn boundary, from
	// clock readings Zero already takes there, so the key and turn paths read
	// the clock no more often than before. Paste and mouse wheel input do not
	// count.
	petSleepAfter = 5 * time.Minute
	// petBigTurn is how long a turn has to run for the companion to celebrate
	// when it succeeds, instead of the usual hop.
	petBigTurn = time.Minute
)

type petCatalogLoadedMsg struct {
	entries []terminalpet.Entry
	err     error
}

type petPreviewDebounceMsg struct {
	seq  uint64
	slug string
}

type petPreviewLoadedMsg struct {
	seq       uint64
	slug      string
	animation *terminalpet.Animation
	err       error
}

type petInstalledMsg struct {
	entry     terminalpet.Entry
	animation *terminalpet.Animation
	err       error
}

type petTickMsg struct{ seq uint64 }

func petTickCmd(seq uint64, delays ...time.Duration) tea.Cmd {
	delay := petFrameDelay
	if len(delays) > 0 && delays[0] > 0 {
		delay = delays[0]
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return petTickMsg{seq: seq} })
}

func (m model) handlePetsCommand(argument string) (tea.Model, tea.Cmd) {
	if m.petClient == nil {
		return m.appendSystemNotice("Pets are unavailable because the user config directory could not be resolved."), nil
	}
	textOnly := m.petRenderer != nil && !m.petRenderer.Support().Supported()
	if textOnly && !m.petTextMode() {
		return m.appendSystemNotice(m.petRenderer.Support().Reason + " Text companions need 256 colours."), nil
	}
	argument = strings.ToLower(strings.TrimSpace(argument))
	switch argument {
	case "off", "disable", "disabled", "hide", "hidden", "none":
		m.cancelPetPreview()
		if _, err := config.SetPet(m.userConfigPath, terminalpet.DisabledID); err != nil {
			return m.appendSystemNotice("Could not disable the terminal companion: " + err.Error()), nil
		}
		m.petID = terminalpet.DisabledID
		m.petName = ""
		m.petAnimation = nil
		m.petPreview = nil
		m.picker = nil
		m.petTickSeq++
		return m.showTransientNoticeInline("Terminal companion hidden. Run /pets to choose another.", transientNoticeInfo), nil
	}
	if entry, ok := terminalpet.ResolveBuiltin(argument); ok {
		// Built-in companions need no catalog, so they can be chosen offline.
		// An installed pet with that exact slug keeps the name it already
		// answered to; the built-in is then chosen from the picker.
		if installed, err := m.petClient.InstalledEntry(argument); err != nil || installed.Builtin {
			m.petEntries = map[string]terminalpet.Entry{entry.Slug: entry}
			return m.installPet(entry.Slug)
		}
	}
	if textOnly {
		return m.openTextPetPicker(argument)
	}
	m.petRequestedSlug = argument
	m.petCatalogError = ""
	localEntries, _ := m.petClient.InstalledEntries()
	var items []pickerItem
	m.petEntries, items = petPickerItems(localEntries)
	m.picker = &commandPicker{
		kind: pickerPet, title: "Choose a companion", loading: true,
		items: items, allItems: append([]pickerItem{}, items...), selected: selectedPetPickerItem(items, m.petID),
	}
	m, previewCmd := m.schedulePetPreview()
	catalogCmd := func() tea.Msg {
		entries, err := m.petClient.Catalog(m.ctx)
		return petCatalogLoadedMsg{entries: entries, err: err}
	}
	return m, batchCommands(previewCmd, catalogCmd)
}

// openTextPetPicker offers the built-in companions on a terminal that cannot
// show images. Community pets are pictures, so they are not listed and the
// catalog is not fetched.
func (m model) openTextPetPicker(requested string) (tea.Model, tea.Cmd) {
	if requested != "" {
		return m.appendSystemNotice(fmt.Sprintf("%q needs terminal images. Run /pets for the built-in companions.", requested)), nil
	}
	var items []pickerItem
	m.petEntries, items = petPickerItems(terminalpet.BuiltinEntries())
	m.petCatalogError = "Community pets need images"
	selected := selectedPetPickerItem(items, m.petID)
	if selected == 0 && m.petID != "" && m.petID != terminalpet.DisabledID {
		// The current pet is a community pet, which this picker cannot list.
		// Start on the first companion rather than "No companion", so Enter
		// cannot switch the pet off by accident.
		selected = minInt(1, len(items)-1)
	}
	m.picker = &commandPicker{
		kind: pickerPet, title: "Choose a companion",
		items: items, allItems: append([]pickerItem{}, items...), selected: selected,
	}
	return m.schedulePetPreview()
}

func (m model) applyPetCatalog(msg petCatalogLoadedMsg) (tea.Model, tea.Cmd) {
	if m.picker == nil || m.picker.kind != pickerPet {
		return m, nil
	}
	if msg.err != nil && len(msg.entries) == 0 {
		m.picker = nil
		return m.appendSystemNotice("Could not load the pet catalog: " + msg.err.Error()), nil
	}
	var items []pickerItem
	m.petEntries, items = petPickerItems(msg.entries)
	m.petCatalogError = ""
	if msg.err != nil {
		// The built-in and installed companions are still listed, so say why
		// the rest are missing instead of failing the whole picker.
		m.petCatalogError = "Community catalog unavailable"
	}
	if requested := strings.TrimSpace(m.petRequestedSlug); requested != "" {
		m.petRequestedSlug = ""
		if requested == terminalpet.DisabledID {
			return m.installPet(requested)
		}
		if _, ok := m.petEntries[requested]; !ok {
			m.picker = nil
			if msg.err != nil {
				// The pet may well exist: the catalog that would list it did not load.
				return m.appendSystemNotice("Could not load the pet catalog: " + msg.err.Error()), nil
			}
			return m.appendSystemNotice(fmt.Sprintf("No pet named %q. Run /pets to search the catalog.", requested)), nil
		}
		return m.installPet(requested)
	}
	query := m.picker.query
	m.picker = &commandPicker{
		kind: pickerPet, title: "Choose a companion", items: items,
		allItems: append([]pickerItem{}, items...), query: query, selected: selectedPetPickerItem(items, m.petID),
	}
	m.picker.applyQuery()
	return m.schedulePetPreview()
}

func petPickerItems(entries []terminalpet.Entry) (map[string]terminalpet.Entry, []pickerItem) {
	entryBySlug := make(map[string]terminalpet.Entry, len(entries))
	items := make([]pickerItem, 0, len(entries)+1)
	items = append(items, pickerItem{Label: "No companion", Value: terminalpet.DisabledID, Meta: "off"})
	for _, entry := range entries {
		entryBySlug[entry.Slug] = entry
		group := "Discover"
		switch {
		case entry.Builtin:
			group = "Built in"
		case entry.Local:
			group = "Installed"
		}
		items = append(items, pickerItem{Group: group, Label: entry.Label(), Value: entry.Slug, Local: entry.Local, Remote: !entry.Local})
	}
	return entryBySlug, items
}

func selectedPetPickerItem(items []pickerItem, petID string) int {
	for index, item := range items {
		if item.Value == petID {
			return index
		}
	}
	return 0
}

func (m model) schedulePetPreview() (model, tea.Cmd) {
	m.cancelPetPreview()
	m.petPreviewSeq++
	m.petPreview = nil
	m.petPreviewError = ""
	if m.picker == nil {
		m.petPreviewLoading = false
		m.petPreviewSlug = ""
		return m, nil
	}
	item, ok := m.picker.current()
	if !ok || item.Value == terminalpet.DisabledID {
		m.petPreviewLoading = false
		m.petPreviewSlug = ""
		return m, nil
	}
	m.petPreviewLoading = true
	m.petPreviewSlug = item.Value
	seq := m.petPreviewSeq
	slug := item.Value
	return m, tea.Tick(petPreviewDelay, func(time.Time) tea.Msg {
		return petPreviewDebounceMsg{seq: seq, slug: slug}
	})
}

func (m model) startPetPreview(msg petPreviewDebounceMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.petPreviewSeq || m.picker == nil || m.picker.kind != pickerPet {
		return m, nil
	}
	item, ok := m.picker.current()
	if !ok || item.Value != msg.slug {
		return m, nil
	}
	entry, ok := m.petEntries[msg.slug]
	if !ok {
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.petPreviewCancel = cancel
	return m, func() tea.Msg {
		animation, err := m.petClient.Preview(ctx, entry)
		return petPreviewLoadedMsg{seq: msg.seq, slug: msg.slug, animation: animation, err: err}
	}
}

func (m model) applyPetPreview(msg petPreviewLoadedMsg) model {
	if msg.seq != m.petPreviewSeq || msg.slug != m.petPreviewSlug {
		return m
	}
	m.petPreviewCancel = nil
	m.petPreviewLoading = false
	if msg.err != nil {
		if !errorsIsContext(msg.err) {
			m.petPreviewError = "Preview unavailable"
		}
		return m
	}
	m.petPreview = msg.animation
	m.petPreviewError = ""
	return m
}

func errorsIsContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (m *model) cancelPetPreview() {
	if m.petPreviewCancel != nil {
		m.petPreviewCancel()
		m.petPreviewCancel = nil
	}
}

func (m model) installPet(slug string) (tea.Model, tea.Cmd) {
	m.cancelPetPreview()
	m.picker = nil
	if slug == terminalpet.DisabledID {
		if _, err := config.SetPet(m.userConfigPath, terminalpet.DisabledID); err != nil {
			return m.appendSystemNotice("Could not save the pet preference: " + err.Error()), nil
		}
		m.petID = terminalpet.DisabledID
		m.petName = ""
		m.petAnimation = nil
		m.petTickSeq++
		return m.showTransientNoticeInline("Terminal companion hidden. Run /pets to choose another.", transientNoticeInfo), nil
	}
	entry, ok := m.petEntries[slug]
	if !ok {
		return m.appendSystemNotice(fmt.Sprintf("Pet %q is no longer in the catalog.", slug)), nil
	}
	m = m.showTransientNoticeInline("Installing "+entry.Label()+"…", transientNoticeInfo)
	return m, func() tea.Msg {
		animation, err := m.petClient.Install(m.ctx, entry)
		return petInstalledMsg{entry: entry, animation: animation, err: err}
	}
}

func (m model) applyPetInstall(msg petInstalledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m.appendSystemNotice("Could not install " + msg.entry.Label() + ": " + msg.err.Error()), nil
	}
	if _, err := config.SetPet(m.userConfigPath, msg.entry.Slug); err != nil {
		return m.appendSystemNotice("The pet was downloaded but could not be selected: " + err.Error()), nil
	}
	m.petID = msg.entry.Slug
	m.petName = msg.entry.Label()
	m.petAnimation = msg.animation
	m.petPhase = 0
	m.petTickSeq++
	m.petClickAnimationIndex = 0
	m.greetWithPet()
	notice := msg.entry.Label() + " is now your terminal companion. Use /pets off to hide it."
	if m.petTextMode() && m.width < petTextMinWidth {
		notice = fmt.Sprintf("%s shows once the window is %d+ columns wide.", msg.entry.Label(), petTextMinWidth)
	}
	m = m.showTransientNoticeInline(notice, transientNoticeSuccess)
	if m.reducedMotion {
		return m, nil
	}
	return m, petTickCmd(m.petTickSeq, m.petFrameDelay())
}

func (m model) petPickerOverlay(width int) string {
	if m.picker == nil {
		return ""
	}
	overlayWidth := minInt(width, petPickerMaxWidth)
	if overlayWidth < pickerOverlayMinWidth {
		overlayWidth = width
	}
	innerWidth := maxInt(1, overlayWidth-4)
	item, hasItem := m.picker.current()
	hasPreviewTarget := hasItem && item.Value != terminalpet.DisabledID
	sidePreview := hasPreviewTarget && innerWidth >= petSidePreviewMin
	previewColumns, previewRows := m.petSlotColumns(), m.petSlotRows()
	// On a text-only terminal the preview is drawn into the overlay itself;
	// otherwise the image is placed over the blank pane afterwards.
	var previewText []string
	if m.petTextMode() && m.petPreview != nil {
		previewText, _ = m.petPreview.TextFrame(terminalpet.Idle, m.petPhase, m.petTextColors())
	}
	listWidth := innerWidth
	if sidePreview {
		listWidth -= previewColumns + petPreviewPaneGap
	}
	previewDivider := zeroTheme.line.Render("│") + " "
	listLine := func(line string) string {
		line = fitStyledLine(line, listWidth)
		if !sidePreview {
			return line
		}
		return padStyledLine(line, listWidth) + previewDivider + strings.Repeat(" ", previewColumns)
	}
	listHeight := minInt(8, len(m.picker.items))
	start := 0
	if len(m.picker.items) > 0 {
		m.picker.selected = clampInt(m.picker.selected, 0, len(m.picker.items)-1)
		start = selectableListStart(len(m.picker.items), listHeight, m.picker.selected)
	}
	lines := []string{renderPickerSearchLine(m.picker.query, "search companions…", innerWidth), zeroTheme.line.Render(strings.Repeat("─", innerWidth))}
	listStartRow := len(lines)
	if len(m.picker.items) == 0 {
		lines = append(lines, listLine(zeroTheme.faint.Render("  no matching companions")))
	} else {
		lastGroup := ""
		for index, item := range m.picker.items[start : start+listHeight] {
			if item.Group != "" && item.Group != lastGroup {
				lines = append(lines, listLine(zeroTheme.accent.Render(item.Group)))
				lastGroup = item.Group
			}
			selected := start+index == m.picker.selected
			surface := transparentSurface
			marker := "  "
			if selected {
				surface = zeroTheme.onSel
				marker = "❯ "
			}
			name := truncatePetPickerColumn(item.Label, maxInt(1, listWidth-lipgloss.Width(marker)))
			lines = append(lines, listLine(surface(zeroTheme.accent).Render(marker)+surface(zeroTheme.ink).Render(name)))
		}
	}
	switch {
	case m.picker.loading:
		lines = append(lines, listLine(zeroTheme.accent.Render("Discover")))
		lines = append(lines, listLine(zeroTheme.faint.Render("  Fetching companions…")))
	case m.petCatalogError != "":
		lines = append(lines, listLine(zeroTheme.accent.Render("Discover")))
		lines = append(lines, listLine(zeroTheme.faint.Render("  "+m.petCatalogError)))
	}
	if sidePreview && m.petPreview != nil {
		// Images start on the title row; text starts on the row below it.
		needed := previewRows
		if previewText != nil {
			needed = len(previewText) + 1
		}
		for len(lines)-listStartRow < needed {
			lines = append(lines, listLine(""))
		}
	}
	if sidePreview && len(lines) > listStartRow {
		previewTitle := centerRenderedBlock(zeroTheme.accent.Render("Preview"), previewColumns)
		lines[listStartRow] = padStyledLine(ansi.Cut(lines[listStartRow], 0, listWidth), listWidth) +
			previewDivider + previewTitle
		for index, previewLine := range previewText {
			row := listStartRow + 1 + index
			lines[row] = padStyledLine(ansi.Cut(lines[row], 0, listWidth), listWidth) + previewDivider + previewLine
		}
	}
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	switch {
	case m.petPreviewLoading:
		lines = append(lines, zeroTheme.faint.Render("Loading preview…"))
	case m.petPreviewError != "":
		lines = append(lines, zeroTheme.faint.Render(m.petPreviewError))
	case m.petPreview != nil && !sidePreview && previewText != nil:
		lines = append(lines, strings.Split(centerRenderedBlock(strings.Join(previewText, "\n"), innerWidth), "\n")...)
	case m.petPreview != nil && !sidePreview:
		for range petImageRows {
			lines = append(lines, "")
		}
	}
	if hasItem && item.Value != terminalpet.DisabledID {
		if entry, ok := m.petEntries[item.Value]; ok {
			details := []string{entry.Label()}
			if kind := strings.TrimSpace(entry.Kind); kind != "" {
				details = append(details, kind)
			}
			if entry.SubmittedBy != "" {
				details = append(details, "by "+entry.SubmittedBy)
			}
			lines = append(lines, centerRenderedBlock(zeroTheme.faint.Render(strings.Join(details, " · ")), innerWidth))
		}
	}
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	lines = append(lines, zeroTheme.faint.Render("↑/↓ preview   Enter select   Esc close"))
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, "Choose a companion", lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

func truncatePetPickerColumn(value string, width int) string {
	value = strings.TrimSpace(value)
	if width <= 0 || value == "" {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return strings.TrimSpace(ansi.Cut(value, 0, width-1)) + "…"
}

func (m model) petLayoutActive() bool {
	if m.petLayoutRendering {
		return false
	}
	if m.petAnimation == nil || m.petID == "" || m.petID == terminalpet.DisabledID {
		return false
	}
	if m.petRenderer != nil && !m.petRenderer.Support().Supported() {
		// Without images, only a companion that can be drawn as text shows.
		// A permission card is drawn in the transcript, where the companion
		// would cover its options, so the companion steps aside for it.
		if !m.petTextMode() || !m.petAnimation.HasText() || m.width < petTextMinWidth || m.pendingPermission != nil {
			return false
		}
	}
	layoutWidth := m.width
	if !m.altScreen || layoutWidth < m.petSlotColumns()+4 || m.height < m.petSlotRows()+8 || m.subchat.active || m.transcriptDetailed {
		return false
	}
	if m.petObscuringModalActive() || m.suggestionsActive() || m.setup.visible || m.helpOverlay || m.leaderHelpOverlay {
		return false
	}
	return true
}

// A permission prompt replaces the composer but does not take over the whole
// viewport, so the ambient companion can remain visible beside it. Other modal
// surfaces own or cover the viewport and continue to suppress the companion.
func (m model) petObscuringModalActive() bool {
	return m.pendingAskUser != nil || m.pendingSpecReview != nil || m.providerWizard != nil ||
		m.mcpAddWizard != nil || m.mcpManager != nil || m.picker != nil ||
		m.sttKeyPrompt != nil || m.renamePrompt != nil || m.terminalAttach != nil
}

func (m model) petComposerReservedColumns(width int) int {
	if !m.petLayoutRendering && !m.petLayoutActive() {
		return 0
	}
	if m.petSupportsAlphaOverlay() && (m.petPositionSet || (m.petDragActive && !m.petDragStartedDocked)) {
		return 0
	}
	return minInt(m.petSlotReservedColumns(), maxInt(0, width-8))
}

func (m model) footerStatusLine(width int) string {
	// A text companion ends one row above the status line, so the line keeps
	// its full width.
	if m.petTextDocked() {
		return m.statusLine(width)
	}
	reserved := m.petComposerReservedColumns(width)
	return m.statusLine(width-reserved) + strings.Repeat(" ", reserved)
}

func (m model) floatingPetTranscriptView() string {
	chatModel := m
	chatModel.petLayoutRendering = true
	chat := m.reservePetImageSlot(viewLines(chatModel.transcriptView()), m.width)
	if m.petTextMode() {
		chat = m.paintPetText(chat, m.width)
	}
	return strings.Join(chat, "\n")
}

// petTextMode reports whether companions are drawn as text: the terminal
// cannot show images, but it has the colours the half-block art needs.
// model.petColorProfile is the profile Bubble Tea detected.
func (m model) petTextMode() bool {
	return m.petRenderer != nil && !m.petRenderer.Support().Supported() && m.petColorProfile >= colorprofile.ANSI256
}

// petTextDocked reports whether a text companion stands in its slot in the
// view being drawn or measured. Like petComposerReservedColumns it holds both
// while the floating layout renders and for callers that measure the footer
// outside it, such as mouse hit-testing, so every measurement agrees.
func (m model) petTextDocked() bool {
	return m.petTextMode() && (m.petLayoutRendering || m.petLayoutActive())
}

// petTextReservedColumns is how many columns at the right of the footer rows a
// docked text companion takes, or zero.
func (m model) petTextReservedColumns(width int) int {
	if !m.petTextDocked() {
		return 0
	}
	return m.petComposerReservedColumns(width)
}

// petTextShelf makes room for a docked text companion. It is taller than the
// composer, and the rows above the footer belong to the transcript, whose
// right-hand columns it would hide. Blank rows above the footer push the
// transcript up instead, so the companion stands only beside footer rows,
// which end before it.
func (m model) petTextShelf(footer string) string {
	if !m.petTextDocked() {
		return footer
	}
	// The companion's bottom row is one above the last line, the status line.
	missing := m.petSlotRows() + 1 - (strings.Count(footer, "\n") + 1)
	if missing <= 0 {
		return footer
	}
	return strings.Repeat("\n", missing) + footer
}

// petTextColors is the colour depth to draw text companions in. A
// 256-colour terminal gets the art's own 256-colour palette rather than
// Bubble Tea's generic conversion, which turns several of its hues.
func (m model) petTextColors() terminalpet.TextColors {
	if m.petColorProfile == colorprofile.ANSI256 {
		return terminalpet.Text256
	}
	return terminalpet.TextTrueColor
}

// petSlotColumns and petSlotRows are the cells the companion occupies.
func (m model) petSlotColumns() int {
	if m.petTextMode() {
		return terminalpet.TextColumns
	}
	return petImageColumns
}

func (m model) petSlotRows() int {
	if m.petTextMode() {
		return terminalpet.TextRows
	}
	return petImageRows
}

func (m model) petSlotReservedColumns() int {
	return m.petSlotColumns() + petWrapGapColumns
}

// paintPetText draws a text companion into the slot reservePetImageSlot
// cleared, where petHit looks for it.
func (m model) paintPetText(chat []string, width int) []string {
	state, phase := m.petTextPlayback()
	lines, ok := m.petAnimation.TextFrame(state, phase, m.petTextColors())
	if !ok {
		return chat
	}
	x, y := m.ambientPetPosition(width, len(chat))
	for index, line := range lines {
		row := y + index
		if row < 0 || row >= len(chat) {
			continue
		}
		left := padStyledLine(ansi.Cut(fitStyledLine(chat[row], width), 0, x), x)
		// Reset first: the sprite's blank cells must not take a style left
		// open by the text beside it.
		chat[row] = left + "\x1b[0m" + line + strings.Repeat(" ", maxInt(0, width-x-terminalpet.TextColumns))
	}
	return chat
}

// petTextPlayback is the state and phase of the ambient companion's current
// frame, as petImageDraw computes them for images.
func (m model) petTextPlayback() (terminalpet.State, int) {
	state := m.petState()
	phase := m.petPhase
	if state != m.petPlaybackState {
		phase = 0
	}
	return state, phase
}

func (m model) reservePetImageSlot(lines []string, width int) []string {
	chat := append([]string(nil), lines...)
	if m.petSupportsAlphaOverlay() {
		return chat
	}
	rows := m.petSlotRows()
	start := maxInt(0, len(chat)-rows-1)
	rightStart := maxInt(0, width-m.petSlotReservedColumns())
	for row := start; row < minInt(len(chat)-1, start+rows); row++ {
		line := fitStyledLine(chat[row], width)
		chat[row] = padStyledLine(ansi.Cut(line, 0, rightStart), rightStart) + strings.Repeat(" ", width-rightStart)
	}
	return chat
}

func (m model) petSupportsAlphaOverlay() bool {
	if m.petRenderer == nil {
		return false
	}
	switch m.petRenderer.Support().Protocol {
	case terminalpet.ImageProtocolKitty, terminalpet.ImageProtocolKittyLocalFile:
		return true
	default:
		return false
	}
}

func (m model) petImageDraw(content string) *terminalpet.ImageDraw {
	if m.petRenderer == nil || !m.petRenderer.Support().Supported() {
		return nil
	}
	if m.picker != nil && m.picker.kind == pickerPet && m.petPreview != nil {
		x, y, ok := petPickerImagePosition(content, petImageColumns, petImageRows)
		if !ok {
			return nil
		}
		heightPixels := m.petImageHeightPixels(m.petPreview, terminalpet.Idle, m.petPhase)
		columns, rows := m.petImageCells(m.petPreview, terminalpet.Idle, m.petPhase, heightPixels)
		return &terminalpet.ImageDraw{
			ID: petPreviewImageID, Animation: m.petPreview, State: terminalpet.Idle, Phase: m.petPhase,
			X: x, Y: y, Columns: columns, Rows: rows,
			HeightPixels: heightPixels,
		}
	}
	if !m.petLayoutActive() {
		return nil
	}
	x, y := m.ambientPetPosition(m.width, m.height)
	offsetX, offsetY := m.ambientPetOffset(x, y, m.width, m.height)
	phase := m.petPhase
	if state := m.petState(); state != m.petPlaybackState {
		// State changes are visible before the timer for the previous state fires.
		// Start the new track at its first frame instead of briefly rendering it
		// at the old track's phase.
		phase = 0
	}
	heightPixels := m.petImageHeightPixels(m.petAnimation, m.petState(), phase)
	columns, rows := m.petImageCells(m.petAnimation, m.petState(), phase, heightPixels)
	return &terminalpet.ImageDraw{
		ID: petAmbientImageID, Animation: m.petAnimation, State: m.petState(), Phase: phase,
		X: x, Y: y, OffsetX: offsetX, OffsetY: offsetY,
		Columns: columns, Rows: rows,
		HeightPixels: heightPixels,
	}
}

// petImageCells reports the cell footprint to record for an image.
//
// For Kitty this is the placement REQUEST, so the constants are the answer: the
// terminal scales the image into that many cells and owns the region.
//
// For sixel it is what will later be ERASED, so it has to describe what was
// actually painted, and the constants are wrong for that. The rendered height is
// clamped to preferredHeight, so the true row count is ceil(height/cellHeight),
// which equals petImageRows only when a cell happens to be exactly 15 pixels
// tall. At a 20-pixel cell it is 4 rows while the erase blanked 5, taking a row
// of interface with it every time the companion moved.
func (m model) petImageCells(animation *terminalpet.Animation, state terminalpet.State, phase, heightPixels int) (int, int) {
	if m.petRenderer == nil || m.petRenderer.Support().Protocol != terminalpet.ImageProtocolSixel {
		return petImageColumns, petImageRows
	}
	if m.petCellPixelWidth <= 0 || m.petCellPixelHeight <= 0 || heightPixels <= 0 {
		return petImageColumns, petImageRows
	}
	rows := ceilDivInt(heightPixels, m.petCellPixelHeight)
	columns := petImageColumns
	if animation != nil {
		if frame := animation.Frame(state, phase); frame != nil && frame.Bounds().Dy() > 0 && frame.Bounds().Dx() > 0 {
			widthPixels := heightPixels * frame.Bounds().Dx() / frame.Bounds().Dy()
			columns = ceilDivInt(widthPixels, m.petCellPixelWidth)
		}
	}
	// Never beyond the reserved area: the layout keeps that many cells clear and
	// the drag clamps to it, so erasing past it would reach live interface even
	// when the arithmetic above says the image is larger.
	return clampInt(columns, 1, petImageColumns), clampInt(rows, 1, petImageRows)
}

// ceilDivInt rounds up, because a sixel covering part of a cell still dirties
// the whole cell and the erase has to cover it.
func ceilDivInt(value, divisor int) int {
	if divisor <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}

func (m model) petImageHeightPixels(animation *terminalpet.Animation, state terminalpet.State, phase int) int {
	const preferredHeight = 75
	if m.petRenderer == nil || m.petRenderer.Support().Protocol != terminalpet.ImageProtocolSixel || m.petCellPixelHeight <= 0 {
		return preferredHeight
	}
	height := minInt(preferredHeight, petImageRows*m.petCellPixelHeight)
	if m.petCellPixelWidth <= 0 || animation == nil {
		return maxInt(1, height)
	}
	frame := animation.Frame(state, phase)
	if frame == nil || frame.Bounds().Dx() <= 0 || frame.Bounds().Dy() <= 0 {
		return maxInt(1, height)
	}
	widthLimitedHeight := petImageColumns * m.petCellPixelWidth * frame.Bounds().Dy() / frame.Bounds().Dx()
	return maxInt(1, minInt(height, widthLimitedHeight))
}

func (m model) ambientPetPosition(width, height int) (int, int) {
	maxX := maxInt(0, width-m.petSlotColumns())
	maxY := maxInt(0, height-m.petSlotRows())
	if m.petSupportsAlphaOverlay() {
		if m.petDragActive {
			return clampInt(m.petDragTargetX, 0, maxX), clampInt(m.petDragTargetY, 0, maxY)
		}
		if m.petPositionSet {
			return clampInt(m.petPositionX, 0, maxX), clampInt(m.petPositionY, 0, maxY)
		}
	}
	return m.petHomePosition(width, height)
}

func (m *model) resizeFreePetPosition(oldWidth, oldHeight, newWidth, newHeight int) {
	if !m.petPositionSet || oldWidth <= 0 || oldHeight <= 0 {
		return
	}
	oldMaxX := maxInt(0, oldWidth-petImageColumns)
	oldMaxY := maxInt(0, oldHeight-petImageRows)
	newMaxX := maxInt(0, newWidth-petImageColumns)
	newMaxY := maxInt(0, newHeight-petImageRows)
	if m.petCellPixelWidth > 0 && m.petCellPixelHeight > 0 {
		x := m.petPositionX*m.petCellPixelWidth + m.petPositionOffsetX
		y := m.petPositionY*m.petCellPixelHeight + m.petPositionOffsetY
		x = resizePetCoordinate(x, oldMaxX*m.petCellPixelWidth, newMaxX*m.petCellPixelWidth, petResizeEdgeAnchorCells*m.petCellPixelWidth)
		y = resizePetCoordinate(y, oldMaxY*m.petCellPixelHeight, newMaxY*m.petCellPixelHeight, petResizeEdgeAnchorCells*m.petCellPixelHeight)
		m.petPositionX, m.petPositionOffsetX = x/m.petCellPixelWidth, x%m.petCellPixelWidth
		m.petPositionY, m.petPositionOffsetY = y/m.petCellPixelHeight, y%m.petCellPixelHeight
		return
	}
	m.petPositionX = resizePetCoordinate(m.petPositionX, oldMaxX, newMaxX, petResizeEdgeAnchorCells)
	m.petPositionY = resizePetCoordinate(m.petPositionY, oldMaxY, newMaxY, petResizeEdgeAnchorCells)
}

func resizePetCoordinate(value, oldMaximum, newMaximum, edgeThreshold int) int {
	if newMaximum <= 0 {
		return 0
	}
	if oldMaximum <= 0 {
		return clampInt(value, 0, newMaximum)
	}
	value = clampInt(value, 0, oldMaximum)
	if value <= edgeThreshold {
		return clampInt(value, 0, newMaximum)
	}
	if gap := oldMaximum - value; gap <= edgeThreshold {
		return clampInt(newMaximum-gap, 0, newMaximum)
	}
	return clampInt(value+newMaximum/2-oldMaximum/2, 0, newMaximum)
}

func (m model) petHomePosition(width, height int) (int, int) {
	columns, rows := m.petSlotColumns(), m.petSlotRows()
	maxX := maxInt(0, width-columns)
	maxY := maxInt(0, height-rows)
	return clampInt(width-columns-2, 0, maxX), clampInt(height-rows-1, 0, maxY)
}

func (m model) ambientPetOffset(x, y, width, height int) (int, int) {
	offsetX, offsetY := 0, 0
	if m.petSupportsAlphaOverlay() {
		if m.petDragActive {
			offsetX, offsetY = m.petDragTargetOffsetX, m.petDragTargetOffsetY
		} else if m.petPositionSet {
			offsetX, offsetY = m.petPositionOffsetX, m.petPositionOffsetY
		}
	}
	if x >= maxInt(0, width-petImageColumns) {
		offsetX = 0
	}
	if y >= maxInt(0, height-petImageRows) {
		offsetY = 0
	}
	return maxInt(0, offsetX), maxInt(0, offsetY)
}

func (m model) petPixelProtocolSupported() bool {
	if m.petRenderer == nil {
		return false
	}
	switch m.petRenderer.Support().Protocol {
	case terminalpet.ImageProtocolKitty, terminalpet.ImageProtocolKittyLocalFile:
		return true
	default:
		return false
	}
}

func (m model) petPixelDragAvailable() bool {
	return m.petPixelProtocolSupported() && m.petCellPixelWidth > 0 && m.petCellPixelHeight > 0
}

// petCellMetricsWanted reports whether to ask the terminal for its cell size.
//
// Kept SEPARATE from petPixelProtocolSupported, which gates pixel-precise
// dragging, because the two questions only looked like one. Dragging is a Kitty
// feature; the measurement is needed by every image protocol, and by sixel most
// of all.
//
// Sixel was excluded from the request, so on a sixel terminal the reply never
// arrived, petCellPixelHeight stayed zero, petImageHeightPixels fell to its
// blind preferredHeight, and the erase kept using the petImageColumns and
// petImageRows constants. Those constants describe the RESERVED area, not what
// was painted: at a 20-pixel cell a 75-pixel sprite is 4 rows, so the erase
// blanked a fifth row of live interface every time the companion moved.
//
// Windows Terminal answers this request, so it was never a question of terminal
// support. Nothing asked it.
func (m model) petCellMetricsWanted() bool {
	if m.petRenderer == nil {
		return false
	}
	switch m.petRenderer.Support().Protocol {
	case terminalpet.ImageProtocolKitty, terminalpet.ImageProtocolKittyLocalFile, terminalpet.ImageProtocolSixel:
		return true
	default:
		return false
	}
}

func petPixelMouseEnableCmd() tea.Cmd {
	return tea.Raw(ansi.SetModeMouseExtSgrPixel)
}

func petPixelMouseDisableCmd() tea.Cmd {
	return tea.Raw(ansi.ResetModeMouseExtSgrPixel + ansi.SetModeMouseExtSgr)
}

func petImageFlushCmd() tea.Cmd {
	return tea.Raw(terminalSyncStart + terminalSyncEnd)
}

func petPixelMouseDisableAndFlushCmd() tea.Cmd {
	return tea.Raw(ansi.ResetModeMouseExtSgrPixel + ansi.SetModeMouseExtSgr + terminalSyncStart + terminalSyncEnd)
}

func (m model) petHit(x, y int) bool {
	// Permission choices own all pointer input even though the companion remains
	// visible. It must never intercept an approval or denial click.
	if m.pendingPermission != nil || !m.petLayoutActive() {
		return false
	}
	petX, petY := m.ambientPetPosition(m.width, m.height)
	return x >= petX && x < petX+m.petSlotColumns() && y >= petY && y < petY+m.petSlotRows()
}

func (m model) handlePetMouse(msg tea.MouseMsg) (model, tea.Cmd, bool) {
	x, y := mouseX(msg), mouseY(msg)
	switch {
	case mouseLeftPress(msg) && m.petHit(x, y):
		petX, petY := m.ambientPetPosition(m.width, m.height)
		petOffsetX, petOffsetY := m.ambientPetOffset(petX, petY, m.width, m.height)
		m.petDragActive = true
		m.petDragMoved = false
		m.petDragStartedDocked = !m.petPositionSet
		m.petDragOffsetX = x - petX
		m.petDragOffsetY = y - petY
		m.petDragTargetX = petX
		m.petDragTargetY = petY
		m.petDragTargetOffsetX = petOffsetX
		m.petDragTargetOffsetY = petOffsetY
		m.petDragState = terminalpet.Idle
		m.petPixelDrag = m.petPixelDragAvailable()
		m.petPixelAnchorSet = false
		if m.petPixelDrag {
			return m, petPixelMouseEnableCmd(), true
		}
		return m, nil, true
	case mouseMotion(msg) && m.petDragActive && m.petPixelDrag:
		previousState := m.petDragState
		leavePixelMode := m.updatePixelPetTarget(x, y)
		animationCmd := m.restartPetDragPlayback(previousState)
		if leavePixelMode {
			return m.leavePixelPetDrag(), batchCommands(petPixelMouseDisableAndFlushCmd(), animationCmd), true
		}
		return m, batchCommands(petImageFlushCmd(), animationCmd), true
	case mouseMotion(msg) && m.petDragActive && !m.petSupportsAlphaOverlay():
		// Sixel images and text companions are part of the terminal cell grid.
		// Keep them in the dedicated dock so moving one can never overwrite
		// cells Bubble Tea owns.
		return m, nil, true
	case mouseMotion(msg) && m.petDragActive:
		oldX, oldY := m.petDragTargetX, m.petDragTargetY
		newX := clampInt(x-m.petDragOffsetX, 0, maxInt(0, m.width-petImageColumns))
		newY := clampInt(y-m.petDragOffsetY, 0, maxInt(0, m.height-petImageRows))
		if newX == oldX && newY == oldY {
			return m, nil, true
		}
		m.petDragTargetX = newX
		m.petDragTargetY = newY
		m.petDragTargetOffsetX = 0
		m.petDragTargetOffsetY = 0
		m.petDragMoved = true
		previousState := m.petDragState
		m.petDragState = petMovementState(oldX, newX, previousState)
		return m, batchCommands(petImageFlushCmd(), m.restartPetDragPlayback(previousState)), true
	case mouseRelease(msg) && m.petDragActive && m.petPixelDrag:
		_ = m.updatePixelPetTarget(x, y)
		return m.finishPetDrag(petPixelMouseDisableAndFlushCmd())
	case mouseRelease(msg) && m.petDragActive:
		return m.finishPetDrag(nil)
	default:
		return m, nil, false
	}
}

func (m *model) updatePixelPetTarget(pointerX, pointerY int) bool {
	cellWidth, cellHeight := m.petCellPixelWidth, m.petCellPixelHeight
	if cellWidth <= 0 || cellHeight <= 0 {
		return false
	}
	if !m.petPixelAnchorSet {
		m.petDragOffsetPixelX = m.petDragOffsetX*cellWidth + positiveRemainder(pointerX, cellWidth)
		m.petDragOffsetPixelY = m.petDragOffsetY*cellHeight + positiveRemainder(pointerY, cellHeight)
		m.petPixelAnchorSet = true
	}
	oldAbsoluteX, oldAbsoluteY := m.petDragAbsolutePosition()
	maxAbsoluteX := maxInt(0, (m.width-petImageColumns)*cellWidth)
	maxAbsoluteY := maxInt(0, (m.height-petImageRows)*cellHeight)
	newAbsoluteX := clampInt(pointerX-m.petDragOffsetPixelX, 0, maxAbsoluteX)
	newAbsoluteY := clampInt(pointerY-m.petDragOffsetPixelY, 0, maxAbsoluteY)
	terminalPixelWidth := m.width * cellWidth
	terminalPixelHeight := m.height * cellHeight
	nearEdge := pointerX <= cellWidth || pointerY <= cellHeight ||
		terminalPixelWidth-pointerX <= cellWidth || terminalPixelHeight-pointerY <= cellHeight
	if newAbsoluteX == oldAbsoluteX && newAbsoluteY == oldAbsoluteY {
		return nearEdge
	}
	m.setPetDragAbsolutePosition(newAbsoluteX, newAbsoluteY)
	m.petDragMoved = true
	m.petDragState = petMovementState(oldAbsoluteX, newAbsoluteX, m.petDragState)
	return nearEdge
}

func petMovementState(oldX, newX int, current terminalpet.State) terminalpet.State {
	switch {
	case newX > oldX:
		return terminalpet.MoveRight
	case newX < oldX:
		return terminalpet.MoveLeft
	case current == terminalpet.MoveLeft || current == terminalpet.MoveRight:
		return current
	default:
		// A vertical-only drag still represents movement. With no prior facing
		// direction, use the right-running row as the stable default.
		return terminalpet.MoveRight
	}
}

func (m *model) restartPetDragPlayback(previous terminalpet.State) tea.Cmd {
	if m.petDragState == previous {
		return nil
	}
	m.petPhase = 0
	m.petPlaybackState = m.petDragState
	if m.reducedMotion || m.petAnimation == nil {
		return nil
	}
	m.petTickSeq++
	return petTickCmd(m.petTickSeq, m.petFrameDelay())
}

func (m model) leavePixelPetDrag() model {
	if m.petCellPixelWidth > 0 {
		m.petDragOffsetX = clampInt(m.petDragOffsetPixelX/m.petCellPixelWidth, 0, petImageColumns-1)
	}
	if m.petCellPixelHeight > 0 {
		m.petDragOffsetY = clampInt(m.petDragOffsetPixelY/m.petCellPixelHeight, 0, petImageRows-1)
	}
	m.petPixelDrag = false
	m.petPixelAnchorSet = false
	return m
}

func (m model) petDragAbsolutePosition() (int, int) {
	return m.petDragTargetX*m.petCellPixelWidth + m.petDragTargetOffsetX,
		m.petDragTargetY*m.petCellPixelHeight + m.petDragTargetOffsetY
}

func (m *model) setPetDragAbsolutePosition(x, y int) {
	if m.petCellPixelWidth <= 0 || m.petCellPixelHeight <= 0 {
		return
	}
	m.petDragTargetX, m.petDragTargetOffsetX = x/m.petCellPixelWidth, x%m.petCellPixelWidth
	m.petDragTargetY, m.petDragTargetOffsetY = y/m.petCellPixelHeight, y%m.petCellPixelHeight
}

func positiveRemainder(value, divisor int) int {
	if divisor <= 0 {
		return 0
	}
	remainder := value % divisor
	if remainder < 0 {
		remainder += divisor
	}
	return remainder
}

func (m model) finishPetDrag(modeCmd tea.Cmd) (model, tea.Cmd, bool) {
	m.petDragActive = false
	m.petPixelDrag = false
	m.petPixelAnchorSet = false
	m.petDragState = terminalpet.Idle
	if m.petDragMoved {
		m.commitPetDragPosition()
		m.petDragStartedDocked = false
		return m, modeCmd, true
	}
	now := m.now()
	if action, ok := m.petAnimation.ClickAnimation(m.petClickAnimationIndex); ok {
		m.petOutcome = action
		m.petClickAnimationIndex++
		m.petLastClickAt = time.Time{}
	} else if !m.petLastClickAt.IsZero() && now.Sub(m.petLastClickAt) <= petDoubleClickWindow {
		m.petOutcome = terminalpet.Jumping
		m.petLastClickAt = time.Time{}
	} else {
		m.petOutcome = terminalpet.Waving
		m.petLastClickAt = now
	}
	m.petPhase = 0
	if m.petOutcome == terminalpet.Waving {
		// The default waving row begins with the same neutral stance as idle.
		// Start on its first visibly active frame so a click feels immediate;
		// later repeated passes still include the neutral transition frame.
		m.petPhase = 1
	}
	m.petPlaybackState = m.petOutcome
	m.petOutcomeAt = now
	m.petDragStartedDocked = false
	if m.reducedMotion || m.petAnimation == nil {
		return m, modeCmd, true
	}
	// The existing ticker may still be sleeping on a long idle-frame delay.
	// Replace it so the click action advances at its authored cadence from the
	// moment the user releases the pet.
	m.petTickSeq++
	return m, batchCommands(modeCmd, petTickCmd(m.petTickSeq, m.petFrameDelay())), true
}

func (m *model) commitPetDragPosition() {
	if m.petDragTargetIsDocked() {
		m.petPositionSet = false
		m.petPositionX, m.petPositionY = 0, 0
		m.petPositionOffsetX, m.petPositionOffsetY = 0, 0
	} else {
		m.petPositionSet = true
		m.petPositionX = m.petDragTargetX
		m.petPositionY = m.petDragTargetY
		m.petPositionOffsetX = m.petDragTargetOffsetX
		m.petPositionOffsetY = m.petDragTargetOffsetY
	}
	m.petDragMoved = false
	m.petLastClickAt = time.Time{}
}

func (m model) petDragTargetIsDocked() bool {
	homeX, homeY := m.petHomePosition(m.width, m.height)
	if m.petCellPixelWidth > 0 && m.petCellPixelHeight > 0 {
		targetX := m.petDragTargetX*m.petCellPixelWidth + m.petDragTargetOffsetX
		targetY := m.petDragTargetY*m.petCellPixelHeight + m.petDragTargetOffsetY
		verticalDelta := targetY - homeY*m.petCellPixelHeight
		return petAbs(targetX-homeX*m.petCellPixelWidth) <= petDockSnapColumns*m.petCellPixelWidth &&
			verticalDelta >= -m.petCellPixelHeight && verticalDelta <= maxInt(1, m.petCellPixelHeight/2)
	}
	return petAbs(m.petDragTargetX-homeX) <= petDockSnapColumns &&
		m.petDragTargetY >= homeY-1 && m.petDragTargetY <= homeY
}

func petAbs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (m *model) cancelPetDrag() {
	m.petDragActive = false
	m.petPixelDrag = false
	m.petPixelAnchorSet = false
	m.petDragMoved = false
	m.petDragStartedDocked = false
	m.petDragState = terminalpet.Idle
}

func petPickerImagePosition(content string, columns, rows int) (int, int, bool) {
	lines := viewLines(content)
	top, search, footer := -1, -1, -1
	for index, line := range lines {
		plain := ansi.Strip(line)
		if top < 0 && strings.Contains(plain, "Choose a companion") {
			top = index
		}
		if search < 0 && strings.Contains(plain, "search >") {
			search = index
			if top < 0 {
				top = index
				if index > 0 {
					previous := ansi.Strip(lines[index-1])
					searchLeft := len(plain) - len(strings.TrimLeft(plain, " "))
					previousLeft := len(previous) - len(strings.TrimLeft(previous, " "))
					if previousLeft == searchLeft {
						// Narrow stacked previews use the border row immediately above
						// search for their full modal width. A clipped picker may instead
						// expose a transcript separator there, whose indentation differs.
						top = index - 1
					}
				}
			}
		}
		if top >= 0 && strings.Contains(plain, "↑/↓ preview") {
			footer = index
			break
		}
	}
	if top < 0 {
		return 0, 0, false
	}
	topLine := ansi.Strip(lines[top])
	left := len(topLine) - len(strings.TrimLeft(topLine, " "))
	overlayWidth := len([]rune(strings.TrimRight(topLine, " "))) - left
	if overlayWidth < columns {
		return 0, 0, false
	}
	if overlayWidth-4 >= petSidePreviewMin && search >= 0 {
		scanEnd := len(lines)
		if footer >= 0 {
			scanEnd = footer
		}
		firstRule, secondRule := -1, -1
		for index := search + 1; index < scanEnd; index++ {
			if strings.Count(ansi.Strip(lines[index]), "─") < columns {
				continue
			}
			if firstRule < 0 {
				firstRule = index
			} else {
				secondRule = index
				break
			}
		}
		listTop := firstRule + 1
		if firstRule >= 0 && secondRule-listTop >= rows {
			x := left + overlayWidth - columns - 2
			y := listTop + (secondRule-listTop-rows)/2
			return x, y, true
		}
	}
	if footer < top || footer-rows-2 < 0 {
		return 0, 0, false
	}
	return left + (overlayWidth-columns)/2, footer - rows - 2, true
}

func (m model) petState() terminalpet.State {
	if m.petDragActive && m.petDragState != terminalpet.Idle {
		return m.petDragState
	}
	if m.pendingPermission != nil || m.pendingAskUser != nil || m.pendingSpecReview != nil {
		return terminalpet.Waiting
	}
	if m.pending {
		if m.petAnimation.Has(terminalpet.Thinking) && m.petAgentThinking() {
			return terminalpet.Thinking
		}
		return terminalpet.Running
	}
	sleeps := m.petAnimation.Has(terminalpet.Sleeping)
	if m.petOutcome == "" && !sleeps {
		return terminalpet.Idle
	}
	// One clock reading serves both checks below.
	now := m.now()
	if m.petOutcome != "" {
		hold := petOutcomeHold
		if duration, customClick := m.petAnimation.ClickDuration(m.petOutcome); customClick && duration > 0 {
			hold = duration
		} else if duration := m.petAnimation.PrimaryDuration(m.petOutcome); duration > hold {
			hold = duration
		}
		if now.Sub(m.petOutcomeAt) < hold {
			return m.petOutcome
		}
	}
	if sleeps {
		// A reaction, such as the one a click starts, counts as activity too.
		lastActive := m.petLastActiveAt
		if m.petOutcomeAt.After(lastActive) {
			lastActive = m.petOutcomeAt
		}
		if !lastActive.IsZero() && now.Sub(lastActive) >= petSleepAfter {
			return terminalpet.Sleeping
		}
	}
	return terminalpet.Idle
}

// petAgentThinking reports whether a running turn is between visible actions:
// no tool call is streaming or running, and no answer text is arriving. It
// mirrors the "thinking" phase of the working status line.
func (m model) petAgentThinking() bool {
	if m.streamCallName != "" || len(bytes.TrimSpace(m.streamingText)) > 0 {
		return false
	}
	_, toolRunning := m.activeToolCall()
	return !toolRunning
}

// greetWithPet starts the companion's wave, as when it first appears.
func (m *model) greetWithPet() {
	m.petOutcome = terminalpet.Waving
	m.petOutcomeAt = m.now()
	m.petPlaybackState = terminalpet.Waving
	// The default waving row begins with the same neutral stance as idle.
	m.petPhase = 1
}

// petTurnOutcome is the companion's reaction to a finished turn: a flinch for
// a failure, a celebration for a long success, a hop otherwise.
func (m model) petTurnOutcome(err error, finishedAt time.Time) terminalpet.State {
	switch {
	case err != nil:
		return terminalpet.Failed
	case !m.turnStartedAt.IsZero() && m.activeTurnElapsedAt(m.turnStartedAt, finishedAt) >= petBigTurn:
		return terminalpet.Jumping
	default:
		return terminalpet.Review
	}
}

func (m model) petPlayback() (*terminalpet.Animation, terminalpet.State) {
	if m.picker != nil && m.picker.kind == pickerPet && m.petPreview != nil {
		return m.petPreview, terminalpet.Idle
	}
	return m.petAnimation, m.petState()
}

func (m model) petFrameDelay() time.Duration {
	animation, state := m.petPlayback()
	if animation == nil {
		return petFrameDelay
	}
	phase := m.petPhase
	if state != m.petPlaybackState {
		phase = 0
	}
	if delay := animation.FrameDelay(state, phase); delay > 0 {
		return delay
	}
	return petFrameDelay
}
