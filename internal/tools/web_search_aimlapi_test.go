package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/config"
)

// Trimmed from a real 200 web/search run.
const aimlapiCompletedRun = `{"id":"h3X2expTh3D7vd4pwyxiY","tool":"web/search","status":"completed","is_error":false,"untrusted":true,
"output":{"results":[
 {"url":"https://pkg.go.dev/net/http","title":"http package - net/http - Go Packages","snippet":"For control over proxies, TLS configuration, keep-alives..."},
 {"url":"https://go.dev/src/net/http/client.go?m=text","title":"go.dev","snippet":"// Timeout specifies a time limit for requests made by this Client."},
 {"url":"https://stackoverflow.com/questions/16895294/how-to-set-timeout-for-http-get-requests-in-golang","title":"How to set timeout for http.Get() requests in Golang?","snippet":"Apparently in Go 1.3 http.Client has Timeout field","published_at":"2013-11-02T00:00:00.000Z"}
]},
"cost":{"quoted_usd":0.0091,"actual_usd":0.0091,"over_max":false}}`

// A real 400 upfront refusal (not billed).
const aimlapiMaxCostExceeded = `{"error":{"message":"The cheapest source costs 0.0091 USD, above the limit of 0.001 USD.","type":"invalid_request_error","code":"max_cost_exceeded","request_id":"hnbQUvsseM1EX9RrgPjBT","refused_by":"request","estimated_cost_usd":0.0091,"max_cost_usd":0.001}}`

type toolboxCall struct {
	method    string
	url       string
	header    http.Header
	body      map[string]any
	remaining time.Duration
}

// fakeToolbox is an http.RoundTripper that records every request and answers
// from respond, so no request leaves the process.
type fakeToolbox struct {
	mu      sync.Mutex
	calls   []toolboxCall
	respond func(index int, request *http.Request) *http.Response
}

func (fake *fakeToolbox) RoundTrip(request *http.Request) (*http.Response, error) {
	call := toolboxCall{method: request.Method, url: request.URL.String(), header: request.Header.Clone()}
	if deadline, ok := request.Context().Deadline(); ok {
		call.remaining = time.Until(deadline)
	}
	if request.Body != nil {
		raw, _ := io.ReadAll(request.Body)
		_ = request.Body.Close()
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.body)
		}
	}
	fake.mu.Lock()
	index := len(fake.calls)
	fake.calls = append(fake.calls, call)
	fake.mu.Unlock()
	// Like a real transport, refuse a request whose context has already ended.
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	response := fake.respond(index, request)
	response.Request = request
	return response, nil
}

func (fake *fakeToolbox) recorded() []toolboxCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]toolboxCall(nil), fake.calls...)
}

func toolboxResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func alwaysRespond(status int, body string) func(int, *http.Request) *http.Response {
	return func(int, *http.Request) *http.Response { return toolboxResponse(status, body) }
}

func staticKey(key string) func() (string, error) {
	return func() (string, error) { return key, nil }
}

// isolateAIMLAPISearchEnv points every config, cache and credential root at a
// temporary home and clears the variables the backend selection and the key
// resolution read.
func isolateAIMLAPISearchEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("ZERO_CRED_STORAGE", "file")
	t.Setenv("ZERO_PROVIDER", "")
	t.Setenv("AIMLAPI_API_KEY", "")
	t.Setenv("AIMLAPI_INFERENCE_URL", "")
	t.Setenv("AIMLAPI_PARTNER_ID", "")
	t.Setenv("ZERO_WEBSEARCH_PROVIDER", "")
	t.Setenv("ZERO_WEBSEARCH_BASE_URL", "")
	t.Setenv("ZERO_WEBSEARCH_API_KEY", "")
}

func hasWebSearchTool(tools []Tool) bool {
	for _, tool := range tools {
		if tool.Name() == "web_search" {
			return true
		}
	}
	return false
}

func TestWebSearchAIMLAPISelectedOnlyByExplicitProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		baseURL  string
		want     bool
	}{
		{name: "key alone", provider: "", want: false},
		{name: "other provider", provider: "searxng", want: false},
		{name: "explicit", provider: "aimlapi", want: true},
		{name: "case-insensitive", provider: " AIMLAPI ", want: true},
		{name: "base URL ignored", provider: "aimlapi", baseURL: "https://search.example/api", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAIMLAPISearchEnv(t)
			t.Setenv("AIMLAPI_API_KEY", "aiml-env-key")
			t.Setenv("ZERO_WEBSEARCH_PROVIDER", tc.provider)
			t.Setenv("ZERO_WEBSEARCH_BASE_URL", tc.baseURL)

			_, isAIMLAPI := defaultSearchBackend().(*aimlapiSearchBackend)
			if isAIMLAPI != tc.want {
				t.Fatalf("defaultSearchBackend() is aimlapi = %v, want %v", isAIMLAPI, tc.want)
			}
			if registered := hasWebSearchTool(CoreNetworkTools()); registered != tc.want && tc.baseURL == "" {
				t.Fatalf("web_search registered = %v, want %v", registered, tc.want)
			}
		})
	}
}

func TestWebSearchAIMLAPIDefaultBackendUsesServiceKeyAndFixedHost(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	t.Setenv("ZERO_WEBSEARCH_PROVIDER", "aimlapi")
	t.Setenv("ZERO_WEBSEARCH_BASE_URL", "https://search.example/api")
	t.Setenv("ZERO_WEBSEARCH_API_KEY", "zws-generic-key")
	t.Setenv("AIMLAPI_API_KEY", "aiml-env-key")

	backend, ok := defaultSearchBackend().(*aimlapiSearchBackend)
	if !ok {
		t.Fatalf("defaultSearchBackend() = %T, want *aimlapiSearchBackend", defaultSearchBackend())
	}
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, aimlapiCompletedRun)}
	backend.client.Transport = fake // keeps the production redirect policy and key loader

	res := newWebSearchToolWithBackend(backend).Run(context.Background(), map[string]any{"query": "go http timeout"})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
	calls := fake.recorded()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(calls))
	}
	if calls[0].url != aimlapi.ToolboxBaseURL+"/run" {
		t.Fatalf("request URL = %q, want the fixed Toolbox URL", calls[0].url)
	}
	if got := calls[0].header.Get("Authorization"); got != "Bearer aiml-env-key" {
		t.Fatalf("Authorization = %q, want the AI/ML API service key", got)
	}
}

func TestAIMLAPISearchSendsExactRequest(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, aimlapiCompletedRun)}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

	for range 2 {
		res := tool.Run(context.Background(), map[string]any{"query": "go http timeout", "limit": 3})
		if res.Status != StatusOK {
			t.Fatalf("status = %v, output = %q", res.Status, res.Output)
		}
		for _, want := range []string{
			"1. http package - net/http - Go Packages — https://pkg.go.dev/net/http",
			"   For control over proxies",
			"3. How to set timeout for http.Get() requests in Golang?",
		} {
			if !strings.Contains(res.Output, want) {
				t.Fatalf("output missing %q:\n%s", want, res.Output)
			}
		}
	}

	calls := fake.recorded()
	if len(calls) != 2 {
		t.Fatalf("requests = %d, want 2", len(calls))
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, call := range calls {
		if call.method != http.MethodPost || call.url != "https://tools.aimlapi.com/v1/tools/run" {
			t.Fatalf("request = %s %s, want POST https://tools.aimlapi.com/v1/tools/run", call.method, call.url)
		}
		for header, want := range map[string]string{
			"Authorization":           "Bearer test-aiml-key",
			aimlapi.SourceHeaderName:  "agent/zero",
			aimlapi.PartnerHeaderName: aimlapi.DefaultPartnerID,
			"User-Agent":              "zero-web-search/0.1",
			"Content-Type":            "application/json",
		} {
			if got := call.header.Get(header); got != want {
				t.Fatalf("%s = %q, want %q", header, got, want)
			}
		}
		body := call.body
		if body["tool"] != "web/search" {
			t.Fatalf("tool = %v, want web/search", body["tool"])
		}
		if body["max_cost_usd"] != 0.025 {
			t.Fatalf("max_cost_usd = %v, want 0.025", body["max_cost_usd"])
		}
		if body["wait_ms"] != float64(20000) {
			t.Fatalf("wait_ms = %v, want 20000", body["wait_ms"])
		}
		input, _ := body["input"].(map[string]any)
		if input["query"] != "go http timeout" || input["max_results"] != float64(3) {
			t.Fatalf("input = %v, want query and max_results 3", input)
		}
		if _, ok := input["include_domains"]; ok {
			t.Fatalf("include_domains sent without a domains argument: %v", input)
		}
		if key, _ := body["idempotency_key"].(string); !uuid.MatchString(key) {
			t.Fatalf("idempotency_key = %q, want a UUIDv4", key)
		}
		// The aimlapi budget replaces the generic 10 s one, and the server-side
		// wait stays inside it.
		if call.remaining <= webSearchTimeout || call.remaining > aimlapiSearchTimeout {
			t.Fatalf("request deadline in %v, want within (%v, %v]", call.remaining, webSearchTimeout, aimlapiSearchTimeout)
		}
	}
	if calls[0].body["idempotency_key"] == calls[1].body["idempotency_key"] {
		t.Fatalf("idempotency_key reused across searches: %v", calls[0].body["idempotency_key"])
	}
	if time.Duration(aimlapiSearchWaitMS)*time.Millisecond >= aimlapiSearchTimeout {
		t.Fatalf("wait_ms %d must stay below the %v budget", aimlapiSearchWaitMS, aimlapiSearchTimeout)
	}
}

func TestAIMLAPISearchResolvesKeyOnEveryCall(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, aimlapiCompletedRun)}
	loads := 0
	backend := newAIMLAPISearchBackend(fake, func() (string, error) {
		loads++
		if loads == 1 {
			return "first-key", nil
		}
		return "", config.ErrAIMLAPIKeyMissing
	})
	tool := newWebSearchToolWithBackend(backend)

	if res := tool.Run(context.Background(), map[string]any{"query": "q"}); res.Status != StatusOK {
		t.Fatalf("first search: status = %v, output = %q", res.Status, res.Output)
	}
	// The key was removed after the first search: the next one must see that.
	res := tool.Run(context.Background(), map[string]any{"query": "q"})
	if res.Status != StatusError || !strings.Contains(res.Output, "no AI/ML API key is configured") {
		t.Fatalf("second search: status = %v, output = %q", res.Status, res.Output)
	}
	if calls := fake.recorded(); len(calls) != 1 || calls[0].header.Get("Authorization") != "Bearer first-key" {
		t.Fatalf("requests = %+v, want exactly the first search", calls)
	}
}

func TestAIMLAPISearchSendsNothingWithoutUsableKey(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "missing", want: config.ErrAIMLAPIKeyMissing.Error()},
		{name: "proxy", env: map[string]string{"AIMLAPI_API_KEY": "proxy-key", "AIMLAPI_INFERENCE_URL": "https://proxy.example/v1"}, want: config.ErrAIMLAPIProxyProfile.Error()},
		{name: "invalid", env: map[string]string{"AIMLAPI_API_KEY": "two words"}, want: config.ErrAIMLAPIKeyInvalid.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAIMLAPISearchEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, aimlapiCompletedRun)}
			tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, config.LoadAIMLAPIServiceKey))

			res := tool.Run(context.Background(), map[string]any{"query": "q"})
			if res.Status != StatusError || !strings.Contains(res.Output, tc.want) {
				t.Fatalf("status = %v, output = %q, want an error mentioning %q", res.Status, res.Output, tc.want)
			}
			if calls := fake.recorded(); len(calls) != 0 {
				t.Fatalf("sent %d requests without a usable key", len(calls))
			}
		})
	}
}

func TestAIMLAPISearchPollsQueuedRun(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: func(index int, _ *http.Request) *http.Response {
		switch index {
		case 0:
			return toolboxResponse(http.StatusAccepted, `{"id":"run/1","status":"queued","next_poll_hint_ms":1}`)
		case 1:
			return toolboxResponse(http.StatusOK, `{"id":"run/1","status":"running","is_error":false,"next_poll_hint_ms":1}`)
		default:
			return toolboxResponse(http.StatusOK, aimlapiCompletedRun)
		}
	}}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

	res := tool.Run(context.Background(), map[string]any{"query": "q"})
	if res.Status != StatusOK || !strings.Contains(res.Output, "https://pkg.go.dev/net/http") {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
	calls := fake.recorded()
	if len(calls) != 3 {
		t.Fatalf("requests = %d, want POST then two polls", len(calls))
	}
	for _, call := range calls[1:] {
		if call.method != http.MethodGet || call.url != "https://tools.aimlapi.com/v1/tools/runs/run%2F1" {
			t.Fatalf("poll = %s %s, want GET of the run status", call.method, call.url)
		}
		if call.header.Get("Authorization") != "Bearer test-aiml-key" || call.header.Get(aimlapi.SourceHeaderName) != "agent/zero" {
			t.Fatalf("poll headers = %v, want the key and attribution", call.header)
		}
	}
}

func TestAIMLAPIPollDelayIsClamped(t *testing.T) {
	ms := func(value float64) *float64 { return &value }
	cases := []struct {
		hint *float64
		want time.Duration
	}{
		{hint: nil, want: time.Second},
		{hint: ms(1), want: 250 * time.Millisecond},
		{hint: ms(1200), want: 1200 * time.Millisecond},
		{hint: ms(60_000), want: 5 * time.Second},
	}
	for _, tc := range cases {
		if got := aimlapiPollDelay(tc.hint); got != tc.want {
			t.Fatalf("aimlapiPollDelay(%v) = %v, want %v", tc.hint, got, tc.want)
		}
	}
}

func TestAIMLAPISearchStopsPollingWhenContextEnds(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusAccepted, `{"id":"run_1","status":"queued","next_poll_hint_ms":5000}`)}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	res := tool.Run(ctx, map[string]any{"query": "q"})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("search took %v after the context ended", elapsed)
	}
	if res.Status != StatusError || !strings.Contains(res.Output, "did not finish") {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
}

func TestAIMLAPISearchCompletedRunWithIsErrorFails(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, `{"id":"run_9","status":"completed","is_error":true,
		"error":{"code":"source_failed","message":"upstream search failed"},
		"output":{"results":[{"url":"https://example.com/partial","title":"partial"}]}}`)}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

	res := tool.Run(context.Background(), map[string]any{"query": "q"})
	if res.Status != StatusError {
		t.Fatalf("status = %v, output = %q, want an error", res.Status, res.Output)
	}
	for _, want := range []string{"source_failed", "upstream search failed", "run_9"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q: %q", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "example.com/partial") {
		t.Fatalf("results of a failed run were shown: %q", res.Output)
	}
}

func TestAIMLAPISearchTerminalFailureStatuses(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	for _, status := range []string{"failed", "stopped", "timed_out"} {
		t.Run(status, func(t *testing.T) {
			fake := &fakeToolbox{respond: func(index int, _ *http.Request) *http.Response {
				if index > 0 { // a poll would mean the status was not treated as terminal
					return toolboxResponse(http.StatusInternalServerError, `{}`)
				}
				return toolboxResponse(http.StatusOK, `{"id":"run_2","status":"`+status+`","is_error":false}`)
			}}
			tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

			res := tool.Run(context.Background(), map[string]any{"query": "q"})
			if res.Status != StatusError || !strings.Contains(res.Output, "run "+status) {
				t.Fatalf("status = %v, output = %q", res.Status, res.Output)
			}
			if calls := fake.recorded(); len(calls) != 1 {
				t.Fatalf("requests = %d, want no polling after a terminal status", len(calls))
			}
		})
	}
}

func TestAIMLAPISearchMalformedResponses(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	cases := map[string]struct{ body, want string }{
		"not json":   {body: `<html>oops</html>`, want: "invalid response"},
		"no results": {body: `{"id":"run_3","status":"completed","is_error":false,"output":{}}`, want: "no results list"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, tc.body)}
			tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))
			res := tool.Run(context.Background(), map[string]any{"query": "q"})
			if res.Status != StatusError || !strings.Contains(res.Output, tc.want) {
				t.Fatalf("status = %v, output = %q, want an error mentioning %q", res.Status, res.Output, tc.want)
			}
		})
	}
}

func TestAIMLAPISearchMaxCostExceededReportsCodeAndRequestID(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusBadRequest, aimlapiMaxCostExceeded)}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

	res := tool.Run(context.Background(), map[string]any{"query": "q"})
	if res.Status != StatusError {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
	for _, want := range []string{"HTTP 400", "max_cost_exceeded", "cheapest source costs", "request_id: hnbQUvsseM1EX9RrgPjBT"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q: %q", want, res.Output)
		}
	}
}

func TestAIMLAPISearchRateLimitedReportsRetryAfter(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: func(int, *http.Request) *http.Response {
		response := toolboxResponse(http.StatusTooManyRequests, `{"error":{"code":"rate_limited","message":"slow down","request_id":"req_429"}}`)
		response.Header.Set("Retry-After", "7")
		return response
	}}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

	res := tool.Run(context.Background(), map[string]any{"query": "q"})
	for _, want := range []string{"HTTP 429 rate_limited", "retry after 7s", "request_id: req_429"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q: %q", want, res.Output)
		}
	}
}

func TestAIMLAPISearchRefusesRedirect(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	for _, location := range []string{"https://collector.example/run", "https://tools.aimlapi.com/v1/tools/run"} {
		t.Run(location, func(t *testing.T) {
			fake := &fakeToolbox{respond: func(index int, _ *http.Request) *http.Response {
				if index == 0 {
					response := toolboxResponse(http.StatusTemporaryRedirect, ``)
					response.Header.Set("Location", location)
					return response
				}
				return toolboxResponse(http.StatusOK, aimlapiCompletedRun)
			}}
			tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

			res := tool.Run(context.Background(), map[string]any{"query": "q"})
			if res.Status != StatusError || !strings.Contains(res.Output, "refusing redirect") {
				t.Fatalf("status = %v, output = %q, want a refused redirect", res.Status, res.Output)
			}
			if calls := fake.recorded(); len(calls) != 1 {
				t.Fatalf("requests = %d, want the redirect not followed", len(calls))
			}
		})
	}
}

func TestAIMLAPISearchRedactsKeyEchoedInErrors(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	const key = "zq7Mv2Lp9Xr4Tn8Wk3"
	if redacted := redactWebSearchText("key " + key); !strings.Contains(redacted, key) {
		t.Fatalf("precondition: the generic redactor already hides this key shape (%q); pick another", redacted)
	}
	bodies := map[string]struct {
		status int
		body   string
	}{
		"http error": {status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key","message":"API key ` + key + ` is not valid","request_id":"req_` + key + `"}}`},
		"run error":  {status: http.StatusOK, body: `{"id":"run_` + key + `","status":"failed","is_error":true,"error":{"code":"bad_auth","message":"rejected ` + key + `"}}`},
		// The same key split by characters nobody sees: a reader that drops
		// them gets the key back, so the split form has to be redacted too.
		"http error split by NUL": {status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key","message":"API key ` + splitAIMLAPIKey(key, `\u0000`) + ` is not valid","request_id":"req_` + splitAIMLAPIKey(key, `\u200b`) + `"}}`},
		"run error split by ZWSP": {status: http.StatusOK, body: `{"id":"run_` + splitAIMLAPIKey(key, `\u200b`) + `","status":"failed","is_error":true,"error":{"code":"bad_auth","message":"rejected ` + splitAIMLAPIKey(key, `\u0000`) + `"}}`},
	}
	for name, tc := range bodies {
		t.Run(name, func(t *testing.T) {
			fake := &fakeToolbox{respond: alwaysRespond(tc.status, tc.body)}
			tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey(key)))
			res := tool.Run(context.Background(), map[string]any{"query": "q"})
			if res.Status != StatusError {
				t.Fatalf("status = %v, output = %q", res.Status, res.Output)
			}
			if strings.Contains(withoutInvisibleRunes(res.Output), key) {
				t.Fatalf("the AI/ML API key leaked into the output: %q", res.Output)
			}
			if !strings.Contains(res.Output, "req_") && !strings.Contains(res.Output, "run_") {
				t.Fatalf("output lost the request or run id: %q", res.Output)
			}
		})
	}
}

// A failed request must not echo its URL. The polling URL carries the run id
// the Toolbox returned, percent-escaped, and an escaped key ("%00",
// "%E2%80%8B") is not the text the key scrubber matches.
func TestAIMLAPISearchTransportErrorOmitsPollingURL(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	const key = "zq7Mv2Lp9Xr4Tn8Wk3"
	for name, separator := range map[string]string{"NUL": `\u0000`, "ZWSP": `\u200b`} {
		t.Run(name, func(t *testing.T) {
			queued := `{"id":"run_` + splitAIMLAPIKey(key, separator) + `","status":"queued","next_poll_hint_ms":1}`
			var polls int
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodPost {
					response := toolboxResponse(http.StatusAccepted, queued)
					response.Request = request
					return response, nil
				}
				polls++
				return nil, errors.New("connection reset by peer")
			})
			tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(transport, staticKey(key)))

			res := tool.Run(context.Background(), map[string]any{"query": "q"})
			if polls != 1 {
				t.Fatalf("polls = %d, want the failing poll to be sent once", polls)
			}
			decoded, err := url.PathUnescape(res.Output)
			if err != nil {
				decoded = res.Output
			}
			if strings.Contains(withoutInvisibleRunes(decoded), key) {
				t.Fatalf("the AI/ML API key leaked into the output: %q", res.Output)
			}
			if strings.Contains(res.Output, "/runs/") {
				t.Fatalf("the polling URL leaked into the output: %q", res.Output)
			}
			if res.Status != StatusError || !strings.Contains(res.Output, "AI/ML API search request failed: connection reset by peer") {
				t.Fatalf("status = %v, output = %q, want the transport failure", res.Status, res.Output)
			}
		})
	}
}

func TestAIMLAPIToolboxRequestRefusesOtherDestinations(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	for _, target := range []string{
		"https://tools.aimlapi.com.evil.example/v1/tools/run",
		"http://tools.aimlapi.com/v1/tools/run",
		"https://tools.aimlapi.com:8443/v1/tools/run",
		"https://user@tools.aimlapi.com/v1/tools/run",
		"https://search.example/api",
	} {
		request, err := newAIMLAPIToolboxRequest(context.Background(), http.MethodPost, target, strings.NewReader(`{}`), "test-aiml-key")
		if err == nil || request != nil {
			t.Fatalf("newAIMLAPIToolboxRequest(%q) = %v, %v; want a refusal", target, request, err)
		}
		if strings.Contains(err.Error(), "test-aiml-key") {
			t.Fatalf("refusal leaked the key: %v", err)
		}
	}
	request, err := newAIMLAPIToolboxRequest(context.Background(), http.MethodGet, aimlapi.ToolboxBaseURL+"/runs/x", nil, "test-aiml-key")
	if err != nil {
		t.Fatalf("Toolbox URL refused: %v", err)
	}
	if request.Header.Get("Authorization") != "Bearer test-aiml-key" || request.Header.Get(aimlapi.SourceHeaderName) != "agent/zero" {
		t.Fatalf("headers = %v, want key and attribution on the Toolbox URL", request.Header)
	}
}

func TestAIMLAPISearchDomainsAreSentAndStillFiltered(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: alwaysRespond(http.StatusOK, aimlapiCompletedRun)}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-aiml-key")))

	res := tool.Run(context.Background(), map[string]any{
		"query":   "go http timeout",
		"domains": []any{"https://WWW.Go.dev/doc", "pkg.go.dev", "go.dev"},
	})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
	calls := fake.recorded()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(calls))
	}
	input, _ := calls[0].body["input"].(map[string]any)
	if got, _ := json.Marshal(input["include_domains"]); string(got) != `["go.dev","pkg.go.dev"]` {
		t.Fatalf("include_domains = %s, want the normalized allowlist", got)
	}
	// The Toolbox filter is not trusted alone: a result outside the allowlist
	// is still dropped.
	if strings.Contains(res.Output, "stackoverflow.com") {
		t.Fatalf("result outside the allowlist was shown: %q", res.Output)
	}
	if !strings.Contains(res.Output, "https://pkg.go.dev/net/http") || !strings.Contains(res.Output, "https://go.dev/src/net/http/client.go") {
		t.Fatalf("allowed results missing: %q", res.Output)
	}
}

func TestWebSearchUnconfiguredHintNamesAIMLAPI(t *testing.T) {
	res := newWebSearchToolWithBackend(nil).Run(context.Background(), map[string]any{"query": "q"})
	if res.Status != StatusError || !strings.Contains(res.Output, "ZERO_WEBSEARCH_PROVIDER=aimlapi") {
		t.Fatalf("status = %v, output = %q", res.Status, res.Output)
	}
}

func TestAIMLAPISearchRedactsKeyInSuccessfulResults(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	const key = "zq7Mv2Lp9Xr4Tn8Wk3"
	fake := &fakeToolbox{respond: func(_ int, _ *http.Request) *http.Response {
		return toolboxResponse(http.StatusOK, `{"status":"completed","output":{"results":[{"title":"`+key+`","url":"https://example.com/`+key+`","snippet":"echo `+key+`"}]}}`)
	}}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey(key)))
	result := tool.Run(context.Background(), map[string]any{"query": "q"})
	if result.Status != StatusOK || strings.Contains(result.Output, key) {
		t.Fatalf("successful result leaked key: %s", result.Output)
	}
}

func TestAIMLAPISearchRedactsSplitKeyInSuccessfulResults(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	const key = "zq7Mv2Lp9Xr4Tn8Wk3"
	fake := &fakeToolbox{respond: func(_ int, _ *http.Request) *http.Response {
		return toolboxResponse(http.StatusOK, `{"status":"completed","output":{"results":[{"title":"`+splitAIMLAPIKey(key, `\u0000`)+`","url":"https://example.com/`+splitAIMLAPIKey(key, `\u200b`)+`","snippet":"echo `+splitAIMLAPIKey(key, `\u2060`)+`"}]}}`)
	}}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey(key)))
	result := tool.Run(context.Background(), map[string]any{"query": "q"})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, output = %q", result.Status, result.Output)
	}
	if strings.Contains(withoutInvisibleRunes(result.Output), key) {
		t.Fatalf("successful result leaked the split key: %q", result.Output)
	}
}

// splitAIMLAPIKey puts a JSON-escaped invisible character into the middle of
// key, as a response that echoes it that way would.
func splitAIMLAPIKey(key, escapedSeparator string) string {
	return key[:len(key)/2] + escapedSeparator + key[len(key)/2:]
}

// withoutInvisibleRunes drops the control and format characters a terminal,
// log viewer or copy-paste would never show, so a key split by one reads whole.
func withoutInvisibleRunes(value string) string {
	return strings.Map(func(r rune) rune {
		if r != '\t' && r != '\n' && r != '\r' && (unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)) {
			return -1
		}
		return r
	}, value)
}
func TestAIMLAPISearchEmptyScopedResultsReportAllowlist(t *testing.T) {
	isolateAIMLAPISearchEnv(t)
	fake := &fakeToolbox{respond: func(_ int, _ *http.Request) *http.Response {
		return toolboxResponse(http.StatusOK, `{"status":"completed","output":{"results":[]}}`)
	}}
	tool := newWebSearchToolWithBackend(newAIMLAPISearchBackend(fake, staticKey("test-key")))
	result := tool.Run(context.Background(), map[string]any{"query": "q", "domains": []any{"example.com"}})
	if result.Status != StatusError || !strings.Contains(result.Output, "loosen the allowlist") {
		t.Fatalf("missing allowlist guidance: %s", result.Output)
	}
}
