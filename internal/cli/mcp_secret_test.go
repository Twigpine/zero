package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
)

const (
	testMemlawbPassphrase = "correct-horse-battery-staple"
	testMemlawbAPIKey     = "mk_live_zero_test_key"
)

func storeMemlawbSecrets(t *testing.T, configPath string) {
	t.Helper()
	for name, value := range map[string]string{
		config.MemlawbPassphraseCredential: testMemlawbPassphrase,
		config.MemlawbAPIKeyCredential:     testMemlawbAPIKey,
	} {
		var out, errBuf bytes.Buffer
		code := runWithDeps([]string{"mcp", "secret", "set", name}, &out, &errBuf, appDeps{
			userConfigPath: func() (string, error) { return configPath, nil },
			stdin:          strings.NewReader(value + "\n"),
		})
		if code != exitSuccess {
			t.Fatalf("secret set %s exit=%d stderr=%s", name, code, errBuf.String())
		}
		if strings.Contains(out.String(), value) {
			t.Fatalf("secret set echoed the value: %q", out.String())
		}
	}
}

func TestRunMCPSecretSetStoresValueFromStdin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeMCPCommandRawConfig(t, configPath, `{"activeProvider":"fast"}`)
	storeMemlawbSecrets(t, configPath)

	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	value, ok, err := store.Get(config.MemlawbPassphraseCredential)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != testMemlawbPassphrase {
		t.Fatalf("stored passphrase = %q, %v", value, ok)
	}
	// The value must not land in config.json.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), testMemlawbPassphrase) {
		t.Fatalf("config.json carries the secret: %s", data)
	}
}

func TestRunMCPSecretSetRejectsEmptyValue(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var out, errBuf bytes.Buffer
	code := runWithDeps([]string{"mcp", "secret", "set", "memlawb-passphrase"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		stdin:          strings.NewReader("   \n"),
	})
	if code == exitSuccess {
		t.Fatalf("an empty value must not be stored, stdout=%q", out.String())
	}
}

// AE2: enabling memlawb with both secrets in the credential store writes
// reference NAMES to config.json and neither VALUE.
func TestRunMCPEnableMemlawbWritesReferencesNotValues(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeMCPCommandRawConfig(t, configPath, `{"activeProvider":"fast"}`)
	storeMemlawbSecrets(t, configPath)

	var out, errBuf bytes.Buffer
	code := runWithDeps([]string{"mcp", "enable", "memlawb"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("enable exit=%d stderr=%s", code, errBuf.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	written := string(data)
	if strings.Contains(written, testMemlawbPassphrase) || strings.Contains(written, testMemlawbAPIKey) {
		t.Fatalf("config.json carries a secret value: %s", written)
	}

	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	memlawb := cfg.Servers["memlawb"]
	if memlawb.Disabled {
		t.Fatalf("`mcp enable memlawb` must turn the seeded default on: %#v", memlawb)
	}
	if memlawb.EnvFrom["MEMLAWB_PASSPHRASE"] != config.MemlawbPassphraseCredential ||
		memlawb.EnvFrom["MEMLAWB_API_KEY"] != config.MemlawbAPIKeyCredential {
		t.Fatalf("resolved server lost its credential references: %#v", memlawb)
	}
	if memlawb.Env["MEMLAWB_PASSPHRASE"] != "" || memlawb.Env["MEMLAWB_API_KEY"] != "" {
		t.Fatalf("secrets must never be verbatim env: %#v", memlawb.Env)
	}
}

// Positive control for the assertion above: the same read of the same writer
// DOES find a secret when one is written inline, so the absence assertion is
// not passing against a writer that writes nothing.
func TestRunMCPAddInlineSecretsAreWrittenToConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeMCPCommandRawConfig(t, configPath, `{"activeProvider":"fast"}`)

	var out, errBuf bytes.Buffer
	code := runWithDeps([]string{
		"mcp", "add", "memlawb",
		"--env", "MEMLAWB_PASSPHRASE=" + testMemlawbPassphrase,
		"--env", "MEMLAWB_API_KEY=" + testMemlawbAPIKey,
		"--", "memlawb", "mcp",
	}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("add exit=%d stderr=%s", code, errBuf.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	written := string(data)
	if !strings.Contains(written, testMemlawbPassphrase) || !strings.Contains(written, testMemlawbAPIKey) {
		t.Fatalf("inline env values should be written verbatim; the absence check above is worthless without this: %s", written)
	}
}

func TestRunMCPEnableMemlawbPrintsMinimumVersion(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeMCPCommandRawConfig(t, configPath, `{"activeProvider":"fast"}`)

	var out, errBuf bytes.Buffer
	code := runWithDeps([]string{"mcp", "enable", "memlawb"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("enable exit=%d stderr=%s", code, errBuf.String())
	}
	printed := out.String()
	if !strings.Contains(printed, config.MemlawbMinimumVersion) {
		t.Fatalf("enable must print the minimum memlawb version: %q", printed)
	}
	for _, want := range []string{"zero mcp secret set", config.MemlawbPassphraseCredential, config.MemlawbAPIKeyCredential} {
		if !strings.Contains(printed, want) {
			t.Fatalf("enable output missing %q:\n%s", want, printed)
		}
	}
	// The notice is memlawb-specific: enabling another default must not carry it.
	out.Reset()
	code = runWithDeps([]string{"mcp", "enable", "exa"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("enable exa exit=%d stderr=%s", code, errBuf.String())
	}
	if strings.Contains(out.String(), config.MemlawbMinimumVersion) {
		t.Fatalf("the memlawb notice leaked onto another server: %q", out.String())
	}
}

func TestRunMCPDisableMemlawbAfterEnable(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeMCPCommandRawConfig(t, configPath, `{"activeProvider":"fast"}`)
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "enable", "memlawb"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code != exitSuccess {
		t.Fatalf("enable exit=%d stderr=%s", code, errBuf.String())
	}
	if code := runWithDeps([]string{"mcp", "disable", "memlawb"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}); code != exitSuccess {
		t.Fatalf("disable exit=%d stderr=%s", code, errBuf.String())
	}
	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !cfg.Servers["memlawb"].Disabled {
		t.Fatal("disable must turn it back off")
	}
}
