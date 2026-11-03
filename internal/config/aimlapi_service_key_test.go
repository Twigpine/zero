package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func aimlapiServiceKeyEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestAIMLAPIServiceKeyUsesCanonicalProfileKey(t *testing.T) {
	resolved := ResolvedConfig{Providers: []ProviderProfile{
		{Name: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-openai"},
		{Name: "aimlapi", CatalogID: "aimlapi", BaseURL: "https://api.aimlapi.com/v1/", APIKey: " aiml-key "},
	}}
	key, err := AIMLAPIServiceKey(resolved, nil, aimlapiServiceKeyEnv(nil))
	if err != nil || key != "aiml-key" {
		t.Fatalf("AIMLAPIServiceKey = %q, %v; want aiml-key", key, err)
	}
}

func TestAIMLAPIServiceKeyReadsCredentialStore(t *testing.T) {
	resolved := ResolvedConfig{Provider: ProviderProfile{Name: "aimlapi", CatalogID: "aimlapi", BaseURL: "https://api.aimlapi.com/v1", APIKeyStored: true}}
	store := fakeKeyGetter{keys: map[string]string{"aimlapi": "stored-key"}}
	key, err := AIMLAPIServiceKey(resolved, store, aimlapiServiceKeyEnv(nil))
	if err != nil || key != "stored-key" {
		t.Fatalf("AIMLAPIServiceKey = %q, %v; want stored-key", key, err)
	}
}

func TestAIMLAPIServiceKeyFallsBackToEnvironmentWithoutProfile(t *testing.T) {
	key, err := AIMLAPIServiceKey(ResolvedConfig{}, nil, aimlapiServiceKeyEnv(map[string]string{"AIMLAPI_API_KEY": "env-key"}))
	if err != nil || key != "env-key" {
		t.Fatalf("AIMLAPIServiceKey = %q, %v; want env-key", key, err)
	}
	if _, err := AIMLAPIServiceKey(ResolvedConfig{}, nil, aimlapiServiceKeyEnv(nil)); !errors.Is(err, ErrAIMLAPIKeyMissing) {
		t.Fatalf("missing key error = %v, want ErrAIMLAPIKeyMissing", err)
	}
}

func TestAIMLAPIServiceKeyWithholdsProxyCredentials(t *testing.T) {
	env := aimlapiServiceKeyEnv(map[string]string{"AIMLAPI_API_KEY": "proxy-key"})
	cases := map[string]ResolvedConfig{
		"active aimlapi profile on a proxy": {Provider: ProviderProfile{Name: "aimlapi", CatalogID: "aimlapi", BaseURL: "https://proxy.example/v1", APIKey: "proxy-key"}},
		"a second aimlapi profile on a proxy": {Providers: []ProviderProfile{
			{Name: "aimlapi", CatalogID: "aimlapi", BaseURL: "https://api.aimlapi.com/v1", APIKey: "proxy-key"},
			{Name: "aimlapi-proxy", CatalogID: "aimlapi", BaseURL: "https://proxy.example/v1", APIKey: "proxy-key"},
		}},
		"a custom profile reading AIMLAPI_API_KEY": {Providers: []ProviderProfile{
			{Name: "gateway", BaseURL: "https://proxy.example/v1", APIKeyEnv: "AIMLAPI_API_KEY", APIKey: "proxy-key"},
		}},
	}
	for name, resolved := range cases {
		key, err := AIMLAPIServiceKey(resolved, nil, env)
		if !errors.Is(err, ErrAIMLAPIProxyProfile) || key != "" {
			t.Errorf("%s: AIMLAPIServiceKey = %q, %v; want ErrAIMLAPIProxyProfile", name, key, err)
		}
	}

	overridden := aimlapiServiceKeyEnv(map[string]string{"AIMLAPI_API_KEY": "proxy-key", "AIMLAPI_INFERENCE_URL": "https://staging.example/v1"})
	if key, err := AIMLAPIServiceKey(ResolvedConfig{}, nil, overridden); !errors.Is(err, ErrAIMLAPIProxyProfile) || key != "" {
		t.Fatalf("inference URL override: AIMLAPIServiceKey = %q, %v; want ErrAIMLAPIProxyProfile", key, err)
	}
}

func TestAIMLAPIServiceKeyRejectsKeysThatAreNotOneToken(t *testing.T) {
	for _, raw := range []string{"first\u00a0second", "first\u2003second", "first,second", "first\nsecond", "first\r\nsecond", "a b", "key\x00tail", "key\u0085tail"} {
		resolved := ResolvedConfig{Provider: ProviderProfile{Name: "aimlapi", CatalogID: "aimlapi", BaseURL: "https://api.aimlapi.com/v1", APIKey: raw}}
		if key, err := AIMLAPIServiceKey(resolved, nil, aimlapiServiceKeyEnv(nil)); !errors.Is(err, ErrAIMLAPIKeyInvalid) || key != "" {
			t.Errorf("key %q: AIMLAPIServiceKey = %q, %v; want ErrAIMLAPIKeyInvalid", raw, key, err)
		}
	}
}

func TestLoadAIMLAPIServiceKeyResolvesUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_CRED_STORAGE", "file")
	t.Setenv("ZERO_PROVIDER", "")
	t.Setenv("AIMLAPI_INFERENCE_URL", "")
	t.Setenv("AIMLAPI_API_KEY", "env-key")

	if key, err := LoadAIMLAPIServiceKey(); err != nil || key != "env-key" {
		t.Fatalf("without config: LoadAIMLAPIServiceKey = %q, %v; want env-key", key, err)
	}

	path, err := DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	proxy := `{"activeProvider":"aimlapi","providers":[{"name":"aimlapi","catalogID":"aimlapi","baseURL":"https://proxy.example/v1","model":"m"}]}`
	if err := os.WriteFile(path, []byte(proxy), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadAIMLAPIServiceKey(); !errors.Is(err, ErrAIMLAPIProxyProfile) || key != "" {
		t.Fatalf("proxy profile: LoadAIMLAPIServiceKey = %q, %v; want ErrAIMLAPIProxyProfile", key, err)
	}

	canonical := `{"activeProvider":"aimlapi","providers":[{"name":"aimlapi","catalogID":"aimlapi"}]}`
	if err := os.WriteFile(path, []byte(canonical), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadAIMLAPIServiceKey(); err != nil || key != "env-key" {
		t.Fatalf("canonical profile: LoadAIMLAPIServiceKey = %q, %v; want env-key", key, err)
	}
}

func TestLoadAIMLAPIServiceKeyStoreOpenFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_PROVIDER", "")
	t.Setenv("AIMLAPI_INFERENCE_URL", "")
	t.Setenv("AIMLAPI_API_KEY", "env-key")
	t.Setenv("ZERO_CRED_STORAGE", "invalid-test-storage")
	if key, err := LoadAIMLAPIServiceKey(); err != nil || key != "env-key" {
		t.Fatalf("environment key must not require a store: %q, %v", key, err)
	}
	path, err := DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMLAPI_API_KEY", "")
	data := `{"activeProvider":"aimlapi","providers":[{"name":"aimlapi","catalogID":"aimlapi","model":"m","apiKeyStored":true}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadAIMLAPIServiceKey(); key != "" || err == nil || !strings.Contains(err.Error(), "unknown storage") {
		t.Fatalf("stored key must report store failure: %q, %v", key, err)
	}
	data = `{"activeProvider":"aimlapi","providers":[{"name":"aimlapi","catalogID":"aimlapi","model":"m","apiKeyStored":true,"apiKey":"inline-key"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadAIMLAPIServiceKey(); err != nil || key != "inline-key" {
		t.Fatalf("inline key must not require a store: %q, %v", key, err)
	}
}
