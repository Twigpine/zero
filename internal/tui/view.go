package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"

	"github.com/Gitlawb/zero/internal/agent"
)

const (
	suggestionPaletteMaxVisible = 7
	suggestionPaletteMaxWidth   = 76
	suggestionPaletteMinWidth   = 44
	pickerOverlayMaxVisible     = 10
	pickerOverlayMaxWidth       = 92
	pickerOverlayMinWidth       = 56
	modelPickerOverlayMaxWidth  = 76
	modelPickerOverlayMinWidth  = 58
)

// layoutTier buckets the terminal width into the spec's adaptive tiers. It
// is derived from the live width at every render, so a WindowSizeMsg
// re-evaluates it implicitly.
type layoutTier int

const (
	tierTiny   layoutTier = iota // < 58: single-segment header, rail-less cards
	tierNarrow                   // 58–79: no gutters, bare badge, lean status
	tierMedium                   // 80–99: no tool-arg column, no ctx
	tierFull                     // ≥ 100: everything
)

func widthTier(width int) layoutTier {
	switch {
	case width >= 100:
		return tierFull
	case width >= 80:
		return tierMedium
	case width >= minStartupWidth:
		return tierNarrow
	default:
		return tierTiny
	}
}

// titleBar renders the top zone of the chat surface: git branch and cwd on the
// left, provider/model and context window on the right, then a rule. Segments
// drop with the width tier (full → no ctx → no cwd → branch/path only), reusing
// the startupHeaderLine candidate fallback.
func (m model) titleBar(width int) string {
	tier := widthTier(width)

	workspace := m.titleWorkspaceSegment()
	workspaceShort := m.titleWorkspaceSegmentShort()
	branchOnly := m.titleBranchSegment()
	cwdOnly := zeroTheme.faint.Render(shortenPath(m.cwd))
	compactLeft := cwdOnly
	if branchOnly != "" {
		compactLeft = branchOnly
	}
	model := m.titleModelSegment()
	ctx := ""
	if window := m.modelContextWindow(m.modelName); window > 0 {
		ctx = zeroTheme.faint.Render(" · " + formatContextWindow(window))
	}

	var candidates []headerCandidate
	switch tier {
	case tierFull:
		candidates = []headerCandidate{
			{left: workspace, right: model + ctx},
			{left: workspace, right: model},
			{left: workspaceShort, right: model},
			{left: cwdOnly, right: model},
			{left: compactLeft, right: model},
		}
	case tierMedium:
		candidates = []headerCandidate{
			{left: workspace, right: model},
			{left: workspaceShort, right: model},
			{left: cwdOnly, right: model},
			{left: compactLeft, right: model},
		}
	case tierNarrow:
		candidates = []headerCandidate{
			{left: branchOnly, right: model},
			{left: "", right: model},
		}
	default:
		// Tiny: one segment, no right column.
		candidates = []headerCandidate{
			{left: compactLeft, right: ""},
			{left: cwdOnly, right: ""},
		}
	}

	line := startupHeaderLine(width, candidates)
	rule := zeroTheme.line.Render(strings.Repeat("─", width))
	return line + "\n" + rule
}

func (m model) titleWorkspaceSegment() string {
	cwd := zeroTheme.faint.Render(shortenPath(m.cwd))
	parts := []string{}
	if branch := m.titleBranchSegment(); branch != "" {
		parts = append(parts, branch)
	}
	if pr := m.titlePRSegment(); pr != "" {
		parts = append(parts, pr)
	}
	if len(parts) > 0 {
		return strings.Join(append(parts, cwd), "  ")
	}
	return cwd
}

func (m model) titleWorkspaceSegmentShort() string {
	cwd := zeroTheme.faint.Render(shortenPath(m.cwd))
	parts := []string{}
	branch := strings.TrimSpace(m.gitBranch)
	if branch != "" {
		icon := zeroTheme.muted.Render("")
		parts = append(parts, icon+" "+zeroTheme.muted.Render(middleTruncate(branch, 22)))
	}
	if pr := m.titlePRSegment(); pr != "" {
		parts = append(parts, pr)
	}
	if len(parts) > 0 {
		return strings.Join(append(parts, cwd), "  ")
	}
	return cwd
}

func (m model) titleBranchSegment() string {
	branch := strings.TrimSpace(m.gitBranch)
	if branch == "" {
		return ""
	}
	return zeroTheme.muted.Render("") + " " + zeroTheme.muted.Render(branch)
}

func (m model) titlePRSegment() string {
	return renderPRSegments(BuildPRSegments(m.prState, false))
}

func (m model) titleModelSegment() string {
	provider := strings.TrimSpace(m.providerDisplayName())
	model := strings.TrimSpace(m.modelName)
	switch {
	case provider == "" && model == "":
		return zeroTheme.muted.Render("no provider")
	case model == "":
		return zeroTheme.ink.Render(provider)
	case provider == "":
		return zeroTheme.ink.Render(model)
	default:
		return zeroTheme.ink.Render(provider + "/" + model)
	}
}

func (m model) composerDividerLine(width int) string {
	model := displayValue(strings.TrimSpace(m.modelName), "no model")
	// The composer rule is a quiet model reminder above the input. Permission mode
	// and reasoning effort now live in the persistent status line (the conventional
	// footer for run-state), so they're not duplicated on this rule.
	meta := zeroTheme.muted.Render(model)
	metaWidth := lipgloss.Width(meta)
	reserved := m.petComposerReservedColumns(width)
	availableWidth := width - reserved
	if width < 8 {
		return zeroTheme.lineStrong.Render(strings.Repeat("─", width))
	}
	if availableWidth < metaWidth+4 {
		line := zeroTheme.lineStrong.Render("╰" + strings.Repeat("─", availableWidth-2) + "╯")
		return line + strings.Repeat(" ", reserved)
	}
	rule := strings.Repeat("─", availableWidth-metaWidth-4)
	line := zeroTheme.lineStrong.Render("╰"+rule+" ") + meta + zeroTheme.lineStrong.Render(" ╯")
	return line + strings.Repeat(" ", reserved)
}

// statusLine renders the bottom readout as ` │ `-separated groups: the run-state
// chip (permission mode + effort/fast tier) on the left, a flexible gap, then the
// context-fill gauge and token/cost usage on the right. The provider lives in the
// title bar and is NOT duplicated here. Groups drop with the width tier.
func (m model) statusLine(width int) string {
	tier := widthTier(width)
	separator := zeroTheme.line.Render(" │ ")
	prefix := "  "

	// Left chip: the safety-relevant run-state — permission mode (auto/ask/unsafe)
	// in its mode colour. This was previously only on the easy-to-miss composer
	// rule; the persistent footer is where users look for "will it run commands?".
	modeText, modeStyle := m.modeLabel()
	btwChip := ""
	if m.btw.active {
		btwChip = zeroTheme.amber.Render("BTW") + zeroTheme.muted.Render(" · ")
	}
	left := prefix + btwChip + zeroTheme.accent.Render("●") + " " + modeStyle.Render(modeText)

	if tier == tierTiny {
		if m.exitConfirmActive {
			return fitStyledLine(prefix+btwChip+zeroTheme.amber.Render("●")+" "+zeroTheme.amber.Render(ctrlCExitConfirmText), width)
		}
		if m.cancelConfirmActive {
			return fitStyledLine(prefix+btwChip+zeroTheme.amber.Render("●")+" "+zeroTheme.amber.Render(escCancelConfirmText), width)
		}
		if dictation := m.dictationStatusChip(); dictation != "" {
			return fitStyledLine(prefix+btwChip+dictation, width)
		}
		if goalSummary := m.goalFooterSummary(); goalSummary != "" {
			left += zeroTheme.muted.Render(" · ") + zeroTheme.accent.Render("◎ ") + zeroTheme.muted.Render(goalSummary)
		}
		return fitStyledLine(left, width)
	}

	// Non-tiny: append the active reasoning effort (brand lime, omitted on auto).
	if m.reasoningEffort != "" {
		left += zeroTheme.muted.Render(" · ") + zeroTheme.accent.Render(string(m.reasoningEffort))
	}
	if m.activeServiceTier() == "priority" {
		left += zeroTheme.muted.Render(" · ") + zeroTheme.accent.Render("fast")
	}
	if m.exitConfirmActive {
		left = prefix + btwChip + zeroTheme.amber.Render("●") + " " + zeroTheme.amber.Render(ctrlCExitConfirmText)
	} else if m.cancelConfirmActive {
		left = prefix + btwChip + zeroTheme.amber.Render("●") + " " + zeroTheme.amber.Render(escCancelConfirmText)
	} else if m.dictation.downloading && m.dictation.downloadStatus != "" {
		// A model download in progress takes over the left chip with a live percentage.
		left = prefix + btwChip + zeroTheme.accent.Render("⬇ ") + zeroTheme.muted.Render(m.dictation.downloadStatus)
	} else if dictation := m.dictationStatusChip(); dictation != "" && m.dictation.active() {
		// An active recording/transcription takes over the left chip — it is the
		// most time-sensitive thing on screen (the mic is live).
		left = prefix + btwChip + dictation
	} else {
		if voice := m.voiceModeIndicator(); voice != "" {
			left += zeroTheme.muted.Render(" · ") + voice
		}
		if summary := m.backgroundTerminalSummary(); summary != "" {
			left += separator + zeroTheme.muted.Render(summary)
		}
	}
	// Active loops surface a persistent "↻ N loops · next 3:05pm" segment so a
	// running loop is always visible (hidden during an exit/cancel confirm above).
	if !m.exitConfirmActive && !m.cancelConfirmActive {
		if goalSummary := m.goalFooterSummary(); goalSummary != "" {
			left += separator + zeroTheme.accent.Render("◎ ") + zeroTheme.muted.Render(goalSummary)
		}
		if loopSummary := m.loopFooterSummary(); loopSummary != "" {
			left += separator + zeroTheme.accent.Render("↻ ") + zeroTheme.muted.Render(loopSummary)
		}
	}

	rightGroups := []string{}
	// Context-fill gauge: surface it down to the narrow tier, where it is most
	// useful for deciding whether a session needs compaction.
	gaugeShown := false
	if tier >= tierNarrow {
		if gauge := m.contextWindowSegment(); gauge != "" {
			rightGroups = append(rightGroups, gauge)
			gaugeShown = true
		}
	}
	// The gauge and the usage segment read the same token total, so once the
	// gauge is present the usage segment only contributes cost.
	usage := m.usageStatusSegment()
	if gaugeShown {
		usage = m.usageCostSegment()
	}
	if usage != "" {
		rightGroups = append(rightGroups, zeroTheme.muted.Render(usage))
	}
	right := strings.Join(rightGroups, separator)

	return fitStyledLine(joinHeaderLine(left, right, width), width)
}

func (m model) providerDisplayName() string {
	provider := strings.TrimSpace(m.providerName)
	if provider == "" {
		provider = strings.TrimSpace(m.providerProfile.Name)
	}
	if !providerDisplayNameIsGenericCustom(provider) {
		return provider
	}
	baseURL := strings.TrimSpace(m.providerProfile.BaseURL)
	if baseURL == "" || strings.Contains(strings.ToLower(baseURL), "example.invalid") {
		return provider
	}
	derived := providerWizardNameFromBaseURL(baseURL)
	if derived == "" || derived == "custom" {
		return provider
	}
	return derived
}

func providerDisplayNameIsGenericCustom(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "custom-openai-compatible", "custom-anthropic-compatible":
		return true
	default:
		return false
	}
}

// nextPermissionMode toggles between the two prompt-respecting modes:
// Auto ⇄ Ask. Unsafe (which disables permission prompts entirely) is
// deliberately NOT reachable by a casual keypress — a single shift+tab landing
// on it would let prompt-required tools run with no decision. Unsafe stays an
// explicit opt-in (the launch/--skip-permissions-unsafe path), not a UI toggle.
// Unsafe is folded back to Ask so the toggle always lands somewhere safe.
// Plan is left untouched: folding it to Ask would be a LESS strict landing
// (Ask allows write/shell tools with a prompt; Plan hides them entirely), so
// the read-only guarantee must only be given up through the explicit /plan
// off exit, never a stray shift+tab.
func nextPermissionMode(mode agent.PermissionMode) agent.PermissionMode {
	switch mode {
	case agent.PermissionModeAuto:
		return agent.PermissionModeAsk
	case agent.PermissionModeAsk:
		return agent.PermissionModeAuto
	case agent.PermissionModePlan:
		return agent.PermissionModePlan
	default:
		// Anything else (incl. an externally-set Unsafe) folds to Ask — the stricter
		// landing, so toggling never makes an Unsafe session less strict.
		return agent.PermissionModeAsk
	}
}

func (m model) modeLabel() (string, lipgloss.Style) {
	switch m.permissionMode {
	case agent.PermissionModeAuto:
		return "auto-approve", zeroTheme.modeAuto
	case agent.PermissionModeAsk:
		return "ask", zeroTheme.modeAsk
	case agent.PermissionModeUnsafe:
		return "unsafe", zeroTheme.modeUnsafe
	case agent.PermissionModePlan:
		return "plan", zeroTheme.modePlan
	default:
		mode := strings.TrimSpace(string(m.permissionMode))
		if mode == "" {
			return "auto-approve", zeroTheme.modeAuto
		}
		return mode, zeroTheme.muted
	}
}

// usageStatusSegment shows the latest provider step's token footprint, plus
// cumulative cost once anything is priced.
func (m model) usageStatusSegment() string {
	if m.usageTracker == nil {
		return ""
	}
	summary := m.usageTracker.Summary()
	tokens := m.latestUsageTokens(summary)
	if tokens <= 0 {
		return ""
	}
	if summary.RecordCount == 0 {
		return humanCount(tokens) + " tok"
	}
	return fmt.Sprintf("%s tok · %s",
		humanCount(tokens),
		summary.FormattedTotalCost,
	)
}

// usageCostSegment returns just the session cost, with the token figure dropped.
// Used in the status line when the sidebar is open and already showing tokens at
// its floor, so the cost survives without duplicating the token count. Empty
// until a priced usage record lands (no cost to show yet).
func (m model) usageCostSegment() string {
	if m.usageTracker == nil {
		return ""
	}
	summary := m.usageTracker.Summary()
	if summary.RecordCount == 0 {
		return ""
	}
	return strings.TrimSpace(summary.FormattedTotalCost)
}

// contextFillPercent returns the latest request's context-window fill as a percent
// (0-100), the tokens used, the model's window, and a colour graded for the fill
// (green <75% → amber ≥75% → red ≥90%). ok is false until a usage event lands or
// when the model's window is unknown. Shared by the status-line gauge and the
// sidebar context chip so they grade identically. This is the "you're at X% of
// context" reading the compaction trigger already reasons about at ~80%.
func (m model) contextFillPercent() (pct, used, window int, style lipgloss.Style, ok bool) {
	if m.usageTracker == nil {
		return 0, 0, 0, lipgloss.Style{}, false
	}
	summary := m.usageTracker.Summary()
	used = m.latestUsageTokens(summary)
	window = m.modelContextWindow(m.modelName)
	if used <= 0 || window <= 0 {
		return 0, 0, 0, lipgloss.Style{}, false
	}
	ratio := float64(used) / float64(window)
	if ratio > 1 {
		ratio = 1
	}
	style = zeroTheme.green
	switch {
	case ratio >= 0.90:
		style = zeroTheme.red
	case ratio >= 0.75:
		style = zeroTheme.amber
	}
	return int(ratio*100 + 0.5), used, window, style, true
}

// contextWindowSegment renders the status-line context-fill gauge as
// "◔ used/window · NN%", graded by contextFillPercent.
func (m model) contextWindowSegment() string {
	pct, used, window, style, ok := m.contextFillPercent()
	if !ok {
		return ""
	}
	return style.Render(fmt.Sprintf("◔ %s/%s · %d%%", humanCount(used), humanCount(window), pct))
}

// humanCount renders a token count the way the status line wants it: 999,
// 12.4K, 200K, 1M, 1.2M.
func humanCount(n int) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return humanCountScaled(float64(n)/1000, "K")
	default:
		return humanCountScaled(float64(n)/1_000_000, "M")
	}
}

func humanCountScaled(value float64, suffix string) string {
	text := fmt.Sprintf("%.1f%s", value, suffix)
	return strings.Replace(text, ".0"+suffix, suffix, 1)
}

// formatContextWindow renders a model's context window for the title bar
// (200000 → 200K, 1048576 → 1M).
func formatContextWindow(window int) string {
	if window <= 0 {
		return ""
	}
	if window >= 1_000_000 && window%1_000_000 < 100_000 {
		return strconv.Itoa(window/1_000_000) + "M"
	}
	return strconv.Itoa(window/1000) + "K"
}

func shortenPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "unknown"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// Match on a path boundary: a bare prefix check would mangle siblings
		// like /Users/alice2 when home is /Users/alice.
		if path == home {
			return "~"
		}
		if strings.HasPrefix(path, home+string(os.PathSeparator)) {
			return "~" + path[len(home):]
		}
	}
	return path
}

// gitBranch reads the current branch (or short SHA when detached) for cwd, handling
// both regular checkouts (.git dir) and worktrees (.git file). Returns "" on any
// problem — the header simply omits the segment.
func gitBranch(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	gitRoot := agent.FindProjectGitRoot(cwd)
	if gitRoot == "" {
		return ""
	}
	gitPath := filepath.Join(gitRoot, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}

	headPath := filepath.Join(gitPath, "HEAD")
	if !info.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		dir := strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: ")
		if dir == "" {
			return ""
		}
		// In worktree mode the gitdir is often RELATIVE (e.g.
		// "gitdir: ../.git/worktrees/<name>") — resolve it against the worktree root (gitRoot), not the
		// process working directory, or HEAD lookup fails and we drop the branch.
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(gitRoot, dir)
		}
		headPath = filepath.Join(dir, "HEAD")
	}

	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(data))
	if strings.HasPrefix(ref, "ref: ") {
		ref = strings.TrimPrefix(ref, "ref: ")
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if len(ref) >= 7 {
		return ref[:7]
	}
	return ref
}

// suggestionOverlay renders the slash-command/file autocomplete palette below
// the composer: compact, bordered, and capped so short prefixes cannot flood
// the chat surface. Returns "" when no overlay should show.
func (m model) suggestionOverlay(width int) string {
	if !m.suggestionsActive() {
		return ""
	}
	title := "Commands"
	query := commandSuggestionQuery(m.input.Value())
	footer := "↑/↓ move   Enter run   Esc close"
	if m.selectedCommandSuggestionRequiresInput() {
		footer = "↑/↓ move   Enter insert   Esc close"
	}
	if m.suggestionsAreFiles {
		title = "Files"
		query = fileSuggestionQuery(m.input.Value())
		footer = "↑/↓ move   Enter insert   Esc close"
	}
	return centerRenderedBlock(renderSuggestionPalette(selectableItems(m.suggestions, m.suggestionsAreFiles), m.suggestionIdx, width, title, query, footer), width)
}

func commandSuggestionQuery(value string) string {
	trimmed := strings.TrimLeft(value, " ")
	if trimmed == "" {
		return ""
	}
	if fields := strings.Fields(trimmed); len(fields) > 0 {
		return strings.TrimPrefix(fields[0], "/")
	}
	return strings.TrimPrefix(strings.TrimSpace(trimmed), "/")
}

func fileSuggestionQuery(value string) string {
	if token, ok := trailingAtToken(value); ok {
		return token
	}
	return ""
}

func renderSuggestionPalette(items []selectableListItem, selected, width int, title, query, footer string) string {
	if width <= 0 {
		width = defaultStartupWidth
	}
	paletteWidth := minInt(width, suggestionPaletteMaxWidth)
	if paletteWidth < suggestionPaletteMinWidth {
		paletteWidth = width
	}
	innerWidth := maxInt(1, paletteWidth-4)
	maxVisible := minInt(suggestionPaletteMaxVisible, len(items))
	visible := []selectableListItem{}
	start := 0
	if len(items) > 0 {
		selected = clampInt(selected, 0, len(items)-1)
		start = selectableListStart(len(items), maxVisible, selected)
		visible = items[start : start+maxVisible]
	}

	labelWidth := 0
	for _, item := range visible {
		if w := lipgloss.Width(item.Label); w > labelWidth {
			labelWidth = w
		}
	}
	labelWidth = minInt(labelWidth, maxInt(8, innerWidth/2))

	lines := make([]string, 0, len(visible)+5)
	searchInset := lipgloss.Width("❯ ")
	searchPrefix := transparentSurface(zeroTheme.ink).Render(strings.Repeat(" ", searchInset))
	lines = append(lines, fillPaletteLine(searchPrefix+renderSuggestionSearchLine(query, maxInt(1, innerWidth-searchInset)), innerWidth, transparentSurface))
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))

	for index, item := range visible {
		absoluteIndex := start + index
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if absoluteIndex == selected {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}

		labelText := truncateRunes(item.Label, labelWidth)
		label := surface(zeroTheme.ink).Render(labelText)
		pad := surface(zeroTheme.ink).Render(strings.Repeat(" ", maxInt(0, labelWidth-lipgloss.Width(labelText))))
		line := marker + label + pad
		if desc := strings.TrimSpace(item.Description); desc != "" {
			descWidth := innerWidth - lipgloss.Width(marker) - labelWidth - 2
			if truncated := truncateRunes(desc, maxInt(0, descWidth)); truncated != "" {
				line += surface(zeroTheme.faint).Render("  " + truncated)
			}
		}
		lines = append(lines, fillPaletteLine(line, innerWidth, surface))
	}
	if len(visible) == 0 {
		message := "no matching commands"
		if strings.EqualFold(strings.TrimSpace(title), "Files") {
			message = "no matching files"
		}
		lines = append(lines, fillPaletteLine(searchPrefix+zeroTheme.faint.Render(message), innerWidth, transparentSurface))
	}

	if footer = strings.TrimSpace(footer); footer != "" {
		lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
		line := zeroTheme.faint.Render(footer)
		lines = append(lines, fillPaletteLine(line, innerWidth, transparentSurface))
	}
	return styledBlockFillTitle(paletteWidth, strings.TrimSpace(title), lines, zeroTheme.lineStrong, lipgloss.NewStyle())
}

func styledBlockFillTitle(width int, title string, lines []string, borderStyle lipgloss.Style, fill lipgloss.Style) string {
	return styledBlockFillTitleStyled(width, title, lines, borderStyle, fill, zeroTheme.ink.Bold(true))
}

// styledBlockFillTitleStyled is styledBlockFillTitle with a caller-supplied style
// for the inset title text, so a card can pick a calmer/status-tinted title
// without changing the default bright-bold heading every other card uses.
func styledBlockFillTitleStyled(width int, title string, lines []string, borderStyle lipgloss.Style, fill lipgloss.Style, titleStyle lipgloss.Style) string {
	if width < 4 {
		width = 4
	}
	if title = strings.TrimSpace(title); title == "" || widthTier(width) == tierTiny {
		return styledBlockFill(width, lines, borderStyle, fill)
	}
	ruleWidth := width - 2
	titleText := " " + title + " "
	titleWidth := lipgloss.Width(titleText)
	if titleWidth >= ruleWidth {
		return styledBlockFill(width, lines, borderStyle, fill)
	}

	leftRule := "──"
	rightRule := strings.Repeat("─", maxInt(0, ruleWidth-lipgloss.Width(leftRule)-titleWidth))
	top := borderStyle.Render("╭"+leftRule) + titleStyle.Render(titleText) + borderStyle.Render(rightRule+"╮")
	bottom := borderStyle.Render("╰" + strings.Repeat("─", width-2) + "╯")

	body := make([]string, 0, len(lines)+2)
	body = append(body, top)
	for _, line := range lines {
		available := width - 4
		fitted := fitStyledLine(line, available)
		pad := fill.Render(strings.Repeat(" ", maxInt(0, available-lipgloss.Width(fitted))))
		body = append(body, borderStyle.Render("│ ")+fitted+pad+borderStyle.Render(" │"))
	}
	body = append(body, bottom)
	return strings.Join(body, "\n")
}

func renderSuggestionSearchLine(query string, width int) string {
	query = strings.TrimSpace(query)
	label := zeroTheme.userPrompt.Render("search > ")
	valueWidth := maxInt(1, width-lipgloss.Width(label))
	value := zeroTheme.ink.Render(truncateRunes(query, valueWidth))
	return fitStyledLine(label+value, width)
}

func transparentSurface(style lipgloss.Style) lipgloss.Style {
	return style
}

func fillPaletteLine(line string, width int, surface func(lipgloss.Style) lipgloss.Style) string {
	line = fitStyledLine(line, width)
	pad := maxInt(0, width-lipgloss.Width(line))
	if pad > 0 {
		line += surface(zeroTheme.ink).Render(strings.Repeat(" ", pad))
	}
	return line
}

func centerRenderedBlock(block string, width int) string {
	if block == "" || width <= 0 {
		return block
	}
	lines := strings.Split(block, "\n")
	blockWidth := 0
	for _, line := range lines {
		if w := lipgloss.Width(line); w > blockWidth {
			blockWidth = w
		}
	}
	pad := maxInt(0, (width-blockWidth)/2)
	if pad == 0 {
		return block
	}
	return indentBlock(block, pad)
}

func selectableItems(suggestions []commandSuggestion, files bool) []selectableListItem {
	items := make([]selectableListItem, 0, len(suggestions))
	for _, suggestion := range suggestions {
		item := selectableListItem{Label: suggestion.Name, Description: suggestion.Desc}
		if files {
			item = fileSelectableItem(suggestion.Name)
		} else {
			item.Label = strings.TrimPrefix(item.Label, "/")
		}
		items = append(items, item)
	}
	return items
}

func fileSelectableItem(token string) selectableListItem {
	rel := strings.TrimPrefix(token, "@")
	rel = filepath.ToSlash(rel)
	isDir := strings.HasSuffix(rel, "/")
	cleanRel := strings.TrimSuffix(rel, "/")
	base := path.Base(cleanRel)
	if base == "." || base == "/" || base == "" {
		return selectableListItem{Label: strings.TrimPrefix(token, "@"), Description: "file"}
	}
	if isDir {
		base += "/"
	}
	dir := path.Dir(cleanRel)
	if dir == "." || dir == "" {
		return selectableListItem{Label: base}
	}
	return selectableListItem{Label: base, Description: dir}
}

// pickerOverlay renders an open interactive selector as a centered modal: a
// bordered panel with a title-and-hints row, rows carrying a provider dot and
// right metadata when the catalog exposes them, and the selected row on the
// selection tint.
func (m model) pickerOverlay(width int) string {
	if m.picker == nil {
		return ""
	}
	if m.picker.kind == pickerModel {
		return m.modelPickerOverlay(width)
	}
	if m.picker.kind == pickerPet {
		return m.petPickerOverlay(width)
	}
	if m.picker.kind == pickerTheme {
		return m.themePickerOverlay(width)
	}
	overlayWidth := minInt(width, pickerOverlayMaxWidth)
	if overlayWidth < pickerOverlayMinWidth {
		overlayWidth = width
	}
	innerWidth := maxInt(1, overlayWidth-4)
	maxVisible := pickerMaxVisible(m.picker, width)
	start := 0
	visible := []pickerItem{}
	if len(m.picker.items) > 0 {
		m.picker.selected = clampInt(m.picker.selected, 0, len(m.picker.items)-1)
		start = selectableListStart(len(m.picker.items), maxVisible, m.picker.selected)
		visible = m.picker.items[start : start+maxVisible]
	}
	showDetails := pickerShowsDetails(m.picker, width)

	lines := make([]string, 0, len(visible)+7)
	title := strings.TrimSpace(m.picker.title)
	// A visible "search > …" line so typing to filter shows what you've typed,
	// matching the /model picker. Followed by a separator, then the rows.
	lines = append(lines, renderPickerSearchLine(m.picker.query, "type to filter…", innerWidth))
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	lastGroup := ""
	for index, item := range visible {
		absoluteIndex := start + index
		if item.Group != "" && item.Group != lastGroup {
			lines = append(lines, zeroTheme.accent.Render(item.Group))
			lastGroup = item.Group
		}
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if absoluteIndex == m.picker.selected {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}
		left := marker
		switch {
		case item.Local:
			left += surface(zeroTheme.blue).Render("● ")
		case item.Remote:
			left += surface(zeroTheme.accent).Render("● ")
		}
		if item.Favorite {
			left += surface(zeroTheme.accent).Render("* ")
		}
		left += surface(zeroTheme.ink).Render(item.Label)
		right := ""
		if item.Meta != "" {
			right = surface(zeroTheme.faintest).Render(item.Meta)
		}
		// Paint the gap on the row surface so selected rows read as one solid
		// band; joinHeaderLine would pad with bare (untinted) spaces.
		gap := innerWidth - lipgloss.Width(left) - lipgloss.Width(right)
		line := left + surface(zeroTheme.ink).Render(strings.Repeat(" ", maxInt(1, gap))) + right
		lines = append(lines, fitStyledLine(line, innerWidth))
		if showDetails && item.Detail != "" {
			// The detail line sits under the row on the same selection band,
			// indented to the label column so it reads as part of the item.
			// Pad the remainder like the row above so the band is full-width.
			detailText := "  " + item.Detail
			detailGap := innerWidth - lipgloss.Width(detailText)
			detail := surface(zeroTheme.faint).Render(detailText) +
				surface(zeroTheme.faint).Render(strings.Repeat(" ", maxInt(0, detailGap)))
			lines = append(lines, fitStyledLine(detail, innerWidth))
		}
	}
	if len(visible) == 0 {
		if m.picker.loading {
			lines = append(lines, zeroTheme.faint.Render("Fetching available models…"))
		} else {
			lines = append(lines, zeroTheme.faint.Render("  no matching items"))
		}
	}
	// Hints live in the footer (a separator + faint keys), matching the /model
	// picker and the other bordered boxes.
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	footer := zeroTheme.faint.Render("↑/↓ move   Enter select   Esc close")
	if m.picker.kind == pickerSession {
		position := 0
		if len(m.picker.items) > 0 {
			position = clampInt(m.picker.selected, 0, len(m.picker.items)-1) + 1
		}
		count := zeroTheme.faint.Render(fmt.Sprintf("%d / %d", position, len(m.picker.items)))
		footer = joinHeaderLine(footer, count, innerWidth)
	}
	lines = append(lines, footer)
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, title, lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

// pickerShowsDetails reports whether the overlay renders each row's optional
// Detail line: only when a row actually carries one and the terminal is at
// least medium width, where the two-line treatment reads cleanly. Narrower
// tiers keep the single-line list fallback.
func pickerShowsDetails(p *commandPicker, width int) bool {
	if p == nil || widthTier(width) < tierMedium {
		return false
	}
	for _, item := range p.items {
		if item.Detail != "" {
			return true
		}
	}
	return false
}

// pickerMaxVisible caps the visible window in items. When detail lines render,
// each item costs two rows, so the budget halves to keep the overlay the same
// height as the single-line list.
func pickerMaxVisible(p *commandPicker, width int) int {
	if p == nil {
		return 0
	}
	budget := pickerOverlayMaxVisible
	if pickerShowsDetails(p, width) {
		budget = pickerOverlayMaxVisible / 2
	}
	return minInt(budget, len(p.items))
}

// themePickerOverlay keeps candidate rendering inside the picker. Moving through
// themes therefore shows their hierarchy and code treatment without repainting the
// transcript, terminal canvas, or any other active UI.
func (m model) themePickerOverlay(width int) string {
	overlayWidth := minInt(width, pickerOverlayMaxWidth)
	if overlayWidth < pickerOverlayMinWidth {
		overlayWidth = width
	}
	innerWidth := maxInt(1, overlayWidth-4)
	listWidth, previewWidth, showPreview := themePickerColumnWidths(innerWidth)

	lines := []string{
		renderPickerSearchLine(m.picker.query, "find a theme…", innerWidth),
		zeroTheme.line.Render(strings.Repeat("─", innerWidth)),
	}
	listLines := m.themePickerListLines(listWidth)
	previewLines := m.themePickerPreviewLines(previewWidth)
	if showPreview {
		lines = append(lines, joinThemePickerColumns(listLines, previewLines, listWidth, previewWidth)...)
	} else {
		lines = append(lines, listLines...)
		if item, ok := m.picker.current(); ok {
			lines = append(lines, zeroTheme.faint.Render("Preview: "+item.Label+" — Enter applies"))
		}
	}
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	lines = append(lines, zeroTheme.faint.Render("↑/↓ preview   Enter apply   Esc close"))
	title := strings.TrimSpace(m.picker.title)
	if title == "" {
		title = "Choose a theme"
	}
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, title, lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

// themePickerColumnWidths keeps the candidate list usable while reserving a
// compact, readable preview. It is shared with pointer hit-testing so the
// preview behaves as a display, not an accidental selection target.
func themePickerColumnWidths(innerWidth int) (listWidth, previewWidth int, showPreview bool) {
	if innerWidth < 72 {
		return innerWidth, 0, false
	}
	listWidth = maxInt(28, (innerWidth*3)/5)
	previewWidth = maxInt(18, innerWidth-listWidth-3)
	return listWidth, previewWidth, true
}

func (m model) themePickerListLines(width int) []string {
	if width < 1 {
		return nil
	}
	maxVisible := minInt(pickerOverlayMaxVisible, len(m.picker.items))
	start := 0
	visible := []pickerItem{}
	if len(m.picker.items) > 0 {
		m.picker.selected = clampInt(m.picker.selected, 0, len(m.picker.items)-1)
		start = selectableListStart(len(m.picker.items), maxVisible, m.picker.selected)
		visible = m.picker.items[start : start+maxVisible]
	}
	lines := make([]string, 0, len(visible)+2)
	lastGroup := ""
	for index, item := range visible {
		absoluteIndex := start + index
		if item.Group != "" && item.Group != lastGroup {
			lines = append(lines, fitStyledLine(zeroTheme.accent.Render(item.Group), width))
			lastGroup = item.Group
		}
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if absoluteIndex == m.picker.selected {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}
		left := marker + surface(zeroTheme.ink).Render(item.Label)
		right := ""
		if item.Meta != "" {
			right = surface(zeroTheme.faintest).Render(item.Meta)
		}
		gap := width - lipgloss.Width(left) - lipgloss.Width(right)
		line := left + surface(zeroTheme.ink).Render(strings.Repeat(" ", maxInt(1, gap))) + right
		lines = append(lines, fitStyledLine(line, width))
	}
	if len(visible) == 0 {
		lines = append(lines, zeroTheme.faint.Render("  no matching themes"))
	}
	return lines
}

func (m model) themePickerPreviewLines(width int) []string {
	if width < 1 {
		return nil
	}
	item, ok := m.picker.current()
	if !ok {
		return []string{zeroTheme.faint.Render("Preview"), zeroTheme.faint.Render("No matching theme")}
	}
	_, preview := themeForMode(themeMode(item.Value), m.hasDarkBg)
	fill := func(line string) string {
		return fitStyledLine(line, width)
	}
	// The preview stays transparent. A candidate-colored background becomes a
	// disconnected slab that competes with the list; foreground roles are enough
	// to show the palette while keeping the terminal canvas visually calm.
	codeOpen := tokenStyleForTheme(preview, chroma.Keyword).Render("func") + preview.ink.Render(" ") + tokenStyleForTheme(preview, chroma.NameFunction).Render("shipIt") + preview.ink.Render("() {")
	codeWork := preview.ink.Render("  ") + tokenStyleForTheme(preview, chroma.Name).Render("tests") + tokenStyleForTheme(preview, chroma.Operator).Render("++")
	codeComment := tokenStyleForTheme(preview, chroma.Comment).Render("  // no bugs, probably")
	codeClose := preview.ink.Render("}")
	status := preview.green.Render("✓") + preview.faint.Render(" tests   ") + preview.accent.Render("0") + preview.faint.Render(" bugs (probably)")
	return []string{
		fill(preview.accent.Bold(true).Render("Preview")),
		fill(codeOpen),
		fill(codeWork),
		fill(codeComment),
		fill(codeClose),
		fill(status),
	}
}

func joinThemePickerColumns(left, right []string, leftWidth, rightWidth int) []string {
	count := maxInt(len(left), len(right))
	lines := make([]string, 0, count)
	for index := 0; index < count; index++ {
		leftLine := ""
		if index < len(left) {
			leftLine = left[index]
		}
		rightLine := ""
		if index < len(right) {
			rightLine = right[index]
		}
		leftLine = fillPaletteLine(leftLine, leftWidth, transparentSurface)
		rightLine = fillPaletteLine(rightLine, rightWidth, transparentSurface)
		lines = append(lines, leftLine+zeroTheme.line.Render(" │ ")+rightLine)
	}
	return lines
}

func (m model) modelPickerOverlay(width int) string {
	if m.picker == nil {
		return ""
	}
	if m.modelPickerLoading {
		return m.modelPickerLoadingOverlay(width)
	}
	overlayWidth := modelPickerOverlayWidth(width, m.picker)
	innerWidth := maxInt(1, overlayWidth-4)
	maxVisible := minInt(pickerOverlayMaxVisible, len(m.picker.items))
	start := 0
	visible := []pickerItem{}
	if len(m.picker.items) > 0 {
		m.picker.selected = clampInt(m.picker.selected, 0, len(m.picker.items)-1)
		start = selectableListStart(len(m.picker.items), maxVisible, m.picker.selected)
		visible = m.picker.items[start : start+maxVisible]
	}

	lines := make([]string, 0, len(visible)+6)
	searchInset := lipgloss.Width("❯ ")
	searchPrefix := transparentSurface(zeroTheme.ink).Render(strings.Repeat(" ", searchInset))
	lines = append(lines, fillPaletteLine(searchPrefix+renderModelPickerSearchLine(m.picker.query, maxInt(1, innerWidth-searchInset)), innerWidth, transparentSurface))
	if status := strings.TrimSpace(m.modelPickerLoadError); status != "" {
		lines = append(lines, fillPaletteLine(searchPrefix+zeroTheme.faint.Render(status), innerWidth, transparentSurface))
	}
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	lastGroup := ""
	for index, item := range visible {
		if item.Group != "" && item.Group != lastGroup {
			lines = append(lines, fillPaletteLine(zeroTheme.accent.Bold(true).Render(item.Group), innerWidth, transparentSurface))
			lastGroup = item.Group
		}
		lines = append(lines, renderModelPickerRow(innerWidth, start+index == m.picker.selected, item))
	}
	if len(visible) == 0 {
		lines = append(lines, fillPaletteLine(searchPrefix+zeroTheme.faint.Render("no matching models"), innerWidth, transparentSurface))
	}
	if item, ok := m.picker.current(); ok {
		if detail := modelPickerItemDetail(item); detail != "" {
			lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
			lines = append(lines, fillPaletteLine(searchPrefix+zeroTheme.faint.Render(detail), innerWidth, transparentSurface))
		}
	}
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	footer := "↑/↓ move   Enter select   Ctrl+F favorite   Esc close"
	lines = append(lines, fillPaletteLine(zeroTheme.faint.Render(footer), innerWidth, transparentSurface))
	title := strings.TrimSpace(m.picker.title)
	if title == "" {
		title = "Choose a model"
	}
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, title, lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

func (m model) modelPickerLoadingOverlay(width int) string {
	overlayWidth := modelPickerLoadingOverlayWidth(width)
	innerWidth := maxInt(1, overlayWidth-4)
	lines := []string{
		fillPaletteLine(zeroTheme.faint.Render("Checking available models..."), innerWidth, transparentSurface),
		fillPaletteLine(zeroTheme.faint.Render("Built-in models will be used if discovery fails."), innerWidth, transparentSurface),
		zeroTheme.line.Render(strings.Repeat("─", innerWidth)),
		fillPaletteLine(zeroTheme.faint.Render("Esc close"), innerWidth, transparentSurface),
	}
	title := strings.TrimSpace(m.picker.title)
	if title == "" {
		title = "Choose a model"
	}
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, title, lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

func modelPickerLoadingOverlayWidth(terminalWidth int) int {
	if terminalWidth <= 0 {
		terminalWidth = defaultStartupWidth
	}
	available := minInt(terminalWidth, modelPickerOverlayMaxWidth)
	if terminalWidth < modelPickerOverlayMinWidth {
		available = terminalWidth
	}
	target := lipgloss.Width("Built-in models will be used if discovery fails.")
	target = maxInt(target, lipgloss.Width("Choose a model"))
	target = maxInt(target, lipgloss.Width("Esc close"))
	overlayWidth := maxInt(modelPickerOverlayMinWidth, target+4)
	return minInt(overlayWidth, maxInt(4, available))
}

func modelPickerOverlayWidth(terminalWidth int, picker *commandPicker) int {
	if terminalWidth <= 0 {
		terminalWidth = defaultStartupWidth
	}
	available := minInt(terminalWidth, modelPickerOverlayMaxWidth)
	if terminalWidth < modelPickerOverlayMinWidth {
		available = terminalWidth
	}
	target := lipgloss.Width("Choose a model")
	target = maxInt(target, lipgloss.Width("  search > model name..."))
	target = maxInt(target, lipgloss.Width("↑/↓ move   Enter select   Ctrl+F favorite   Esc close"))
	target = maxInt(target, lipgloss.Width("  Using built-in model list"))
	if picker != nil {
		for _, item := range picker.items {
			labelWidth := lipgloss.Width(item.Label)
			if item.Favorite {
				labelWidth += lipgloss.Width("* ")
			}
			target = maxInt(target, lipgloss.Width("❯ ")+labelWidth)
			if detail := modelPickerItemDetail(item); detail != "" {
				target = maxInt(target, lipgloss.Width("  "+detail))
			}
		}
	}
	overlayWidth := maxInt(modelPickerOverlayMinWidth, target+4)
	return minInt(overlayWidth, maxInt(4, available))
}

func renderModelPickerSearchLine(query string, width int) string {
	return renderPickerSearchLine(query, "model name...", width)
}

// renderPickerSearchLine renders the "search > <query>▌" input line shared by the
// popup pickers, so what you type while filtering is always visible. placeholder
// is the faint hint shown when the query is empty.
func renderPickerSearchLine(query, placeholder string, width int) string {
	query = strings.TrimSpace(query)
	prompt := zeroTheme.userPrompt.Render("search > ")
	cursor := zeroTheme.accent.Render("▌")
	if query == "" {
		return fitStyledLine(prompt+cursor+zeroTheme.faint.Render(placeholder), width)
	}
	return fitStyledLine(prompt+zeroTheme.ink.Render(query)+cursor, width)
}

func renderModelPickerRow(width int, selected bool, item pickerItem) string {
	surface := transparentSurface
	marker := surface(zeroTheme.faintest).Render("  ")
	if selected {
		surface = zeroTheme.onSel
		marker = surface(zeroTheme.accent).Render("❯ ")
	}
	label := strings.TrimSpace(item.Label)
	if label == "" {
		label = strings.TrimSpace(item.Value)
	}
	prefix := ""
	if item.Favorite {
		prefix = "* "
	}
	left := marker + surface(zeroTheme.ink).Render(prefix+label)
	// The provider is shown as a section header above each group, so rows no longer
	// repeat it as a right-aligned tag (matches a grouped provider+model list).
	return fillPaletteLine(left, width, surface)
}

func modelPickerItemDetail(item pickerItem) string {
	parts := []string{}
	value := strings.TrimSpace(item.Value)
	label := strings.TrimSpace(item.Label)
	if value != "" && value != label {
		parts = append(parts, value)
	}
	if meta := strings.TrimSpace(item.Meta); meta != "" {
		parts = append(parts, meta)
	}
	return strings.Join(parts, " · ")
}

// argHint extracts the most representative argument from a tool call's raw JSON
// arguments for the single-line tool row (the path, pattern, or command acted on).
func argHint(raw string) string {
	return firstArgValue(raw, []string{"path", "file", "file_path", "filepath", "pattern", "query", "command", "cmd", "url", "task"})
}

// argHintSecondary extracts the card head's faintest arg column: the
// non-target argument (pattern/query/command) when argHint already resolved to
// a path. With no path argument the value is argHint itself, so it stays in
// the target slot and this returns "".
func argHintSecondary(raw string) string {
	secondary := firstArgValue(raw, []string{"pattern", "query", "command", "cmd", "url"})
	if secondary == "" || secondary == argHint(raw) {
		return ""
	}
	return secondary
}

func firstArgValue(raw string, keys []string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := args[key]; ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return singleLineToolHeadText(text)
			}
		}
	}
	return ""
}

// looksLikeDiff reports whether output should be rendered as a diff card: a
// real hunk header, or both old/new file headers. A single line starting with
// "---" (a Markdown rule, YAML document marker, log separator…) must NOT
// hijack ordinary bash/tool output into the diff renderer.
func looksLikeDiff(text string) bool {
	if !strings.Contains(text, "\n") {
		return false
	}
	hasOld, hasNew := false, false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case hunkHeaderPattern.MatchString(line):
			return true
		case strings.HasPrefix(line, "+++ "):
			hasNew = true
		case strings.HasPrefix(line, "--- "):
			hasOld = true
		}
		if hasOld && hasNew {
			return true
		}
	}
	return false
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}
