package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeValidateFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestValidateFileReturnsParseIssueForMalformedJSON(t *testing.T) {
	path := writeValidateFixture(t, `{"activeProvider": "openai",`)

	_, issues := ValidateFile(path)
	if len(issues) == 0 {
		t.Fatalf("expected parse issue, got none")
	}
	if !strings.Contains(issues[0].Message, "invalid config JSON") {
		t.Fatalf("expected parse issue message, got %#v", issues)
	}
}

func TestValidateBytesSelectsNamelessOpenAIProvider(t *testing.T) {
	cfg, issues := ValidateBytes([]byte(`{"activeProvider":"openai","providers":[{"providerKind":"openai","model":"gpt-4.1"}]}`))
	if len(issues) != 0 {
		t.Fatalf("ValidateBytes() issues = %+v", issues)
	}
	if cfg.ActiveProvider != "openai" {
		t.Fatalf("activeProvider = %q, want openai", cfg.ActiveProvider)
	}
}

func TestValidateFileSurfacesSemanticIssue(t *testing.T) {
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "baseURL": "https://example.test/v1", "model": "gpt-4.1"}
		]
	}`)

	cfg, issues := ValidateFile(path)
	if cfg.ActiveProvider != "main" {
		t.Fatalf("expected parsed config, got %#v", cfg)
	}
	if len(issues) == 0 {
		t.Fatalf("expected semantic issue for openai custom baseURL, got none")
	}
}

func TestValidateFileRedactsSecretInIssue(t *testing.T) {
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "baseURL": "https://example.test/v1", "apiKey": "sk-proj-secret1234567890", "model": "gpt-4.1"}
		]
	}`)

	_, issues := ValidateFile(path)
	if len(issues) == 0 {
		t.Fatalf("expected semantic issue, got none")
	}
	for _, issue := range issues {
		if strings.Contains(issue.Message, "sk-proj-secret") {
			t.Fatalf("issue leaked apiKey: %q", issue.Message)
		}
	}
}

func TestValidateFileMissingModelWarns(t *testing.T) {
	// An openai-compatible CUSTOM endpoint has no catalog default to fall back
	// on, so a missing model is still a real issue — and the message must tell
	// the user how to fix it.
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai-compatible", "baseURL": "https://gateway.example/v1"}
		]
	}`)

	_, issues := ValidateFile(path)
	if len(issues) == 0 {
		t.Fatalf("expected requires-model issue, got none")
	}
	if !strings.Contains(issues[0].Message, "requires model") {
		t.Fatalf("expected requires-model issue, got %#v", issues)
	}
	if !strings.Contains(issues[0].Message, "config.json") {
		t.Fatalf("requires-model issue should carry an actionable hint, got %#v", issues)
	}
}

func TestValidateFileDefaultsOfficialKindModels(t *testing.T) {
	// Official-API kinds (anthropic/google) fall back to their catalog default
	// model, so a hand-written model-less profile validates clean instead of
	// bricking zero config / bare zero setup — the only commands that could
	// have fixed it (the reported google case).
	path := writeValidateFixture(t, `{
		"activeProvider": "google",
		"providers": [
			{"name": "google", "provider_kind": "google", "apiKey": "AIza-x"},
			{"name": "anthropic", "provider_kind": "anthropic", "apiKey": "sk-ant-x"}
		]
	}`)

	_, issues := ValidateFile(path)
	for _, issue := range issues {
		if strings.Contains(issue.Message, "requires model") {
			t.Fatalf("official-kind profiles must default their model, got issue %#v", issue)
		}
	}
}

func TestValidateFileValidConfigHasNoIssues(t *testing.T) {
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "model": "gpt-4.1"}
		]
	}`)

	_, issues := ValidateFile(path)
	if len(issues) != 0 {
		t.Fatalf("expected no issues for valid config, got %#v", issues)
	}
}

func TestValidateFileFlagsUnknownTopLevelKey(t *testing.T) {
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "model": "gpt-4.1"}
		],
		"maxTurn": 1
	}`)

	_, issues := ValidateFile(path)
	if len(issues) == 0 {
		t.Fatalf("expected unknown-field issue for top-level typo, got none")
	}
	var got Issue
	found := false
	for _, issue := range issues {
		if issue.FieldPath == "maxTurn" {
			got, found = issue, true
		}
	}
	if !found {
		t.Fatalf("expected unknown-field issue at maxTurn, got %#v", issues)
	}
	if !strings.Contains(got.Message, `did you mean "maxTurns"`) {
		t.Fatalf("expected near-match suggestion, got %q", got.Message)
	}
}

func TestValidateFileFlagsUnknownNestedKey(t *testing.T) {
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "model": "gpt-4.1"}
		],
		"sandbox": {
			"network": "allow",
			"blockUnixSocket": true
		}
	}`)

	_, issues := ValidateFile(path)
	var got Issue
	found := false
	for _, issue := range issues {
		if issue.FieldPath == "sandbox.blockUnixSocket" {
			got, found = issue, true
		}
	}
	if !found {
		t.Fatalf("expected nested unknown-field issue at sandbox.blockUnixSocket, got %#v", issues)
	}
	if !strings.Contains(got.Message, `did you mean "sandbox.blockUnixSockets"`) {
		t.Fatalf("expected near-match suggestion, got %q", got.Message)
	}
}

func TestValidateFileFlagsUnknownInProvider(t *testing.T) {
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "model": "gpt-4.1", "endpoint": "https://example.test/v1"}
		]
	}`)

	_, issues := ValidateFile(path)
	var found bool
	for _, issue := range issues {
		if issue.FieldPath == "providers[0].endpoint" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown-field issue for providers[0].endpoint, got %#v", issues)
	}
}

func TestValidateFileAllowsLegacyAliases(t *testing.T) {
	// The legacy mcpServers / mcp_servers aliases are still read by
	// FileConfig.UnmarshalJSON and must not be reported as unknown.
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "model": "gpt-4.1"}
		],
		"mcpServers": {
			"docs": {"type": "stdio", "command": "docs-mcp"}
		}
	}`)

	_, issues := ValidateFile(path)
	for _, issue := range issues {
		if strings.Contains(issue.FieldPath, "mcpServers") {
			t.Fatalf("legacy alias mcpServers reported as unknown: %#v", issues)
		}
	}
}

func TestValidateFileAllowsCaseVariantKey(t *testing.T) {
	// encoding/json matches object keys case-insensitively, so "MaxTurns"
	// parses into MaxTurns. The scanner compares lower-cased keys,
	// so a valid case variant must not be flagged as unknown.
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "provider_kind": "openai", "model": "gpt-4.1"}
		],
		"MaxTurns": 1
	}`)

	_, issues := ValidateFile(path)
	for _, issue := range issues {
		if strings.Contains(issue.Message, "unknown config field") {
			t.Fatalf("valid case-variant key flagged as unknown: %#v", issues)
		}
	}
}

func TestValidateFileAllowsLegacyProviderKeys(t *testing.T) {
	// ProviderProfile.UnmarshalJSON accepts snake_case legacy keys
	// (base_url, api_key, providerKind, ...). These are valid and
	// must not be flagged as unknown — only the canonical camelCase
	// tags are visible to the reflection scan, so they are allowlisted.
	path := writeValidateFixture(t, `{
		"activeProvider": "main",
		"providers": [
			{"name": "main", "providerKind": "openai", "base_url": "https://example.test/v1", "api_key": "sk-x", "model": "gpt-4.1"}
		]
	}`)

	_, issues := ValidateFile(path)
	for _, issue := range issues {
		if strings.Contains(issue.Message, "unknown config field") {
			t.Fatalf("legacy provider key reported as unknown: %#v", issues)
		}
	}
}

func TestValidateBytesUnnamedExplicitActive(t *testing.T) {
	_, issues := ValidateBytes([]byte(`{"activeProvider":"openai","providers":[{"provider":"openai","model":"gpt-4o"}]}`))
	if len(issues) != 0 {
		t.Fatalf("unnamed project provider rejected: %+v", issues)
	}
}
