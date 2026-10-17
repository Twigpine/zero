package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type MCPViewState struct {
	Servers     []MCPServerView
	Tools       []MCPToolView
	Permissions MCPPermissionSummary
	OAuth       MCPOAuthSummary
}

type MCPServerView struct {
	// Name is the CANONICAL runtime name: trimmed, and the spelling the registry,
	// skipped-failure records and tool counts all use. It is the display label and
	// the join key, and it is deliberately not unique across config entries.
	Name string
	// ConfigKey is the EXACT key in the configuration map, untrimmed. Actions
	// address the config by that key, so it is the only identity that can select
	// one row when a padded alias renders under the same canonical name.
	ConfigKey string
	Transport string
	State     string
	Target    string
	Auth      string
	ToolCount int
	// Error explains a "failed" state. Empty for every other state.
	Error string
}

type MCPToolView struct {
	ServerName   string
	Name         string
	RegistryName string
	SideEffect   string
	Permission   string
	Description  string
}

type MCPPermissionSummary struct {
	Mode         string
	GrantCount   int
	ServerGrants int
	ToolGrants   int
	PromptCount  int
	DeniedCount  int
	Grants       []MCPPermissionGrantView
}

type MCPPermissionGrantView struct {
	Target     string
	Autonomy   string
	ApprovedAt string
}

type MCPOAuthSummary struct {
	Servers []MCPOAuthServerView
}

type MCPOAuthServerView struct {
	ServerName      string
	Configured      bool
	HasToken        bool
	HasRefreshToken bool
	TokenType       string
	Scopes          []string
	ExpiresAt       time.Time
	Expired         bool
}

func renderMCPView(state MCPViewState, width int) string {
	lines := []string{
		"Manage MCP servers",
		pluralCount(len(state.Servers), "server"),
		"",
	}

	if !hasMCPViewContent(state) {
		lines = append(lines,
			"User MCPs",
			"  No MCP servers configured.",
			"  › Add MCP server      zero mcp add <name> --url <url>",
			"  › Add local stdio MCP zero mcp add <name> -- <command> [args...]",
			"  › List configured     zero mcp list",
			"",
			"Actions",
			"  add remote · add stdio · list configured · check health",
		)
		return fitMCPManagerLines(lines, width)
	}

	if len(state.Servers) > 0 {
		lines = append(lines, "User MCPs")
		lines = append(lines, mcpManagerServerLines(state.Servers)...)
	}
	if len(state.Tools) > 0 {
		lines = append(lines, "", "Tools")
		lines = append(lines, mcpToolLines(state.Tools)...)
	}
	if hasMCPPermissionSummary(state.Permissions) {
		lines = append(lines, "", "Permissions")
		for _, line := range mcpPermissionLines(state.Permissions) {
			lines = append(lines, "  "+line)
		}
	}
	if len(state.OAuth.Servers) > 0 {
		lines = append(lines, "", "OAuth")
		lines = append(lines, mcpOAuthLines(state.OAuth.Servers)...)
	}

	lines = append(lines,
		"",
		"Actions",
		"  add: zero mcp add <name> --url <url>",
		"  disconnect: zero mcp disable <name>",
		"  reconnect: zero mcp enable <name>",
		"  remove: zero mcp remove <name>",
	)
	return fitMCPManagerLines(lines, width)
}

func hasMCPViewContent(state MCPViewState) bool {
	return hasMCPOperationalContent(state) ||
		hasMCPPermissionActivity(state.Permissions)
}

func hasMCPOperationalContent(state MCPViewState) bool {
	return len(state.Servers) > 0 ||
		len(state.Tools) > 0 ||
		len(state.OAuth.Servers) > 0
}

func hasMCPPermissionActivity(summary MCPPermissionSummary) bool {
	return summary.GrantCount > 0 ||
		summary.ServerGrants > 0 ||
		summary.ToolGrants > 0 ||
		summary.PromptCount > 0 ||
		summary.DeniedCount > 0 ||
		len(summary.Grants) > 0
}

func mcpManagerServerLines(servers []MCPServerView) []string {
	lines := make([]string, 0, len(servers)*3)
	for index, server := range servers {
		name := displayValue(strings.TrimSpace(server.Name), "unnamed")
		transport := displayValue(strings.TrimSpace(server.Transport), "unknown")
		state := displayValue(strings.TrimSpace(server.State), "configured")
		prefix := "  "
		if index == 0 {
			prefix = "› "
		}

		parts := []string{name, state}
		if auth := strings.TrimSpace(server.Auth); auth != "" {
			parts = append(parts, auth)
		}
		if server.ToolCount > 0 {
			parts = append(parts, pluralCount(server.ToolCount, "tool"))
		}
		parts = append(parts, transport)
		lines = append(lines, prefix+strings.Join(parts, " · "))
		// The reason sits directly under the server rather than in the actions
		// line, because "failed" on its own sends the reader to check their
		// config when the answer is usually in the error: a missing binary, a
		// refused connection, a bad token.
		if reason := sanitizeTerminalReason(server.Error); reason != "" {
			lines = append(lines, "  "+reason)
		}
		if target := strings.TrimSpace(server.Target); target != "" {
			lines = append(lines, "  "+target)
		}
		if actions := strings.TrimSpace(mcpServerActionLine(server)); actions != "" {
			lines = append(lines, "  "+actions)
		}
	}
	return lines
}

// maxMCPReasonLen bounds the failure reason so one verbose server cannot push
// the rest of the panel off screen.
const maxMCPReasonLen = 400

// maxMCPReasonRawLen bounds the input the sanitizer walks. Nothing upstream caps
// the handshake error a server hands back, and maxMCPReasonLen alone cannot end
// the walk: escape sequences are consumed without producing output, so they
// spend input against a budget that never fills. The bound sits far above the
// visible cap so a genuinely long error is still truncated by display rules
// rather than by this.
const maxMCPReasonRawLen = 16 * 1024

// sanitizeTerminalReason turns a server-authored string into one safe terminal
// line.
//
// The failure reason is the only value on this panel that the MCP server writes
// itself, and it goes straight to a terminal. redaction.ErrorMessage removes
// credentials, not control bytes, so without this a hostile handshake error can
// clear the screen, reposition the cursor, or embed a newline followed by text
// shaped like a real entry and forge a row for a server that does not exist.
//
// Escape sequences are consumed whole rather than dropping ESC alone: removing
// the ESC and leaving "[2J" behind would print visible junk, and an abandoned
// OSC payload can still smuggle a title-set or a hyperlink.
// stripTerminalRejoiners removes the bytes that DISAPPEAR WITHOUT LEAVING A GAP:
// escape sequences, and every other control byte except the newline, carriage
// return and tab that become spaces upstream.
//
// The name is the point. These bytes do not merely vanish, they close up behind
// themselves, so text on either side of one becomes adjacent. A server that
// echoes a credential back with an escape inserted into the middle of it sends a
// value that matches nothing during redaction and is reassembled into the intact
// credential here. Splitting this out of sanitizeTerminalReason lets redaction
// run against the same text the reader will eventually see, without inheriting
// the display truncation, which would cut a secret in half and leave the head of
// it unmatched.
//
// Newline, carriage return and tab are deliberately left alone: upstream turns
// them into spaces that survive the collapse, so they separate rather than
// rejoin, and normalizing them here would delete the word boundaries the reason
// is easier to read with.
func stripTerminalRejoiners(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	runes := []rune(value)
	for index := 0; index < len(runes); index++ {
		current := runes[index]
		if current == 0x1b {
			index++
			if index >= len(runes) {
				break
			}
			switch runes[index] {
			case '[': // CSI: parameters, then a final byte in @ to ~
				index++
				for index < len(runes) && (runes[index] < '@' || runes[index] > '~') {
					index++
				}
			case ']': // OSC: runs until BEL or ST
				index++
				for index < len(runes) {
					if runes[index] == 0x07 {
						break
					}
					if runes[index] == 0x1b && index+1 < len(runes) && runes[index+1] == '\\' {
						index++
						break
					}
					index++
				}
			}
			continue
		}
		if current == '\n' || current == '\r' || current == '\t' {
			out.WriteRune(current)
			continue
		}
		// Every other control byte is dropped: none carries a display meaning
		// worth preserving here.
		if current < 0x20 || current == 0x7f || (current >= 0x80 && current <= 0x9f) {
			continue
		}
		// Unicode format characters rejoin exactly like control bytes do, and
		// they are the more comfortable way to do it: a zero-width space or a
		// soft hyphen inside a credential is invisible on the terminal, so the
		// reader sees an unbroken secret while equality redaction saw two
		// fragments. Dropping the whole Cf category covers the zero-width
		// characters, the word joiner, the bidi controls and the byte order
		// mark together. Combining marks are deliberately NOT dropped: they are
		// ordinary content in most of the world's scripts.
		if unicode.Is(unicode.Cf, current) {
			continue
		}
		// DEFAULT-IGNORABLE MARKS REJOIN THE SAME WAY, and they are Mn rather than
		// Cf, so dropping Cf alone left them. A combining grapheme joiner or a
		// variation selector inside a credential renders as nothing, so the reader
		// sees an unbroken secret while equality redaction saw two fragments.
		//
		// Only the ignorable subset is dropped, NOT the Mn category: an ordinary
		// combining acute is content in most of the world's scripts, and deleting
		// it would corrupt the diagnostic this pass exists to show.
		if unicode.Is(unicode.Other_Default_Ignorable_Code_Point, current) ||
			unicode.Is(unicode.Variation_Selector, current) {
			continue
		}
		out.WriteRune(current)
	}
	return out.String()
}

func sanitizeTerminalReason(value string) string {
	if len(value) > maxMCPReasonRawLen {
		value = value[:maxMCPReasonRawLen]
		// The cut lands on an arbitrary byte. Drop a rune the bound split so the
		// panel never shows a replacement character it produced itself.
		for len(value) > 0 {
			decoded, width := utf8.DecodeLastRuneInString(value)
			if decoded != utf8.RuneError || width > 1 {
				break
			}
			value = value[:len(value)-1]
		}
	}
	var out strings.Builder
	for _, current := range stripTerminalRejoiners(value) {
		// Newlines and tabs become spaces so the reason stays on the single row
		// the panel counted for it.
		if current == '\n' || current == '\r' || current == '\t' {
			out.WriteRune(' ')
			continue
		}
		out.WriteRune(current)
	}
	// Fields also collapses the runs of spaces the substitutions above create.
	collapsed := strings.Join(strings.Fields(out.String()), " ")
	if trimmed := []rune(collapsed); len(trimmed) > maxMCPReasonLen {
		// Truncate by rune so a multi-byte character is never cut in half.
		collapsed = string(trimmed[:maxMCPReasonLen]) + "..."
	}
	return collapsed
}

func mcpToolLines(tools []MCPToolView) []string {
	grouped := map[string][]MCPToolView{}
	order := []string{}
	for _, tool := range tools {
		serverName := displayValue(strings.TrimSpace(tool.ServerName), "unknown")
		if _, ok := grouped[serverName]; !ok {
			order = append(order, serverName)
		}
		grouped[serverName] = append(grouped[serverName], tool)
	}

	lines := make([]string, 0, len(tools)+len(order))
	for _, serverName := range order {
		serverTools := grouped[serverName]
		lines = append(lines, commandBullet(fmt.Sprintf("%s tools (%d)", serverName, len(serverTools))))
		for _, tool := range serverTools {
			lines = append(lines, "  "+commandBullet(mcpToolLine(tool)))
		}
	}
	return lines
}

func mcpToolLine(tool MCPToolView) string {
	name := strings.TrimSpace(tool.RegistryName)
	if name == "" {
		name = strings.Trim(strings.Join([]string{"mcp", tool.ServerName, tool.Name}, "_"), "_")
	}
	if name == "" {
		name = "mcp_tool"
	}

	sideEffect := displayValue(strings.TrimSpace(tool.SideEffect), "unknown")
	permission := displayValue(strings.TrimSpace(tool.Permission), "prompt")
	displayName := displayValue(strings.TrimSpace(tool.Name), name)
	line := fmt.Sprintf("%s [%s/%s]", displayName, sideEffect, permission)
	if name != "" && name != displayName {
		line += " - " + name
	}
	if target := mcpToolTarget(tool); target != "" && target != displayName {
		line += " - " + target
	}
	if description := strings.TrimSpace(tool.Description); description != "" {
		line += " - " + description
	}
	return line
}

func mcpToolTarget(tool MCPToolView) string {
	server := strings.TrimSpace(tool.ServerName)
	name := strings.TrimSpace(tool.Name)
	switch {
	case server != "" && name != "":
		return server + "/" + name
	case server != "":
		return server + "/*"
	default:
		return name
	}
}

func hasMCPPermissionSummary(summary MCPPermissionSummary) bool {
	return strings.TrimSpace(summary.Mode) != "" ||
		summary.GrantCount > 0 ||
		summary.ServerGrants > 0 ||
		summary.ToolGrants > 0 ||
		summary.PromptCount > 0 ||
		summary.DeniedCount > 0 ||
		len(summary.Grants) > 0
}

func mcpPermissionLines(summary MCPPermissionSummary) []string {
	lines := []string{
		"mode: " + displayValue(strings.TrimSpace(summary.Mode), "unknown"),
		fmt.Sprintf("persistent grants: %d", summary.GrantCount),
		fmt.Sprintf("server grants: %d", summary.ServerGrants),
		fmt.Sprintf("tool grants: %d", summary.ToolGrants),
		fmt.Sprintf("prompted this session: %d", summary.PromptCount),
		fmt.Sprintf("denied this session: %d", summary.DeniedCount),
	}
	if len(summary.Grants) == 0 {
		return lines
	}
	for _, grant := range summary.Grants {
		target := displayValue(strings.TrimSpace(grant.Target), "unknown")
		autonomy := displayValue(strings.TrimSpace(grant.Autonomy), "low")
		line := fmt.Sprintf("%s [%s]", target, autonomy)
		if approvedAt := strings.TrimSpace(grant.ApprovedAt); approvedAt != "" {
			line += " approved " + approvedAt
		}
		lines = append(lines, commandBullet(line))
	}
	return lines
}

func mcpOAuthLines(servers []MCPOAuthServerView) []string {
	lines := make([]string, 0, len(servers))
	for _, server := range servers {
		name := displayValue(strings.TrimSpace(server.ServerName), "unnamed")
		parts := []string{name}
		if server.Configured {
			parts = append(parts, "configured")
			if server.HasToken {
				parts = append(parts, "token")
			} else {
				parts = append(parts, "no token")
			}
			if server.HasRefreshToken {
				parts = append(parts, "refresh")
			}
			if server.Expired {
				parts = append(parts, "expired")
			} else if !server.ExpiresAt.IsZero() {
				parts = append(parts, "expires", server.ExpiresAt.UTC().Format(time.RFC3339))
			}
			if tokenType := strings.TrimSpace(server.TokenType); tokenType != "" {
				parts = append(parts, tokenType)
			}
			if len(server.Scopes) > 0 {
				parts = append(parts, "scopes", strings.Join(server.Scopes, ","))
			}
		} else {
			parts = append(parts, "not configured")
		}
		lines = append(lines, commandBullet(strings.Join(parts, " ")))
		if actions := mcpOAuthActionLine(server); actions != "" {
			lines = append(lines, actions)
		}
	}
	return lines
}

func mcpServerActionLine(server MCPServerView) string {
	name := strings.TrimSpace(server.Name)
	if name == "" {
		return ""
	}
	actions := []string{"zero mcp check " + name}
	if strings.EqualFold(strings.TrimSpace(server.State), "disabled") {
		actions = append(actions, "zero mcp enable "+name)
	} else {
		actions = append(actions, "zero mcp disable "+name)
	}
	actions = append(actions, "zero mcp remove "+name)
	if strings.EqualFold(strings.TrimSpace(server.Auth), "oauth") {
		actions = append(actions, "zero mcp oauth login "+name)
	}
	return "    actions: " + strings.Join(actions, " | ")
}

func mcpOAuthActionLine(server MCPOAuthServerView) string {
	name := strings.TrimSpace(server.ServerName)
	if name == "" || !server.Configured {
		return ""
	}
	actions := []string{}
	if server.HasToken {
		actions = append(actions, "zero mcp oauth refresh "+name, "zero mcp oauth logout "+name)
	} else {
		actions = append(actions, "zero mcp oauth login "+name)
	}
	if server.Expired && !server.HasRefreshToken {
		actions = append(actions, "zero mcp oauth refresh "+name)
	}
	return "    actions: " + strings.Join(dedupeStrings(actions), " | ")
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func fitMCPManagerLines(lines []string, width int) string {
	if width <= 0 {
		return strings.Join(lines, "\n")
	}
	for index, line := range lines {
		lines[index] = fitStyledLine(line, width)
	}
	return strings.Join(lines, "\n")
}

func pluralCount(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
