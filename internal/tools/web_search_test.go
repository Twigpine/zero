package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

type fakeSearchBackend struct {
	results  []searchResult
	err      error
	gotQuery string
	gotLimit int
}

func (f *fakeSearchBackend) Search(_ context.Context, query string, limit int) ([]searchResult, error) {
	f.gotQuery = query
	f.gotLimit = limit
	return f.results, f.err
}

func TestWebSearchFormatsResults(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "Go errors", URL: "https://go.dev/blog/errors", Snippet: "Working with errors in Go."},
		{Title: "Wrapping", URL: "https://go.dev/blog/wrap", Snippet: "Error wrapping."},
	}}
	tool := newWebSearchToolWithBackend(backend)

	res := tool.Run(context.Background(), map[string]any{"query": "go errors"})

	if res.Status != StatusOK {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
	for _, want := range []string{
		"1. Go errors — https://go.dev/blog/errors",
		"   Working with errors in Go.",
		"2. Wrapping — https://go.dev/blog/wrap",
		"   Error wrapping.",
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q:\n%s", want, res.Output)
		}
	}
	if backend.gotQuery != "go errors" {
		t.Fatalf("backend query = %q, want %q", backend.gotQuery, "go errors")
	}
}

func TestWebSearchClampsAndDefaultsLimit(t *testing.T) {
	backend := &fakeSearchBackend{}
	tool := newWebSearchToolWithBackend(backend)

	// Above the cap clamps to 10 rather than erroring.
	tool.Run(context.Background(), map[string]any{"query": "q", "limit": 50})
	if backend.gotLimit != maxWebSearchLimit {
		t.Fatalf("limit = %d, want clamp to %d", backend.gotLimit, maxWebSearchLimit)
	}
	// Missing limit falls back to the default.
	tool.Run(context.Background(), map[string]any{"query": "q"})
	if backend.gotLimit != defaultWebSearchLimit {
		t.Fatalf("default limit = %d, want %d", backend.gotLimit, defaultWebSearchLimit)
	}
}

func TestWebSearchRequiresQuery(t *testing.T) {
	tool := newWebSearchToolWithBackend(&fakeSearchBackend{})
	res := tool.Run(context.Background(), map[string]any{})
	if res.Status != StatusError {
		t.Fatalf("expected StatusError for missing query, got %v", res.Status)
	}
}

func TestWebSearchUnconfiguredBackend(t *testing.T) {
	tool := newWebSearchToolWithBackend(nil)
	res := tool.Run(context.Background(), map[string]any{"query": "q"})
	if res.Status != StatusError {
		t.Fatalf("expected StatusError, got %v", res.Status)
	}
	if !strings.Contains(res.Output, "no search backend configured") {
		t.Fatalf("output should explain the missing backend, got %q", res.Output)
	}
}

func TestWebSearchRedactsBackendError(t *testing.T) {
	secret := "sk-livesecret0123456789abcdef"
	backend := &fakeSearchBackend{err: fmt.Errorf("backend rejected key %s", secret)}
	tool := newWebSearchToolWithBackend(backend)

	res := tool.Run(context.Background(), map[string]any{"query": "q"})

	if res.Status != StatusError {
		t.Fatalf("expected StatusError, got %v", res.Status)
	}
	if strings.Contains(res.Output, secret) {
		t.Fatalf("backend error leaked the API key into output: %q", res.Output)
	}
}

func TestWebSearchRegisteredInCoreNetworkTools(t *testing.T) {
	// web_search is registered only when a backend is configured.
	t.Setenv("ZERO_WEBSEARCH_BASE_URL", "https://search.example/api")
	found := false
	for _, tool := range CoreNetworkTools() {
		if tool.Name() == "web_search" {
			found = true
		}
	}
	if !found {
		t.Fatal("web_search should be registered in CoreNetworkTools() when a backend is configured")
	}
}

func TestWebSearchSafetyPromptsForHostedSearchButAdvertisesInAuto(t *testing.T) {
	tool := newWebSearchToolWithBackend(&fakeSearchBackend{})
	safety := tool.Safety()
	if safety.SideEffect != SideEffectNetwork {
		t.Fatalf("side effect = %s, want network", safety.SideEffect)
	}
	if safety.Permission != PermissionPrompt {
		t.Fatalf("permission = %s, want prompt", safety.Permission)
	}
	if !safety.AdvertiseInAuto {
		t.Fatal("web_search should be advertised in auto mode while still requiring permission")
	}
}

func TestWebSearchRegistryRequiresPermissionBeforeBackendCall(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{{Title: "T", URL: "https://x.test"}}}
	registry := NewRegistry()
	registry.Register(newWebSearchToolWithBackend(backend))

	res := registry.Run(context.Background(), "web_search", map[string]any{"query": "private workspace detail"})

	if res.Status != StatusError {
		t.Fatalf("expected permission error, got %s: %s", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "Permission required for web_search") {
		t.Fatalf("expected permission-required output, got %q", res.Output)
	}
	if backend.gotQuery != "" {
		t.Fatalf("backend must not be called before permission, got query %q", backend.gotQuery)
	}
}

func TestWebSearchRegistryRunsAfterPermissionGranted(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{{Title: "T", URL: "https://x.test"}}}
	registry := NewRegistry()
	registry.Register(newWebSearchToolWithBackend(backend))

	res := registry.RunWithOptions(context.Background(), "web_search", map[string]any{"query": "go errors"}, RunOptions{
		PermissionGranted: true,
	})

	if res.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %s", res.Status, res.Output)
	}
	if backend.gotQuery != "go errors" {
		t.Fatalf("backend query = %q, want %q", backend.gotQuery, "go errors")
	}
}

func TestHTTPSearchBackendSendsProviderAndParsesResults(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Title","url":"https://x.dev","snippet":"snip"}]}`))
	}))
	defer server.Close()

	backend := &httpSearchBackend{client: server.Client(), baseURL: server.URL, apiKey: "k", provider: "exa"}
	results, err := backend.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Title" || results[0].URL != "https://x.dev" {
		t.Fatalf("results = %#v", results)
	}
	// The configured provider and query must reach the backend.
	if gotBody["provider"] != "exa" {
		t.Fatalf("ZERO_WEBSEARCH_PROVIDER not forwarded: %#v", gotBody)
	}
	if gotBody["query"] != "q" {
		t.Fatalf("query not forwarded: %#v", gotBody)
	}
}

type fakeHostedBackend struct {
	results []searchResult
	called  bool
}

func (b *fakeHostedBackend) Search(context.Context, string, int) ([]searchResult, error) {
	b.called = true
	return b.results, nil
}

func TestWebSearchRunWithSandboxAllowsUnderShellNetworkDeny(t *testing.T) {
	backend := &fakeHostedBackend{results: []searchResult{{Title: "T", URL: "https://x.test"}}}
	tool := newWebSearchToolWithBackend(backend).(webSearchTool)
	engine := zeroSandbox.NewEngine(zeroSandbox.EngineOptions{
		Policy: zeroSandbox.Policy{Mode: zeroSandbox.ModeEnforce, Network: zeroSandbox.NetworkDeny},
	})
	res := tool.RunWithSandbox(context.Background(), map[string]any{"query": "hi"}, engine)
	if res.Status != StatusOK {
		t.Fatalf("web_search must run under shell network deny, got %q: %s", res.Status, res.Output)
	}
	if !backend.called {
		t.Fatal("search backend must be called")
	}
}

func TestWebSearchRunWithSandboxAllowsUnderShellNetworkAllow(t *testing.T) {
	backend := &fakeHostedBackend{results: []searchResult{{Title: "T", URL: "https://x.test"}}}
	tool := newWebSearchToolWithBackend(backend).(webSearchTool)
	engine := zeroSandbox.NewEngine(zeroSandbox.EngineOptions{
		Policy: zeroSandbox.Policy{Mode: zeroSandbox.ModeEnforce, Network: zeroSandbox.NetworkAllow},
	})
	res := tool.RunWithSandbox(context.Background(), map[string]any{"query": "hi"}, engine)
	if res.Status != StatusOK {
		t.Fatalf("web_search must run under shell network allow, got %q: %s", res.Status, res.Output)
	}
	if !backend.called {
		t.Fatal("search backend must be called")
	}
}

func TestSameHostRedirectPolicy(t *testing.T) {
	orig, _ := http.NewRequest(http.MethodGet, "https://search.example/api", nil)
	same, _ := http.NewRequest(http.MethodGet, "https://search.example/v2/api", nil)
	cross, _ := http.NewRequest(http.MethodGet, "https://evil.test/x", nil)

	if err := sameHostRedirectPolicy(same, []*http.Request{orig}); err != nil {
		t.Fatalf("same-host redirect must be allowed, got %v", err)
	}
	if err := sameHostRedirectPolicy(cross, []*http.Request{orig}); err == nil {
		t.Fatal("cross-host redirect must be refused so host checks cannot be bypassed via a hop")
	}
	// A same-host HTTPS→HTTP downgrade must be refused (it would leak the query and
	// bearer token over plaintext).
	downgrade, _ := http.NewRequest(http.MethodGet, "http://search.example/api", nil)
	if err := sameHostRedirectPolicy(downgrade, []*http.Request{orig}); err == nil {
		t.Fatal("https→http downgrade redirect must be refused")
	}
	chain := make([]*http.Request, webSearchRedirectLimit)
	for i := range chain {
		chain[i] = orig
	}
	if err := sameHostRedirectPolicy(same, chain); err == nil {
		t.Fatal("redirect limit must be enforced")
	}
}

// ---- new: domains allowlist + score field ----------------------------------

func TestWebSearchDomainsFilterKeepsOnlyAllowedHosts(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "RSC", URL: "https://react.dev/rsc", Snippet: "Server components"},
		{Title: "RFC", URL: "https://github.com/reactjs/rfcs", Snippet: "RFC 0188"},
		{Title: "Other", URL: "https://stackoverflow.com/q/1", Snippet: "unrelated"},
	}}
	tool := newWebSearchToolWithBackend(backend)

	res := tool.Run(context.Background(), map[string]any{
		"query":   "react server components",
		"domains": []any{"react.dev", "github.com"},
	})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "stackoverflow.com") {
		t.Errorf("stackoverflow result must be filtered out, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "react.dev/rsc") || !strings.Contains(res.Output, "github.com/reactjs/rfcs") {
		t.Errorf("allowed-host results missing, got: %s", res.Output)
	}
}

func TestWebSearchDomainsFilterEmptyResultIsError(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "x", URL: "https://stackoverflow.com/q/1"},
	}}
	tool := newWebSearchToolWithBackend(backend)

	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": []any{"react.dev"},
	})
	if res.Status != StatusError {
		t.Fatalf("expected error when allowlist eats every result, got %v: %s", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "no web_search results matched domains") {
		t.Errorf("expected clear allowlist error, got: %s", res.Output)
	}
}

func TestWebSearchDomainsFilterToleratesStringSlice(t *testing.T) {
	// Some providers send []string instead of []any. The tool must accept both.
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://react.dev/x"},
	}}
	tool := newWebSearchToolWithBackend(backend)
	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": []string{"react.dev"},
	})
	if res.Status != StatusOK {
		t.Fatalf("expected ok for []string domains, got %v: %s", res.Status, res.Output)
	}
}

func TestWebSearchDomainsFilterNormalizesSchemeAndCase(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://React.Dev/x"},
	}}
	tool := newWebSearchToolWithBackend(backend)
	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": []any{"https://REACT.dev/path?ignored=1", "WWW.react.dev"},
	})
	if res.Status != StatusOK {
		t.Fatalf("expected ok after normalization, got %v: %s", res.Status, res.Output)
	}
}

func TestWebSearchDomainsFilterStripsWWWFromResult(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://www.react.dev/x"},
	}}
	tool := newWebSearchToolWithBackend(backend)
	// Allowlist "react.dev" must still match a result that has www. in the URL.
	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": []any{"react.dev"},
	})
	if res.Status != StatusOK {
		t.Fatalf("expected ok when result host has www., got %v: %s", res.Status, res.Output)
	}
}

func TestWebSearchDomainsFilterRejectsMalformedURLs(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://react.dev/x"},
		{Title: "bad", URL: "::::not a url::::"},
	}}
	tool := newWebSearchToolWithBackend(backend)
	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": []any{"react.dev"},
	})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, output = %s", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "not a url") {
		t.Errorf("malformed URL must be filtered out, got: %s", res.Output)
	}
}

func TestWebSearchDomainsFilterRejectsBadArgType(t *testing.T) {
	tool := newWebSearchToolWithBackend(&fakeSearchBackend{})
	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": "react.dev", // string instead of array
	})
	if res.Status != StatusError {
		t.Fatalf("expected error for non-array domains, got %v: %s", res.Status, res.Output)
	}
}

func TestWebSearchRendersScoreWhenPresent(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "ranked", URL: "https://react.dev/rsc", Score: 0.91},
		{Title: "unranked", URL: "https://react.dev/other"}, // zero score, must be omitted
	}}
	tool := newWebSearchToolWithBackend(backend)
	res := tool.Run(context.Background(), map[string]any{"query": "x"})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, output = %s", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "score 0.91") {
		t.Errorf("expected 'score 0.91' in output, got: %s", res.Output)
	}
	// The zero-score row must NOT render "score 0.00" — that would be noisy.
	if strings.Contains(res.Output, "score 0.00") {
		t.Errorf("zero score must not be rendered, got: %s", res.Output)
	}
}

func TestHTTPSearchBackendParsesScoreField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"A","url":"https://a.test","snippet":"sa","score":0.77},{"title":"B","url":"https://b.test","snippet":"sb"}]}`))
	}))
	defer server.Close()

	backend := &httpSearchBackend{client: server.Client(), baseURL: server.URL}
	results, err := backend.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Score != 0.77 {
		t.Errorf("first result Score = %v, want 0.77", results[0].Score)
	}
	if results[1].Score != 0 {
		t.Errorf("absent score must be zero, got %v", results[1].Score)
	}
}

func TestCanonicalizeWebSearchHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"react.dev", "react.dev"},
		{"  REACT.dev  ", "react.dev"},
		{"https://react.dev/x", "react.dev"},
		{"http://www.react.dev", "react.dev"},
		{"WWW.react.dev", "react.dev"},
		{"react.dev/path?x=1", "react.dev"},
		{"", ""},
		{"   ", ""},
		{"https://", ""},
		{"react .dev", ""}, // space inside
	}
	for _, c := range cases {
		if got := canonicalizeWebSearchHost(c.in); got != c.want {
			t.Errorf("canonicalizeWebSearchHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeWebSearchDomainList(t *testing.T) {
	got := normalizeWebSearchDomainList([]string{"  REACT.dev  ", "https://github.com/x", "www.example.com", "react.dev", "", "   "})
	want := []string{"react.dev", "github.com", "example.com"}
	if len(got) != len(want) {
		t.Fatalf("normalizeWebSearchDomainList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFilterWebSearchByDomains(t *testing.T) {
	rows := []searchResult{
		{URL: "https://react.dev/x"},
		{URL: "https://github.com/y"},
		{URL: "https://stackoverflow.com/z"},
	}
	filtered, err := filterWebSearchByDomains(rows, []string{"react.dev", "github.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered len = %d, want 2; got %v", len(filtered), filtered)
	}
	// Empty allowlist must be rejected when called directly with no rows that match.
	_, err = filterWebSearchByDomains([]searchResult{{URL: "https://x.test"}}, []string{"nope.test"})
	if err == nil {
		t.Fatal("expected error when allowlist is non-empty and no rows match")
	}
}

func TestWebSearchSchemaHasDomainsField(t *testing.T) {
	tool := NewWebSearchTool()
	domains := tool.Parameters().Properties["domains"]
	if domains.Type != "array" {
		t.Fatalf("domains.Type = %q, want array", domains.Type)
	}
	if domains.Items == nil || domains.Items.Type != "string" {
		t.Fatalf("domains.Items = %#v, want array of string", domains.Items)
	}
	if domains.Description == "" {
		t.Fatal("domains.Description should explain the prompt-injection defense")
	}
}

// TestWebSearchDomainsFilterRejectsAllInvalidInputs covers the fail-closed
// contract: when the caller passes a 'domains' argument but every entry is
// invalid (e.g. all whitespace, all with embedded spaces), the tool must
// error rather than silently return unfiltered results. The whole point of
// the parameter is the prompt-injection defense; an unfiltered result set
// would defeat it.
func TestWebSearchDomainsFilterRejectsAllInvalidInputs(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "x", URL: "https://react.dev/x"},
	}}
	tool := newWebSearchToolWithBackend(backend)

	cases := []struct {
		name    string
		domains []any
	}{
		{"all whitespace", []any{"   ", "\t", "\n"}},
		{"empty strings", []any{"", ""}},
		{"embedded spaces", []any{"react .dev", "github .com"}},
		{"mixed invalid", []any{"", "   ", "react .dev"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := tool.Run(context.Background(), map[string]any{
				"query":   "x",
				"domains": c.domains,
			})
			if res.Status != StatusError {
				t.Fatalf("expected error when all domains are invalid, got %v: %s", res.Status, res.Output)
			}
			if !strings.Contains(res.Output, "no valid hostnames") {
				t.Errorf("expected 'no valid hostnames' in error, got: %s", res.Output)
			}
			if backend.gotQuery != "" {
				t.Errorf("backend must not have been called when domains are invalid; got query %q", backend.gotQuery)
			}
		})
	}
}

// TestWebSearchDomainsFilterAcceptsHostPort covers the canonicalization
// contract: an allowlist entry that includes a port WITH a scheme (e.g. the
// result of a model passing "https://react.dev:443/path") must still match
// a result URL on the same hostname. The CodeRabbit-flagged bug was that
// the canonicalizer was using parsed.Host (which includes ":443") rather
// than parsed.Hostname() (which strips the port), so a "react.dev" allowlist
// silently never matched results from "https://react.dev:443/...". The fix
// uses Hostname(), so this test locks in the corrected contract.
func TestWebSearchDomainsFilterAcceptsHostPort(t *testing.T) {
	cases := []struct {
		name        string
		allowlist   []any
		expectMatch bool
	}{
		{"with explicit https port", []any{"https://react.dev:443/path"}, true},
		{"with explicit http port", []any{"http://react.dev:80/x"}, true},
		{"scheme-less host:port is rejected", []any{"react.dev:9999"}, false}, // not a valid hostname
		{"port on different host", []any{"github.com:443"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			backend := &fakeSearchBackend{results: []searchResult{
				{Title: "ok", URL: "https://react.dev/x"},
			}}
			tool := newWebSearchToolWithBackend(backend)
			res := tool.Run(context.Background(), map[string]any{
				"query":   "x",
				"domains": c.allowlist,
			})
			if c.expectMatch {
				if res.Status != StatusOK {
					t.Fatalf("expected match, got %v: %s", res.Status, res.Output)
				}
				if !strings.Contains(res.Output, "react.dev/x") {
					t.Errorf("expected react.dev result, got: %s", res.Output)
				}
			} else {
				if res.Status != StatusError {
					t.Fatalf("expected no-match error, got %v: %s", res.Status, res.Output)
				}
			}
		})
	}
}

func TestCanonicalizeWebSearchHostStripsPort(t *testing.T) {
	// Lock in the contract that parsed.Host (which includes ":443") is NOT
	// used; parsed.Hostname() is. CodeRabbit flagged this inconsistency.
	cases := []struct {
		in, want string
	}{
		{"https://react.dev:443/x", "react.dev"},
		{"http://react.dev:80", "react.dev"},
		{"https://api.react.dev:8443/v1", "api.react.dev"},
		{"https://react.dev", "react.dev"},
	}
	for _, c := range cases {
		if got := canonicalizeWebSearchHost(c.in); got != c.want {
			t.Errorf("canonicalizeWebSearchHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Test added to verify mixed-type array handling
func TestStringListArgWebSearchRejectsMixedTypes(t *testing.T) {
	args := map[string]any{
		"domains": []any{"react.dev", 123, "github.com"},
	}

	domains, provided, err := stringListArgWebSearch(args, "domains")

	if err == nil {
		t.Fatal("Expected error for mixed-type array, but got nil")
	}
	if !provided {
		t.Fatal("Expected provided=true since the argument was passed")
	}
	if err.Error() != "domains[1] must be a string" {
		t.Fatalf("Expected 'domains[1] must be a string', got '%v'", err)
	}
	if domains != nil {
		t.Fatalf("Expected nil domains on error, got %v", domains)
	}
	t.Logf("Test passed: got expected error: %v", err)
}

func TestWebSearchRejectsMixedTypeDomainsE2E(t *testing.T) {
	// End-to-end test: web_search tool rejects mixed-type array
	tool := newWebSearchToolWithBackend(&fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://react.dev/x"},
	}})

	res := tool.Run(context.Background(), map[string]any{
		"query":   "x",
		"domains": []any{"react.dev", 123, "github.com"},
	})

	if res.Status != StatusError {
		t.Fatalf("Expected error for mixed-type domains, got %v: %s", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "domains[1] must be a string") {
		t.Fatalf("Expected 'domains[1] must be a string' in error message, got: %s", res.Output)
	}
}

// ---- fix/web-search-209: robustness + rendering + normalization -------------

// #1: a stringified or malformed score must not discard the whole response.
func TestParseSearchResultsToleratesNonNumericScore(t *testing.T) {
	body := []byte(`{"results":[
		{"title":"num","url":"https://a.test","snippet":"s","score":0.77},
		{"title":"str","url":"https://b.test","snippet":"s","score":"0.5"},
		{"title":"bad","url":"https://c.test","snippet":"s","score":"high"},
		{"title":"obj","url":"https://d.test","snippet":"s","score":{"x":1}},
		{"title":"null","url":"https://e.test","snippet":"s","score":null},
		{"title":"nan","url":"https://f.test","snippet":"s","score":"NaN"},
		{"title":"inf","url":"https://g.test","snippet":"s","score":"Inf"}
	]}`)
	results, err := parseSearchResults(body)
	if err != nil {
		t.Fatalf("a single odd score must not fail the parse: %v", err)
	}
	if len(results) != 7 {
		t.Fatalf("want 7 results (none dropped), got %d", len(results))
	}
	if results[0].Score != 0.77 {
		t.Errorf("numeric score = %v, want 0.77", results[0].Score)
	}
	if results[1].Score != 0.5 {
		t.Errorf("numeric-string score = %v, want 0.5", results[1].Score)
	}
	// Indices 5 and 6 cover ParseFloat-accepted non-finite values ("NaN"/"Inf"),
	// which must be rejected so the documented filter holds and the renderer never
	// emits a non-finite score.
	for _, i := range []int{2, 3, 4, 5, 6} {
		if results[i].Score != 0 {
			t.Errorf("unparseable/null/non-finite score[%d] = %v, want 0 (absent)", i, results[i].Score)
		}
	}
}

// #2: tiny positive and negative scores must not render; >=0.005 rounds in.
func TestWebSearchScoreRenderingGate(t *testing.T) {
	backend := &fakeSearchBackend{results: []searchResult{
		{Title: "tiny", URL: "https://a.test/1", Score: 0.0001},
		{Title: "neg", URL: "https://a.test/2", Score: -0.5},
		{Title: "round-in", URL: "https://a.test/3", Score: 0.005},
		{Title: "shown", URL: "https://a.test/4", Score: 0.91},
	}}
	res := newWebSearchToolWithBackend(backend).Run(context.Background(), map[string]any{"query": "x"})
	if res.Status != StatusOK {
		t.Fatalf("status = %v: %s", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "score 0.00") {
		t.Errorf("tiny/negative score must not render 'score 0.00', got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "score 0.01") {
		t.Errorf("0.005 should round to 'score 0.01', got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "score 0.91") {
		t.Errorf("0.91 should render, got: %s", res.Output)
	}
}

// #4: trailing-dot FQDN entries/results normalize symmetrically.
func TestWebSearchDomainsFilterTrailingDotSymmetry(t *testing.T) {
	// allowlist has the dot, result does not
	res := newWebSearchToolWithBackend(&fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://react.dev/x"},
	}}).Run(context.Background(), map[string]any{"query": "x", "domains": []any{"react.dev."}})
	if res.Status != StatusOK {
		t.Fatalf("allowlist 'react.dev.' should match 'react.dev' result, got %v: %s", res.Status, res.Output)
	}
	// result has the dot, allowlist does not
	res = newWebSearchToolWithBackend(&fakeSearchBackend{results: []searchResult{
		{Title: "ok", URL: "https://react.dev./x"},
	}}).Run(context.Background(), map[string]any{"query": "x", "domains": []any{"react.dev"}})
	if res.Status != StatusOK {
		t.Fatalf("allowlist 'react.dev' should match 'react.dev.' result, got %v: %s", res.Status, res.Output)
	}
}
