package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
)

func newSecretValuesTestStore(t *testing.T) *TokenStore {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store, err := NewTokenStore(TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "tokens.json"),
		// Pinned to the file backend: NewTokenStore otherwise reads
		// ZERO_OAUTH_STORAGE from the real process environment, and a developer
		// with it set to the keyring would run this against their own keychain.
		Env: map[string]string{},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	return store
}

// SecretValues has to find the tokens that actually exist.
//
// The login path saves IDENTITY-BOUND (SaveForServer), and the name-only Save
// has no non-test callers. A redaction pass that looked a token up by server
// name would therefore come back empty for exactly the servers that hold a real
// bearer, and would look correct in any test that seeded the store by name.
func TestSecretValuesFindsIdentityBoundTokens(t *testing.T) {
	store := newSecretValuesTestStore(t)
	servers, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"linear": {Type: "http", URL: "https://linear.example/mcp", Auth: "oauth"},
	}})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	if err := store.SaveForServer(servers[0], StoredToken{
		AccessToken:  "identity-access-token",
		RefreshToken: "identity-refresh-token",
	}); err != nil {
		t.Fatalf("SaveForServer() error = %v", err)
	}

	// The name-only read is what a per-server lookup would have used. It finding
	// nothing is the whole reason SecretValues enumerates instead.
	if _, ok, err := store.Load("linear"); err == nil && ok {
		t.Fatal("the name-only key resolved, so this test no longer proves identity-bound tokens are reachable")
	}

	values := store.SecretValues()
	for _, want := range []string{"identity-access-token", "identity-refresh-token"} {
		if !containsValue(values, want) {
			t.Errorf("SecretValues() = %q, missing %q", values, want)
		}
	}
}

// A legacy name-keyed token is still live material and must be returned too.
func TestSecretValuesFindsNameKeyedTokens(t *testing.T) {
	store := newSecretValuesTestStore(t)
	if err := store.Save("docs", StoredToken{AccessToken: "legacy-access-token"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !containsValue(store.SecretValues(), "legacy-access-token") {
		t.Errorf("SecretValues() = %q, missing the legacy token", store.SecretValues())
	}
}

// READING SECRETS FOR REDACTION MUST NOT WRITE.
//
// The obvious way to get the material the runtime sends is LoadForServer, which
// is what the bearer path uses. It also migrates a legacy entry to the identity
// key as a side effect, so wiring it into a view builder would make opening a
// panel rewrite the token store on disk and take its cross-process lock. This
// pins that SecretValues does not.
func TestSecretValuesDoesNotWriteToTheStore(t *testing.T) {
	store := newSecretValuesTestStore(t)
	if err := store.Save("docs", StoredToken{AccessToken: "legacy-access-token"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path := store.FilePath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}

	if len(store.SecretValues()) == 0 {
		t.Fatal("SecretValues() returned nothing, so this test would pass without reading anything")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(before) != string(after) {
		t.Error("SecretValues() rewrote the token store; a redaction pass must not mutate it")
	}
	if afterInfo, err := os.Stat(path); err == nil && !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Error("SecretValues() touched the token store file")
	}
}

// A nil store is the production state whenever initialization soft-failed at
// startup, so the method has to answer rather than crash.
func TestSecretValuesOnNilStoreReturnsNothing(t *testing.T) {
	var store *TokenStore
	if got := store.SecretValues(); len(got) != 0 {
		t.Errorf("SecretValues() on a nil store = %q, want nothing", got)
	}
}

// An unreadable store degrades to redacting less, never to failing the caller.
func TestSecretValuesSwallowsAnUnreadableStore(t *testing.T) {
	store := newSecretValuesTestStore(t)
	if err := os.WriteFile(store.FilePath(), []byte(`{"schemaVersion":999}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if got := store.SecretValues(); len(got) != 0 {
		t.Errorf("SecretValues() on an unreadable store = %q, want nothing", got)
	}
}

// LOGIN STORES MORE THAN THE BEARER.
//
// A dynamically registered client secret and a discovered token endpoint are
// stored so that Refresh works without an OAuth block in config, and Refresh
// sends both. The secret is opaque material like the tokens. The endpoint is a
// URL and comes back separately, so a caller can take it apart instead of
// hiding the whole thing; the public client id is neither. Reported by @jatmn.
func TestSecretMaterialIncludesTheStoredClientSecretAndEndpoint(t *testing.T) {
	const endpoint = "https://auth.invalid/token?workspace=opaque-workspace-9f3c2b7"
	store := newSecretValuesTestStore(t)
	if err := store.Save("docs", StoredToken{
		AccessToken:   "access-material",
		ClientID:      "public-client-id",
		ClientSecret:  "cs9f3c",
		TokenEndpoint: endpoint,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	values, endpoints := store.SecretMaterial()
	if !containsValue(values, "cs9f3c") {
		t.Errorf("SecretMaterial() values = %q, missing the stored client secret", values)
	}
	if containsValue(values, "public-client-id") {
		t.Errorf("the public client id was returned as a secret: %q", values)
	}
	if containsValue(values, endpoint) {
		t.Errorf("the endpoint is in the opaque values, so it would be hidden whole: %q", values)
	}
	if len(endpoints) != 1 || endpoints[0] != endpoint {
		t.Errorf("SecretMaterial() endpoints = %q, want the stored token endpoint", endpoints)
	}
	if !containsValue(store.SecretValues(), "cs9f3c") {
		t.Errorf("SecretValues() = %q, missing the stored client secret", store.SecretValues())
	}
	material := store.CredentialMaterial()
	for _, want := range []string{"access-material", "cs9f3c", endpoint} {
		if !containsValue(material, want) {
			t.Errorf("CredentialMaterial() = %q, missing %q", material, want)
		}
	}
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
