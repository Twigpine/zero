package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/sessions"
)

func TestRunDoctorFormatsRedactedProviderDiagnostics(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()

	exitCode := runWithDeps([]string{"doctor"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.ResolvedConfig{
				Provider: config.ProviderProfile{
					Name:         "openai",
					ProviderKind: config.ProviderKindOpenAI,
					BaseURL:      config.OpenAIBaseURL,
					APIKey:       "sk-proj-secret1234567890",
					Model:        "gpt-4.1",
				},
			}, nil
		},
		now: fixedCLITime("2026-06-04T16:00:00Z"),
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Zero doctor report", "Overall: pass", "[pass] provider.config", "[warn] provider.connectivity"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected doctor output to contain %q, got %q", want, output)
		}
	}
	if strings.Contains(output, "sk-proj-secret") {
		t.Fatalf("doctor output leaked secret: %q", output)
	}
}

func TestRunDoctorConnectivityProbesProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	probed := false

	exitCode := runWithDeps([]string{"doctor", "--connectivity"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			profile := config.ProviderProfile{
				Name:         "local",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://api.example.com/v1",
				APIKey:       "sk-test-secret",
				Model:        "custom-model",
			}
			return config.ResolvedConfig{Provider: profile, Providers: []config.ProviderProfile{profile}, ActiveProvider: "local"}, nil
		},
		probeProviderHealth: func(_ context.Context, options providerhealth.Options) providerhealth.Result {
			probed = true
			if !options.Connectivity || options.Profile.Name != "local" {
				t.Fatalf("unexpected health probe options: %#v", options)
			}
			return providerhealth.Result{
				Status: providerhealth.StatusPass,
				Checks: []providerhealth.Check{
					{ID: "provider.connectivity", Status: providerhealth.StatusPass, Message: "reachable"},
				},
			}
		},
		now: fixedCLITime("2026-06-04T16:00:00Z"),
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "[pass] provider.connectivity") {
		t.Fatalf("expected provider.connectivity pass, got %q", output)
	}
	if !probed {
		t.Fatal("doctor did not call probeProviderHealth")
	}
	if strings.Contains(output, "not wired") {
		t.Fatalf("doctor still returned placeholder connectivity message: %q", output)
	}
}

func TestRunDoctorConnectivityUsesOAuthLogin(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "oauth-tokens.json")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: tokenPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{
		AccessToken: "oauth-doctor-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed OAuth token: %v", err)
	}

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	profile := config.ProviderProfile{
		Name:         "xai",
		CatalogID:    "xai",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/v1",
		Model:        "grok-4",
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"doctor", "--connectivity"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				Provider:       profile,
				Providers:      []config.ProviderProfile{profile},
				ActiveProvider: profile.Name,
			}, nil
		},
		probeProviderHealth: providerhealth.Probe,
		now:                 fixedCLITime("2026-07-29T20:00:00Z"),
	})

	if exitCode != exitSuccess {
		t.Fatalf("doctor exit = %d, want success; stderr=%q\n%s", exitCode, stderr.String(), stdout.String())
	}
	if gotAuth != "Bearer oauth-doctor-token" {
		t.Fatalf("Authorization = %q, want OAuth bearer", gotAuth)
	}
	if !strings.Contains(stdout.String(), "[pass] provider.connectivity") {
		t.Fatalf("doctor did not report passing connectivity:\n%s", stdout.String())
	}
}

func TestRunDoctorConnectivityReportsProviderHealthFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()

	exitCode := runWithDeps([]string{"doctor", "--connectivity"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			profile := config.ProviderProfile{
				Name:         "openai",
				ProviderKind: config.ProviderKindOpenAI,
				BaseURL:      config.OpenAIBaseURL,
				Model:        "gpt-4.1",
			}
			return config.ResolvedConfig{Provider: profile, Providers: []config.ProviderProfile{profile}, ActiveProvider: "openai"}, nil
		},
		now: fixedCLITime("2026-06-04T16:05:00Z"),
	})

	if exitCode != exitProvider {
		t.Fatalf("expected exit code %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "[fail] provider.connectivity") {
		t.Fatalf("expected provider.connectivity failure, got %q", output)
	}
	if !strings.Contains(output, "requires API credentials") {
		t.Fatalf("expected concrete auth failure, got %q", output)
	}
	if strings.Contains(output, "completed without a connectivity check") || strings.Contains(output, "not wired") {
		t.Fatalf("doctor returned generic connectivity message: %q", output)
	}
}

func TestRunDoctorConnectivityDoesNotMaskConfigHealthFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()

	exitCode := runWithDeps([]string{"doctor", "--connectivity"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			profile := config.ProviderProfile{
				Name:         "local",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://example.invalid/v1",
			}
			return config.ResolvedConfig{Provider: profile, Providers: []config.ProviderProfile{profile}, ActiveProvider: "local"}, nil
		},
		now: fixedCLITime("2026-06-04T16:06:00Z"),
	})

	if exitCode != exitProvider {
		t.Fatalf("expected exit code %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "[fail] provider.connectivity") || !strings.Contains(output, "requires model") {
		t.Fatalf("expected concrete config health failure, got %q", output)
	}
	if strings.Contains(output, "runtime did not resolve") || strings.Contains(output, "completed without a connectivity check") {
		t.Fatalf("doctor masked provider health failure: %q", output)
	}
}

func TestRunDoctorJSONReturnsFailureForMissingProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"doctor", "--json"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		now: fixedCLITime("2026-06-04T16:30:00Z"),
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d", exitProvider, exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor JSON did not decode: %v\n%s", err, stdout.String())
	}
	if report.OK {
		t.Fatalf("expected report ok=false, got %#v", report)
	}
}

func TestRunSearchFindsSessionEventsFromInjectedStore(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-04T17:00:00Z")})
	session, err := store.Create(sessions.CreateInput{SessionID: "cli_search", Title: "CLI Search", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "needle with token=sk-secret1234567890"}}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"search", "needle", "--limit", "3"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store {
			return store
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Found 1 local session event") || !strings.Contains(output, "cli_search") {
		t.Fatalf("unexpected search output: %q", output)
	}
	if strings.Contains(output, "sk-secret") {
		t.Fatalf("search output leaked secret: %q", output)
	}
}

func TestRunSearchAcceptsLegacyAliasFlags(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-04T17:15:00Z")})
	session, err := store.Create(sessions.CreateInput{SessionID: "legacy_search", Title: "CLI Search", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "needle alias context"}}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"search", "--context", "4", "--session", "legacy_search", "needle"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store {
			return store
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "legacy_search") {
		t.Fatalf("expected legacy search alias to find session, got %q", stdout.String())
	}
}

func TestRunSearchJSONAndValidation(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-04T17:30:00Z")})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"search", "--json", "missing"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store {
			return store
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var result struct {
		Query     string `json:"query"`
		TotalHits int    `json:"totalHits"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("search JSON did not decode: %v\n%s", err, stdout.String())
	}
	if result.Query != "missing" || result.TotalHits != 0 {
		t.Fatalf("unexpected search result: %#v", result)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"search"}, &stdout, &stderr, appDeps{})
	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "search query required") {
		t.Fatalf("expected search validation error, got %q", stderr.String())
	}
}

func TestRunSearchJSONRedactsQueryAndSessionMetadata(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-04T18:00:00Z")})
	querySecret := "sk-proj-querysecret1234567890"
	metadataSecret := "sk-proj-metadatasecret1234567890"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"search", "--json", querySecret}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store {
			return store
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), querySecret) {
		t.Fatalf("search JSON leaked raw query secret: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("expected redacted query marker in JSON output: %q", stdout.String())
	}

	session, err := store.Create(sessions.CreateInput{
		SessionID: "json_metadata_secret",
		Title:     "Title " + metadataSecret,
		Cwd:       "/repo/" + metadataSecret,
		ModelID:   "model-" + metadataSecret,
		Provider:  "provider-token=" + metadataSecret,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "metadata needle"}}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"search", "--json", "metadata", "needle"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store {
			return store
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), metadataSecret) {
		t.Fatalf("search JSON leaked session metadata secret: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("expected redacted metadata marker in JSON output: %q", stdout.String())
	}
}

func TestRunDoctorReportsConfigValidationForMalformedFile(t *testing.T) {
	cwd := t.TempDir()
	zeroDir := filepath.Join(cwd, ".zero")
	if err := os.MkdirAll(zeroDir, 0o755); err != nil {
		t.Fatalf("mkdir .zero: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zeroDir, "config.json"), []byte("{\n  \"activeProvider\": \"openai\",\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"doctor", "--json"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, fmt.Errorf("invalid config JSON %s: unexpected end of JSON input", filepath.Join(zeroDir, "config.json"))
		},
		now: fixedCLITime("2026-06-08T11:00:00Z"),
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor JSON did not decode: %v\n%s", err, stdout.String())
	}
	if report.OK {
		t.Fatalf("expected ok=false for malformed config, got %#v", report)
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == "config.validation" {
			found = true
			if check.Status != "fail" {
				t.Fatalf("expected config.validation fail, got %q", check.Status)
			}
		}
	}
	if !found {
		t.Fatalf("expected config.validation check in report: %#v", report)
	}
}

func TestRunDoctorForwardsPersistedProviderNameValidation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":""},{"name":"work"},{"name":"WORK"}]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cwd := t.TempDir()

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"doctor"}, &stdout, &stderr, appDeps{
		getwd:          func() (string, error) { return cwd, nil },
		userConfigPath: func() (string, error) { return configPath, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.Resolve(config.ResolveOptions{UserConfigPath: configPath})
		},
		now: fixedCLITime("2026-06-08T11:00:00Z"),
	})

	if exitCode != exitProvider {
		t.Fatalf("exit = %d, want provider failure %d; stderr = %q", exitCode, exitProvider, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "zero providers repair-config") {
		t.Fatalf("doctor output = %q, want provider-name repair guidance", stdout.String())
	}
}

func fixedCLITime(value string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}
