package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	cfg := readCLIConfigFixture(t, configPath)
	if cfg.ActiveProvider != "openrouter" || len(cfg.Providers) != 1 {
		t.Fatalf("config = %#v", cfg)
	}
	profile := cfg.Providers[0]
	if profile.Name != "openrouter" || profile.CatalogID != "openrouter" || !profile.APIKeyStored || profile.APIKey != "" || profile.APIKeyEnv != "" {
		t.Fatalf("provider not stored-key sanitized: %#v", profile)
	}
	store, err := config.ProviderKeyStore()
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

	cfg := readCLIConfigFixture(t, configPath)
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
	cfg = readCLIConfigFixture(t, configPath)
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
	cfg := readCLIConfigFixture(t, configPath)
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
// the browser flow is open. A legacy duplicate-row config that appears during
// authorization must still be rejected, and the rejection must not cost the user
// the OpenRouter key they were already working with — the capture is validated
// first, and a rejected publication restores the previous secret rather than
// deleting the shared entry.
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
	store, err := config.ProviderKeyStore()
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
	if !strings.Contains(stdout.String(), "ambiguous persisted provider names") {
		t.Fatalf("stdout = %q, want ambiguous persisted-name rejection", stdout.String())
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
	// The minted key is still handed over for manual use.
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
	store, err := config.ProviderKeyStore()
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
