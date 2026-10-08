package config

import (
	"encoding/json"
	"testing"
)

func TestToolsConfigJSONRoundTrip(t *testing.T) {
	var cfg FileConfig
	if err := json.Unmarshal([]byte(`{"tools":{"deferThreshold":25}}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if cfg.Tools.DeferThreshold != 25 {
		t.Fatalf("Tools.DeferThreshold = %d, want 25", cfg.Tools.DeferThreshold)
	}

	encoded, err := json.Marshal(ToolsConfig{DeferThreshold: 7})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(encoded) != `{"deferThreshold":7}` {
		t.Fatalf("Marshal() = %s, want {\"deferThreshold\":7}", encoded)
	}

	// omitempty: a zero value must not emit the field.
	emptyEncoded, err := json.Marshal(ToolsConfig{})
	if err != nil {
		t.Fatalf("Marshal(empty) error = %v", err)
	}
	if string(emptyEncoded) != `{}` {
		t.Fatalf("Marshal(empty) = %s, want {}", emptyEncoded)
	}
}

func TestToolsConfigPresentOnOverridesAndResolved(t *testing.T) {
	// Compile-time guard that Overrides and ResolvedConfig carry the field too.
	overrides := Overrides{Tools: ToolsConfig{DeferThreshold: 3}}
	resolved := ResolvedConfig{Tools: ToolsConfig{DeferThreshold: 4}}
	if overrides.Tools.DeferThreshold != 3 {
		t.Fatalf("Overrides.Tools.DeferThreshold = %d, want 3", overrides.Tools.DeferThreshold)
	}
	if resolved.Tools.DeferThreshold != 4 {
		t.Fatalf("ResolvedConfig.Tools.DeferThreshold = %d, want 4", resolved.Tools.DeferThreshold)
	}
}

func TestFileConfigExtraCannotOverrideKnownFields(t *testing.T) {
	cfg := FileConfig{
		MaxTurns: 3,
		Extra: map[string]json.RawMessage{
			"maxTurns": json.RawMessage(`99`),
			"MaxTurns": json.RawMessage(`100`),
			"future":   json.RawMessage(`{"enabled":true}`),
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := string(persisted["maxTurns"]); got != "3" {
		t.Fatalf("maxTurns = %s, want 3", got)
	}
	if _, exists := persisted["MaxTurns"]; exists {
		t.Fatalf("case-variant extra overrode a known field: %s", data)
	}
	if got := string(persisted["future"]); got != `{"enabled":true}` {
		t.Fatalf("future = %s, want preserved object", got)
	}
}

func TestFileConfigUnicodeFoldedKnownFieldRoundTrip(t *testing.T) {
	const data = `{"mcpſervers":{"docs":{"url":"https://example.com/mcp"}}}`

	var cfg FileConfig
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, exists := cfg.MCP.Servers["docs"]; !exists {
		t.Fatalf("MCP.Servers = %#v, want docs server", cfg.MCP.Servers)
	}
	if cfg.Extra != nil {
		t.Fatalf("Extra = %#v, want Unicode-folded known field excluded", cfg.Extra)
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped FileConfig
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if _, exists := roundTripped.MCP.Servers["docs"]; !exists {
		t.Fatalf("round-tripped MCP.Servers = %#v, want docs server", roundTripped.MCP.Servers)
	}
}
