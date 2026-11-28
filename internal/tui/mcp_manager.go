package tui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	mcpManagerOverlayMaxWidth = 168
	mcpManagerOverlayMinWidth = 58
	mcpManagerMaxVisible      = 12
)

type mcpManagerState struct {
	selected int
	query    string
}

type mcpManagerItemKind int

const (
	mcpManagerItemServer mcpManagerItemKind = iota
	mcpManagerItemMarketplace
	mcpManagerItemAddRemote
	mcpManagerItemAddStdio
	mcpManagerItemList
)

type mcpManagerItem struct {
	Kind mcpManagerItemKind
	// Name is the display label. It is NOT an identity: two config entries whose
	// keys differ only by padding render under the same name.
	Name string
	// ConfigKey addresses the configuration map. Every action and every lookup
	// uses it, because Name cannot distinguish the rows.
	ConfigKey      string
	Label          string
	Meta           string
	Detail         string
	InstallCommand string
}

type mcpMarketplaceEntry struct {
	ID             string
	Name           string
	Description    string
	Meta           string
	InstallCommand string
	Tags           []string
}

var mcpMarketplaceCatalog = []mcpMarketplaceEntry{
	{
		ID:             "filesystem",
		Name:           "Filesystem",
		Description:    "Read and manage files under an explicit workspace path.",
		Meta:           "stdio · official · files",
		InstallCommand: "/mcp add filesystem -- npx -y @modelcontextprotocol/server-filesystem .",
		Tags:           []string{"files", "local", "official", "workspace"},
	},
	{
		ID:             "memory",
		Name:           "Memory",
		Description:    "Persistent knowledge graph memory for long-lived project facts.",
		Meta:           "stdio · official · memory",
		InstallCommand: "/mcp add memory -- npx -y @modelcontextprotocol/server-memory",
		Tags:           []string{"memory", "knowledge", "official"},
	},
	{
		ID:             "fetch",
		Name:           "Fetch",
		Description:    "Fetch and convert web content for model-readable context.",
		Meta:           "stdio · official · web",
		InstallCommand: "/mcp add fetch -- npx -y @modelcontextprotocol/server-fetch",
		Tags:           []string{"web", "fetch", "official"},
	},
	{
		ID:             "sequential-thinking",
		Name:           "Sequential Thinking",
		Description:    "Structured step-by-step reasoning as an MCP tool.",
		Meta:           "stdio · official · reasoning",
		InstallCommand: "/mcp add sequential-thinking -- npx -y @modelcontextprotocol/server-sequential-thinking",
		Tags:           []string{"reasoning", "planning", "official"},
	},
	{
		ID:             "context7",
		Name:           "Context7",
		Description:    "Fresh library documentation and examples over HTTP MCP.",
		Meta:           "http · docs · remote",
		InstallCommand: "/mcp add context7 --url https://mcp.context7.com/mcp",
		Tags:           []string{"docs", "libraries", "context7", "remote"},
	},
	{
		ID:             "playwright",
		Name:           "Playwright",
		Description:    "Browser automation and page inspection through Playwright MCP.",
		Meta:           "stdio · browser · @playwright/mcp",
		InstallCommand: "/mcp add playwright -- npx -y @playwright/mcp",
		Tags:           []string{"browser", "automation", "testing", "playwright"},
	},
}

func (m model) openMCPManager() model {
	m.refreshMCPViewState()
	m.mcpManager = &mcpManagerState{}
	m.clearSuggestions()
	return m
}

func (m model) handleMCPManagerKey(msg tea.KeyMsg) (model, tea.Cmd) {
	if m.mcpManager == nil {
		return m, nil
	}
	switch {
	case keyIs(msg, tea.KeyEsc):
		m.mcpManager = nil
	case keyIs(msg, tea.KeyUp):
		m.moveMCPManager(-1)
	case keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		m.moveMCPManager(1)
	case keyIs(msg, tea.KeyEnter):
		return m.chooseMCPManagerItem()
	case keyBackspace(msg) || keyIs(msg, tea.KeyDelete):
		m.deleteMCPManagerQueryRune()
	case keyCtrl(msg, 'u'):
		m.mcpManager.query = ""
		m.mcpManager.selected = 0
	case keyText(msg) != "":
		if !keyAlt(msg) {
			m.appendMCPManagerQuery(keyRunes(msg))
			return m, nil
		}
		switch strings.ToLower(keyText(msg)) {
		case "a":
			return m.openMCPAddWizard("http"), nil
		case "s":
			return m.openMCPAddWizard("stdio"), nil
		case "l":
			return m.runMCPManagerCommand([]string{"list"})
		case "c":
			if item, ok := m.currentMCPManagerItem(); ok && item.Kind == mcpManagerItemServer {
				return m.runMCPManagerCommand([]string{"check", item.ConfigKey})
			}
		case "d":
			if item, ok := m.currentMCPManagerItem(); ok && item.Kind == mcpManagerItemServer {
				return m.runMCPManagerCommand([]string{"disable", item.ConfigKey})
			}
		case "e":
			if item, ok := m.currentMCPManagerItem(); ok && item.Kind == mcpManagerItemServer {
				return m.runMCPManagerCommand([]string{"enable", item.ConfigKey})
			}
		case "r":
			if item, ok := m.currentMCPManagerItem(); ok && item.Kind == mcpManagerItemServer {
				return m.runMCPManagerCommand([]string{"remove", item.ConfigKey})
			}
		}
	}
	return m, nil
}

func (m *model) appendMCPManagerQuery(runes []rune) {
	if m.mcpManager == nil {
		return
	}
	for _, r := range runes {
		if r == '\t' || r == '\n' || r == '\r' || unicode.IsControl(r) {
			continue
		}
		m.mcpManager.query += string(r)
	}
	m.mcpManager.selected = 0
}

func (m *model) deleteMCPManagerQueryRune() {
	if m.mcpManager == nil || m.mcpManager.query == "" {
		return
	}
	runes := []rune(m.mcpManager.query)
	m.mcpManager.query = string(runes[:len(runes)-1])
	m.mcpManager.selected = 0
}

func (m *model) moveMCPManager(delta int) {
	if m.mcpManager == nil {
		return
	}
	count := len(m.mcpManagerItems())
	if count == 0 {
		m.mcpManager.selected = 0
		return
	}
	m.mcpManager.selected = ((m.mcpManager.selected+delta)%count + count) % count
}

func (m model) chooseMCPManagerItem() (model, tea.Cmd) {
	item, ok := m.currentMCPManagerItem()
	if !ok {
		return m, nil
	}
	switch item.Kind {
	case mcpManagerItemServer:
		return m.runMCPManagerCommand([]string{"check", item.ConfigKey})
	case mcpManagerItemMarketplace:
		return m.prefillMCPManagerCommand(item.InstallCommand), nil
	case mcpManagerItemAddRemote:
		return m.openMCPAddWizard("http"), nil
	case mcpManagerItemAddStdio:
		return m.openMCPAddWizard("stdio"), nil
	case mcpManagerItemList:
		return m.runMCPManagerCommand([]string{"list"})
	default:
		return m, nil
	}
}

func (m model) prefillMCPManagerCommand(command string) model {
	m.mcpManager = nil
	m.input.SetValue(command)
	m.input.SetCursor(len([]rune(command)))
	m.resetComposerFromInput()
	m.clearSuggestions()
	return m
}

func (m model) runMCPManagerCommand(args []string) (model, tea.Cmd) {
	request := mcpCommandRequest{origin: mcpCommandOriginManager, args: append([]string{}, args...)}
	if m.mcpManager != nil {
		request.managerSelected = m.mcpManager.selected
		request.managerQuery = m.mcpManager.query
	}
	return m.startMCPCommand(request)
}

func (m model) currentMCPManagerItem() (mcpManagerItem, bool) {
	if m.mcpManager == nil {
		return mcpManagerItem{}, false
	}
	items := m.mcpManagerItems()
	if len(items) == 0 {
		return mcpManagerItem{}, false
	}
	m.mcpManager.selected = clampInt(m.mcpManager.selected, 0, len(items)-1)
	return items[m.mcpManager.selected], true
}

func (m model) mcpManagerItems() []mcpManagerItem {
	state := m.mcpViewState()
	query := ""
	if m.mcpManager != nil {
		query = strings.ToLower(strings.TrimSpace(m.mcpManager.query))
	}
	installed := make(map[string]bool, len(state.Servers))
	items := make([]mcpManagerItem, 0, len(state.Servers)+len(mcpMarketplaceCatalog)+3)
	for _, server := range state.Servers {
		name := displayValue(strings.TrimSpace(server.Name), "unnamed")
		installed[strings.ToLower(name)] = true
		item := mcpManagerItem{
			Kind:      mcpManagerItemServer,
			Name:      name,
			ConfigKey: server.ConfigKey,
			Label:     name,
			Meta:      mcpManagerServerMeta(server),
			Detail:    strings.Join([]string{name, server.Transport, server.State, server.Auth, server.Target}, " "),
		}
		if mcpManagerItemMatches(item, query) {
			items = append(items, item)
		}
	}
	for _, entry := range mcpMarketplaceCatalog {
		if installed[strings.ToLower(entry.ID)] {
			continue
		}
		item := mcpMarketplaceItem(entry)
		if mcpManagerItemMatches(item, query) {
			items = append(items, item)
		}
	}
	for _, item := range []mcpManagerItem{
		{Kind: mcpManagerItemAddRemote, Name: "custom-remote", Label: "Add MCP server", Meta: "zero mcp add <name> --url <url>", Detail: "custom remote http sse url"},
		{Kind: mcpManagerItemAddStdio, Name: "custom-stdio", Label: "Add local stdio MCP", Meta: "zero mcp add <name> -- <command> [args...]", Detail: "custom local stdio command"},
		{Kind: mcpManagerItemList, Name: "list", Label: "List configured", Meta: "zero mcp list", Detail: "configured installed servers"},
	} {
		if mcpManagerItemMatches(item, query) {
			items = append(items, item)
		}
	}
	return items
}

func mcpManagerFirstItemRow(state MCPViewState) int {
	baseRow := 4 // top border + server count + search + "User MCPs"
	if len(state.Servers) == 0 {
		baseRow++
	}
	return baseRow
}

func mcpMarketplaceItem(entry mcpMarketplaceEntry) mcpManagerItem {
	return mcpManagerItem{
		Kind:           mcpManagerItemMarketplace,
		Name:           entry.ID,
		Label:          entry.Name,
		Meta:           entry.Meta,
		Detail:         strings.Join(append([]string{entry.Description, entry.InstallCommand}, entry.Tags...), " "),
		InstallCommand: entry.InstallCommand,
	}
}

func mcpManagerItemMatches(item mcpManagerItem, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{item.Name, item.Label, item.Meta, item.Detail, item.InstallCommand}, " "))
	return strings.Contains(haystack, query)
}

func mcpManagerServerMeta(server MCPServerView) string {
	parts := []string{
		displayValue(strings.TrimSpace(server.State), "configured"),
	}
	if auth := strings.TrimSpace(server.Auth); auth != "" {
		parts = append(parts, auth)
	}
	if server.ToolCount > 0 {
		parts = append(parts, pluralCount(server.ToolCount, "tool"))
	}
	parts = append(parts, displayValue(strings.TrimSpace(server.Transport), "unknown"))
	return strings.Join(parts, " · ")
}

func (m model) mcpManagerOverlay(width int) string {
	if m.mcpManager == nil {
		return ""
	}
	if width <= 0 {
		width = defaultStartupWidth
	}
	overlayWidth := minInt(width, mcpManagerOverlayMaxWidth)
	if overlayWidth < mcpManagerOverlayMinWidth {
		overlayWidth = width
	}
	innerWidth := maxInt(1, overlayWidth-4)
	items := m.mcpManagerItems()
	if len(items) > 0 {
		m.mcpManager.selected = clampInt(m.mcpManager.selected, 0, len(items)-1)
	}

	state := m.mcpViewState()
	lines := []string{
		fillPaletteLine(zeroTheme.ink.Bold(true).Render(pluralCount(len(state.Servers), "server")), innerWidth, transparentSurface),
		fillPaletteLine(renderMCPManagerSearchLine(m.mcpManager.query, innerWidth), innerWidth, transparentSurface),
		zeroTheme.accent.Bold(true).Render("User MCPs"),
	}
	if len(state.Servers) == 0 {
		lines = append(lines, zeroTheme.faint.Render("  No MCP servers configured."))
	}
	itemLines, _ := m.renderMCPManagerItemLines(innerWidth, items)
	lines = append(lines, itemLines...)
	if detail := m.mcpManagerSelectionDetail(innerWidth); len(detail) > 0 {
		lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
		lines = append(lines, detail...)
	}
	lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", innerWidth)))
	lines = append(lines, fillPaletteLine(zeroTheme.faint.Render("type search   up/down navigate   Enter action   Alt+d disable   Esc close"), innerWidth, transparentSurface))
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, "Manage MCP servers", lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

func renderMCPManagerSearchLine(query string, width int) string {
	query = strings.TrimSpace(query)
	prompt := zeroTheme.userPrompt.Render("search > ")
	if query == "" {
		return fitStyledLine(prompt+zeroTheme.faint.Render("MCP servers, tools, docs..."), width)
	}
	return fitStyledLine(prompt+zeroTheme.ink.Render(query), width)
}

func (m model) renderMCPManagerItemLines(width int, items []mcpManagerItem) ([]string, []int) {
	if len(items) == 0 {
		return []string{fillPaletteLine(zeroTheme.faint.Render("  no MCP actions"), width, transparentSurface)}, []int{-1}
	}
	maxVisible := minInt(mcpManagerMaxVisible, len(items))
	start := selectableListStart(len(items), maxVisible, m.mcpManager.selected)
	visible := items[start : start+maxVisible]
	lines := make([]string, 0, len(visible))
	itemRows := make([]int, 0, len(visible))
	lastGroup := ""
	for offset, item := range visible {
		index := start + offset
		if group := mcpManagerItemGroup(item.Kind); group != "" && group != lastGroup {
			lines = append(lines, zeroTheme.accent.Bold(true).Render(group))
			itemRows = append(itemRows, -1)
			lastGroup = group
		}
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if index == m.mcpManager.selected {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("› ")
		}
		left := marker + surface(zeroTheme.ink).Render(item.Label)
		right := ""
		if item.Meta != "" {
			right = surface(zeroTheme.faint).Render(item.Meta)
		}
		gap := width - lipgloss.Width(left) - lipgloss.Width(right)
		line := left + surface(zeroTheme.ink).Render(strings.Repeat(" ", maxInt(1, gap))) + right
		lines = append(lines, fillPaletteLine(line, width, surface))
		itemRows = append(itemRows, index)
	}
	return lines, itemRows
}

func mcpManagerItemGroup(kind mcpManagerItemKind) string {
	switch kind {
	case mcpManagerItemMarketplace:
		return "Marketplace"
	case mcpManagerItemAddRemote, mcpManagerItemAddStdio, mcpManagerItemList:
		return "Actions"
	default:
		return ""
	}
}

func (m model) mcpManagerSelectionDetail(width int) []string {
	item, ok := m.currentMCPManagerItem()
	if !ok {
		return nil
	}
	switch item.Kind {
	case mcpManagerItemServer:
		server, ok := m.mcpManagerServer(item.ConfigKey)
		if !ok {
			return nil
		}
		lines := []string{
			fillPaletteLine(zeroTheme.ink.Bold(true).Render(server.Name)+" "+zeroTheme.faint.Render(server.Transport+" · "+server.State), width, transparentSurface),
		}
		// Directly under the header, above the target, and through the same
		// sanitizer mcpManagerServerLines uses.
		//
		// This overlay is what a bare /mcp opens, so it is the first place a user
		// goes after a startup warning has scrolled away. It reported the state
		// and stopped: "failed" on its own sends the reader to check their config
		// when the answer is usually in the error, which is the whole point of
		// recording it. The transcript path showed the reason and this one did
		// not, so the fix only reached the surface people were not looking at.
		if reason := sanitizeTerminalReason(server.Error); reason != "" {
			lines = append(lines, fitStyledLine(zeroTheme.faint.Render(reason), width))
		}
		if target := strings.TrimSpace(server.Target); target != "" {
			lines = append(lines, fitStyledLine(zeroTheme.faint.Render(target), width))
		}
		action := "Alt+d disable"
		if strings.EqualFold(strings.TrimSpace(server.State), "disabled") {
			action = "Alt+e enable"
		}
		lines = append(lines, fillPaletteLine(zeroTheme.faint.Render("Enter check   Alt+c check   "+action+"   Alt+r remove"), width, transparentSurface))
		return lines
	case mcpManagerItemMarketplace:
		lines := []string{
			fillPaletteLine(zeroTheme.ink.Bold(true).Render(item.Label)+" "+zeroTheme.faint.Render(item.Meta), width, transparentSurface),
		}
		if detail := firstMCPMarketplaceDetailLine(item); detail != "" {
			lines = append(lines, fitStyledLine(zeroTheme.faint.Render(detail), width))
		}
		lines = append(lines, fillPaletteLine(zeroTheme.faint.Render("Enter fills composer: "+item.InstallCommand), width, transparentSurface))
		return lines
	case mcpManagerItemAddRemote:
		return []string{fillPaletteLine(zeroTheme.faint.Render("Enter fills composer: /mcp add <name> --url <url>"), width, transparentSurface)}
	case mcpManagerItemAddStdio:
		return []string{fillPaletteLine(zeroTheme.faint.Render("Enter fills composer: /mcp add <name> -- <command> [args...]"), width, transparentSurface)}
	case mcpManagerItemList:
		return []string{fillPaletteLine(zeroTheme.faint.Render("Enter runs zero mcp list and refreshes this manager."), width, transparentSurface)}
	default:
		return nil
	}
}

func firstMCPMarketplaceDetailLine(item mcpManagerItem) string {
	detail := strings.TrimSpace(item.Detail)
	if detail == "" {
		return ""
	}
	if before, _, ok := strings.Cut(detail, " /mcp add "); ok {
		return strings.TrimSpace(before)
	}
	return detail
}

// mcpManagerServer selects a row by its CONFIG KEY, not its display name.
//
// Two entries whose keys differ only by padding render under one canonical name,
// and ValidateUniqueNames deliberately accepts that when one of them is
// disabled. Matching on the name returned whichever came first, so a detail
// pane or an action chosen on the disabled row could land on the enabled one.
func (m model) mcpManagerServer(configKey string) (MCPServerView, bool) {
	for _, server := range m.mcpViewState().Servers {
		if server.ConfigKey == configKey {
			return server, true
		}
	}
	return MCPServerView{}, false
}
