package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
)

func TestRunReportRedactsProviderSecretsAndWarnsWithoutConnectivity(t *testing.T) {
	report := Run(Options{
		Now:        fixedDoctorClock("2026-06-04T15:00:00Z"),
		Runtime:    "go",
		UserConfig: "missing",
		Provider: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			APIKey:       "sk-proj-secret1234567890",
			Model:        "gpt-4.1",
		},
	})

	if !report.OK {
		t.Fatalf("report should be ok when only connectivity is skipped: %#v", report)
	}
	if got := report.Check("provider.config"); got == nil || got.Status != StatusPass {
		t.Fatalf("provider config check missing/pass expected: %#v", report.Checks)
	}
	formatted := Format(report)
	if strings.Contains(formatted, "sk-proj-secret") {
		t.Fatalf("formatted report leaked provider secret: %q", formatted)
	}
	if !strings.Contains(formatted, "[warn] provider.connectivity") {
		t.Fatalf("expected skipped connectivity warning: %q", formatted)
	}
}

func TestRunReportFailsInvalidModelAndMissingProvider(t *testing.T) {
	missing := Run(Options{Now: fixedDoctorClock("2026-06-04T15:30:00Z"), Runtime: "go"})
	if missing.OK {
		t.Fatalf("missing provider should fail: %#v", missing)
	}
	if check := missing.Check("provider.config"); check == nil || check.Status != StatusFail {
		t.Fatalf("expected provider config failure: %#v", missing.Checks)
	}

	invalid := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:30:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "not-a-zero-model",
		},
	})
	if invalid.OK {
		t.Fatalf("invalid model should fail: %#v", invalid)
	}
	if check := invalid.Check("provider.model"); check == nil || check.Status != StatusFail || !strings.Contains(check.Message, "unknown Zero model") {
		t.Fatalf("expected model failure: %#v", invalid.Checks)
	}
}

func TestRunReportWarnsForUnknownOpenAICompatibleModel(t *testing.T) {
	report := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "local",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "http://127.0.0.1:11434/v1",
			Model:        "local-custom-model",
		},
	})

	if !report.OK {
		t.Fatalf("unknown custom model should warn, not fail: %#v", report)
	}
	if check := report.Check("provider.model"); check == nil || check.Status != StatusWarn || !strings.Contains(check.Message, "pass it through") {
		t.Fatalf("expected custom model warning: %#v", report.Checks)
	}
}

func TestProviderModelUnknownOpenAICompatibleModelWarnsPassThrough(t *testing.T) {
	report := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "openrouter",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://openrouter.ai/api/v1",
			Model:        "vendor/new-dynamic-model",
			APIKey:       "sk-test", // isolate the model pass-through behavior from the credential check
		},
	})

	if !report.OK {
		t.Fatalf("unknown openai-compatible model should warn, not fail: %#v", report)
	}
	check := report.Check("provider.model")
	if check == nil {
		t.Fatalf("expected provider.model check: %#v", report.Checks)
	}
	if check.Status != StatusWarn {
		t.Fatalf("provider.model status = %s, want warn: %#v", check.Status, check)
	}
	if !strings.Contains(check.Message, "runtime will pass it through") || !strings.Contains(check.Message, "doctor --connectivity") {
		t.Fatalf("provider.model message should explain pass-through connectivity validation, got %q", check.Message)
	}
}

func TestProviderModelMissingModelFails(t *testing.T) {
	report := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			Model:        "   ",
		},
	})

	if report.OK {
		t.Fatalf("missing model should fail: %#v", report)
	}
	check := report.Check("provider.model")
	if check == nil || check.Status != StatusFail || !strings.Contains(check.Message, "required") {
		t.Fatalf("expected missing model failure: %#v", report.Checks)
	}
}

func TestProviderModelBuiltInUnknownModelFails(t *testing.T) {
	report := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "vendor/new-dynamic-model",
		},
	})

	if report.OK {
		t.Fatalf("built-in provider unknown model should fail: %#v", report)
	}
	check := report.Check("provider.model")
	if check == nil || check.Status != StatusFail || !strings.Contains(check.Message, "unknown Zero model") {
		t.Fatalf("expected built-in unknown model failure: %#v", report.Checks)
	}
}

func TestProviderModelRegisteredProviderMismatchFails(t *testing.T) {
	report := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "gemini-2.5-pro",
		},
	})

	if report.OK {
		t.Fatalf("registered provider mismatch should fail: %#v", report)
	}
	check := report.Check("provider.model")
	if check == nil || check.Status != StatusFail || !strings.Contains(check.Message, "not available for provider") {
		t.Fatalf("expected registered provider mismatch failure: %#v", report.Checks)
	}
}

func writeDoctorConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestConfigValidationCheckPassesForValidConfig(t *testing.T) {
	path := writeDoctorConfig(t, `{
		"activeProvider": "main",
		"providers": [{"name": "main", "provider_kind": "openai", "model": "gpt-4.1"}]
	}`)

	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:00:00Z"), Runtime: "go", ProjectConfig: path})

	check := report.Check("config.validation")
	if check == nil || check.Status != StatusPass {
		t.Fatalf("expected config.validation pass, got %#v", report.Checks)
	}
}

func TestConfigValidationCheckFailsMalformedJSONWithLineCol(t *testing.T) {
	// Unterminated object: the trailing comma + EOF yields a *json.SyntaxError
	// whose offset is the end of the 32-byte document (line 3, col 1).
	path := writeDoctorConfig(t, "{\n  \"activeProvider\": \"openai\",\n")

	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:05:00Z"), Runtime: "go", ProjectConfig: path})

	if report.OK {
		t.Fatalf("malformed config should fail report: %#v", report)
	}
	check := report.Check("config.validation")
	if check == nil || check.Status != StatusFail {
		t.Fatalf("expected config.validation fail, got %#v", report.Checks)
	}
	entry, ok := check.Details[path].(map[string]any)
	if !ok {
		t.Fatalf("expected per-path map entry for %q, got %#v", path, check.Details)
	}
	// Redaction widens ints to int64 as it round-trips the details map.
	if entry["line"] != int64(3) || entry["col"] != int64(1) {
		t.Fatalf("line/col = (%v,%v), want (3,1): %#v", entry["line"], entry["col"], entry)
	}
	// The colliding flat top-level keys must be gone (they were ambiguous across
	// multiple malformed files).
	if _, exists := check.Details["line"]; exists {
		t.Fatalf("flat details[\"line\"] should be removed, got %#v", check.Details)
	}
	if _, exists := check.Details["col"]; exists {
		t.Fatalf("flat details[\"col\"] should be removed, got %#v", check.Details)
	}
}

func TestConfigValidationCheckFailsTypeMismatchWithLineCol(t *testing.T) {
	// {"maxTurns":"twelve"} is structurally valid JSON but the wrong type for
	// FileConfig.MaxTurns. The probe (var probe config.FileConfig) surfaces a
	// *json.UnmarshalTypeError carrying the offset, exercising the branch that a
	// `var probe any` probe could never reach.
	path := writeDoctorConfig(t, `{"maxTurns":"twelve"}`)

	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:07:00Z"), Runtime: "go", ProjectConfig: path})

	if report.OK {
		t.Fatalf("type-mismatch config should fail report: %#v", report)
	}
	check := report.Check("config.validation")
	if check == nil || check.Status != StatusFail {
		t.Fatalf("expected config.validation fail, got %#v", report.Checks)
	}
	entry, ok := check.Details[path].(map[string]any)
	if !ok {
		t.Fatalf("expected per-path map entry for %q, got %#v", path, check.Details)
	}
	if entry["line"] == nil || entry["col"] == nil {
		t.Fatalf("expected non-nil line/col for type mismatch, got %#v", entry)
	}
	// offset=20 in a single-line document -> line 1, col 21. Redaction widens
	// ints to int64 as it round-trips the details map.
	if entry["line"] != int64(1) || entry["col"] != int64(21) {
		t.Fatalf("line/col = (%v,%v), want (1,21): %#v", entry["line"], entry["col"], entry)
	}
}

func TestConfigValidationCheckFailsSemanticIssue(t *testing.T) {
	path := writeDoctorConfig(t, `{
		"activeProvider": "main",
		"providers": [{"name": "main", "provider_kind": "openai", "baseURL": "https://example.test/v1", "model": "gpt-4.1"}]
	}`)

	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:10:00Z"), Runtime: "go", ProjectConfig: path})

	check := report.Check("config.validation")
	if check == nil || check.Status != StatusFail {
		t.Fatalf("expected semantic fail, got %#v", report.Checks)
	}
}

func TestConfigValidationCheckFailsUnreadableConfig(t *testing.T) {
	// A present-but-unreadable path (here: a directory, which os.ReadFile rejects
	// with a non-not-exist error) must surface as a failing detail rather than
	// silently passing validation. A missing path stays a skip (covered by
	// TestRunReportRedactsProviderSecretsAndWarnsWithoutConnectivity), so this
	// guards only the non-not-exist branch.
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:13:00Z"), Runtime: "go", ProjectConfig: configDir})

	if report.OK {
		t.Fatalf("unreadable config should fail report: %#v", report)
	}
	check := report.Check("config.validation")
	if check == nil || check.Status != StatusFail {
		t.Fatalf("expected config.validation fail for unreadable path, got %#v", report.Checks)
	}
	entry, ok := check.Details[configDir].(map[string]any)
	if !ok {
		t.Fatalf("expected per-path map entry for %q, got %#v", configDir, check.Details)
	}
	message, _ := entry["error"].(string)
	if !strings.Contains(message, "unreadable:") {
		t.Fatalf("expected unreadable error detail, got %#v", entry)
	}
}

func TestConfigValidationCheckSkippedWhenNoPaths(t *testing.T) {
	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:15:00Z"), Runtime: "go"})

	check := report.Check("config.validation")
	if check == nil || check.Status != StatusWarn {
		t.Fatalf("expected config.validation warn-skip, got %#v", report.Checks)
	}
}

func TestConfigValidationCheckDoesNotLeakSecret(t *testing.T) {
	path := writeDoctorConfig(t, `{
		"activeProvider": "main",
		"providers": [{"name": "main", "provider_kind": "openai", "baseURL": "https://example.test/v1", "apiKey": "sk-proj-secret1234567890", "model": "gpt-4.1"}]
	}`)

	report := Run(Options{Now: fixedDoctorClock("2026-06-08T10:20:00Z"), Runtime: "go", ProjectConfig: path})

	if strings.Contains(Format(report), "sk-proj-secret") {
		t.Fatalf("config.validation leaked apiKey: %q", Format(report))
	}
}

func TestOffsetToLineCol(t *testing.T) {
	data := []byte("{\n  \"a\": 1,\n  bad\n}")
	cases := []struct {
		name     string
		data     []byte
		offset   int64
		wantLine int
		wantCol  int
	}{
		{name: "start", data: data, offset: 0, wantLine: 1, wantCol: 1},
		{name: "after first newline", data: data, offset: 2, wantLine: 2, wantCol: 1},
		{name: "mid second line", data: data, offset: 7, wantLine: 2, wantCol: 6},
		{name: "negative clamps to start", data: data, offset: -5, wantLine: 1, wantCol: 1},
		{name: "past end clamps to last", data: data, offset: 9999, wantLine: 4, wantCol: 2},
		// Multi-byte UTF-8: the rune '£' is 2 bytes (0xC2 0xA3).  The JSON parser
		// reports byte offsets, so the column after '£' is byte-column 3, not
		// rune-column 2.  This documents that offsetToLineCol counts BYTES, not
		// runes.
		{name: "utf8 multibyte byte columns", data: []byte("{\"£\": bad}"), offset: 6, wantLine: 1, wantCol: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, col := offsetToLineCol(tc.data, tc.offset)
			if line != tc.wantLine || col != tc.wantCol {
				t.Fatalf("offsetToLineCol(%d) = (%d,%d), want (%d,%d)", tc.offset, line, col, tc.wantLine, tc.wantCol)
			}
		})
	}
}

func fixedDoctorClock(value string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}

func TestProviderConfigCheckCredentialPresence(t *testing.T) {
	base := func(extra func(*config.ProviderProfile)) config.ProviderProfile {
		p := config.ProviderProfile{Name: "main", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"}
		extra(&p)
		return p
	}
	cases := []struct {
		name    string
		profile config.ProviderProfile
		want    string
	}{
		{"api key", base(func(p *config.ProviderProfile) { p.APIKey = "sk-x" }), "set"},
		{"auth header only", base(func(p *config.ProviderProfile) { p.AuthHeaderValue = "Bearer t" }), "set"},
		{"both", base(func(p *config.ProviderProfile) { p.APIKey = "sk-x"; p.AuthHeaderValue = "Bearer t" }), "set"},
		{"whitespace api key", base(func(p *config.ProviderProfile) { p.APIKey = "   " }), "not set"},
		{"whitespace auth header only", base(func(p *config.ProviderProfile) { p.AuthHeaderValue = "   " }), "not set"},
		{"no credential", base(func(*config.ProviderProfile) {}), "not set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerConfigCheck(tc.profile).Details["authConfigured"]
			if got != tc.want {
				t.Fatalf("authConfigured = %v, want %q (matches ProviderSnapshot.APIKeySet trimming)", got, tc.want)
			}
		})
	}
}

func TestFormatDetailValueRendersMapForHumans(t *testing.T) {
	// AUDIT-H8: nested-map details (lsp.servers missing tools, config.validation) must
	// not leak Go's map[...] syntax.
	got := formatDetailValue(map[string]any{
		"gopls":   "install with `go install ...`",
		"pyright": "install with `npm i -g pyright`",
	})
	if strings.Contains(got, "map[") {
		t.Fatalf("formatDetailValue leaked Go map syntax: %q", got)
	}
	if !strings.Contains(got, "gopls: install") || !strings.Contains(got, "pyright: install") {
		t.Fatalf("formatDetailValue should render sorted k: v entries, got %q", got)
	}
}

func TestDoctorFailsRemoteProviderWithoutCredential(t *testing.T) {
	// AUDIT-H9: a remote provider with no key must NOT yield "Overall: pass".
	report := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      "https://api.openai.com/v1",
			Model:        "gpt-4.1",
		},
	})
	if report.OK {
		t.Fatal("doctor must not report Overall: pass for a remote provider with no credential")
	}
	if c := report.Check("provider.config"); c == nil || c.Status != StatusFail {
		t.Fatalf("provider.config should fail without a credential, got %#v", c)
	}

	// A keyless LOCAL provider (loopback base_url) is legitimately fine.
	local := Run(Options{
		Now:     fixedDoctorClock("2026-06-04T15:45:00Z"),
		Runtime: "go",
		Provider: config.ProviderProfile{
			Name:         "ollama",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "http://localhost:11434/v1",
			Model:        "llama3",
		},
	})
	if c := local.Check("provider.config"); c == nil || c.Status != StatusPass {
		t.Fatalf("keyless local provider should pass provider.config, got %#v", c)
	}
}
