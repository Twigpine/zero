package mcp

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

// aimlapiRecorder stands in for mcp.aimlapi.com: it records every request it
// is handed (the URL stays https://mcp.aimlapi.com/mcp, nothing is dialed) and
// answers JSON-RPC, or 401 with the server's OAuth challenge when unauthorized.
type aimlapiRecorder struct {
	mu           sync.Mutex
	headers      []http.Header
	urls         []string
	unauthorized bool
	// redirectFirstTo, when set, answers the first request with a 307 to it.
	redirectFirstTo string
	// failStatus, when set, answers every request with it and failBody.
	failStatus int
	failBody   string
}

func (recorder *aimlapiRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		_ = request.Body.Close()
	}
	recorder.mu.Lock()
	recorder.headers = append(recorder.headers, request.Header.Clone())
	recorder.urls = append(recorder.urls, request.URL.String())
	unauthorized := recorder.unauthorized
	redirect := recorder.redirectFirstTo != "" && len(recorder.headers) == 1
	recorder.mu.Unlock()

	if redirect {
		response := fakeHTTPResponse(request, http.StatusTemporaryRedirect, "")
		response.Header.Set("Location", recorder.redirectFirstTo)
		return response, nil
	}
	if recorder.failStatus != 0 {
		return fakeHTTPResponse(request, recorder.failStatus, recorder.failBody), nil
	}
	if unauthorized {
		response := fakeHTTPResponse(request, http.StatusUnauthorized, `{"error":"invalid_token"}`)
		response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.aimlapi.com/.well-known/oauth-protected-resource/mcp"`)
		return response, nil
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &message)
	if len(message.ID) == 0 {
		return fakeHTTPResponse(request, http.StatusAccepted, ""), nil
	}
	result := `{}`
	switch message.Method {
	case "initialize":
		result = `{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}`
	case "tools/list":
		result = `{"tools":[{"name":"tools_search","inputSchema":{"type":"object"}}]}`
	}
	return fakeHTTPResponse(request, http.StatusOK, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, message.ID, result)), nil
}

func (recorder *aimlapiRecorder) recorded() []http.Header {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]http.Header(nil), recorder.headers...)
}

func fakeHTTPResponse(request *http.Request, status int, body string) *http.Response {
	header := http.Header{}
	if body != "" {
		header.Set("Content-Type", "application/json")
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}
}

// isolateAIMLAPIKeyLookup points the real key resolver at an empty temporary
// home and returns the user config path it reads.
func isolateAIMLAPIKeyLookup(t *testing.T, envKey string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	t.Setenv("ZERO_CRED_STORAGE", "file")
	t.Setenv("ZERO_PROVIDER", "")
	t.Setenv("AIMLAPI_INFERENCE_URL", "")
	t.Setenv("AIMLAPI_PARTNER_ID", "")
	t.Setenv("AIMLAPI_API_KEY", envKey)
	path, err := config.DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeAIMLAPIProviderConfig(t *testing.T, path string, baseURL string, apiKey string) {
	t.Helper()
	profile := map[string]any{"name": "aimlapi", "catalogID": "aimlapi", "model": "m", "apiKey": apiKey}
	if baseURL != "" {
		profile["baseURL"] = baseURL
	}
	data, err := json.Marshal(map[string]any{"activeProvider": "aimlapi", "providers": []any{profile}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRecordedNetworkClient(server Server, recorder *aimlapiRecorder) *networkClient {
	return &networkClient{server: server, client: mcpHTTPClient(server, recorder), nextID: 1}
}

func aimlapiTestServer(name string, rawURL string) Server {
	return Server{Name: name, Type: ServerTypeHTTP, URL: rawURL}
}

func assertAIMLAPIAttribution(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get(aimlapi.SourceHeaderName); got != aimlapi.SourceValue {
		t.Fatalf("%s = %q, want %q", aimlapi.SourceHeaderName, got, aimlapi.SourceValue)
	}
	if got, want := header.Get(aimlapi.PartnerHeaderName), aimlapi.ResolvePartnerID(""); got == "" || got != want {
		t.Fatalf("%s = %q, want %q", aimlapi.PartnerHeaderName, got, want)
	}
}

func TestAIMLAPIMCPRequestsCarryServiceKeyAndAttribution(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, "aiml-secret")
	// A hand-added server on the exact host is treated like the built-in one.
	for _, name := range []string{"aimlapi", "media"} {
		recorder := &aimlapiRecorder{}
		client := newRecordedNetworkClient(aimlapiTestServer(name, aimlapi.MCPServerURL), recorder)
		if err := client.initialize(context.Background()); err != nil {
			t.Fatalf("%s: initialize: %v", name, err)
		}
		if _, err := client.ListTools(context.Background()); err != nil {
			t.Fatalf("%s: ListTools: %v", name, err)
		}
		requests := recorder.recorded()
		if len(requests) != 3 {
			t.Fatalf("%s: recorded %d requests, want initialize, initialized and tools/list", name, len(requests))
		}
		for _, header := range requests {
			if got := header.Get("Authorization"); got != "Bearer aiml-secret" {
				t.Fatalf("%s: Authorization = %q, want the AI/ML API key", name, got)
			}
			assertAIMLAPIAttribution(t, header)
		}
	}
}

func TestAIMLAPIMCPHeadersOnlyForExactHost(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, "aiml-secret")
	for _, rawURL := range []string{
		"https://mcp.aimlapi.com.example.net/mcp",
		"http://mcp.aimlapi.com/mcp",
		"https://mcp.aimlapi.com:8443/mcp",
		"https://user@mcp.aimlapi.com/mcp",
		"https://api.aimlapi.com/mcp",
		"https://evilmcp.aimlapi.com/mcp",
		"https://mcp.exa.ai/mcp",
	} {
		recorder := &aimlapiRecorder{}
		server := aimlapiTestServer("other", rawURL)
		server.Headers = map[string]string{"X-Custom": "kept"}
		client := newRecordedNetworkClient(server, recorder)
		if err := client.initialize(context.Background()); err != nil {
			t.Fatalf("%s: initialize: %v", rawURL, err)
		}
		for _, header := range recorder.recorded() {
			if strings.Contains(rawURL, "user@") && strings.HasPrefix(header.Get("Authorization"), "Basic ") {
				// net/http turns URL userinfo into Basic auth on its own.
				header.Del("Authorization")
			}
			// Byte-for-byte what any other MCP server gets.
			want := http.Header{
				"X-Custom":     {"kept"},
				"Content-Type": {"application/json"},
				"Accept":       {"application/json, text/event-stream"},
			}
			if fmt.Sprint(header) != fmt.Sprint(want) {
				t.Fatalf("%s: headers = %v, want %v", rawURL, header, want)
			}
		}
	}

	// Port 443 spelled out and a different host case are still the exact host.
	for _, rawURL := range []string{"https://mcp.aimlapi.com:443/mcp", "https://MCP.AIMLAPI.COM/mcp"} {
		recorder := &aimlapiRecorder{}
		client := newRecordedNetworkClient(aimlapiTestServer("aimlapi", rawURL), recorder)
		if err := client.initialize(context.Background()); err != nil {
			t.Fatalf("%s: initialize: %v", rawURL, err)
		}
		header := recorder.recorded()[0]
		if header.Get("Authorization") != "Bearer aiml-secret" {
			t.Fatalf("%s: Authorization = %q, want the key", rawURL, header.Get("Authorization"))
		}
		assertAIMLAPIAttribution(t, header)
	}
}

// The key and its eligibility are resolved for every request, so switching the
// aimlapi profile to a proxy keeps the proxy's key off mcp.aimlapi.com on the
// very next request of an already-connected client and on new connections.
func TestAIMLAPIMCPKeyIsResolvedPerRequestAcrossProviderSwitch(t *testing.T) {
	path := isolateAIMLAPIKeyLookup(t, "")
	writeAIMLAPIProviderConfig(t, path, "", "canonical-key")

	recorder := &aimlapiRecorder{}
	server := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	client := newRecordedNetworkClient(server, recorder)
	if err := client.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := recorder.recorded()[0].Get("Authorization"); got != "Bearer canonical-key" {
		t.Fatalf("canonical profile: Authorization = %q, want Bearer canonical-key", got)
	}

	writeAIMLAPIProviderConfig(t, path, "https://proxy.example/v1", "proxy-key")
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools after switch: %v", err)
	}
	fresh := newRecordedNetworkClient(server, recorder)
	if err := fresh.initialize(context.Background()); err != nil {
		t.Fatalf("initialize after switch: %v", err)
	}
	requests := recorder.recorded()
	for _, header := range requests[2:] {
		if got := header.Get("Authorization"); got != "" {
			t.Fatalf("proxy profile: Authorization = %q, want none", got)
		}
		assertAIMLAPIAttribution(t, header)
	}
	for _, header := range requests {
		if strings.Contains(fmt.Sprint(header), "proxy-key") {
			t.Fatalf("proxy key reached mcp.aimlapi.com: %v", header)
		}
	}
}

func TestAIMLAPIMCPUnusableKeySendsNoAuthorization(t *testing.T) {
	cases := map[string]string{
		"multi-line key pool": "first-key\nsecond-key",
		"comma key pool":      "first-key,second-key",
		"no key":              "",
	}
	for name, apiKey := range cases {
		t.Run(name, func(t *testing.T) {
			path := isolateAIMLAPIKeyLookup(t, "")
			writeAIMLAPIProviderConfig(t, path, "", apiKey)
			recorder := &aimlapiRecorder{unauthorized: true}
			client := newRecordedNetworkClient(aimlapiTestServer("aimlapi", aimlapi.MCPServerURL), recorder)
			err := client.initialize(context.Background())
			if err == nil {
				t.Fatal("initialize succeeded against a 401")
			}
			requests := recorder.recorded()
			if len(requests) != 1 {
				t.Fatalf("recorded %d requests, want one intact request", len(requests))
			}
			header := requests[0]
			if got := header.Get("Authorization"); got != "" {
				t.Fatalf("Authorization = %q, want none", got)
			}
			if header.Get("Content-Type") != "application/json" {
				t.Fatalf("request headers broken: %v", header)
			}
			assertAIMLAPIAttribution(t, header)
			if !strings.Contains(err.Error(), "returned HTTP 401") || !strings.Contains(err.Error(), "zero mcp oauth login aimlapi") || !strings.Contains(err.Error(), "AIMLAPI_API_KEY") {
				t.Fatalf("401 error lacks the authentication hint: %v", err)
			}
			if strings.Contains(err.Error(), "first-key") {
				t.Fatalf("error leaks key material: %v", err)
			}
		})
	}
}

func TestAIMLAPIMCPUnauthorizedHintOnlyForAIMLAPIWithoutOAuth(t *testing.T) {
	if hint := aimlapiUnauthorizedHint(aimlapiTestServer("docs", "https://mcp.example.com/mcp"), http.StatusUnauthorized); hint != "" {
		t.Fatalf("hint for another server = %q", hint)
	}
	oauth := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	oauth.Auth = ServerAuthOAuth
	if hint := aimlapiUnauthorizedHint(oauth, http.StatusUnauthorized); hint != "" {
		t.Fatalf("hint for an OAuth server = %q", hint)
	}
	if hint := aimlapiUnauthorizedHint(aimlapiTestServer("aimlapi", aimlapi.MCPServerURL), http.StatusForbidden); hint != "" {
		t.Fatalf("hint for 403 = %q", hint)
	}
}

func TestAIMLAPIMCPKeyDefersToUserAuthorizationOAuthAndProjectConfig(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, "aiml-secret")

	userHeader := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	userHeader.Headers = map[string]string{"authorization": "Bearer user-token"}
	oauth := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	oauth.Auth = ServerAuthOAuth
	project := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	project.ProjectConfigured = true

	cases := []struct {
		name   string
		server Server
		want   string
	}{
		{"user Authorization header", userHeader, "Bearer user-token"},
		{"OAuth server", oauth, ""},
		{"project-configured server", project, ""},
	}
	for _, tc := range cases {
		recorder := &aimlapiRecorder{}
		client := newRecordedNetworkClient(tc.server, recorder)
		if err := client.initialize(context.Background()); err != nil {
			t.Fatalf("%s: initialize: %v", tc.name, err)
		}
		header := recorder.recorded()[0]
		if got := header.Get("Authorization"); got != tc.want {
			t.Fatalf("%s: Authorization = %q, want %q", tc.name, got, tc.want)
		}
		assertAIMLAPIAttribution(t, header)
	}
}

func TestAIMLAPIMCPSSERequestsCarryKeyAndAttribution(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, "aiml-secret")
	server := Server{Name: "aimlapi", Type: ServerTypeSSE, URL: aimlapi.MCPServerURL}
	client := &remoteSSEClient{server: server}
	apply := map[string]func(*http.Request){
		"stream": client.applyStreamHeaders,
		"post":   client.applyPostHeaders,
	}
	for name, applyHeaders := range apply {
		request, err := http.NewRequest(http.MethodPost, aimlapi.MCPServerURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		applyHeaders(request)
		if got := request.Header.Get("Authorization"); got != "Bearer aiml-secret" {
			t.Fatalf("%s: Authorization = %q, want the key", name, got)
		}
		assertAIMLAPIAttribution(t, request.Header)

		other, err := http.NewRequest(http.MethodPost, "https://mcp.aimlapi.com.example.net/mcp", nil)
		if err != nil {
			t.Fatal(err)
		}
		applyHeaders(other)
		if other.Header.Get("Authorization") != "" || other.Header.Get(aimlapi.SourceHeaderName) != "" || other.Header.Get(aimlapi.PartnerHeaderName) != "" {
			t.Fatalf("%s: lookalike host got AI/ML API headers: %v", name, other.Header)
		}
	}
}

func TestAIMLAPIMCPKeyStaysOutOfServerConfigAndIdentity(t *testing.T) {
	enabled := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"aimlapi": {Type: "http", URL: aimlapi.MCPServerURL},
	}}
	isolateAIMLAPIKeyLookup(t, "")
	withoutKey, err := NormalizeConfig(enabled)
	if err != nil {
		t.Fatal(err)
	}
	isolateAIMLAPIKeyLookup(t, "aiml-secret")
	withKey, err := NormalizeConfig(enabled)
	if err != nil {
		t.Fatal(err)
	}
	if len(withKey) != 1 || withKey[0].Identity != withoutKey[0].Identity {
		t.Fatalf("server identity depends on the key: %#v vs %#v", withKey, withoutKey)
	}
	if strings.Contains(fmt.Sprintf("%#v", withKey), "aiml-secret") {
		t.Fatalf("normalized server carries the key: %#v", withKey)
	}

	// The shipped default is off, so it is not even normalized for a session.
	defaults, err := NormalizeConfig(config.MCPConfig{Servers: config.DefaultMCPServers()})
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range defaults {
		if server.Name == "aimlapi" {
			t.Fatalf("disabled aimlapi default was normalized for connection: %#v", server)
		}
	}
}

func TestAIMLAPIMCPUnauthorizedHintRespectsCredentialEligibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		headers   map[string]string
		project   bool
		wantSetup bool
	}{
		{"default", nil, false, true},
		{"project", nil, true, false},
		{"custom bearer", map[string]string{"Authorization": "Bearer manual"}, false, false},
		{"mixed case header", map[string]string{"aUtHoRiZaTiOn": "Bearer manual"}, false, false},
		{"empty header", map[string]string{"authorization": ""}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
			server.Headers = tc.headers
			server.ProjectConfigured = tc.project
			hint := aimlapiUnauthorizedHint(server, http.StatusUnauthorized)
			if strings.Contains(hint, "zero providers setup aimlapi") != tc.wantSetup || !strings.Contains(hint, "zero mcp oauth login aimlapi") {
				t.Fatalf("incorrect hint: %s", hint)
			}
		})
	}
}

// net/http copies the first request's headers onto every redirect hop without
// calling the header hooks, so the redirect guard has to apply the per-request
// rule again: a same-origin hop that fails the exact-URL rule (userinfo) gets
// neither the key nor attribution, an eligible hop gets a freshly resolved key,
// and a user's own Authorization header still follows as before.
func TestAIMLAPIMCPRedirectHopsReapplyAutomaticHeaders(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, "")
	userHeader := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	userHeader.Headers = map[string]string{"authorization": "Bearer user-token"}
	for _, tc := range []struct {
		name            string
		server          Server
		location        string
		wantFirst       string
		wantHop         string
		wantAttribution bool
	}{
		{"userinfo hop", aimlapiTestServer("aimlapi", aimlapi.MCPServerURL), "https://user@mcp.aimlapi.com/mcp", "Bearer aiml-first", "", false},
		{"eligible hop", aimlapiTestServer("aimlapi", aimlapi.MCPServerURL), "https://mcp.aimlapi.com/mcp/", "Bearer aiml-first", "Bearer aiml-second", true},
		{"user header hop", userHeader, "https://user@mcp.aimlapi.com/mcp", "Bearer user-token", "Bearer user-token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := []string{"aiml-first", "aiml-second"}
			previous := loadAIMLAPIServiceKey
			loadAIMLAPIServiceKey = func() (string, error) {
				key := keys[0]
				if len(keys) > 1 {
					keys = keys[1:]
				}
				return key, nil
			}
			t.Cleanup(func() { loadAIMLAPIServiceKey = previous })

			recorder := &aimlapiRecorder{redirectFirstTo: tc.location}
			client := newRecordedNetworkClient(tc.server, recorder)
			if err := client.initialize(context.Background()); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			requests := recorder.recorded()
			if len(requests) < 2 || recorder.urls[1] != tc.location {
				t.Fatalf("redirect was not followed: %v", recorder.urls)
			}
			if got := requests[0].Get("Authorization"); got != tc.wantFirst {
				t.Fatalf("first hop Authorization = %q, want %q", got, tc.wantFirst)
			}
			hop := requests[1]
			// With no key, net/http fills Basic auth from the hop's own userinfo;
			// only the automatic key must be absent.
			if got := hop.Get("Authorization"); (tc.wantHop == "" && strings.Contains(got, "aiml-")) || (tc.wantHop != "" && got != tc.wantHop) {
				t.Fatalf("redirect hop Authorization = %q, want %q", got, tc.wantHop)
			}
			if tc.wantAttribution {
				assertAIMLAPIAttribution(t, hop)
			} else if hop.Get(aimlapi.SourceHeaderName) != "" || hop.Get(aimlapi.PartnerHeaderName) != "" {
				t.Fatalf("redirect hop kept attribution headers: %v", hop)
			}
		})
	}
}

// An error body can echo the key Zero injected; the body is left out of the
// error rather than matched, so a key cut by the 1024-byte read limit cannot
// leak its prefix either. Requests without the automatic key keep the detail.
func TestAIMLAPIMCPStatusErrorOmitsBodyWhenKeyWasInjected(t *testing.T) {
	const key = "sk-echo-0123456789abcdef"
	isolateAIMLAPIKeyLookup(t, key)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"echoed key", `{"error":"invalid credential ` + key + `"}`},
		{"key across the read limit", strings.Repeat("x", 1020) + key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &aimlapiRecorder{failStatus: http.StatusPaymentRequired, failBody: tc.body}
			err := newRecordedNetworkClient(aimlapiTestServer("aimlapi", aimlapi.MCPServerURL), recorder).initialize(context.Background())
			if err == nil {
				t.Fatal("initialize succeeded against a failing server")
			}
			if got := recorder.recorded()[0].Get("Authorization"); got != "Bearer "+key {
				t.Fatalf("request did not carry the injected key: %q", got)
			}
			if strings.Contains(err.Error(), key[:4]) || !strings.Contains(err.Error(), "HTTP 402") {
				t.Fatalf("error = %q, want HTTP 402 without the echoed key", err)
			}
		})
	}

	userHeader := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	userHeader.Headers = map[string]string{"Authorization": "Bearer user-token"}
	recorder := &aimlapiRecorder{failStatus: http.StatusPaymentRequired, failBody: `{"error":"insufficient balance"}`}
	err := newRecordedNetworkClient(userHeader, recorder).initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("error without the automatic key = %v, want the response detail", err)
	}
}
