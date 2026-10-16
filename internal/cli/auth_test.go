package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/provideroauth"
)

// withAuthStore points the provider OAuth store at a temp file for the test,
// pinning the file backend so an inherited ZERO_OAUTH_STORAGE=keyring can't
// ignore the temp path and hit the OS keychain.
func withAuthStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", path)
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	return path
}

func TestRunAuthRejectsInvalidStorageMode(t *testing.T) {
	withAuthStore(t)
	// A mistyped value must fail fast, not silently fall back to plaintext while
	// the user believes encryption is active.
	t.Setenv("ZERO_OAUTH_STORAGE", "encryptd")
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "status"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatalf("invalid ZERO_OAUTH_STORAGE should fail, got success; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "ZERO_OAUTH_STORAGE") {
		t.Fatalf("error should name the offending env var, stderr=%q", stderr.String())
	}
}

func TestRunAuthStatusEmpty(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "status"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No OAuth provider logins are stored.") {
		t.Fatalf("status output = %q", stdout.String())
	}
}

func TestRunAuthStatusReportsLoginWithoutSecret(t *testing.T) {
	path := withAuthStore(t)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("demo"), oauth.Token{
		AccessToken: "super-secret", RefreshToken: "super-secret-rt", Account: "me@example.com",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "status"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "demo") || !strings.Contains(out, "me@example.com") {
		t.Fatalf("status should show provider + account: %q", out)
	}
	if strings.Contains(out, "super-secret") {
		t.Fatalf("status leaked token material: %q", out)
	}
}

func TestRunAuthLogoutNothing(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "logout", "demo"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No stored credential for demo") {
		t.Fatalf("logout output = %q", stdout.String())
	}
}

func TestRunAuthLoginValidation(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	// Missing provider.
	if code := runWithDeps([]string{"auth", "login"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("login with no provider should fail")
	}
	// --json is rejected for the interactive login.
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"auth", "login", "demo", "--json"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("login --json should be rejected")
	}
}

func TestRunAuthLoginUnknownProvider(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "login", "does-not-exist"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("unknown provider login should fail")
	}
	if !strings.Contains(stderr.String(), "not configured") {
		t.Fatalf("stderr = %q, want not-configured error", stderr.String())
	}
}

func TestRunAuthRefreshNoToken(t *testing.T) {
	withAuthStore(t)
	t.Setenv("ZERO_OAUTH_DEMO_CLIENT_ID", "client") // so config resolves; refresh still fails (no token)
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "refresh", "demo"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("refresh with no stored token should fail")
	}
}

func TestRunAuthRejectsWrongFlags(t *testing.T) {
	withAuthStore(t)
	cases := [][]string{
		{"auth", "login", "demo", "--watch"},       // watch is refresh-only
		{"auth", "login", "demo", "--json"},        // json not for interactive login
		{"auth", "status", "demo", "--device"},     // device is login-only
		{"auth", "logout", "demo", "--scope", "x"}, // scope is login-only
		{"auth", "refresh", "demo", "--json"},      // json not for refresh
		{"auth", "login", "demo", "--scope", ""},   // empty scope rejected
		{"auth", "status", "--confirm"},            // confirm is reset-only
		{"auth", "reset", "--confirm", "--watch"},  // confirmation does not allow unrelated flags
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := runWithDeps(args, &stdout, &stderr, appDeps{}); code == exitSuccess {
			t.Errorf("args %v should be rejected, got success", args)
		}
	}
}

func TestRunAuthOpenRouterRejectsArgs(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	// An unexpected arg/flag must fail fast, not silently run the login.
	if code := runWithDeps([]string{"auth", "openrouter", "--json"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatalf("openrouter with an unexpected flag should fail; stdout=%q", stdout.String())
	}
	// --help still works.
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"auth", "openrouter", "--help"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("openrouter --help should succeed, stderr=%q", stderr.String())
	}
}

func TestRunAuthOpenRouterSavesMintedKey(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer

	code := runWithDeps([]string{"auth", "openrouter"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		openRouterLogin: func(context.Context, provideroauth.OpenRouterOptions) (string, error) {
			return "sk-openrouter-test", nil
		},
	})

	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "new API key saved") {
		t.Fatalf("expected saved-key confirmation, got %q", stdout.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "openrouter" || len(cfg.Providers) != 1 {
		t.Fatalf("config = %#v", cfg)
	}
	profile := cfg.Providers[0]
	if profile.Name != "openrouter" || profile.CatalogID != "openrouter" || !profile.APIKeyStored || profile.APIKey != "" || profile.APIKeyEnv != "" {
		t.Fatalf("provider not stored-key sanitized: %#v", profile)
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := store.Get("openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != "sk-openrouter-test" {
		t.Fatalf("stored key = %q, %v", key, ok)
	}
}

func TestRunAuthHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "--help"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"zero auth", "login", "logout", "status", "refresh", "reset --confirm", "--device"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunAuthResetRequiresConfirmation(t *testing.T) {
	for _, args := range [][]string{{}, {"--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			path := withAuthStore(t)
			store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(oauth.ProviderKey("demo"), oauth.Token{AccessToken: "synthetic-reset-test-token"}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := runAuthReset(args, &stdout, &stderr, appDeps{})
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("unconfirmed reset changed the store: %v", readErr)
			}
			if code == exitSuccess || !strings.Contains(stderr.String(), "--confirm") || stdout.Len() != 0 {
				t.Fatalf("unconfirmed reset: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunAuthResetJSON(t *testing.T) {
	path := withAuthStore(t)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("demo"), oauth.Token{AccessToken: "synthetic-reset-test-token"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "reset", "--confirm", "--json"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	var payload struct {
		Reset bool `json:"reset"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v (stdout=%q)", err, stdout.String())
	}
	if !payload.Reset {
		t.Fatalf("payload = %+v, want reset=true", payload)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"auth", "status"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("status exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No OAuth provider logins are stored.") {
		t.Fatalf("expected empty store after JSON reset, got: %q", stdout.String())
	}
}

func TestRunAuthReset(t *testing.T) {
	path := withAuthStore(t)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("demo"), oauth.Token{AccessToken: "secret"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "reset", "--confirm"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Reset OAuth token store") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	// Verify store is now empty.
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"auth", "status"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "No OAuth provider logins are stored.") {
		t.Fatalf("expected empty store after reset, got: %q", stdout.String())
	}
}

// TestRunAuthLoginChatGPTRoutesToDedicatedFlow verifies `zero auth login
// chatgpt` reaches the dedicated ChatGPT login (fixed-port loopback + mandatory
// authorize params), not the generic manager path. The generic login accepts
// --device, so a ChatGPT-specific rejection proves the routing took effect.
// See issue #430: the generic path built a random-port 127.0.0.1 redirect_uri
// without the required extra params, so OpenAI's authorize endpoint rejected it.
func TestRunAuthLoginChatGPTRoutesToDedicatedFlow(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "login", "chatgpt", "--device"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("auth login chatgpt --device should be rejected (ChatGPT is loopback-only)")
	}
	if !strings.Contains(stderr.String(), "ChatGPT login does not support --device") {
		t.Fatalf("stderr = %q, want the ChatGPT-specific --device rejection", stderr.String())
	}
	// Case-insensitive provider name should still route.
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"auth", "login", "ChatGPT", "--device"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("auth login ChatGPT --device should be rejected")
	}
	if !strings.Contains(stderr.String(), "ChatGPT login does not support --device") {
		t.Fatalf("stderr = %q, want the ChatGPT-specific rejection (case-insensitive)", stderr.String())
	}
}

// TestRunAuthLoginChatGPTRejectsScope mirrors the --device rejection: --scope
// must not be silently dropped on the ChatGPT path. The Codex client
// registration pins a fixed scope set (incl. api.connectors.*), so custom
// scopes are rejected up front rather than plumbed through.
func TestRunAuthLoginChatGPTRejectsScope(t *testing.T) {
	withAuthStore(t)
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "login", "chatgpt", "--scope", "custom-scope"}, &stdout, &stderr, appDeps{}); code == exitSuccess {
		t.Fatal("auth login chatgpt --scope should be rejected")
	}
	if !strings.Contains(stderr.String(), "ChatGPT login does not support --scope") {
		t.Fatalf("stderr = %q, want the ChatGPT-specific --scope rejection", stderr.String())
	}
}

func TestEnsureLoginProviderProfileAddsProviderWithoutStealingActive(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{"activeProvider":"opengateway","providers":[{"name":"opengateway","provider_kind":"openai-compatible","baseURL":"https://gateway.example.com/v1","apiKeyStored":true,"model":"some-model"}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}

	line := ensureLoginProviderProfile(deps, "chatgpt")
	if !strings.Contains(line, `Added provider "chatgpt"`) {
		t.Fatalf("expected added-provider guidance, got %q", line)
	}
	if !strings.Contains(line, "zero providers use chatgpt") {
		t.Fatalf("expected switch hint, got %q", line)
	}

	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "opengateway" {
		t.Fatalf("active provider changed to %q", cfg.ActiveProvider)
	}
	names := make([]string, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		names = append(names, provider.Name)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[1].CatalogID != "chatgpt" {
		t.Fatalf("expected chatgpt profile appended, got %v", names)
	}

	// A second login must be a no-op with switch guidance, not a duplicate.
	line = ensureLoginProviderProfile(deps, "chatgpt")
	if !strings.Contains(line, "already configured") {
		t.Fatalf("expected already-configured guidance, got %q", line)
	}
	cfg = readFileConfig(t, configPath)
	if len(cfg.Providers) != 2 {
		t.Fatalf("repeat login duplicated the profile: %d providers", len(cfg.Providers))
	}
}

func TestEnsureLoginProviderProfileActivatesOnFreshConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}

	line := ensureLoginProviderProfile(deps, "chatgpt")
	if !strings.Contains(line, "set it active") {
		t.Fatalf("fresh config should adopt the login as active, got %q", line)
	}
	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "chatgpt" {
		t.Fatalf("active provider = %q, want chatgpt", cfg.ActiveProvider)
	}
}

func TestEnsureLoginProviderProfileSkipsNonCatalogProviders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}

	if line := ensureLoginProviderProfile(deps, "my-custom-oauth-server"); line != "" {
		t.Fatalf("custom OAuth server must not scaffold a profile, got %q", line)
	}
	if _, err := os.Stat(configPath); err == nil {
		t.Fatalf("config must not be created for a non-catalog login")
	}
}

func readCLIConfigFixture(t *testing.T, path string) config.FileConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

func TestRunAuthLogoutClearsCaseVariantStoredMarker(t *testing.T) {
	withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	configPath, err := config.DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-old"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}
	if code := runWithDeps([]string{"auth", "logout", "WORK"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("logout failed: code=%d stderr=%s", code, stderr.String())
	}
	if _, ok, getErr := store.Get("work"); getErr != nil || ok {
		t.Fatalf("stored key still present: ok=%v err=%v", ok, getErr)
	}
	cfg := readCLIConfigFixture(t, configPath)
	if cfg.Providers[0].APIKeyStored {
		t.Fatal("case-variant logout left apiKeyStored set")
	}
}

func TestRunAuthLogoutRejectsAmbiguousConfigBeforeCredentialDeletion(t *testing.T) {
	withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	configPath, err := config.DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`)
	if err := os.WriteFile(configPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-shared"); err != nil {
		t.Fatal(err)
	}
	oauthStore, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oauthToken := oauth.Token{AccessToken: "oauth-access", RefreshToken: "oauth-refresh", Account: "work@example.com"}
	if err := oauthStore.Save(oauth.ProviderKey("work"), oauthToken); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}
	if code := runWithDeps([]string{"auth", "logout", "WORK"}, &stdout, &stderr, deps); code != exitCrash {
		t.Fatalf("logout exit = %d, want validation failure", code)
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || !ok || key != "sk-shared" {
		t.Fatalf("shared API credential changed before rejection: present=%v err=%v", ok, getErr)
	}
	storedOAuth, ok, loadErr := oauthStore.Load(oauth.ProviderKey("work"))
	if loadErr != nil || !ok {
		t.Fatalf("OAuth credential missing after rejection: ok=%v err=%v", ok, loadErr)
	}
	if storedOAuth.AccessToken != oauthToken.AccessToken ||
		storedOAuth.RefreshToken != oauthToken.RefreshToken ||
		storedOAuth.Account != oauthToken.Account {
		t.Fatal("OAuth credential changed before ambiguous-config rejection")
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(seed) {
		t.Fatal("rejected logout rewrote ambiguous config")
	}
}

func TestRunAuthLoginRejectsAmbiguousConfigBeforeTokenReplacement(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		args     []string
	}{
		{name: "generic", provider: "xai", args: []string{"auth", "login", "xai"}},
		{name: "chatgpt", provider: "chatgpt", args: []string{"auth", "chatgpt"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			withAuthStore(t)
			configPath := filepath.Join(t.TempDir(), "config.json")
			seed := []byte(`{"providers":[{"name":"xai"},{"name":"XAI"}]}`)
			if err := os.WriteFile(configPath, seed, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := oauth.NewStore(oauth.StoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			previous := oauth.Token{AccessToken: "previous-access", RefreshToken: "previous-refresh", Account: "previous-account"}
			if err := store.Save(oauth.ProviderKey(test.provider), previous); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := runWithDeps(test.args, &stdout, &stderr, appDeps{userConfigPath: func() (string, error) { return configPath, nil }})
			if code != exitCrash || !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
				t.Fatalf("login exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			stored, ok, err := store.Load(oauth.ProviderKey(test.provider))
			if err != nil || !ok || stored.AccessToken != previous.AccessToken || stored.RefreshToken != previous.RefreshToken || stored.Account != previous.Account {
				t.Fatalf("rejected login changed previous token: ok=%v err=%v", ok, err)
			}
			after, err := os.ReadFile(configPath)
			if err != nil || !bytes.Equal(after, seed) {
				t.Fatalf("rejected login changed config: readErr=%v", err)
			}
		})
	}
}

func TestRunAuthLogoutRejectsConfigPathFailureBeforeCredentialDeletion(t *testing.T) {
	withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	store, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-shared"); err != nil {
		t.Fatal(err)
	}
	oauthStore, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oauthToken := oauth.Token{AccessToken: "oauth-access", RefreshToken: "oauth-refresh", Account: "work@example.com"}
	if err := oauthStore.Save(oauth.ProviderKey("work"), oauthToken); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	pathErr := errors.New("config path unavailable")
	code := runWithDeps([]string{"auth", "logout", "work"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return "", pathErr },
	})
	if code != exitCrash {
		t.Fatalf("logout exit = %d, want path failure", code)
	}
	if !strings.Contains(stderr.String(), pathErr.Error()) {
		t.Fatalf("stderr = %q, want config path failure", stderr.String())
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || !ok || key != "sk-shared" {
		t.Fatalf("API credential changed before path rejection: present=%v len=%d err=%v", ok, len(key), getErr)
	}
	storedOAuth, ok, loadErr := oauthStore.Load(oauth.ProviderKey("work"))
	if loadErr != nil || !ok {
		t.Fatalf("OAuth credential missing after path rejection: ok=%v err=%v", ok, loadErr)
	}
	if storedOAuth.AccessToken != oauthToken.AccessToken || storedOAuth.RefreshToken != oauthToken.RefreshToken || storedOAuth.Account != oauthToken.Account {
		t.Fatal("OAuth credential changed before config path rejection")
	}
}

// Local state that already makes publication impossible must be rejected BEFORE
// the browser flow mints a live remote credential. Validation used to live only
// inside saveOpenRouterProviderKey, past that irreversible boundary, so a legacy
// config sent the user through OpenRouter's authorization and then handed back
// an orphaned key with a nonzero exit.
func TestRunAuthOpenRouterRejectsInvalidConfigBeforeAuthorizing(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{"activeProvider":"openrouter","providers":[{"name":"openrouter","apiKeyStored":true},{"name":"OPENROUTER","apiKeyStored":true}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "openrouter"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		openRouterLogin: func(context.Context, provideroauth.OpenRouterOptions) (string, error) {
			t.Error("browser authorization started despite config that cannot be published")
			return "sk-minted", nil
		},
	})
	if code == exitSuccess {
		t.Fatalf("exit = %d, want non-zero for a config that cannot be published", code)
	}
	if !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("stderr = %q, want the local rejection", stderr.String())
	}
	// No key was minted, so nothing is handed back for manual use either.
	if strings.Contains(stdout.String(), "sk-minted") {
		t.Fatalf("stdout = %q, want no minted key", stdout.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != seed {
		t.Fatalf("rejected login rewrote config:\n%s", after)
	}
}

// The second check is not redundant with the first: the config can change while
// the browser flow is open. A valid pre-authorization config can become a legacy
// duplicate-row config during authorization; that edit must be rejected without
// writing the newly minted secret over the existing key.
func TestRunAuthOpenRouterPreservesExistingKeyWhenConfigRejected(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// Valid when the command starts, so the preflight before authorization passes.
	if err := os.WriteFile(configPath, []byte(`{"activeProvider":"openrouter","providers":[{"name":"openrouter","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := `{"activeProvider":"openrouter","providers":[{"name":"openrouter","apiKeyStored":true},{"name":"OPENROUTER","apiKeyStored":true}]}`
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("openrouter", "sk-working"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "openrouter"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		openRouterLogin: func(context.Context, provideroauth.OpenRouterOptions) (string, error) {
			// Another process edits config.json while the browser flow is open.
			if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
				t.Fatalf("seed the mid-authorization config: %v", err)
			}
			return "sk-minted", nil
		},
	})

	// A login that could not be persisted is a failure, not a success.
	if code == exitSuccess {
		t.Fatalf("exit = %d, want non-zero for an unsaved login: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("stderr = %q, want ambiguous persisted-name rejection", stderr.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != seed {
		t.Fatalf("rejected login rewrote config:\n%s", after)
	}
	key, ok, err := store.Get("openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != "sk-working" {
		t.Fatalf("stored key does not match (present=%v, len=%d), want the previous sk-working preserved", ok, len(key))
	}
	// The minted key is still handed over for manual use because publication
	// failed only after the browser flow completed.
	if !strings.Contains(stdout.String(), "sk-minted") {
		t.Fatalf("stdout = %q, want the manual-export hint with the minted key", stdout.String())
	}
}

// auth logout deletes the secret by normalized identity, so the marker cleanup
// must use the same relation: a mixed-case argument against a lowercase row
// previously left apiKeyStored:true with no secret behind it.
func TestRunAuthLogoutClearsMarkerForCaseVariantSpelling(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	withAuthStore(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-work"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "logout", "WORK"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	cfg := readCLIConfigFixture(t, configPath)
	if cfg.Providers[0].APIKeyStored {
		t.Fatal("logout left a marker claiming a credential it deleted")
	}
	if _, ok, err := store.Get("work"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("logout left the stored key behind")
	}
}
func TestRunAuthStatusResolvesCatalogLoginCandidates(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"my-xai","catalogId":"xai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "secret", Account: "catalog-account"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "status", "my-xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "catalog-account") || !strings.Contains(stdout.String(), "xai") {
		t.Fatalf("status did not find the catalog-addressed login: %q", stdout.String())
	}
}

func TestRunAuthLoginRevalidatesConfigImmediatelyBeforeSave(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	initial := `{"providers":[{"name":"demo"}]}`
	ambiguous := `{"providers":[{"name":"demo"},{"name":"DEMO"}]}`
	if err := os.WriteFile(configPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("demo"), oauth.Token{AccessToken: "unchanged"}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"device_code":"dc","user_code":"code","verification_uri":"https://example.test","expires_in":60,"interval":1}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.WriteFile(configPath, []byte(ambiguous), 0o600); err != nil {
			t.Errorf("mutate config: %v", err)
		}
		_, _ = io.WriteString(w, `{"access_token":"replacement","token_type":"Bearer"}`)
	})
	t.Setenv("ZERO_OAUTH_DEMO_CLIENT_ID", "client")
	t.Setenv("ZERO_OAUTH_DEMO_TOKEN_URL", server.URL+"/token")
	t.Setenv("ZERO_OAUTH_DEMO_DEVICE_URL", server.URL+"/device")
	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "login", "demo", "--device"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("exit = %d, stderr = %q; want ambiguous-config failure", code, stderr.String())
	}
	token, ok, err := store.Load(oauth.ProviderKey("demo"))
	if err != nil || !ok || token.AccessToken != "unchanged" {
		t.Fatalf("stored token = %+v, %v, %v; want unchanged", token, ok, err)
	}
}

func TestRunAuthChatGPTRevalidatesConfigImmediatelyBeforeSave(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"chatgpt","catalogId":"chatgpt"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "unchanged"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "chatgpt"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		chatGPTLogin: func(context.Context, provideroauth.ChatGPTOptions) (oauth.Token, error) {
			ambiguous := `{"providers":[{"name":"chatgpt"},{"name":"ChatGPT"}]}`
			if err := os.WriteFile(configPath, []byte(ambiguous), 0o600); err != nil {
				return oauth.Token{}, err
			}
			return oauth.Token{AccessToken: "replacement"}, nil
		},
	})
	if code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("exit = %d, stderr = %q; want ambiguous-config failure", code, stderr.String())
	}
	token, ok, err := store.Load(oauth.ProviderKey("chatgpt"))
	if err != nil || !ok || token.AccessToken != "unchanged" {
		t.Fatalf("stored token = %+v, %v, %v; want unchanged", token, ok, err)
	}
}

// TestRunAuthChatGPTAllowsCaseVariantPersistedProfile is the regression test for
// jatmn's #725 finding: preflighting a login as if it were a new provider write
// rejected the very row it was logging into. A config whose sole ChatGPT profile
// is spelled "ChatGPT" made `zero auth chatgpt` fail before the browser flow with
// `provider "chatgpt" already exists as "ChatGPT"` — while the TUI, which only
// validates the file, completed the same login. A login mints no new spelling,
// but reuse still requires the row's catalogId to prove catalog ownership.
func TestRunAuthChatGPTAllowsCaseVariantPersistedProfile(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"ChatGPT","catalogId":"chatgpt"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "chatgpt"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		chatGPTLogin: func(context.Context, provideroauth.ChatGPTOptions) (oauth.Token, error) {
			return oauth.Token{AccessToken: "fresh"}, nil
		},
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want success; stderr = %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "already exists as") {
		t.Fatalf("a case-variant re-login must not be treated as a colliding new provider: %q", stderr.String())
	}
	token, ok, err := store.Load(oauth.ProviderKey("chatgpt"))
	if err != nil || !ok || token.AccessToken != "fresh" {
		t.Fatalf("stored token = %+v, %v, %v; want the fresh login saved", token, ok, err)
	}
	// The ambiguous-config guard is unchanged: a login still refuses to run
	// against a file with two case-duplicate rows.
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"chatgpt"},{"name":"ChatGPT"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runWithDeps([]string{"auth", "chatgpt"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		chatGPTLogin: func(context.Context, provideroauth.ChatGPTOptions) (oauth.Token, error) {
			return oauth.Token{AccessToken: "should-not-save"}, nil
		},
	})
	if code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("exit = %d, stderr = %q; want the ambiguous-config failure preserved", code, stderr.String())
	}
}

// A config written before catalog ids existed has a row NAMED for the provider
// and no catalogId at all. Strict ownership refused that login with an error
// the user could not act on, while the fix — `zero providers add chatgpt` —
// appeared nowhere. An absent catalog id is not a competing claim, so the login
// backfills it onto the row it already found.
func TestRunAuthChatGPTAdoptsLegacyProfileWithoutCatalogID(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"activeProvider":"other","providers":[{"name":"other","catalogId":"xai"},{"name":"ChatGPT","model":"corp-model"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "chatgpt"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		chatGPTLogin: func(context.Context, provideroauth.ChatGPTOptions) (oauth.Token, error) {
			return oauth.Token{AccessToken: "fresh"}, nil
		},
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, want success; stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "does not prove ownership") {
		t.Fatalf("legacy profile must be adopted, not refused: %q %q", stdout.String(), stderr.String())
	}
	if token, ok, err := store.Load(oauth.ProviderKey("chatgpt")); err != nil || !ok || token.AccessToken != "fresh" {
		t.Fatalf("stored token = %+v, %v, %v; want the fresh login saved", token, ok, err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %+v, want the legacy row adopted rather than a duplicate added", cfg.Providers)
	}
	adopted := cfg.Providers[1]
	if adopted.Name != "ChatGPT" || adopted.CatalogID != "chatgpt" || adopted.Model != "corp-model" {
		t.Fatalf("adopted row = %+v, want catalogId backfilled and the user's name/model kept", adopted)
	}
	if cfg.ActiveProvider != "other" {
		t.Fatalf("activeProvider = %q, want the user's active provider untouched", cfg.ActiveProvider)
	}
}

func TestRunAuthRefreshResolvesCatalogLoginCandidates(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"my-xai","catalogId":"xai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{
		AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("ZERO_OAUTH_XAI_TOKEN_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "refresh", "my-xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	token, ok, err := store.Load(oauth.ProviderKey("xai"))
	if err != nil || !ok || token.AccessToken != "fresh" {
		t.Fatalf("refreshed token = %+v, ok = %v, err = %v", token, ok, err)
	}
}

func TestAuthReadCommandsRejectAmbiguousCatalogAddress(t *testing.T) {
	withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work-xai","catalogId":"xai"},{"name":"personal-xai","catalogId":"xai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"auth", "status", "xai"}, {"auth", "refresh", "xai"}} {
		var stdout, stderr bytes.Buffer
		if code := runWithDeps(args, &stdout, &stderr, appDeps{
			userConfigPath: func() (string, error) { return configPath, nil },
		}); code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous") {
			t.Fatalf("args %v: exit = %d, stdout = %q, stderr = %q; want ambiguity", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunAuthLoginRejectsAmbiguousCatalogAddress(t *testing.T) {
	withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work-xai","catalogId":"xai"},{"name":"personal-xai","catalogId":"xai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "login", "xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q; want pre-login ambiguity", code, stdout.String(), stderr.String())
	}
}

// TestRunAuthLogoutResolvesCatalogIdentity covers jatmn's #725 finding: login
// accepts a catalog id and stores its token under that key, and the TUI tells
// users to run `zero auth logout chatgpt` — but logout hard-stopped whenever a
// persisted row matched case-insensitively without matching exactly, so the
// documented command left the token and any stored key in place.
func TestRunAuthLogoutResolvesCatalogIdentity(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"ChatGPT","catalogId":"chatgpt"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "chatgpt"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "capitalization") {
		t.Fatalf("logout refused the spelling the UI documents: %q", stderr.String())
	}
	if _, ok, err := store.Load(oauth.ProviderKey("chatgpt")); err != nil || ok {
		t.Fatalf("stored token survived logout: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(stdout.String(), "Logged out") {
		t.Fatalf("stdout = %q, want a logout confirmation", stdout.String())
	}
}

// TestRunAuthLogoutDeletesCatalogIDToken covers jatmn's #725 follow-up
// finding: a profile addressed by its own name but logged in under its
// catalog id (e.g. {name:"my-xai", catalogId:"xai"} via `zero auth login
// xai`) left the "xai" OAuth token behind when logged out as "my-xai",
// because logout only ever deleted the exact spelling the user typed.
func TestRunAuthLogoutDeletesCatalogIDToken(t *testing.T) {
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"my-xai","catalogId":"xai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "my-xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if _, ok, err := store.Load(oauth.ProviderKey("xai")); err != nil || ok {
		t.Fatalf("catalog-id OAuth token survived logout: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(stdout.String(), "Logged out") {
		t.Fatalf("stdout = %q, want a logout confirmation", stdout.String())
	}
}

// Catalog ids share the flat API-key store with dictation, so logout must leave
// a catalog alias alone when dictation also claims it.
func TestRunAuthLogoutPreservesDictationCatalogAPIKey(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"my-groq","catalogId":"groq","apiKeyStored":true}],"stt":{"provider":"groq"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("my-groq", "chat-key"); err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("groq", "dictation-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "groq"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if _, ok, err := keyStore.Get("my-groq"); err != nil || ok {
		t.Fatalf("persisted row API key survived logout: ok=%v err=%v", ok, err)
	}
	if key, ok, err := keyStore.Get("groq"); err != nil || !ok || key != "dictation-key" {
		t.Fatalf("dictation key = %q, %v, %v; want it preserved", key, ok, err)
	}
	if !strings.Contains(stdout.String(), "Logged out") {
		t.Fatalf("stdout = %q, want a logout confirmation", stdout.String())
	}
}

// TestRunAuthLogoutDeletesCatalogIDAPIKey covers jatmn's #725 finding: a key
// stored under a profile's exclusive catalog id by a catalog-style auth flow
// must not survive logout of that profile's persisted name.
func TestRunAuthLogoutDeletesCatalogIDAPIKey(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"my-xai","catalogId":"xai","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("xai", "catalog-flow-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "my-xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if _, ok, err := keyStore.Get("xai"); err != nil || ok {
		t.Fatalf("catalog-id API key survived logout: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(stdout.String(), "Logged out") {
		t.Fatalf("stdout = %q, want a logout confirmation", stdout.String())
	}
}

func TestRunAuthLogoutWithoutPersistedRowPreservesDictationKey(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"stt":{"provider":"groq"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("groq", "dictation-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "groq"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if key, ok, err := keyStore.Get("groq"); err != nil || !ok || key != "dictation-key" {
		t.Fatalf("dictation key = %q, %v, %v; want it preserved", key, ok, err)
	}
}

// TestRunAuthLogoutKeepsDistinctUnicodeCredentials pins the end-to-end
// guarantee behind jatmn's #725 finding that destructive candidate expansion
// used strings.EqualFold as authority for credential ownership: a saved "s"
// profile with its own token and key must survive `zero auth logout ſ`, which
// names a provider the config never saved (the credential store defines
// identity with credstore.NormalizeProvider, under which "s" and Unicode
// long-s "ſ" are separate entries).
//
// Two independent layers now enforce that, and this test deliberately asserts
// the outcome rather than either mechanism. The one that fires first is
// oauth.ValidateKey, which rejects the non-ASCII spelling before any deletion
// runs — so the folded-name adoption itself is pinned where it is reachable, in
// TestPersistedProviderIdentityRulesMatchTheCredentialStore (internal/config).
func TestRunAuthLogoutKeepsDistinctUnicodeCredentials(t *testing.T) {
	const longS = "ſ"
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	storePath := withAuthStore(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"s","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := tokens.Save(oauth.ProviderKey("s"), oauth.Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("s", "long-s-is-not-s"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	runWithDeps([]string{"auth", "logout", longS}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})

	if _, ok, err := tokens.Load(oauth.ProviderKey("s")); err != nil || !ok {
		t.Fatalf("logging out of a distinct identity deleted the saved token: ok=%v err=%v", ok, err)
	}
	if key, ok, err := keyStore.Get("s"); err != nil || !ok || key != "long-s-is-not-s" {
		t.Fatalf("stored API key = %q, %v, %v; want the unrelated profile untouched", key, ok, err)
	}
	saved := readFileConfig(t, configPath).Providers
	if len(saved) != 1 || saved[0].Name != "s" || !saved[0].APIKeyStored {
		t.Fatalf("providers = %+v, want the saved profile keeping its stored-key marker", saved)
	}
}

// TestRunAuthLogoutRejectsUnrelatedAmbiguousConfigBeforeCredentialDeletion
// verifies that fail-closed config validation runs before any credential is
// changed, even when the addressed profile itself is unambiguous.
func TestRunAuthLogoutRejectsUnrelatedAmbiguousConfigBeforeCredentialDeletion(t *testing.T) {
	setCLIUserConfigRoot(t)
	storePath := withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "config.json")
	configData := []byte(`{"providers":[{"name":"demo"},{"name":"DEMO"},{"name":"my-xai","catalogId":"xai","apiKeyStored":true}]}`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("xai", "catalog-id-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "my-xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("exit = %d stderr = %q, want the unrelated ambiguity surfaced as a truthful marker-update failure", code, stderr.String())
	}
	if _, ok, err := store.Load(oauth.ProviderKey("xai")); err != nil || !ok {
		t.Fatalf("catalog-id OAuth token changed after rejected logout: ok=%v err=%v", ok, err)
	}
	if _, ok, err := keyStore.Get("xai"); err != nil || !ok {
		t.Fatalf("catalog-id API key changed after rejected logout: ok=%v err=%v", ok, err)
	}
}

func TestRunAuthLogoutPreservesCredentialsWhenConfigIsAmbiguous(t *testing.T) {
	storePath := withAuthStore(t)
	configHome := setCLIUserConfigRoot(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(configHome, "zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"providers":[{"name":"demo"},{"name":"DEMO","apiKeyStored":true}]}`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("demo"), oauth.Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("demo", "stored-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "demo"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("exit = %d stderr = %q, want truthful marker-update failure", code, stderr.String())
	}
	if _, ok, err := store.Load(oauth.ProviderKey("demo")); err != nil || !ok {
		t.Fatalf("OAuth credential changed after rejected logout: ok=%v err=%v", ok, err)
	}
	if _, ok, err := keyStore.Get("demo"); err != nil || !ok {
		t.Fatalf("API key changed after rejected logout: ok=%v err=%v", ok, err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(after, configData) {
		t.Fatalf("invalid config changed during recovery logout: err=%v content=%s", err, after)
	}
}

// TestRunAuthOpenRouterPreflightsBeforeTheBrowserFlow covers the second half of
// the same finding: every other auth entry point validates the config before
// opening a browser, and this one minted a key first and only discovered the
// config was unusable when trying to save it.
func TestRunAuthOpenRouterPreflightsBeforeTheBrowserFlow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work"},{"name":"WORK"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loginCalled := false
	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "openrouter"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		openRouterLogin: func(context.Context, provideroauth.OpenRouterOptions) (string, error) {
			loginCalled = true
			return "sk-should-not-be-minted", nil
		},
	})
	if code == exitSuccess {
		t.Fatalf("an unusable config must fail before login; stdout = %q", stdout.String())
	}
	if loginCalled {
		t.Fatal("the browser flow ran before the config was validated")
	}
	if !strings.Contains(stderr.String(), "ambiguous persisted provider names") {
		t.Fatalf("stderr = %q, want the config error", stderr.String())
	}
}

// TestRunAuthOpenRouterFailsWhenTheKeyCannotBeSaved pins the exit code: the
// minted key is still printed so the user does not lose it, but nothing was
// persisted, and reporting success left a script believing the provider was
// configured.
func TestRunAuthOpenRouterFailsWhenTheKeyCannotBeSaved(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loginComplete := false
	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "openrouter"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) {
			if loginComplete {
				// The preflight passed; break the path only for the save that follows.
				return "", errors.New("config path unavailable")
			}
			return configPath, nil
		},
		openRouterLogin: func(context.Context, provideroauth.OpenRouterOptions) (string, error) {
			loginComplete = true
			return "sk-openrouter-test", nil
		},
	})
	if code == exitSuccess {
		t.Fatal("a failed save must not report success")
	}
	if !strings.Contains(stdout.String(), "sk-openrouter-test") {
		t.Fatalf("stdout = %q, want the minted key printed so it is not lost", stdout.String())
	}
	if !strings.Contains(stderr.String(), "could not save") {
		t.Fatalf("stderr = %q, want the save failure reported", stderr.String())
	}
}

func TestRunAuthRefreshRejectsEmptyCredentialCandidates(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--watch"}} {
		args := append([]string{"auth", "refresh", "   "}, extra...)
		var stdout, stderr bytes.Buffer
		code := runWithDeps(args, &stdout, &stderr, appDeps{
			userConfigPath: func() (string, error) { return configPath, nil },
		})
		if code != exitCrash || stdout.Len() != 0 || !strings.Contains(stderr.String(), "no credential candidates") {
			t.Fatalf("args = %q exit = %d stdout = %q stderr = %q, want explicit empty-candidate app error", args, code, stdout.String(), stderr.String())
		}
	}
}

// TestRunAuthLogoutLeavesSharedCatalogCredentialsAlone covers jatmn's #725
// finding that logout cleanup was scoped by catalog id rather than by proven
// profile ownership. Catalog ids are shared by design: stored-key "work-xai",
// stored-key "xai", and keyless "personal-xai" can all carry catalogId "xai".
// Logging out "work-xai" deleted the shared "xai" OAuth token and the "xai"
// profile's API key — another profile's credentials — while clearing only
// work-xai's own marker.
func TestRunAuthLogoutLeavesSharedCatalogCredentialsAlone(t *testing.T) {
	storePath := withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	configData := []byte(`{"providers":[` +
		`{"name":"work-xai","catalogId":"xai","apiKeyStored":true},` +
		`{"name":"xai","catalogId":"xai","apiKeyStored":true},` +
		`{"name":"personal-xai","catalogId":"xai"}]}`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "shared"}); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("xai", "sibling-key"); err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("work-xai", "own-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithDeps([]string{"auth", "logout", "work-xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if _, ok, err := keyStore.Get("work-xai"); err != nil || ok {
		t.Fatalf("the profile's own API key must be deleted: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Load(oauth.ProviderKey("xai")); err != nil || !ok {
		t.Fatalf("a catalog token three profiles can use must survive one profile's logout: ok=%v err=%v", ok, err)
	}
	if _, ok, err := keyStore.Get("xai"); err != nil || !ok {
		t.Fatalf("the sibling xai profile's API key must survive: ok=%v err=%v", ok, err)
	}
}

func TestRunAuthLogoutRejectsAmbiguousCatalogAddress(t *testing.T) {
	storePath := withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "config.json")
	configData := []byte(`{"providers":[` +
		`{"name":"work-xai","catalogId":"xai"},` +
		`{"name":"personal-xai","catalogId":"xai"}]}`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenStore, err := oauth.NewStore(oauth.StoreOptions{FilePath: storePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenStore.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "shared"}); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("xai", "shared-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "logout", "xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code == exitSuccess || !strings.Contains(stderr.String(), "ambiguous") {
		t.Fatalf("exit succeeded or ambiguity was not reported: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, ok, err := tokenStore.Load(oauth.ProviderKey("xai")); err != nil || !ok {
		t.Fatalf("ambiguous catalog token was deleted: ok=%v err=%v", ok, err)
	}
	if _, ok, err := keyStore.Get("xai"); err != nil || !ok {
		t.Fatalf("ambiguous catalog key was deleted: ok=%v err=%v", ok, err)
	}
}

// TestRunAuthLogoutPrefersTheExactlyNamedProfile is the other half of the same
// finding: identity resolution took the first row matching name OR catalog id,
// so `zero auth logout xai` retargeted an earlier {name:"work-xai",
// catalogId:"xai"} row and cleared that profile's marker instead.
func TestRunAuthLogoutPrefersTheExactlyNamedProfile(t *testing.T) {
	withAuthStore(t)
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	configData := []byte(`{"providers":[` +
		`{"name":"work-xai","catalogId":"xai","apiKeyStored":true},` +
		`{"name":"xai","catalogId":"xai","apiKeyStored":true}]}`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	keyStore, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("work-xai", "work-key"); err != nil {
		t.Fatal(err)
	}
	if err := keyStore.Set("xai", "own-key"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"auth", "logout", "xai"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if _, ok, err := keyStore.Get("xai"); err != nil || ok {
		t.Fatalf("the exactly named profile's key must be deleted: ok=%v err=%v", ok, err)
	}
	if _, ok, err := keyStore.Get("work-xai"); err != nil || !ok {
		t.Fatalf("an earlier catalog sibling must not be logged out instead: ok=%v err=%v", ok, err)
	}
	cfg := readFileConfig(t, configPath)
	for _, provider := range cfg.Providers {
		if provider.Name == "xai" && provider.APIKeyStored {
			t.Fatal("the named profile's apiKeyStored marker must be cleared")
		}
		if provider.Name == "work-xai" && !provider.APIKeyStored {
			t.Fatal("the sibling profile's apiKeyStored marker must be left alone")
		}
	}
}
