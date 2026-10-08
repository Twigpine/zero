package config

import (
	"reflect"
	"strings"
)

// DefaultMCPServers returns the MCP servers Zero ships ENABLED by default so
// web search and page fetching work out of the box with no setup and no API
// key. They are seeded before user/project config is merged (see ResolveMCP),
// so a user can override any field — for example add an API-key header to lift
// Exa's anonymous rate limit — or disable it entirely with
// `zero mcp disable <name>` (which writes `"disabled": true`).
//
// Exa's hosted MCP server works anonymously with rate limits. Users can add an
// Exa API key for higher limits.
func DefaultMCPServers() map[string]MCPServerConfig {
	return map[string]MCPServerConfig{
		"exa": {
			Type: "http",
			URL:  "https://mcp.exa.ai/mcp",
		},
	}
}

// IsDefaultMCPServer reports whether name is one of Zero's built-in default MCP
// servers. The config commands use it so a default can be disabled/enabled even
// though it is not written to the user's config file until overridden.
func IsDefaultMCPServer(name string) bool {
	_, ok := DefaultMCPServers()[strings.TrimSpace(name)]
	return ok
}

// IsUnconfiguredDefault reports whether server is one of Zero's built-in
// defaults that the user never wrote an entry for in their config — i.e. it is
// running with whatever Zero ships (e.g. keyless Exa, no credentials).
//
// Both conditions below must hold:
//   - !server.configured: the user's JSON never declared an object for this
//     server key at all (set by MCPServerConfig.UnmarshalJSON only when it
//     actually ran for this key). Any explicit action — including a
//     disable/enable toggle like `zero mcp enable exa` that leaves the
//     resolved value unchanged — sets configured, so it always counts as
//     user-configured, even though the value comparison below could not tell
//     the difference on its own.
//   - reflect.DeepEqual(def, server): the value still matches the default.
//     This is the fallback for callers that construct MCPServerConfig
//     directly rather than through the JSON/merge pipeline (server.configured
//     is then always false) — without it, any hand-built config with
//     different field values would be misreported as unconfigured.
//
// Callers use this to tell "server we turned on for the user" apart from
// "server the user configured themselves," e.g. to avoid warning loudly when
// an out-of-the-box default that was never given credentials fails to connect.
func IsUnconfiguredDefault(name string, server MCPServerConfig) bool {
	def, ok := DefaultMCPServers()[strings.TrimSpace(name)]
	return ok && !server.configured && reflect.DeepEqual(def, server)
}

// retiredDefaultMCPServer describes a built-in default Zero no longer ships.
type retiredDefaultMCPServer struct {
	// successor is the default that replaced it.
	successor string
	// shipped is the value Zero used to seed for this name. A user entry
	// written while the default was still shipped may name only an override
	// (a header, say) and rely on these fields for the rest, so they have to
	// outlive the default itself.
	shipped MCPServerConfig
}

// retiredDefaultMCPServers maps a retired built-in default to what replaced it
// and to the value Zero used to ship for it.
var retiredDefaultMCPServers = map[string]retiredDefaultMCPServer{
	"firecrawl": {
		successor: "exa",
		shipped: MCPServerConfig{
			Type: "http",
			URL:  "https://mcp.firecrawl.dev/v2/mcp",
		},
	},
}

// migrateRetiredDefaultMCPServers keeps an upgrade across a default-provider
// swap from changing what the user's config means. It handles the two ways a
// rename can break a config written against the old default:
//
//   - A user who ran `zero mcp disable <old default>` made an explicit choice
//     not to open that outbound connection. Without carrying that decision to
//     the successor, the upgrade re-opens it under the new name — and because
//     the replacement looks like an untouched default, even the startup
//     warning stays quiet (see IsUnconfiguredDefault and issue #552).
//   - A user who customized the old default could name only the field they
//     were changing, because the seeded default supplied the transport. Once
//     the default stops being seeded that entry resolves to a server with no
//     type, url, or command, which fails NormalizeConfig and takes the whole
//     startup down rather than just that one server.
//
// It runs immediately after the user layer merges (see ResolveMCP), which
// scopes it to user-level decisions — the only scope whose disable is sticky.
// The carried-over disable applies only when the user never declared the
// successor themselves: an explicit `exa` entry wins whether it enables or
// disables. Because the disable is recorded as a user-level decision, the
// lower-trust project layer cannot lift it, while `zero mcp enable exa` still
// can (the CLI override scope merges with canReenable=true).
func migrateRetiredDefaultMCPServers(cfg *MCPConfig) {
	for name, retired := range retiredDefaultMCPServers {
		entry, ok := cfg.Servers[name]
		if !ok {
			continue
		}
		if inheritsRetiredTransport(entry) {
			// Re-seed what Zero used to ship underneath the user's fields, so a
			// partial entry still resolves to the complete server it named when
			// it was written. Their own values still win — this only fills the
			// gap the retired default used to cover.
			entry = mergeMCPServer(retired.shipped, entry, true)
			cfg.Servers[name] = entry
		}
		if !entry.disabledSet || !entry.Disabled {
			continue
		}
		replacement, ok := cfg.Servers[retired.successor]
		if !ok || replacement.configured {
			continue
		}
		replacement.Disabled = true
		replacement.disabledSet = true
		cfg.Servers[retired.successor] = replacement
	}
}

// inheritsRetiredTransport reports whether entry named no transport of its own
// — no type, url, command, or args — and so relied entirely on the retired
// default to supply one.
//
// Anything else is a complete definition the user owns (a self-hosted url, a
// stdio command) and must be left alone: grafting the retired default's http
// type onto an entry that names a command produces a server that is rejected
// outright, turning a working config into a startup failure.
func inheritsRetiredTransport(entry MCPServerConfig) bool {
	return strings.TrimSpace(entry.Type) == "" &&
		strings.TrimSpace(entry.URL) == "" &&
		strings.TrimSpace(entry.Command) == "" &&
		len(entry.Args) == 0
}
