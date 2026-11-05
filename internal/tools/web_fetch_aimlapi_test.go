package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/config"
)

const webFetchAIMLAPITestKey = "aiml-test-key-0042"

// Trimmed from a real search1api/crawl run.
const webFetchAIMLAPICompletedRun = `{"id":"5SJmhg3ePaXxHh_BLdlS9","tool":"search1api/crawl","status":"completed","is_error":false,"untrusted":true,` +
	`"output":{"crawlParameters":{"url":"https://example.com/"},"results":{"title":"Example Domain","link":"https://example.com/",` +
	`"content":"This domain is for use in documentation examples without needing permission.","metadata":{"sourceUrl":"https://example.com/"}}},` +
	`"cost":{"quoted_usd":0.0013,"actual_usd":0.0013,"credits":2600,"over_max":false}}`

// Trimmed from a real 400 max_cost_exceeded refusal (same envelope for every tool).
const webFetchAIMLAPIMaxCostBody = `{"error":{"message":"The cheapest source costs 0.0091 USD, above the limit of 0.001 USD.","type":"invalid_request_error",` +
	`"code":"max_cost_exceeded","request_id":"hnbQUvsseM1EX9RrgPjBT","refused_by":"request","estimated_cost_usd":0.0091,"max_cost_usd":0.001}}`

var webFetchUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type recordedToolboxRequest struct {
	method string
	url    string
	header http.Header
	body   []byte
}

// webFetchFakeToolbox records every request that reaches the Toolbox client and
// answers from respond, indexed by request order.
type webFetchFakeToolbox struct {
	t        *testing.T
	mu       sync.Mutex
	requests []recordedToolboxRequest
	respond  func(index int, request *http.Request) *http.Response
}

func (fake *webFetchFakeToolbox) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			body, _ = io.ReadAll(request.Body)
		}
		fake.mu.Lock()
		index := len(fake.requests)
		fake.requests = append(fake.requests, recordedToolboxRequest{
			method: request.Method,
			url:    request.URL.String(),
			header: request.Header.Clone(),
			body:   body,
		})
		fake.mu.Unlock()
		if fake.respond == nil {
			fake.t.Errorf("unexpected Toolbox request %s %s", request.Method, request.URL)
			return webFetchTestResponse(request, http.StatusInternalServerError, "", ""), nil
		}
		return fake.respond(index, request), nil
	})}
}

func (fake *webFetchFakeToolbox) recorded() []recordedToolboxRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]recordedToolboxRequest(nil), fake.requests...)
}

type webFetchAIMLAPIHarness struct {
	tool    webFetchTool
	toolbox *webFetchFakeToolbox
	direct  *[]string
	sleeps  *[]time.Duration
}

// hermeticWebFetchAIMLAPIEnv redirects every root the real key lookup reads.
func hermeticWebFetchAIMLAPIEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_CRED_STORAGE", "file")
	t.Setenv("ZERO_PROVIDER", "")
	t.Setenv("AIMLAPI_API_KEY", "")
	t.Setenv("AIMLAPI_INFERENCE_URL", "")
	t.Setenv("AIMLAPI_PARTNER_ID", "")
}

// newWebFetchAIMLAPIHarness builds web_fetch with provider set to provider, a
// recording direct client, a fake Toolbox, a fake resolver, a fixed key and a
// sleep that only records.
func newWebFetchAIMLAPIHarness(t *testing.T, provider string, respond func(int, *http.Request) *http.Response) webFetchAIMLAPIHarness {
	t.Helper()
	hermeticWebFetchAIMLAPIEnv(t)
	t.Setenv(webFetchProviderEnv, provider)

	var mu sync.Mutex
	direct := []string{}
	sleeps := []time.Duration{}
	directClient := webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		direct = append(direct, request.URL.String())
		mu.Unlock()
		return webFetchTestResponse(request, http.StatusOK, "text/plain", "direct body"), nil
	})
	resolver := webFetchResolverFunc(func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
		if host == "private.example" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	tool := newWebFetchToolWithClientAndResolver(directClient, resolver).(webFetchTool)
	toolbox := &webFetchFakeToolbox{t: t, respond: respond}
	tool.aimlapi.client = toolbox.client()
	tool.aimlapi.serviceKey = func() (string, error) { return webFetchAIMLAPITestKey, nil }
	tool.aimlapi.sleep = func(ctx context.Context, delay time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, delay)
		mu.Unlock()
		return ctx.Err()
	}
	return webFetchAIMLAPIHarness{tool: tool, toolbox: toolbox, direct: &direct, sleeps: &sleeps}
}

func toolboxJSON(request *http.Request, statusCode int, body string) *http.Response {
	return webFetchTestResponse(request, statusCode, "application/json", body)
}

func completedToolboxRun(content string) string {
	encoded, _ := json.Marshal(content)
	return `{"id":"run-1","status":"completed","is_error":false,"output":{"results":{"title":"T","link":"https://example.com/","content":` +
		string(encoded) + `}},"cost":{"quoted_usd":0.0013,"actual_usd":0.0013,"over_max":false}}`
}

func decodeToolboxBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Toolbox body is not JSON: %v: %s", err, body)
	}
	return decoded
}

func TestWebFetchAIMLAPIKeyAloneKeepsDirectFetch(t *testing.T) {
	for _, provider := range []string{"", "  ", "firecrawl", "aimlapi-x"} {
		t.Run("provider="+provider, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, provider, nil)
			// A usable key from every source must not switch the backend.
			t.Setenv("AIMLAPI_API_KEY", webFetchAIMLAPITestKey)

			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/page"})

			if result.Status != StatusOK || !strings.Contains(result.Output, "direct body") {
				t.Fatalf("expected the direct fetch, got %s: %s", result.Status, result.Output)
			}
			if strings.Contains(result.Output, "Backend:") || result.Meta["backend"] != "" {
				t.Fatalf("direct fetch must not report a backend: %q %#v", result.Output, result.Meta)
			}
			if len(*h.direct) != 1 || len(h.toolbox.recorded()) != 0 {
				t.Fatalf("direct=%v toolbox=%d, want one direct request and no Toolbox request", *h.direct, len(h.toolbox.recorded()))
			}
		})
	}
}

func TestWebFetchAIMLAPISelectedRunsToolboxCrawl(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, " AIMLAPI ", func(_ int, request *http.Request) *http.Response {
		return toolboxJSON(request, http.StatusOK, webFetchAIMLAPICompletedRun)
	})

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/guide?page=2&q=go"})

	if result.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %s", result.Status, result.Output)
	}
	if len(*h.direct) != 0 {
		t.Fatalf("the direct fetch must not run, got %v", *h.direct)
	}
	requests := h.toolbox.recorded()
	if len(requests) != 1 {
		t.Fatalf("Toolbox requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.method != http.MethodPost || request.url != "https://tools.aimlapi.com/v1/tools/run" {
		t.Fatalf("request = %s %s, want POST https://tools.aimlapi.com/v1/tools/run", request.method, request.url)
	}
	body := decodeToolboxBody(t, request.body)
	idempotencyKey, _ := body["idempotency_key"].(string)
	if !webFetchUUIDPattern.MatchString(idempotencyKey) {
		t.Fatalf("idempotency_key = %q, want a UUIDv4", idempotencyKey)
	}
	want := map[string]any{
		"tool":            "search1api/crawl",
		"input":           map[string]any{"url": "https://example.com/guide?page=2&q=go"},
		"max_cost_usd":    0.005,
		"wait_ms":         float64(30000),
		"idempotency_key": idempotencyKey,
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %#v\nwant %#v", body, want)
	}
	for name, value := range map[string]string{
		"Authorization":        "Bearer " + webFetchAIMLAPITestKey,
		"X-AIMLAPI-Source":     "agent/zero",
		"X-AIMLAPI-Partner-ID": aimlapi.DefaultPartnerID,
		"User-Agent":           "zero-web-fetch/0.1",
		"Content-Type":         "application/json",
	} {
		if got := request.header.Get(name); got != value {
			t.Fatalf("header %s = %q, want %q", name, got, value)
		}
	}

	content := "This domain is for use in documentation examples without needing permission."
	for _, line := range []string{
		"URL: https://example.com/guide?page=2&q=go",
		"Status: completed (AI/ML API Toolbox run)",
		"Content-Type: text/markdown",
		"Bytes: 76",
		"Backend: aimlapi (search1api/crawl)",
		"\n\n" + content,
	} {
		if !strings.Contains(result.Output, line) {
			t.Fatalf("output missing %q:\n%s", line, result.Output)
		}
	}
	if result.Truncated {
		t.Fatal("result must not be truncated")
	}
	for key, value := range map[string]string{
		"status_code": "200", "content_type": "text/markdown", "bytes": "76", "truncated": "false",
		"backend": "aimlapi", "tool": "search1api/crawl", "run_id": "5SJmhg3ePaXxHh_BLdlS9", "cost_usd": "0.0013",
	} {
		if result.Meta[key] != value {
			t.Fatalf("meta[%s] = %q, want %q (meta %#v)", key, result.Meta[key], value, result.Meta)
		}
	}
}

func TestWebFetchAIMLAPIResolvesTheKeyOnEveryCall(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		return toolboxJSON(request, http.StatusOK, webFetchAIMLAPICompletedRun)
	})
	keys := []string{"first-key", "second-key"}
	calls := 0
	h.tool.aimlapi.serviceKey = func() (string, error) {
		key := keys[calls]
		calls++
		return key, nil
	}

	for range keys {
		if result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"}); result.Status != StatusOK {
			t.Fatalf("expected ok, got %s: %s", result.Status, result.Output)
		}
	}

	requests := h.toolbox.recorded()
	if len(requests) != 2 || requests[0].header.Get("Authorization") != "Bearer first-key" || requests[1].header.Get("Authorization") != "Bearer second-key" {
		t.Fatalf("each call must resolve the key again, got %d requests", len(requests))
	}
}

func TestWebFetchAIMLAPIWithoutUsableKeySendsNothing(t *testing.T) {
	for name, keyErr := range map[string]error{
		"missing":   config.ErrAIMLAPIKeyMissing,
		"proxy":     config.ErrAIMLAPIProxyProfile,
		"invalid":   config.ErrAIMLAPIKeyInvalid,
		"empty key": nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)
			h.tool.aimlapi.serviceKey = func() (string, error) { return "", keyErr }

			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

			assertWebFetchAIMLAPIKeyRefusal(t, h, result)
			if keyErr != nil && !strings.Contains(result.Output, keyErr.Error()) {
				t.Fatalf("expected the key error in the output, got %q", result.Output)
			}
		})
	}

	// The real lookup, against an empty hermetic home.
	t.Run("real lookup without a key", func(t *testing.T) {
		h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)
		h.tool.aimlapi.serviceKey = defaultWebFetchAIMLAPIBackend().serviceKey

		result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

		assertWebFetchAIMLAPIKeyRefusal(t, h, result)
		if !strings.Contains(result.Output, config.ErrAIMLAPIKeyMissing.Error()) {
			t.Fatalf("expected the missing-key error, got %q", result.Output)
		}
	})

	// The real lookup withholds an environment key once the inference URL
	// points at a proxy, and web_fetch then sends nothing.
	t.Run("real lookup with a proxied key", func(t *testing.T) {
		h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)
		h.tool.aimlapi.serviceKey = defaultWebFetchAIMLAPIBackend().serviceKey
		t.Setenv("AIMLAPI_API_KEY", webFetchAIMLAPITestKey)
		t.Setenv("AIMLAPI_INFERENCE_URL", "https://proxy.example/v1")

		result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

		assertWebFetchAIMLAPIKeyRefusal(t, h, result)
		if !strings.Contains(result.Output, config.ErrAIMLAPIProxyProfile.Error()) {
			t.Fatalf("expected the proxy-profile error, got %q", result.Output)
		}
	})
}

func assertWebFetchAIMLAPIKeyRefusal(t *testing.T, h webFetchAIMLAPIHarness, result Result) {
	t.Helper()
	if result.Status != StatusError {
		t.Fatalf("expected an error result, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "needs a usable AI/ML API key") || !strings.Contains(result.Output, "does not fall back") {
		t.Fatalf("unexpected refusal: %q", result.Output)
	}
	if len(h.toolbox.recorded()) != 0 || len(*h.direct) != 0 {
		t.Fatalf("no request may be sent: toolbox=%d direct=%v", len(h.toolbox.recorded()), *h.direct)
	}
}

func TestWebFetchAIMLAPIRefusesUnsafeURLsBeforeTheToolbox(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1/admin",
		"http://localhost/status",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1/",
		"https://private.example/internal",
		"ftp://example.com/file",
		"https://example.com:8443/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)

			result := h.tool.Run(context.Background(), map[string]any{"url": rawURL})

			if result.Status != StatusError || !strings.Contains(result.Output, "Unsafe URL") {
				t.Fatalf("expected an unsafe URL error, got %s: %s", result.Status, result.Output)
			}
			if len(h.toolbox.recorded()) != 0 || len(*h.direct) != 0 {
				t.Fatalf("an unsafe URL must not reach anyone: toolbox=%d direct=%v", len(h.toolbox.recorded()), *h.direct)
			}
		})
	}

	h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)
	if _, rejected := h.tool.RejectBeforePermission(map[string]any{"url": "http://localhost/"}); !rejected {
		t.Fatal("RejectBeforePermission must still refuse localhost with the Toolbox backend")
	}
}

func TestWebFetchAIMLAPIRefusesNonToolboxHosts(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)
	h.tool.aimlapi.baseURL = "https://tools.aimlapi.com.evil.example/v1/tools"

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

	if result.Status != StatusError || !strings.Contains(result.Output, "refusing to send the AI/ML API key") {
		t.Fatalf("expected a fail-closed refusal, got %s: %s", result.Status, result.Output)
	}
	if len(h.toolbox.recorded()) != 0 {
		t.Fatalf("the key must not leave for a non-Toolbox host, got %d requests", len(h.toolbox.recorded()))
	}
}

func TestWebFetchAIMLAPIDefaultClientIsPlainAndRefusesRedirects(t *testing.T) {
	client := defaultWebFetchAIMLAPIBackend().httpClient()
	// A nil transport is http.DefaultTransport: the proxy environment applies
	// and the target-site SSRF pin does not.
	if client.Transport != nil {
		t.Fatalf("Toolbox client transport = %T, want the default transport", client.Transport)
	}
	if client.CheckRedirect == nil || client.CheckRedirect(nil, nil) == nil {
		t.Fatal("Toolbox client must refuse redirects")
	}
	if defaultWebFetchAIMLAPIBackend().baseURL != aimlapi.ToolboxBaseURL {
		t.Fatalf("baseURL = %q, want %q", defaultWebFetchAIMLAPIBackend().baseURL, aimlapi.ToolboxBaseURL)
	}
}

func TestWebFetchAIMLAPIRefusesRedirects(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(index int, request *http.Request) *http.Response {
		if index == 0 {
			response := toolboxJSON(request, http.StatusFound, "")
			response.Header.Set("Location", "https://collector.example/steal")
			return response
		}
		return toolboxJSON(request, http.StatusOK, webFetchAIMLAPICompletedRun)
	})

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

	if result.Status != StatusError || !strings.Contains(result.Output, "redirects are refused") {
		t.Fatalf("expected a redirect refusal, got %s: %s", result.Status, result.Output)
	}
	if requests := h.toolbox.recorded(); len(requests) != 1 {
		t.Fatalf("the redirect must not be followed, got %d requests (last %s)", len(requests), requests[len(requests)-1].url)
	}
}

func TestWebFetchAIMLAPIPollsAQueuedRun(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(index int, request *http.Request) *http.Response {
		switch index {
		case 0:
			return toolboxJSON(request, http.StatusAccepted, `{"id":"run/7","status":"queued","next_poll_hint_ms":10}`)
		case 1:
			return toolboxJSON(request, http.StatusServiceUnavailable, `{"error":{"code":"unavailable"}}`)
		case 2:
			return toolboxJSON(request, http.StatusOK, `{"id":"run/7","status":"running","next_poll_hint_ms":60000}`)
		default:
			return toolboxJSON(request, http.StatusOK, completedToolboxRun("polled page"))
		}
	})
	deadlineChecked := false
	h.tool.aimlapi.sleep = func(ctx context.Context, delay time.Duration) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > webFetchAIMLAPITimeout {
			t.Errorf("polling must run under the %s overall deadline (deadline %v, set %v)", webFetchAIMLAPITimeout, deadline, ok)
		}
		deadlineChecked = true
		*h.sleeps = append(*h.sleeps, delay)
		return nil
	}

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

	if result.Status != StatusOK || !strings.Contains(result.Output, "polled page") {
		t.Fatalf("expected the polled content, got %s: %s", result.Status, result.Output)
	}
	requests := h.toolbox.recorded()
	if len(requests) != 4 {
		t.Fatalf("requests = %d, want POST and three polls", len(requests))
	}
	for _, request := range requests[1:] {
		if request.method != http.MethodGet || request.url != "https://tools.aimlapi.com/v1/tools/runs/run%2F7" {
			t.Fatalf("poll = %s %s, want GET of the escaped run id", request.method, request.url)
		}
		if request.header.Get("Authorization") != "Bearer "+webFetchAIMLAPITestKey || request.header.Get("X-AIMLAPI-Source") != "agent/zero" {
			t.Fatalf("poll must carry auth and attribution, got %v", request.header)
		}
	}
	// Hints are clamped to 250 ms..5 s; the 503 poll reuses the queued run's hint.
	wantSleeps := []time.Duration{250 * time.Millisecond, 250 * time.Millisecond, 5 * time.Second}
	if !deadlineChecked || !reflect.DeepEqual(*h.sleeps, wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", *h.sleeps, wantSleeps)
	}
}

func TestWebFetchAIMLAPIStopsPollingWhenTheContextEnds(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		if request.Method == http.MethodGet {
			t.Error("poll GET sent after canceled sleep")
			return toolboxJSON(request, http.StatusOK, `{"id":"run-1","status":"failed"}`)
		}
		return toolboxJSON(request, http.StatusAccepted, `{"id":"run-1","status":"queued"}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.tool.aimlapi.sleep = func(sleepCtx context.Context, delay time.Duration) error {
		cancel()
		return sleepWebFetchContext(sleepCtx, delay)
	}

	result := h.tool.Run(ctx, map[string]any{"url": "https://example.com/"})

	if result.Status != StatusError || !strings.Contains(result.Output, "context canceled") {
		t.Fatalf("expected a cancellation error, got %s: %s", result.Status, result.Output)
	}
}

func TestWebFetchAIMLAPIReportsFailedRuns(t *testing.T) {
	for name, body := range map[string]string{
		"is_error with completed": `{"id":"run-1","status":"completed","is_error":true,"error":{"message":"Target returned HTTP 404","kind":"provider_error"},` +
			`"output":{"results":{"content":"stale page"}}}`,
		"failed status": `{"id":"run-1","status":"failed","is_error":false,"output":{"results":{"content":"stale page"}}}`,
		"no content":    `{"id":"run-1","status":"completed","is_error":false,"output":{"results":{"content":""}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
				return toolboxJSON(request, http.StatusOK, body)
			})

			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/missing"})

			if result.Status != StatusError || strings.Contains(result.Output, "stale page") {
				t.Fatalf("expected an error without content, got %s: %s", result.Status, result.Output)
			}
			if len(*h.direct) != 0 {
				t.Fatalf("a failed scrape must not fall back to a direct fetch, got %v", *h.direct)
			}
			if name == "is_error with completed" && !strings.Contains(result.Output, "Target returned HTTP 404") {
				t.Fatalf("expected the run error message, got %q", result.Output)
			}
		})
	}
}

func TestWebFetchAIMLAPIDoesNotRetryMaxCostExceeded(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		return toolboxJSON(request, http.StatusBadRequest, webFetchAIMLAPIMaxCostBody)
	})

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

	if result.Status != StatusError {
		t.Fatalf("expected an error, got %s: %s", result.Status, result.Output)
	}
	for _, want := range []string{"HTTP 400 Bad Request", "(max_cost_exceeded)", "request_id hnbQUvsseM1EX9RrgPjBT", "not billed"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("output missing %q: %q", want, result.Output)
		}
	}
	if len(h.toolbox.recorded()) != 1 || len(*h.sleeps) != 0 {
		t.Fatalf("max_cost_exceeded must not be retried: requests=%d sleeps=%v", len(h.toolbox.recorded()), *h.sleeps)
	}
}

func TestWebFetchAIMLAPIRetriesWithTheSameRunRequest(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(index int, request *http.Request) *http.Response {
		switch index {
		case 0:
			response := toolboxJSON(request, http.StatusTooManyRequests, `{"error":{"code":"rate_limited","message":"slow down"}}`)
			response.Header.Set("Retry-After", "2")
			return response
		case 1:
			return toolboxJSON(request, http.StatusBadGateway, `{"error":{"code":"bad_gateway"}}`)
		default:
			return toolboxJSON(request, http.StatusOK, webFetchAIMLAPICompletedRun)
		}
	})

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

	if result.Status != StatusOK {
		t.Fatalf("expected success after retries, got %s: %s", result.Status, result.Output)
	}
	requests := h.toolbox.recorded()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	for _, request := range requests[1:] {
		if request.method != http.MethodPost || string(request.body) != string(requests[0].body) {
			t.Fatalf("a retry must resend the identical body:\n%s\n%s", requests[0].body, request.body)
		}
	}
	// Retry-After: 2 is honoured; the 502 without one backs off 500 ms << 1.
	if want := []time.Duration{2 * time.Second, time.Second}; !reflect.DeepEqual(*h.sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", *h.sleeps, want)
	}
}

func TestWebFetchAIMLAPIGivesUpAfterTheLastAttempt(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		response := toolboxJSON(request, http.StatusTooManyRequests, `{"error":{"code":"rate_limited","message":"too many concurrent runs"}}`)
		response.Header.Set("Retry-After", "3600")
		return response
	})

	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

	if result.Status != StatusError || !strings.Contains(result.Output, "(rate_limited): too many concurrent runs") {
		t.Fatalf("expected the rate-limit error, got %s: %s", result.Status, result.Output)
	}
	if len(h.toolbox.recorded()) != 3 {
		t.Fatalf("requests = %d, want 3 attempts", len(h.toolbox.recorded()))
	}
	// A huge Retry-After is capped.
	if want := []time.Duration{10 * time.Second, 10 * time.Second}; !reflect.DeepEqual(*h.sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", *h.sleeps, want)
	}
}

func TestWebFetchAIMLAPIUsesAFreshIdempotencyKeyPerCall(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		return toolboxJSON(request, http.StatusOK, webFetchAIMLAPICompletedRun)
	})

	for range 2 {
		if result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"}); result.Status != StatusOK {
			t.Fatalf("expected ok, got %s: %s", result.Status, result.Output)
		}
	}

	requests := h.toolbox.recorded()
	first, _ := decodeToolboxBody(t, requests[0].body)["idempotency_key"].(string)
	second, _ := decodeToolboxBody(t, requests[1].body)["idempotency_key"].(string)
	if !webFetchUUIDPattern.MatchString(first) || !webFetchUUIDPattern.MatchString(second) || first == second {
		t.Fatalf("each web_fetch call needs its own UUID idempotency key, got %q and %q", first, second)
	}
}

func TestWebFetchAIMLAPITruncatesAtMaxBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		content  string
		maxBytes int
		want     string
	}{
		"ascii":         {content: "abcdefghij", maxBytes: 4, want: "abcd"},
		"rune boundary": {content: "aé-tail", maxBytes: 2, want: "a"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
				return toolboxJSON(request, http.StatusOK, completedToolboxRun(tc.content))
			})

			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/", "max_bytes": tc.maxBytes})

			if result.Status != StatusOK || !result.Truncated || result.Meta["truncated"] != "true" {
				t.Fatalf("expected a truncated result, got %s truncated=%v meta=%#v", result.Status, result.Truncated, result.Meta)
			}
			wantBytes := len(tc.want)
			if result.Meta["bytes"] != strconv.Itoa(wantBytes) || !strings.Contains(result.Output, "Bytes: "+strconv.Itoa(wantBytes)+"\n") {
				t.Fatalf("byte count must describe the cut body, meta=%#v output=%q", result.Meta, result.Output)
			}
			if !strings.HasSuffix(result.Output, "\n\n"+tc.want) {
				t.Fatalf("output must end with %q, got %q", tc.want, result.Output)
			}
		})
	}
}

func TestWebFetchAIMLAPIFormats(t *testing.T) {
	content := "# Title\n\n<b>kept</b> as the crawler sent it"
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		return toolboxJSON(request, http.StatusOK, completedToolboxRun(content))
	})

	raw := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/", "format": "raw"})
	if raw.Status != StatusError || !strings.Contains(raw.Output, `format "raw" is not available`) {
		t.Fatalf("format raw must be refused, got %s: %s", raw.Status, raw.Output)
	}
	if len(h.toolbox.recorded()) != 0 {
		t.Fatal("format raw must be refused before a billed run")
	}

	for _, format := range []string{"auto", "markdown"} {
		result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/", "format": format})
		if result.Status != StatusOK || !strings.HasSuffix(result.Output, "\n\n"+content) {
			t.Fatalf("format %s must return the crawler's Markdown unchanged, got %s: %q", format, result.Status, result.Output)
		}
	}
}

func TestWebFetchAIMLAPIRedactsTheKey(t *testing.T) {
	// The key contains "/", which a JSON encoder may write as "\/": the key is
	// scrubbed from the decoded text, so the escaped echo is caught as well.
	const key = "aiml/Zx9-echo-key"
	for name, respond := range map[string]func(*http.Request) *http.Response{
		"error envelope": func(request *http.Request) *http.Response {
			return toolboxJSON(request, http.StatusUnauthorized, `{"error":{"code":"invalid_api_key","message":"key aiml\/Zx9-echo-key is not valid","request_id":"r-1"}}`)
		},
		"plain error body": func(request *http.Request) *http.Response {
			return webFetchTestResponse(request, http.StatusForbidden, "text/plain", "denied for Bearer "+key)
		},
		"run error": func(request *http.Request) *http.Response {
			return toolboxJSON(request, http.StatusOK, `{"id":"r","status":"completed","is_error":true,"error":{"message":"bad key `+key+`"}}`)
		},
		"content": func(request *http.Request) *http.Response {
			return toolboxJSON(request, http.StatusOK, completedToolboxRun("page echoing "+key))
		},
		// The shown body is cut to 4 KiB; the key straddles that cut, so it
		// must be scrubbed before the cut or a fragment of it survives.
		"key at the cut": func(request *http.Request) *http.Response {
			return webFetchTestResponse(request, http.StatusForbidden, "text/plain", strings.Repeat("x", webFetchErrorBodyLimit-8)+key)
		},
		// A key split by an invisible character reads whole once a terminal or
		// log viewer drops that character, so it must be scrubbed as well.
		"split key in error body": func(request *http.Request) *http.Response {
			return webFetchTestResponse(request, http.StatusForbidden, "text/plain", "denied for Bearer aiml/Zx9\u200b-echo-key")
		},
		"split key in run error": func(request *http.Request) *http.Response {
			return toolboxJSON(request, http.StatusOK, `{"id":"r","status":"completed","is_error":true,"error":{"message":"bad key aiml/Zx9\u0000-echo-key"}}`)
		},
		"split key in content": func(request *http.Request) *http.Response {
			return toolboxJSON(request, http.StatusOK, completedToolboxRun("page echoing aiml/Zx9\u200b-echo-key"))
		},
		"split key in run id": func(request *http.Request) *http.Response {
			return toolboxJSON(request, http.StatusOK, strings.Replace(completedToolboxRun("page"), `"id":"run-1"`, `"id":"run aiml/Zx9\u0000-echo-key"`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response { return respond(request) })
			h.tool.aimlapi.serviceKey = func() (string, error) { return key, nil }

			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

			if output := withoutInvisibleRunes(result.Output); strings.Contains(output, key) || strings.Contains(output, "Zx9-echo-key") || strings.Contains(output, "aiml/Z") {
				t.Fatalf("the key leaked: %q", result.Output)
			}
			for metaKey, value := range result.Meta {
				if strings.Contains(withoutInvisibleRunes(value), "Zx9-echo-key") {
					t.Fatalf("the key leaked in meta[%s]: %q", metaKey, value)
				}
			}
			// The cut case keeps only the start of the marker.
			if !strings.Contains(result.Output, "[REDAC") && !strings.Contains(result.Meta["run_id"], "[REDAC") {
				t.Fatalf("expected a redaction marker, got %q", result.Output)
			}
		})
	}
}

// A transport failure is reported by its cause, not by the *url.Error around
// it: that quotes the polling URL, whose run id the Toolbox chose and which
// carries any key echoed in it percent-escaped, past the key scrubbing.
func TestWebFetchAIMLAPITransportErrorOmitsPollingURL(t *testing.T) {
	for name, separator := range map[string]string{"NUL": `\u0000`, "ZWSP": `\u200b`} {
		t.Run(name, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", nil)
			queued := `{"id":"run_` + splitAIMLAPIKey(webFetchAIMLAPITestKey, separator) + `","status":"queued","next_poll_hint_ms":1}`
			polls := 0
			h.tool.aimlapi.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodPost {
					return toolboxJSON(request, http.StatusAccepted, queued), nil
				}
				polls++
				return nil, errors.New("connection reset by peer")
			})}

			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})

			if polls != 1 {
				t.Fatalf("polls = %d, want the failing poll to be sent once", polls)
			}
			decoded, err := url.PathUnescape(result.Output)
			if err != nil {
				decoded = result.Output
			}
			if strings.Contains(withoutInvisibleRunes(decoded), webFetchAIMLAPITestKey) {
				t.Fatalf("the AI/ML API key leaked into the output: %q", result.Output)
			}
			if strings.Contains(result.Output, "/runs/") {
				t.Fatalf("the polling URL leaked into the output: %q", result.Output)
			}
			if result.Status != StatusError || !strings.Contains(result.Output, "connection reset by peer") {
				t.Fatalf("status = %v, output = %q, want the transport failure", result.Status, result.Output)
			}
		})
	}
}

func TestWebFetchAIMLAPIRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		header  string
		attempt int
		want    time.Duration
	}{
		{"", 1, 500 * time.Millisecond},
		{"", 2, time.Second},
		{"3", 1, 3 * time.Second},
		{"0", 1, 0},
		{"99999999999999999999", 1, 500 * time.Millisecond},
		{"99999999999999999", 1, 10 * time.Second},
		{"86400", 1, 10 * time.Second},
		{"soon", 2, time.Second},
	} {
		if got := webFetchAIMLAPIRetryDelay(tc.header, tc.attempt); got != tc.want {
			t.Errorf("retry delay(%q, %d) = %v, want %v", tc.header, tc.attempt, got, tc.want)
		}
	}
	if got := webFetchAIMLAPIRetryDelay(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), 1); got != 10*time.Second {
		t.Errorf("an HTTP-date Retry-After an hour out = %v, want the 10s cap", got)
	}
	if err := sleepWebFetchContext(context.Background(), 0); err != nil {
		t.Fatalf("zero sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepWebFetchContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep = %v, want context.Canceled", err)
	}
}

func TestWebFetchAIMLAPIRawRejectedBeforePermission(t *testing.T) {
	tool := newWebFetchToolWithClientAndResolver(nil, nil)
	for _, provider := range []string{"aimlapi", ""} {
		t.Setenv("ZERO_WEBFETCH_PROVIDER", provider)
		for _, format := range []string{"raw", " RAW ", "markdown", "auto"} {
			result, rejected := tool.(interface {
				RejectBeforePermission(map[string]any) (Result, bool)
			}).RejectBeforePermission(map[string]any{"url": "https://example.com/", "format": format})
			want := provider == "aimlapi" && strings.EqualFold(strings.TrimSpace(format), "raw")
			if rejected != want {
				t.Fatalf("provider=%q format=%q: rejected=%v result=%s", provider, format, rejected, result.Output)
			}
		}
	}
}
func TestWebFetchAIMLAPIPollHonorsRetryAfter(t *testing.T) {
	for _, header := range []string{"2", time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat)} {
		t.Run(header, func(t *testing.T) {
			h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(index int, request *http.Request) *http.Response {
				if index == 0 {
					return toolboxJSON(request, http.StatusAccepted, `{"id":"run-1","status":"queued","next_poll_hint_ms":250}`)
				}
				if index == 1 {
					response := toolboxJSON(request, http.StatusTooManyRequests, `{}`)
					response.Header.Set("Retry-After", header)
					return response
				}
				return toolboxJSON(request, http.StatusOK, webFetchAIMLAPICompletedRun)
			})
			result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/"})
			if result.Status != StatusOK {
				t.Fatalf("fetch failed: %s", result.Output)
			}
			if sleeps := *h.sleeps; len(sleeps) != 2 || sleeps[1] < 2*time.Second {
				t.Fatalf("Retry-After not honored: %v", sleeps)
			}
		})
	}
}
func TestWebFetchAIMLAPIOversizedRunIsRejected(t *testing.T) {
	h := newWebFetchAIMLAPIHarness(t, "aimlapi", func(_ int, request *http.Request) *http.Response {
		return toolboxJSON(request, http.StatusOK, completedToolboxRun(strings.Repeat("x", webFetchAIMLAPIResponseLimit)))
	})
	result := h.tool.Run(context.Background(), map[string]any{"url": "https://example.com/", "max_bytes": 32})
	if result.Status != StatusError || !strings.Contains(result.Output, "larger than") {
		t.Fatalf("oversized envelope accepted: %s", result.Output)
	}
}
