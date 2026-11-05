package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/config"
)

// `zero mcp disable exa` must work even though exa is a built-in
// default that is not written to the user's config file until overridden.
func TestRunMCPDisableSeededExaDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	// A config with no Exa entry — the default lives in code, not the file.
	writeMCPCommandRawConfig(t, configPath, `{"activeProvider":"fast"}`)

	var out, errBuf bytes.Buffer
	code := runWithDeps([]string{"mcp", "disable", "exa", "--json"}, &out, &errBuf, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})
	if code != exitSuccess {
		t.Fatalf("disable exit=%d stderr=%s", code, errBuf.String())
	}
	var payload struct {
		ServerName string `json:"serverName"`
		Disabled   bool   `json:"disabled"`
		Changed    bool   `json:"changed"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode disable JSON: %v\n%s", err, out.String())
	}
	if payload.ServerName != "exa" || !payload.Disabled || !payload.Changed {
		t.Fatalf("disable payload = %#v, want exa disabled+changed", payload)
	}

	// End-to-end: resolving that config now turns the default off.
	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	if !cfg.Servers["exa"].Disabled {
		t.Fatal("expected `mcp disable exa` to turn the seeded default off in the resolved config")
	}
}

// aimlapiWire stands in for https://mcp.aimlapi.com/mcp at the transport layer:
// requests keep their real URL, nothing is dialed, and each one is recorded.
type aimlapiWire struct {
	mu       sync.Mutex
	requests []*http.Request
}

func (wire *aimlapiWire) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		_ = request.Body.Close()
	}
	wire.mu.Lock()
	wire.requests = append(wire.requests, request.Clone(context.Background()))
	wire.mu.Unlock()
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &message)
	respond := func(status int, payload string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: request}
	}
	if request.URL.String() != aimlapi.MCPServerURL {
		return respond(http.StatusBadGateway, `{}`), nil
	}
	if len(message.ID) == 0 {
		return respond(http.StatusAccepted, ""), nil
	}
	result := `{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}`
	if message.Method == "tools/list" {
		result = `{"tools":[{"name":"tools_search","description":"Search the Toolbox","inputSchema":{"type":"object"}}]}`
	}
	return respond(http.StatusOK, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, message.ID, result)), nil
}

func (wire *aimlapiWire) recorded() []*http.Request {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	return append([]*http.Request(nil), wire.requests...)
}

// isolateAIMLAPIMCPCommand redirects every root the real resolve path reads to
// a temporary home, sets an AI/ML API key in the environment, and routes the
// default HTTP transport to a recorder. It returns the user config path.
func isolateAIMLAPIMCPCommand(t *testing.T, key string) (string, *aimlapiWire, appDeps) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("ZERO_CRED_STORAGE", "file")
	t.Setenv("ZERO_PROVIDER", "")
	t.Setenv("AIMLAPI_INFERENCE_URL", "")
	t.Setenv("AIMLAPI_PARTNER_ID", "")
	t.Setenv("AIMLAPI_API_KEY", key)
	configPath, err := config.DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	writeMCPCommandRawConfig(t, configPath, `{}`)

	wire := &aimlapiWire{}
	previous := http.DefaultTransport
	http.DefaultTransport = wire
	t.Cleanup(func() { http.DefaultTransport = previous })

	workspace := t.TempDir()
	return configPath, wire, appDeps{getwd: func() (string, error) { return workspace, nil }}
}

func TestRunMCPEnableAIMLAPIDefaultWritesOnlyTheToggle(t *testing.T) {
	configPath, wire, deps := isolateAIMLAPIMCPCommand(t, "aiml-secret-key")

	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "disable", "aimlapi", "--json"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("disable exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"changed": false`) {
		t.Fatalf("disabling the off-by-default server should be a no-op: %s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := runWithDeps([]string{"mcp", "enable", "aimlapi", "--json"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("enable exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"changed": true`) {
		t.Fatalf("enable should report a change: %s", out.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	if entry := written.MCP.Servers["aimlapi"]; len(entry) != 1 || entry["disabled"] != false {
		t.Fatalf("enable must write only the toggle, got %#v in %s", entry, data)
	}
	if strings.Contains(string(data), "aiml-secret-key") || strings.Contains(string(data), "Authorization") {
		t.Fatalf("config holds credential material: %s", data)
	}

	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if server := cfg.Servers["aimlapi"]; server.Disabled || config.IsUnconfiguredDefault("aimlapi", server) {
		t.Fatalf("enabled aimlapi = %#v, want enabled and user-configured", server)
	}
	// An unrelated save rewrites the whole file through FileConfig; the
	// explicit enable has to survive it or the shipped disable comes back.
	if _, err := config.SetTheme(configPath, "dark"); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["aimlapi"].Disabled {
		data, _ := os.ReadFile(configPath)
		t.Fatalf("an unrelated config save turned aimlapi off again: %s", data)
	}
	if len(wire.recorded()) != 0 {
		t.Fatal("enable must not connect")
	}

	out.Reset()
	if code := runWithDeps([]string{"mcp", "disable", "aimlapi"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("disable exit=%d stderr=%s", code, errBuf.String())
	}
	cfg, err = config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Servers["aimlapi"].Disabled {
		t.Fatal("disable after enable must turn aimlapi off again")
	}
}

func TestRunMCPAddUnderAIMLAPIDefaultNameEnablesIt(t *testing.T) {
	configPath, _, deps := isolateAIMLAPIMCPCommand(t, "")
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "add", "aimlapi", "--type", "http", "--url", aimlapi.MCPServerURL, "--auth", "oauth"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("add exit=%d stderr=%s", code, errBuf.String())
	}
	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if server := cfg.Servers["aimlapi"]; server.Disabled || server.Auth != "oauth" {
		t.Fatalf("added aimlapi = %#v, want enabled with oauth", server)
	}
	if code := runWithDeps([]string{"mcp", "add", "aimlapi", "--type", "http", "--url", aimlapi.MCPServerURL, "--disabled"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("add --disabled exit=%d stderr=%s", code, errBuf.String())
	}
	if cfg, err = config.ResolveMCP(config.ResolveOptions{UserConfigPath: configPath}); err != nil || !cfg.Servers["aimlapi"].Disabled {
		t.Fatalf("add --disabled left aimlapi enabled: %#v, %v", cfg.Servers["aimlapi"], err)
	}
}

// Through the real resolve path (no injected resolveMCPConfig): the default is
// listed as disabled, never connects, and once enabled it reaches session
// startup and `zero mcp check`, which connects with the key on the wire.
func TestAIMLAPIMCPDefaultThroughRealResolvePath(t *testing.T) {
	_, wire, deps := isolateAIMLAPIMCPCommand(t, "aiml-secret-key")

	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "list"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("list exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "aimlapi [http] disabled https://mcp.aimlapi.com/mcp") || strings.Contains(out.String(), "aiml-secret-key") {
		t.Fatalf("list output should show the disabled aimlapi default and no key:\n%s", out.String())
	}
	out.Reset()
	if code := runWithDeps([]string{"mcp", "list", "--json"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("list --json exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"aimlapi"`) || strings.Contains(out.String(), "aiml-secret-key") {
		t.Fatalf("list --json = %s", out.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := runWithDeps([]string{"mcp", "check", "aimlapi"}, &out, &errBuf, deps); code == exitSuccess || !strings.Contains(errBuf.String(), "is disabled") {
		t.Fatalf("check of the disabled default: exit=%d stderr=%s", code, errBuf.String())
	}
	resolved, err := fillAppDeps(deps).resolveMCPConfig(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	critical, optional := splitMCPStartupConfig(resolved)
	if _, ok := critical.Servers["aimlapi"]; ok {
		t.Fatal("the disabled default must not start with a session")
	}
	if _, ok := optional.Servers["aimlapi"]; ok {
		t.Fatal("the disabled default must not start with a session")
	}
	if len(wire.recorded()) != 0 {
		t.Fatalf("the disabled default connected: %d requests", len(wire.recorded()))
	}

	if code := runWithDeps([]string{"mcp", "enable", "aimlapi"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("enable exit=%d stderr=%s", code, errBuf.String())
	}
	resolved, err = fillAppDeps(deps).resolveMCPConfig(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	critical, _ = splitMCPStartupConfig(resolved)
	if _, ok := critical.Servers["aimlapi"]; !ok {
		t.Fatal("an enabled aimlapi must start with the session as a user-configured server")
	}

	out.Reset()
	errBuf.Reset()
	if code := runWithDeps([]string{"mcp", "check", "aimlapi", "--json"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("check exit=%d stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), `"status": "ok"`) || !strings.Contains(out.String(), `"toolCount": 1`) {
		t.Fatalf("check output = %s", out.String())
	}
	if strings.Contains(out.String()+errBuf.String(), "aiml-secret-key") {
		t.Fatalf("check output leaks the key: %s %s", out.String(), errBuf.String())
	}
	requests := wire.recorded()
	if len(requests) == 0 {
		t.Fatal("check did not connect")
	}
	for _, request := range requests {
		if request.URL.String() != aimlapi.MCPServerURL {
			t.Fatalf("request went to %s", request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer aiml-secret-key" {
			t.Fatalf("Authorization = %q, want the AI/ML API key", got)
		}
		if request.Header.Get(aimlapi.SourceHeaderName) != aimlapi.SourceValue || request.Header.Get(aimlapi.PartnerHeaderName) == "" {
			t.Fatalf("attribution headers missing: %v", request.Header)
		}
	}
}
