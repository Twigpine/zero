package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsDefaultMCPServer(t *testing.T) {
	if !IsDefaultMCPServer("exa") {
		t.Fatal("exa should be a built-in default")
	}
	if IsDefaultMCPServer("  exa  ") == false {
		t.Fatal("IsDefaultMCPServer should trim whitespace")
	}
	if IsDefaultMCPServer("not-a-default") {
		t.Fatal("unknown server should not be a default")
	}
}

func TestResolveMCPSeedsEnabledExaDefault(t *testing.T) {
	cfg, err := ResolveMCP(ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	exa, ok := cfg.Servers["exa"]
	if !ok {
		t.Fatal("expected the exa default to be seeded with no user config")
	}
	if exa.Type != "http" || exa.URL != "https://mcp.exa.ai/mcp" {
		t.Fatalf("unexpected exa default: %#v", exa)
	}
	if exa.Disabled {
		t.Fatal("the exa default must be enabled out of the box")
	}
}

func TestResolveMCPUserCanDisableDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"exa":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !cfg.Servers["exa"].Disabled {
		t.Fatal("a user must be able to disable the default by writing over it")
	}
}

func TestIsUnconfiguredDefault(t *testing.T) {
	if !IsUnconfiguredDefault("exa", DefaultMCPServers()["exa"]) {
		t.Fatal("an untouched exa default should be reported as unconfigured")
	}
	if IsUnconfiguredDefault("exa", MCPServerConfig{Type: "http", URL: "https://example.com/mcp"}) {
		t.Fatal("a server overriding the default URL is no longer unconfigured")
	}
	if IsUnconfiguredDefault("exa", MCPServerConfig{Type: "http", URL: "https://mcp.exa.ai/mcp", Auth: "bearer"}) {
		t.Fatal("a server with credentials added is no longer unconfigured")
	}
	if IsUnconfiguredDefault("not-a-default", MCPServerConfig{}) {
		t.Fatal("a server with no matching default can never be unconfigured-default")
	}
}

func TestResolveMCPExplicitReenableIsNotUnconfiguredDefault(t *testing.T) {
	// `zero mcp enable exa` after a prior disable writes {"disabled":false}
	// explicitly. The resolved value is identical to the untouched default (both
	// enabled, no credentials), but the user DID take an explicit action here, so
	// IsUnconfiguredDefault must not treat it as untouched (issue #563 review).
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"exa":{"disabled":false}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	exa := cfg.Servers["exa"]
	if exa.Disabled {
		t.Fatalf("explicit re-enable should leave the server enabled: %#v", exa)
	}
	if IsUnconfiguredDefault("exa", exa) {
		t.Fatal("an explicit enable/disable toggle must count as user-configured, even though the resolved value matches the default")
	}
}

func TestResolveMCPExplicitRedeclareOfDefaultValuesIsNotUnconfiguredDefault(t *testing.T) {
	// A user who copies Exa's exact default type/url into their config
	// (e.g. from an example file, planning to add credentials later) produces a
	// resolved value byte-identical to DefaultMCPServers()["exa"] — the
	// same trap TestResolveMCPExplicitReenableIsNotUnconfiguredDefault covers for
	// the disabled toggle. IsUnconfiguredDefault must still treat this as
	// user-configured because the user's JSON declared an entry for it, even
	// though a plain resolved-value comparison could not tell the difference.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"exa":{"type":"http","url":"https://mcp.exa.ai/mcp"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	exa := cfg.Servers["exa"]
	want := DefaultMCPServers()["exa"]
	if exa.Type != want.Type || exa.URL != want.URL || exa.Disabled != want.Disabled {
		t.Fatalf("expected the resolved value to match the default's fields exactly: %#v", exa)
	}
	if IsUnconfiguredDefault("exa", exa) {
		t.Fatal("redeclaring the default's exact values is still an explicit user configuration, not an untouched default")
	}
}

func TestResolveMCPUserCanOverrideDefaultURLKeepingOtherFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// Point Exa at a proxy; the default's Type must survive.
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"exa":{"url":"https://example.com/mcp"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	exa := cfg.Servers["exa"]
	if exa.URL != "https://example.com/mcp" {
		t.Fatalf("user override of the default URL did not apply: %#v", exa)
	}
	if exa.Type != "http" {
		t.Fatalf("override should keep the default's other fields (type), got %#v", exa)
	}
}

func TestResolveMCPCarriesLegacyDefaultDisableToSuccessor(t *testing.T) {
	// Upgrade path: the user disabled the firecrawl default Zero used to ship.
	// Swapping the default to exa must not re-open an outbound connection they
	// explicitly switched off.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !cfg.Servers["exa"].Disabled {
		t.Fatalf("a prior disable of the retired default must carry to its replacement: %#v", cfg.Servers["exa"])
	}
}

func TestResolveMCPExplicitSuccessorBeatsLegacyDefaultDisable(t *testing.T) {
	// The user disabled the old default but has since explicitly configured exa.
	// Their newer, explicit choice wins — the migration must not override it.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"disabled":true},"exa":{"disabled":false}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	exa := cfg.Servers["exa"]
	if exa.Disabled {
		t.Fatalf("an explicit exa entry must survive the legacy-disable migration: %#v", exa)
	}
	if IsUnconfiguredDefault("exa", exa) {
		t.Fatal("an explicitly configured exa is not an untouched default")
	}
}

func TestResolveMCPLegacyDefaultLeftEnabledDoesNotDisableSuccessor(t *testing.T) {
	// A user who configured firecrawl without disabling it (e.g. added a header)
	// never opted out of the default search server, so exa stays enabled.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"headers":{"Authorization":"Bearer k"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if cfg.Servers["exa"].Disabled {
		t.Fatal("only an explicit disable of the retired default should carry over")
	}
}

func TestResolveMCPLegacyDefaultDisableIsLiftableByOverride(t *testing.T) {
	// The carried-over disable is a user-level decision, not a permanent one:
	// `zero mcp enable exa` merges through the CLI override scope, which is the
	// one layer allowed to re-enable.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := MCPServerConfig{Disabled: false, disabledSet: true}
	cfg, err := ResolveMCP(ResolveOptions{
		UserConfigPath: path,
		Overrides:      Overrides{MCP: MCPConfig{Servers: map[string]MCPServerConfig{"exa": enabled}}},
	})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if cfg.Servers["exa"].Disabled {
		t.Fatalf("an explicit enable must lift the carried-over disable: %#v", cfg.Servers["exa"])
	}
}

func TestResolveMCPLegacyDisableSurvivesProjectReenable(t *testing.T) {
	// The project layer is lower-trust and must not lift the carried-over
	// user-level disable — the same guard a direct `zero mcp disable exa` gets.
	dir := t.TempDir()
	userPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(userPath, []byte(`{"mcp":{"servers":{"firecrawl":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(dir, "project.json")
	if err := os.WriteFile(projectPath, []byte(`{"mcp":{"servers":{"exa":{"disabled":false}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: userPath, ProjectConfigPath: projectPath})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !cfg.Servers["exa"].Disabled {
		t.Fatal("project config must not re-enable a server the user disabled")
	}
}

func TestResolveMCPKeepsRetiredDefaultTransportForPartialEntry(t *testing.T) {
	// A user who added an API key to the firecrawl default wrote only the header
	// — the seeded default supplied the http type and URL. Retiring the default
	// must not strip those out from under them and leave an unusable server.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"headers":{"Authorization":"Bearer k"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	firecrawl := cfg.Servers["firecrawl"]
	if firecrawl.Type != "http" || firecrawl.URL != "https://mcp.firecrawl.dev/v2/mcp" {
		t.Fatalf("a partial entry must keep the retired default's transport: %#v", firecrawl)
	}
	if firecrawl.Headers["Authorization"] != "Bearer k" {
		t.Fatalf("the user's own field must still win: %#v", firecrawl)
	}
}

func TestResolveMCPDoesNotGraftRetiredDefaultOntoOwnTransport(t *testing.T) {
	// A self-hosted firecrawl over stdio is a complete definition the user owns.
	// Re-seeding the retired http default under it would produce an http server
	// carrying a command, which NormalizeConfig rejects outright.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"command":"firecrawl-mcp"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	firecrawl := cfg.Servers["firecrawl"]
	if firecrawl.Type != "" || firecrawl.URL != "" {
		t.Fatalf("an entry naming its own transport must be left alone: %#v", firecrawl)
	}
	if firecrawl.Command != "firecrawl-mcp" {
		t.Fatalf("the user's command must survive: %#v", firecrawl)
	}
}

func TestResolveMCPDisabledRetiredEntryStaysReEnableable(t *testing.T) {
	// The disable carries to exa, and the retired entry itself keeps a usable
	// transport so a later `zero mcp enable firecrawl` does not resolve to a
	// server with no type, url, or command.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveMCP(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	firecrawl := cfg.Servers["firecrawl"]
	if !firecrawl.Disabled {
		t.Fatalf("the retired entry must stay disabled: %#v", firecrawl)
	}
	if firecrawl.Type != "http" || firecrawl.URL != "https://mcp.firecrawl.dev/v2/mcp" {
		t.Fatalf("a disabled retired entry should still carry a usable transport: %#v", firecrawl)
	}
	if !cfg.Servers["exa"].Disabled {
		t.Fatal("the disable must still carry to the successor")
	}
}
