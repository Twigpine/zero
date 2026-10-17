package zerocommands

import (
	"net/url"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/redaction"
)

// MCPServerSnapshot is the typed view of a single configured MCP
// server as it is exposed to the TUI render path, the headless
// `zero mcp` command, and PR/CI automation. The snapshot strips
// every field that can carry a secret. Command arguments are
// preserved because the tool surface (path, flags) is the part a
// maintainer needs to see when triaging an MCP failure. Environment
// variables and HTTP headers are summarised as redacted key counts
// instead of being copied verbatim, so a token in MCP_AUTH_TOKEN
// never reaches the headless JSON output.
type MCPServerSnapshot struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Identity     string `json:"identity,omitempty"`
	URL          string `json:"url,omitempty"`
	Command      string `json:"command,omitempty"`
	ArgCount     int    `json:"argCount"`
	EnvKeyCount  int    `json:"envKeyCount"`
	HeaderCount  int    `json:"headerCount"`
	ToolCount    int    `json:"toolCount"`
	AllowGranted int    `json:"allowGranted"`
	DenyGranted  int    `json:"denyGranted"`
}

// HookSnapshot is the typed view of a single configured hook as it
// is exposed to the TUI render path, the headless `zero hooks`
// command, and PR/CI automation. The command and arguments are
// preserved because they are the operator's primary tool for
// understanding which shell command will run. The matcher is
// preserved because it is the operator's primary tool for
// understanding when the hook will fire.
type HookSnapshot struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Event   string   `json:"event"`
	Matcher string   `json:"matcher,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Enabled bool     `json:"enabled"`
	Source  string   `json:"source,omitempty"`
}

// PluginSnapshot is the typed view of a single loaded plugin as it
// is exposed to the TUI render path, the headless `zero plugins`
// command, and PR/CI automation. The path fields are preserved
// because the operator needs to know where the manifest came from
// when triaging a load failure. Counts replace the full slice of
// tools, hooks, prompts, and skills so the headless JSON output
// stays small even for plugins with many extensions.
type PluginSnapshot struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version,omitempty"`
	Description  string `json:"description,omitempty"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source"`
	Root         string `json:"root,omitempty"`
	PluginDir    string `json:"pluginDir,omitempty"`
	ManifestPath string `json:"manifestPath,omitempty"`
	ToolCount    int    `json:"toolCount"`
	PromptCount  int    `json:"promptCount"`
	SkillCount   int    `json:"skillCount"`
	HookCount    int    `json:"hookCount"`
}

// BackendLifecycleSnapshot bundles the typed snapshots for the
// three backend surfaces that the WorkSplit PRD places in Gnanam's
// lane: MCP servers, hooks, and plugins. `zero doctor` and the
// headless `zero config` command can return one of these to give
// the operator a single payload describing the full extensibility
// surface. The three inner slices are always non-nil so JSON
// output is `[]` and never `null`.
type BackendLifecycleSnapshot struct {
	MCPServers []MCPServerSnapshot `json:"mcpServers"`
	Hooks      []HookSnapshot      `json:"hooks"`
	Plugins    []PluginSnapshot    `json:"plugins"`
}

// MCPServerSnapshotFromServer converts a single mcp.Server into its
// typed snapshot. Secret material in `Env` and `Headers` is never
// copied; only the key counts are recorded. The URL field is run
// through stripURLCredentialsFromURL so userinfo and sensitive
// query values are removed before the snapshot leaves the runtime.
// A URL that fails to parse is returned trimmed, not empty, so the
// operator still sees the configured endpoint when triaging a
// malformed MCP configuration.
func MCPServerSnapshotFromServer(server mcp.Server) MCPServerSnapshot {
	return MCPServerSnapshot{
		Name:        strings.TrimSpace(server.Name),
		Type:        string(server.Type),
		URL:         stripURLCredentialsFromURL(server.URL),
		Command:     redactSnapshotString(server.Command),
		ArgCount:    len(server.Args),
		EnvKeyCount: len(server.Env),
		HeaderCount: len(server.Headers),
	}
}

// MCPServerSnapshots converts a slice of mcp.Server into a stable,
// sorted slice of typed snapshots. Output is sorted alphabetically
// by Name so consumers (TUI, headless, JSON output) see a
// deterministic ordering. An empty input returns a non-nil empty
// slice so JSON output is always `[]` and never `null`.
func MCPServerSnapshots(servers []mcp.Server) []MCPServerSnapshot {
	snapshots := make([]MCPServerSnapshot, 0, len(servers))
	for _, server := range servers {
		snapshots = append(snapshots, MCPServerSnapshotFromServer(server))
	}
	sort.SliceStable(snapshots, func(left, right int) bool {
		return snapshots[left].Name < snapshots[right].Name
	})
	return snapshots
}

// MCPServerCounts is the optional runtime count bundle that the
// snapshot can carry. The struct is split out so callers that
// have a live tool registry and a live permission store can
// supply the values without the snapshot helper needing to know
// about either backend.
type MCPServerCounts struct {
	ToolCount    int
	AllowGranted int
	DenyGranted  int
}

// HookSnapshotFromDefinition converts a hooks.Definition into its
// typed snapshot. The source is the only field that is not on the
// definition itself; callers that have it from hooks.LoadResult
// should pass it through. An empty source stays empty in the
// snapshot.
func HookSnapshotFromDefinition(def hooks.Definition, source hooks.ConfigSource) HookSnapshot {
	return HookSnapshot{
		ID:      redactSnapshotString(def.ID),
		Name:    redactSnapshotString(def.Name),
		Event:   string(def.Event),
		Matcher: redactSnapshotString(def.Matcher),
		Command: redactSnapshotString(def.Command),
		Args:    redactStringSlice(def.Args),
		Enabled: def.Enabled,
		Source:  string(source),
	}
}

// HookSnapshots converts a slice of hooks.Definition into a
// stable, sorted slice of typed snapshots. Output is sorted
// alphabetically by ID, then by event, so consumers see a
// deterministic ordering. An empty input returns a non-nil empty
// slice so JSON output is always `[]` and never `null`.
func HookSnapshots(definitions []hooks.Definition) []HookSnapshot {
	return hookSnapshotsWithSource(definitions, "")
}

func hookSnapshotsWithSource(definitions []hooks.Definition, source hooks.ConfigSource) []HookSnapshot {
	snapshots := make([]HookSnapshot, 0, len(definitions))
	for _, def := range definitions {
		snapshots = append(snapshots, HookSnapshotFromDefinition(def, source))
	}
	sort.SliceStable(snapshots, func(left, right int) bool {
		if snapshots[left].ID != snapshots[right].ID {
			return snapshots[left].ID < snapshots[right].ID
		}
		return snapshots[left].Event < snapshots[right].Event
	})
	return snapshots
}

// PluginSnapshotFromPlugin converts a plugins.LoadedPlugin into its
// typed snapshot. The full slice of tools, hooks, prompts, and
// skills is collapsed to counts so the headless JSON output stays
// small. Operator-facing path and metadata fields are preserved
// after trimming and redaction because they help triage a load
// failure but may include copied token-like strings.
func PluginSnapshotFromPlugin(plugin plugins.LoadedPlugin) PluginSnapshot {
	return PluginSnapshot{
		ID:           redactSnapshotString(plugin.ID),
		Name:         redactSnapshotString(plugin.Name),
		Version:      redactSnapshotString(plugin.Version),
		Description:  redactSnapshotString(plugin.Description),
		Enabled:      plugin.Enabled,
		Source:       string(plugin.Source),
		Root:         redactSnapshotString(plugin.Root),
		PluginDir:    redactSnapshotString(plugin.PluginDir),
		ManifestPath: redactSnapshotString(plugin.ManifestPath),
		ToolCount:    len(plugin.Tools),
		PromptCount:  len(plugin.Prompts),
		SkillCount:   len(plugin.Skills),
		HookCount:    len(plugin.Hooks),
	}
}

// PluginSnapshots converts a slice of plugins.LoadedPlugin into a
// stable, sorted slice of typed snapshots. Output is sorted
// alphabetically by ID so consumers (TUI, headless, JSON output)
// see a deterministic ordering. An empty input returns a non-nil
// empty slice so JSON output is always `[]` and never `null`.
func PluginSnapshots(loaded []plugins.LoadedPlugin) []PluginSnapshot {
	snapshots := make([]PluginSnapshot, 0, len(loaded))
	for _, plugin := range loaded {
		snapshots = append(snapshots, PluginSnapshotFromPlugin(plugin))
	}
	sort.SliceStable(snapshots, func(left, right int) bool {
		return snapshots[left].ID < snapshots[right].ID
	})
	return snapshots
}

// NewBackendLifecycleSnapshot builds the bundled snapshot. Each
// argument may be nil; the snapshot slice stays non-nil for each
// surface so JSON output is always `[]` and never `null`. The
// caller is expected to have already loaded the data using the
// existing mcp.NormalizeConfig, hooks.Load, and plugins.Load
// helpers.
func NewBackendLifecycleSnapshot(servers []mcp.Server, hookDefs []hooks.Definition, loaded []plugins.LoadedPlugin) BackendLifecycleSnapshot {
	return BackendLifecycleSnapshot{
		MCPServers: MCPServerSnapshots(servers),
		Hooks:      HookSnapshots(hookDefs),
		Plugins:    PluginSnapshots(loaded),
	}
}

// redactStringSlice runs every element of the input slice through
// the standard redaction pipeline and trims surrounding whitespace.
// Position is preserved: a fully-redacted element becomes an empty
// string in the output, and a whitespace-only input becomes an
// empty string. The output length always matches the input length
// so consumers can rely on arg position when correlating with the
// source definition. A nil or empty input returns nil so the JSON
// output omits the field entirely.
func redactStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	for index, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			out[index] = ""
			continue
		}
		out[index] = redactSnapshotString(trimmed)
	}
	return out
}

func redactSnapshotString(value string) string {
	return redaction.RedactString(strings.TrimSpace(value), redaction.Options{})
}

// stripURLCredentialsFromURL returns value with any embedded
// userinfo (https://user:token@host) and sensitive query or
// fragment values removed. The helper is
// tolerant of malformed input: a URL that fails to parse is
// returned trimmed, not empty, so the operator still sees the
// configured endpoint when triaging an MCP configuration that
// contains an unparseable URL. A blank input returns blank.
func stripURLCredentialsFromURL(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil {
		return redactSnapshotString(trimmed)
	}
	if parsed.Scheme == "" && parsed.Host == "" {
		return redactSnapshotString(trimmed)
	}
	if parsed.User == nil {
		return redactSensitiveURLQuery(parsed, trimmed)
	}
	parsed.User = nil
	return redactSensitiveURLQuery(parsed, trimmed)
}

func redactSensitiveURLQuery(parsed *url.URL, fallback string) string {
	if parsed.RawQuery != "" {
		parsed.RawQuery = redactSensitiveRawQuery(parsed.RawQuery)
	}
	if parsed.Fragment != "" {
		parsed.Fragment = redactSensitiveRawQuery(parsed.Fragment)
	}
	out := parsed.String()
	if strings.TrimSpace(out) == "" {
		return fallback
	}
	return out
}

func redactSensitiveRawQuery(rawQuery string) string {
	parts := strings.Split(rawQuery, "&")
	for index, part := range parts {
		if part == "" {
			continue
		}
		key, _, hasValue := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		if !isSensitiveURLKey(decodedKey) || !hasValue {
			continue
		}
		parts[index] = key + "=[REDACTED]"
	}
	return strings.Join(parts, "&")
}

func isSensitiveURLKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	if key == "key" {
		return true
	}
	for _, token := range []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "auth", "credential"} {
		if strings.Contains(key, token) {
			return true
		}
	}
	return false
}
