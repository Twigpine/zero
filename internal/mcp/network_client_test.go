package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTokenSource drives the OAuth round tripper without a real provider. It
// hands out a current access token and, on refresh, advances to a new one. A
// failing refresh returns an error to exercise the actionable-error path.
type fakeTokenSource struct {
	access      atomic.Value // string
	refreshFunc func() (string, error)
	refreshes   int32
}

func (s *fakeTokenSource) AccessToken(context.Context) (string, error) {
	value, _ := s.access.Load().(string)
	return value, nil
}

func (s *fakeTokenSource) Refresh(context.Context) (string, error) {
	atomic.AddInt32(&s.refreshes, 1)
	token, err := s.refreshFunc()
	if err != nil {
		return "", err
	}
	s.access.Store(token)
	return token, nil
}

func TestOAuthRoundTripperAttachesBearer(t *testing.T) {
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	source := &fakeTokenSource{}
	source.access.Store("access-1")
	source.refreshFunc = func() (string, error) { return "access-2", nil }

	client := &http.Client{Transport: newOAuthRoundTripper(http.DefaultTransport, source, "demo")}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resp.Body.Close()
	if got, _ := gotAuth.Load().(string); got != "Bearer access-1" {
		t.Fatalf("Authorization = %q, want Bearer access-1", got)
	}
	if atomic.LoadInt32(&source.refreshes) != 0 {
		t.Fatal("refresh called without a 401")
	}
}

func TestOAuthRoundTripperRefreshesOn401AndRetriesOnce(t *testing.T) {
	source := &fakeTokenSource{}
	source.access.Store("access-stale")
	source.refreshFunc = func() (string, error) { return "access-fresh", nil }

	var attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		auth := r.Header.Get("Authorization")
		if n == 1 {
			if auth != "Bearer access-stale" {
				t.Errorf("first attempt auth = %q", auth)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if auth != "Bearer access-fresh" {
			t.Errorf("retry auth = %q, want refreshed token", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: newOAuthRoundTripper(http.DefaultTransport, source, "demo")}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after refresh", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("upstream attempts = %d, want exactly 2 (retry once)", got)
	}
	if got := atomic.LoadInt32(&source.refreshes); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
}

func TestOAuthRoundTripperRefreshFailureSurfacesActionableError(t *testing.T) {
	source := &fakeTokenSource{}
	source.access.Store("access-stale")
	source.refreshFunc = func() (string, error) { return "", context.DeadlineExceeded }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: newOAuthRoundTripper(http.DefaultTransport, source, "demo")}
	_, err := client.Get(upstream.URL)
	if err == nil {
		t.Fatal("Get() error = nil, want actionable refresh failure")
	}
	if !strings.Contains(err.Error(), "zero mcp oauth login demo") {
		t.Fatalf("error = %q, want actionable re-login message", err.Error())
	}
}

func TestOAuthRoundTripperDoesNotRetryAfterSuccessfulNon401(t *testing.T) {
	source := &fakeTokenSource{}
	source.access.Store("access-1")
	source.refreshFunc = func() (string, error) { return "access-2", nil }

	var attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: newOAuthRoundTripper(http.DefaultTransport, source, "demo")}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 passed through", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on non-401)", got)
	}
	if got := atomic.LoadInt32(&source.refreshes); got != 0 {
		t.Fatalf("refreshes = %d, want 0", got)
	}
}

func TestOAuthRoundTripperRejectsCrossOriginRedirectBeforeBearerLeak(t *testing.T) {
	source := &fakeTokenSource{}
	source.access.Store("access-secret")
	source.refreshFunc = func() (string, error) { return "access-fresh", nil }

	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client := mcpHTTPClient(
		Server{Name: "oauth-docs", Type: ServerTypeHTTP},
		newOAuthRoundTripper(http.DefaultTransport, source, "oauth-docs"),
	)
	resp, err := client.Get(redirector.URL + "/mcp")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "cross-origin redirect") {
		t.Fatalf("Get() error = %v, want cross-origin redirect error", err)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

func TestSameMCPOriginNormalizesDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{
			name:  "https default port",
			left:  "https://example.test/mcp",
			right: "https://EXAMPLE.test:443/other",
			want:  true,
		},
		{
			name:  "http default port",
			left:  "http://example.test/mcp",
			right: "http://example.test:80/other",
			want:  true,
		},
		{
			name:  "different scheme",
			left:  "https://example.test/mcp",
			right: "http://example.test:443/other",
			want:  false,
		},
		{
			name:  "different port",
			left:  "https://example.test/mcp",
			right: "https://example.test:8443/other",
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, err := url.Parse(tc.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := url.Parse(tc.right)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameMCPOrigin(left, right); got != tc.want {
				t.Fatalf("sameMCPOrigin(%q, %q) = %v, want %v", tc.left, tc.right, got, tc.want)
			}
		})
	}
}

func TestResolveSSEEndpointURLRestrictsOrigin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		base     string
		endpoint string
		want     string
		wantErr  string
	}{
		{
			name:     "relative endpoint",
			base:     "https://example.test/sse",
			endpoint: "/messages",
			want:     "https://example.test/messages",
		},
		{
			name:     "absolute same origin",
			base:     "https://example.test/sse",
			endpoint: "https://example.test/messages",
			want:     "https://example.test/messages",
		},
		{
			name:     "default port equivalent",
			base:     "https://example.test/sse",
			endpoint: "https://example.test:443/messages",
			want:     "https://example.test:443/messages",
		},
		{
			name:     "cross origin",
			base:     "https://example.test/sse",
			endpoint: "https://evil.test/messages",
			wantErr:  "differs from configured server origin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSSEEndpointURL(tc.base, tc.endpoint)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveSSEEndpointURL() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSSEEndpointURL() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveSSEEndpointURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNonOAuthServerIsUnaffected(t *testing.T) {
	// Regression: a server without auth must not get a bearer header or any
	// OAuth round tripper, and must connect normally.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); strings.HasPrefix(got, "Bearer") {
			t.Errorf("non-oauth server received bearer header %q", got)
		}
		message := readHTTPRPCMessage(t, r)
		switch message.Method {
		case "initialize":
			writeHTTPRPCResponse(t, w, message.ID, map[string]any{"protocolVersion": "2024-11-05"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			writeHTTPRPCResponse(t, w, message.ID, map[string]any{})
		}
	}))
	defer upstream.Close()

	client, err := Connect(ctx, Server{Name: "plain", Type: ServerTypeHTTP, URL: upstream.URL})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNetworkClientRejectsServerInitiatedRequestAsResponse(t *testing.T) {
	// A streamable-HTTP server that answers a tools/call POST with a
	// method-bearing frame (a server-initiated request) must surface a protocol
	// error rather than an empty, error-free result.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := readHTTPRPCMessage(t, r)
		switch message.Method {
		case "initialize":
			writeHTTPRPCResponse(t, w, message.ID, map[string]any{"protocolVersion": "2024-11-05"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(mustRaw(message.ID)) + `,"method":"roots/list","params":{}}`)); err != nil {
				t.Errorf("write server request: %v", err)
			}
		default:
			writeHTTPRPCResponse(t, w, message.ID, map[string]any{})
		}
	}))
	defer upstream.Close()

	client, err := Connect(ctx, Server{Name: "plain", Type: ServerTypeHTTP, URL: upstream.URL})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	result, err := client.CallTool(ctx, "lookup", map[string]any{"query": "zero"})
	if err == nil {
		t.Fatalf("CallTool() error = nil, result = %#v, want protocol error", result)
	}
	if !strings.Contains(err.Error(), `"roots/list"`) {
		t.Fatalf("CallTool() error = %q, want it to name the unexpected method", err)
	}
}

func TestDecodeSSERPCMessageSkipsNotifications(t *testing.T) {
	// A leading server notification (has a method) on the POST's event stream must
	// be skipped so the actual response (no method) is returned, instead of the
	// notification surfacing as an id mismatch.
	stream := "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n\n" +
		"event: message\n" +
		`data: {"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n\n"

	msg, err := decodeSSERPCMessage(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("decodeSSERPCMessage: %v", err)
	}
	if msg.Method != "" {
		t.Fatalf("returned a server message (method %q), want the response", msg.Method)
	}
	if !rpcIDMatches(msg.ID, 7) {
		t.Fatalf("expected response id 7, got %#v", msg.ID)
	}
	if len(msg.Result) == 0 {
		t.Fatalf("expected a result payload, got %#v", msg)
	}
}

func TestDecodeSSERPCMessageSkipsEmptyAndNullMethod(t *testing.T) {
	stream := "event: message\n" +
		`data: {"jsonrpc":"2.0","id":7,"method":""}` + "\n\n" +
		"event: message\n" +
		`data: {"jsonrpc":"2.0","id":7,"method":null}` + "\n\n" +
		"event: message\n" +
		`data: {"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n\n"

	msg, err := decodeSSERPCMessage(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("decodeSSERPCMessage: %v", err)
	}
	if msg.isRequestOrNotification() {
		t.Fatalf("empty/null method must not be treated as the response, got method %q present=%v", msg.Method, msg.methodPresent)
	}
	if !rpcIDMatches(msg.ID, 7) {
		t.Fatalf("expected response id 7, got %#v", msg.ID)
	}
}

func TestDeliverEventMessageSkipsMethodPresence(t *testing.T) {
	client := &remoteSSEClient{pending: map[string]chan ssePendingResponse{}}
	key := rpcResponseKey(1)
	pending := make(chan ssePendingResponse, 1)
	client.pending[key] = pending

	if err := client.deliverEventMessage(`{"jsonrpc":"2.0","id":1,"method":""}`); err != nil {
		t.Fatalf("deliverEventMessage empty method: %v", err)
	}
	if err := client.deliverEventMessage(`{"jsonrpc":"2.0","id":1,"method":null}`); err != nil {
		t.Fatalf("deliverEventMessage null method: %v", err)
	}
	select {
	case got := <-pending:
		t.Fatalf("method presence must not complete pending, got %#v", got)
	default:
	}
	if _, ok := client.pending[key]; !ok {
		t.Fatal("deliverEventMessage must not delete pending for a request/notification")
	}

	if err := client.deliverEventMessage(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`); err != nil {
		t.Fatalf("deliverEventMessage response: %v", err)
	}
	select {
	case got := <-pending:
		if got.err != nil {
			t.Fatalf("true response: %v", got.err)
		}
		if got.message.isRequestOrNotification() {
			t.Fatal("true response must not carry a method")
		}
	default:
		t.Fatal("true response must complete pending")
	}
}

func TestScanSSEEventsLargeImagePayload(t *testing.T) {
	// Construct a 10 MiB image payload (~13.98 MiB in base64).
	tenMiB := 10 * 1024 * 1024
	imgB64 := paddedPNGBase64(tenMiB)
	resultJSON, err := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "analysis screenshot"},
			{"type": "image", "mimeType": "image/png", "data": imgB64},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rpcJSON, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  json.RawMessage(resultJSON),
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Single data line with 10 MiB image", func(t *testing.T) {
		stream := "event: message\ndata: " + string(rpcJSON) + "\n\n"
		msg, err := decodeSSERPCMessage(strings.NewReader(stream))
		if err != nil {
			t.Fatalf("decodeSSERPCMessage failed on single-line 10 MiB image: %v", err)
		}
		if !rpcIDMatches(msg.ID, 1) {
			t.Fatalf("expected id 1, got %#v", msg.ID)
		}
		var decoded CallToolResult
		if err := json.Unmarshal(msg.Result, &decoded); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if len(decoded.Content) != 2 || len(decoded.Content[1].Data) != len(imgB64) {
			t.Fatalf("image payload did not survive the SSE round trip: got %d bytes, want %d",
				len(decoded.Content[1].Data), len(imgB64))
		}
	})

	t.Run("Multi data lines with 10 MiB image", func(t *testing.T) {
		stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\ndata: \"result\":" + string(resultJSON) + "}\n\n"
		msg, err := decodeSSERPCMessage(strings.NewReader(stream))
		if err != nil {
			t.Fatalf("decodeSSERPCMessage failed on multi-line 10 MiB image: %v", err)
		}
		if !rpcIDMatches(msg.ID, 1) {
			t.Fatalf("expected id 1, got %#v", msg.ID)
		}
		var decoded CallToolResult
		if err := json.Unmarshal(msg.Result, &decoded); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if len(decoded.Content) != 2 || len(decoded.Content[1].Data) != len(imgB64) {
			t.Fatalf("image payload did not survive the SSE round trip: got %d bytes, want %d",
				len(decoded.Content[1].Data), len(imgB64))
		}
	})

	t.Run("Oversized event exceeding 32 MiB rejected cleanly", func(t *testing.T) {
		oversizedRPC, marshalErr := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"padding": strings.Repeat("A", 33*1024*1024)},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		stream := "event: message\ndata: " + string(oversizedRPC) + "\n\n"
		_, err := decodeSSERPCMessage(strings.NewReader(stream))
		if err == nil {
			t.Fatal("expected error for 33 MiB event, got nil")
		}
	})
}
