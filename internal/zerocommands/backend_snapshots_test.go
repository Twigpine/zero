package zerocommands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/plugins"
)

func TestMCPServerSnapshotFromServerStripsSecretsAndCountsMaps(t *testing.T) {
	server := mcp.Server{
		Name:    "  work  ",
		Type:    mcp.ServerTypeStdio,
		Command: "npx",
		Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		Env: map[string]string{
			"MCP_AUTH_TOKEN": "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789",
			"LOG_LEVEL":      "info",
		},
		Headers: map[string]string{
			"Authorization": "Bearer sk-proj-zyxwvu",
		},
		Identity: "  work-fs  ",
	}

	snapshot := MCPServerSnapshotFromServer(server)

	if snapshot.Name != "work" {
		t.Fatalf("Name not trimmed: %q", snapshot.Name)
	}
	if snapshot.Identity != "" {
		t.Fatalf("Identity should not be exposed in operator snapshots, got %q", snapshot.Identity)
	}
	if snapshot.Command != "npx" {
		t.Fatalf("Command not trimmed: %q", snapshot.Command)
	}
	if snapshot.Type != "stdio" {
		t.Fatalf("Type = %q, want stdio", snapshot.Type)
	}
	if snapshot.ArgCount != 3 {
		t.Fatalf("ArgCount = %d, want 3", snapshot.ArgCount)
	}
	if snapshot.EnvKeyCount != 2 {
		t.Fatalf("EnvKeyCount = %d, want 2 (counts, not values)", snapshot.EnvKeyCount)
	}
	if snapshot.HeaderCount != 1 {
		t.Fatalf("HeaderCount = %d, want 1 (counts, not values)", snapshot.HeaderCount)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	serialized := string(encoded)
	if strings.Contains(serialized, "sk-proj-") {
		t.Fatalf("snapshot must not serialize secret material, got %q", serialized)
	}
	if strings.Contains(serialized, "Bearer") {
		t.Fatalf("snapshot must not serialize authorization header value, got %q", serialized)
	}
	if strings.Contains(serialized, "MCP_AUTH_TOKEN") {
		t.Fatalf("snapshot must not serialize env var name or value, got %q", serialized)
	}
}

func TestMCPServerSnapshotWithCountsMergesRuntimeCounts(t *testing.T) {
	server := mcp.Server{Name: "work", Type: mcp.ServerTypeStdio, Command: "npx"}
	base := MCPServerSnapshotFromServer(server)
	if base.ToolCount != 0 || base.AllowGranted != 0 || base.DenyGranted != 0 {
		t.Fatalf("expected zero counts on base snapshot, got %#v", base)
	}

	with := MCPServerSnapshotWithCounts(server, &MCPServerCounts{
		ToolCount:    4,
		AllowGranted: 3,
		DenyGranted:  1,
	})
	if with.ToolCount != 4 || with.AllowGranted != 3 || with.DenyGranted != 1 {
		t.Fatalf("expected counts merged, got %#v", with)
	}

	nilCounts := MCPServerSnapshotWithCounts(server, nil)
	if nilCounts.ToolCount != 0 || nilCounts.AllowGranted != 0 || nilCounts.DenyGranted != 0 {
		t.Fatalf("expected nil counts to leave snapshot zero, got %#v", nilCounts)
	}
}

func TestMCPServerSnapshotRedactsSecretCommand(t *testing.T) {
	// Digit-free known OpenAI prefix: exercises the special-case path that
	// redacts sk-proj- even when the body has no digit.
	secret := "sk-proj-" + strings.Repeat("d", 24)
	server := mcp.Server{
		Name:    "leaky",
		Type:    mcp.ServerTypeStdio,
		Command: "mcp-wrapper --token " + secret,
	}

	snapshot := MCPServerSnapshotFromServer(server)
	if strings.Contains(snapshot.Command, secret) || strings.Contains(snapshot.Command, "sk-proj-") {
		t.Fatalf("MCP command should be redacted, got %q", snapshot.Command)
	}
	if !strings.Contains(snapshot.Command, "[REDACTED]") {
		t.Fatalf("expected redaction marker in MCP command, got %q", snapshot.Command)
	}
}

func TestMCPServerSnapshotsSortsAndReturnsEmptySliceForEmptyInput(t *testing.T) {
	servers := []mcp.Server{
		{Name: "zulu", Type: mcp.ServerTypeHTTP, URL: "https://zulu.test"},
		{Name: "alpha", Type: mcp.ServerTypeStdio, Command: "alpha"},
		{Name: "mike", Type: mcp.ServerTypeSSE, URL: "https://mike.test"},
	}
	snapshots := MCPServerSnapshots(servers)
	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].Name != "alpha" || snapshots[1].Name != "mike" || snapshots[2].Name != "zulu" {
		t.Fatalf("snapshots not sorted by name: %#v", snapshots)
	}

	empty := MCPServerSnapshots(nil)
	if empty == nil {
		t.Fatal("expected non-nil empty slice so JSON output is [] not null")
	}
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("expected JSON [] for empty input, got %q", string(encoded))
	}
}

func TestHookSnapshotFromDefinitionRedactsCommandAndTrimsFields(t *testing.T) {
	def := hooks.Definition{
		ID:      "  hook-1  ",
		Name:    "  pre-tool safety  ",
		Event:   hooks.EventBeforeTool,
		Matcher: "  write_file  ",
		Command: "sh",
		Args:    []string{"-c", "auth token sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"},
		Enabled: true,
	}

	snapshot := HookSnapshotFromDefinition(def, hooks.SourceUser)

	if snapshot.ID != "hook-1" {
		t.Fatalf("ID not trimmed: %q", snapshot.ID)
	}
	if snapshot.Name != "pre-tool safety" {
		t.Fatalf("Name not trimmed: %q", snapshot.Name)
	}
	if snapshot.Matcher != "write_file" {
		t.Fatalf("Matcher not trimmed: %q", snapshot.Matcher)
	}
	if snapshot.Event != string(hooks.EventBeforeTool) {
		t.Fatalf("Event = %q, want %q", snapshot.Event, hooks.EventBeforeTool)
	}
	if snapshot.Source != string(hooks.SourceUser) {
		t.Fatalf("Source = %q, want %q", snapshot.Source, hooks.SourceUser)
	}
	if !snapshot.Enabled {
		t.Fatal("expected Enabled=true to round-trip")
	}
	if len(snapshot.Args) != len(def.Args) {
		t.Fatalf("expected %d args after redaction (position preserved), got %d", len(def.Args), len(snapshot.Args))
	}
	for _, arg := range snapshot.Args {
		if strings.Contains(arg, "sk-proj-") {
			t.Fatalf("hook arg should be redacted, got %q", arg)
		}
	}
}

func TestHookSnapshotFromDefinitionPreservesPositionForRedactedArgs(t *testing.T) {
	def := hooks.Definition{
		ID:      "hook-2",
		Event:   hooks.EventBeforeTool,
		Command: "sh",
		Args: []string{
			"-c",
			"safe arg",
			"sk-proj-abcdefghijklmnopqrstuvwxyz0123456789",
			"another safe arg",
		},
	}
	snapshot := HookSnapshotFromDefinition(def, hooks.SourceProject)
	if len(snapshot.Args) != 4 {
		t.Fatalf("expected 4 args after redaction, got %d", len(snapshot.Args))
	}
	if snapshot.Args[0] != "-c" || snapshot.Args[1] != "safe arg" {
		t.Fatalf("non-secret args should round-trip verbatim, got %#v", snapshot.Args[:2])
	}
	if strings.Contains(snapshot.Args[2], "sk-proj-") {
		t.Fatalf("third arg (secret) should be redacted, got %q", snapshot.Args[2])
	}
	if snapshot.Args[3] != "another safe arg" {
		t.Fatalf("fourth arg should be preserved, got %q", snapshot.Args[3])
	}
}

func TestHookSnapshotFromDefinitionRedactsSecretCommand(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("c", 24)
	def := hooks.Definition{
		ID:      "hook-secret-command",
		Event:   hooks.EventAfterTool,
		Command: "curl -H Authorization:Bearer " + secret,
		Enabled: true,
	}

	snapshot := HookSnapshotFromDefinition(def, hooks.SourceUser)
	if strings.Contains(snapshot.Command, secret) || strings.Contains(snapshot.Command, "sk-proj-") {
		t.Fatalf("hook command should be redacted, got %q", snapshot.Command)
	}
	if !strings.Contains(snapshot.Command, "[REDACTED]") {
		t.Fatalf("expected redaction marker in hook command, got %q", snapshot.Command)
	}
}

func TestMCPServerSnapshotStripsURLCredentials(t *testing.T) {
	server := mcp.Server{
		Name: "creds",
		Type: mcp.ServerTypeHTTP,
		URL:  "https://admin:secret123@api.example.com/v1",
	}
	snapshot := MCPServerSnapshotFromServer(server)
	if strings.Contains(snapshot.URL, "admin") {
		t.Fatalf("URL must not contain username, got %q", snapshot.URL)
	}
	if strings.Contains(snapshot.URL, "secret123") {
		t.Fatalf("URL must not contain password, got %q", snapshot.URL)
	}
	if strings.Contains(snapshot.URL, "@") {
		t.Fatalf("URL must not contain userinfo separator, got %q", snapshot.URL)
	}
	if snapshot.URL != "https://api.example.com/v1" {
		t.Fatalf("expected sanitized URL, got %q", snapshot.URL)
	}
}

func TestMCPServerSnapshotRedactsSensitiveURLQueryValues(t *testing.T) {
	server := mcp.Server{
		Name: "query",
		Type: mcp.ServerTypeHTTP,
		URL:  "https://api.example.com/v1?token=secret-token&mode=readonly&api_key=sk-proj-" + strings.Repeat("b", 24),
	}
	snapshot := MCPServerSnapshotFromServer(server)
	if strings.Contains(snapshot.URL, "secret-token") || strings.Contains(snapshot.URL, "sk-proj-") {
		t.Fatalf("URL query must not contain secret values, got %q", snapshot.URL)
	}
	if !strings.Contains(snapshot.URL, "token=[REDACTED]") || !strings.Contains(snapshot.URL, "api_key=[REDACTED]") {
		t.Fatalf("expected sensitive query values to be redacted, got %q", snapshot.URL)
	}
	if !strings.Contains(snapshot.URL, "mode=readonly") {
		t.Fatalf("expected safe query value to be preserved, got %q", snapshot.URL)
	}
}

func TestMCPServerSnapshotPreservesURLWithoutCredentials(t *testing.T) {
	server := mcp.Server{
		Name: "clean",
		Type: mcp.ServerTypeHTTP,
		URL:  "https://api.example.com/v1",
	}
	snapshot := MCPServerSnapshotFromServer(server)
	if snapshot.URL != "https://api.example.com/v1" {
		t.Fatalf("URL without credentials should be preserved, got %q", snapshot.URL)
	}
}

func TestMCPServerSnapshotKeepsUnparseableURLInsteadOfEmpty(t *testing.T) {
	server := mcp.Server{
		Name: "broken",
		Type: mcp.ServerTypeHTTP,
		URL:  "  not a url but still useful  ",
	}
	snapshot := MCPServerSnapshotFromServer(server)
	if snapshot.URL == "" {
		t.Fatal("unparseable URL should be kept (trimmed) so the operator sees the configured endpoint")
	}
	if snapshot.URL != "not a url but still useful" {
		t.Fatalf("expected trimmed raw URL, got %q", snapshot.URL)
	}
}

func TestMCPServerSnapshotRedactsMalformedURLSecretText(t *testing.T) {
	server := mcp.Server{
		Name: "broken-secret",
		Type: mcp.ServerTypeHTTP,
		URL:  "  not a url token=plain-backend-secret  ",
	}
	snapshot := MCPServerSnapshotFromServer(server)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if strings.Contains(string(encoded), "plain-backend-secret") {
		t.Fatalf("malformed URL secret should be redacted, got %s", string(encoded))
	}
}

func TestMCPServerSnapshotOmitsSecretDerivedIdentity(t *testing.T) {
	first := MCPServerSnapshotFromServer(mcp.Server{
		Name:     "same",
		Type:     mcp.ServerTypeStdio,
		Command:  "npx",
		Identity: "identity-derived-from-secret-one",
	})
	second := MCPServerSnapshotFromServer(mcp.Server{
		Name:     "same",
		Type:     mcp.ServerTypeStdio,
		Command:  "npx",
		Identity: "identity-derived-from-secret-two",
	})
	if first.Identity != "" || second.Identity != "" {
		t.Fatalf("operator snapshots must omit secret-derived identities, got %q and %q", first.Identity, second.Identity)
	}
	if first != second {
		t.Fatalf("snapshots should not differ only because secret-derived identity differs: %#v %#v", first, second)
	}
}

func TestHookSnapshotsSortsByIDAndEvent(t *testing.T) {
	defs := []hooks.Definition{
		{ID: "zulu", Event: hooks.EventAfterTool, Command: "echo", Args: []string{"z"}},
		{ID: "alpha", Event: hooks.EventSessionStart, Command: "echo", Args: []string{"a"}},
		{ID: "alpha", Event: hooks.EventBeforeTool, Command: "echo", Args: []string{"a"}},
	}
	snapshots := HookSnapshots(defs)
	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].ID != "alpha" || snapshots[0].Event != string(hooks.EventBeforeTool) {
		t.Fatalf("expected alpha/beforeTool first, got %q/%q", snapshots[0].ID, snapshots[0].Event)
	}
	if snapshots[1].ID != "alpha" || snapshots[1].Event != string(hooks.EventSessionStart) {
		t.Fatalf("expected alpha/sessionStart second, got %q/%q", snapshots[1].ID, snapshots[1].Event)
	}
	if snapshots[2].ID != "zulu" {
		t.Fatalf("expected zulu third, got %q", snapshots[2].ID)
	}
}

func TestHookSnapshotsWithSourceTagsEverySnapshot(t *testing.T) {
	defs := []hooks.Definition{
		{ID: "h1", Event: hooks.EventSessionStart, Command: "echo", Args: []string{"hi"}},
		{ID: "h2", Event: hooks.EventSessionEnd, Command: "echo", Args: []string{"bye"}},
	}
	snapshots := HookSnapshotsWithSource(defs, hooks.SourceProject)
	for _, snap := range snapshots {
		if snap.Source != string(hooks.SourceProject) {
			t.Fatalf("expected project source, got %q on %q", snap.Source, snap.ID)
		}
	}
}

func TestPluginSnapshotFromPluginCollapsesSlicesToCounts(t *testing.T) {
	plugin := plugins.LoadedPlugin{
		ID:           "  example  ",
		Name:         "  Example Plugin  ",
		Version:      "  1.2.3  ",
		Description:  "  A demo plugin.  ",
		Enabled:      true,
		Source:       plugins.SourceCustom,
		Root:         "  /home/user/.config/zero/plugins/example  ",
		PluginDir:    "  /home/user/.config/zero/plugins/example/v1  ",
		ManifestPath: "  /home/user/.config/zero/plugins/example/v1/manifest.json  ",
		Tools: []plugins.ToolExtension{
			{Name: "t1", Command: "echo", Args: []string{"t1"}},
			{Name: "t2", Command: "echo", Args: []string{"t2"}},
		},
		Prompts: []plugins.PathExtension{{Name: "p1", Path: "/p1"}},
		Skills:  []plugins.PathExtension{{Name: "s1", Path: "/s1"}},
		Hooks: []plugins.HookExtension{
			{Name: "h1", Event: plugins.HookBeforeTool, Command: "echo", Args: []string{"h1"}},
		},
	}

	snapshot := PluginSnapshotFromPlugin(plugin)

	if snapshot.ID != "example" {
		t.Fatalf("ID not trimmed: %q", snapshot.ID)
	}
	if snapshot.Name != "Example Plugin" {
		t.Fatalf("Name not trimmed: %q", snapshot.Name)
	}
	if snapshot.Version != "1.2.3" {
		t.Fatalf("Version not trimmed: %q", snapshot.Version)
	}
	if snapshot.Description != "A demo plugin." {
		t.Fatalf("Description not trimmed: %q", snapshot.Description)
	}
	if snapshot.Source != string(plugins.SourceCustom) {
		t.Fatalf("Source = %q, want %q", snapshot.Source, plugins.SourceCustom)
	}
	if snapshot.ToolCount != 2 || snapshot.PromptCount != 1 || snapshot.SkillCount != 1 || snapshot.HookCount != 1 {
		t.Fatalf("counts wrong: %#v", snapshot)
	}
}

func TestPluginSnapshotRedactsOperatorFacingStrings(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("e", 24)
	plugin := plugins.LoadedPlugin{
		ID:           "plugin-" + secret,
		Name:         "Docs " + secret,
		Version:      "1.0.0",
		Description:  "requires " + secret,
		Enabled:      true,
		Source:       plugins.SourceUser,
		Root:         "/tmp/" + secret,
		PluginDir:    "/tmp/plugin?api_key=" + secret,
		ManifestPath: "/tmp/plugin/plugin.json?token=" + secret,
	}

	snapshot := PluginSnapshotFromPlugin(plugin)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "sk-proj-") {
		t.Fatalf("plugin snapshot should redact operator-facing strings, got %s", string(encoded))
	}
	if !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("expected redaction marker in plugin snapshot, got %s", string(encoded))
	}
}

func TestPluginSnapshotsSortsByIDAndReturnsEmptySliceForEmptyInput(t *testing.T) {
	loaded := []plugins.LoadedPlugin{
		{ID: "zulu", Name: "Zulu", Tools: []plugins.ToolExtension{{Name: "t"}}},
		{ID: "alpha", Name: "Alpha"},
	}
	snapshots := PluginSnapshots(loaded)
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].ID != "alpha" || snapshots[1].ID != "zulu" {
		t.Fatalf("snapshots not sorted by id: %#v", snapshots)
	}

	empty := PluginSnapshots(nil)
	if empty == nil {
		t.Fatal("expected non-nil empty slice so JSON output is [] not null")
	}
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("expected JSON [] for empty input, got %q", string(encoded))
	}
}

func TestNewBackendLifecycleSnapshotBundlesAndDefaultsNilsToEmpty(t *testing.T) {
	snapshot := NewBackendLifecycleSnapshot(nil, nil, nil)
	if snapshot.MCPServers == nil || snapshot.Hooks == nil || snapshot.Plugins == nil {
		t.Fatalf("expected all three slices non-nil, got %#v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if !strings.Contains(string(encoded), `"mcpServers":[]`) {
		t.Fatalf("expected empty mcpServers in JSON, got %q", string(encoded))
	}
	if !strings.Contains(string(encoded), `"hooks":[]`) {
		t.Fatalf("expected empty hooks in JSON, got %q", string(encoded))
	}
	if !strings.Contains(string(encoded), `"plugins":[]`) {
		t.Fatalf("expected empty plugins in JSON, got %q", string(encoded))
	}

	full := NewBackendLifecycleSnapshot(
		[]mcp.Server{{Name: "alpha", Type: mcp.ServerTypeStdio, Command: "x"}},
		[]hooks.Definition{{ID: "h1", Event: hooks.EventBeforeTool, Command: "echo"}},
		[]plugins.LoadedPlugin{{ID: "p1", Name: "P"}},
	)
	if len(full.MCPServers) != 1 || len(full.Hooks) != 1 || len(full.Plugins) != 1 {
		t.Fatalf("expected one of each, got %#v", full)
	}
}
