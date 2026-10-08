package tools

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/proxydial"
	zeroSandbox "github.com/Gitlawb/zero/internal/sandbox"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type webFetchResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (fn webFetchResolverFunc) LookupNetIP(ctx context.Context, network string, host string) ([]netip.Addr, error) {
	return fn(ctx, network, host)
}

type webFetchDialFunc func(context.Context, string, string) (net.Conn, error)

func (fn webFetchDialFunc) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	return fn(ctx, network, address)
}

func TestWebFetchToolSafetyAndSchema(t *testing.T) {
	tool := NewWebFetchTool()

	if tool.Name() != "web_fetch" {
		t.Fatalf("Name = %q, want web_fetch", tool.Name())
	}
	if tool.Description() == "" {
		t.Fatal("Description is empty")
	}
	safety := tool.Safety()
	if safety.SideEffect != SideEffectNetwork || safety.Permission != PermissionPrompt || !safety.AdvertiseInAuto {
		t.Fatalf("unexpected safety metadata: %#v", safety)
	}
	if safety.Reason == "" {
		t.Fatal("Safety reason is empty")
	}

	schema := tool.Parameters()
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("unexpected schema envelope: %#v", schema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "url" {
		t.Fatalf("required fields = %#v, want url only", schema.Required)
	}
	if schema.Properties["url"].Type != "string" {
		t.Fatalf("url schema = %#v, want string", schema.Properties["url"])
	}
	maxBytes := schema.Properties["max_bytes"]
	if maxBytes.Type != "integer" || maxBytes.Minimum == nil || *maxBytes.Minimum != 1 || maxBytes.Maximum == nil {
		t.Fatalf("max_bytes schema = %#v, want bounded integer", maxBytes)
	}
}

func TestWebFetchToolFetchesHTTPText(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", request.Method)
		}
		if request.Header.Get("User-Agent") == "" {
			t.Fatal("expected User-Agent header")
		}
		return webFetchTestResponse(request, http.StatusOK, "text/plain; charset=utf-8", "hello zero"), nil
	}))

	result := tool.Run(context.Background(), map[string]any{
		"url": "https://example.com/guide?token=secret-token",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	for _, want := range []string{"URL: https://example.com/guide?token=[REDACTED]", "Status: 200 OK", "Content-Type: text/plain; charset=utf-8", "hello zero"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("expected output to contain %q, got %q", want, result.Output)
		}
	}
	if strings.Contains(result.Output, "secret-token") {
		t.Fatalf("expected URL secrets to be redacted, got %q", result.Output)
	}
	if result.Meta["status_code"] != "200" || result.Meta["content_type"] != "text/plain; charset=utf-8" || result.Meta["truncated"] != "false" {
		t.Fatalf("unexpected metadata: %#v", result.Meta)
	}
}

func TestWebFetchToolTruncatesAtMaxBytes(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		return webFetchTestResponse(request, http.StatusOK, "text/plain", "abcdefg"), nil
	}))

	result := tool.Run(context.Background(), map[string]any{
		"url":       "https://example.com/long",
		"max_bytes": 4,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !result.Truncated || result.Meta["truncated"] != "true" || result.Meta["bytes"] != "4" {
		t.Fatalf("expected truncation metadata, got truncated=%v meta=%#v", result.Truncated, result.Meta)
	}
	if !strings.Contains(result.Output, "abcd") || strings.Contains(result.Output, "efg") {
		t.Fatalf("expected output to contain only truncated body, got %q", result.Output)
	}
}

func TestWebFetchToolRejectsUnsafeURLsBeforeNetwork(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe URL should be rejected before network transport")
		return nil, nil
	}))

	for _, rawURL := range []string{
		"file:///tmp/secret",
		"ftp://example.com/file",
		"http://127.0.0.1/admin",
		"http://localhost/status",
		"http://169.254.169.254/latest/meta-data",
		"http://100.64.0.1/internal",
		"http://0.1.2.3/internal",
		"http://255.255.255.255/internal",
		"http://[64:ff9b::7f00:1]/internal",
		"http://[64:ff9b::a9fe:a9fe]/latest/meta-data",
		"http://[64:ff9b:1:7f00:1::]/internal",
		"http://[2002:7f00:1::]/internal",
		"http://[::7f00:1]/internal",
		"http://user:pass@example.com/private",
	} {
		t.Run(rawURL, func(t *testing.T) {
			result := tool.Run(context.Background(), map[string]any{"url": rawURL})
			if result.Status != StatusError {
				t.Fatalf("expected unsafe URL error, got %s: %s", result.Status, result.Output)
			}
			if !strings.Contains(result.Output, "Unsafe URL") {
				t.Fatalf("expected unsafe URL message, got %q", result.Output)
			}
		})
	}
}

func TestWebFetchRejectBeforePermissionClassifiesURLs(t *testing.T) {
	tool, ok := NewWebFetchTool().(PrePermissionRejecter)
	if !ok {
		t.Fatalf("NewWebFetchTool returned %T, want PrePermissionRejecter", NewWebFetchTool())
	}

	cases := []struct {
		name         string
		args         map[string]any
		wantRejected bool
		wantContains string
	}{
		{
			name: "public remote",
			args: map[string]any{"url": "https://example.com/docs"},
		},
		{
			name: "hostname needs resolver later",
			args: map[string]any{"url": "https://private.example/status"},
		},
		{
			name:         "localhost custom port",
			args:         map[string]any{"url": "http://localhost:8000/status"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "loopback ipv4",
			args:         map[string]any{"url": "http://127.0.0.1/admin"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "loopback ipv6",
			args:         map[string]any{"url": "http://[::1]/admin"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "private ipv4",
			args:         map[string]any{"url": "http://10.0.0.1/status"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "private ipv6",
			args:         map[string]any{"url": "http://[fc00::1]/status"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "localhost suffix",
			args:         map[string]any{"url": "http://app.localhost/status"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "local suffix",
			args:         map[string]any{"url": "http://printer.local/status"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "metadata host",
			args:         map[string]any{"url": "http://metadata.google.internal/latest"},
			wantRejected: true,
			wantContains: "bash with curl",
		},
		{
			name:         "public non-default port",
			args:         map[string]any{"url": "https://example.com:8443/docs"},
			wantRejected: true,
			wantContains: "default ports",
		},
		{
			name:         "unsupported scheme",
			args:         map[string]any{"url": "ftp://example.com/file"},
			wantRejected: true,
			wantContains: "only http and https",
		},
		{
			name:         "bad max bytes",
			args:         map[string]any{"url": "https://example.com/docs", "max_bytes": 0},
			wantRejected: true,
			wantContains: "Invalid arguments",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, rejected := tool.RejectBeforePermission(tc.args)
			if rejected != tc.wantRejected {
				t.Fatalf("rejected = %v, want %v; result=%#v", rejected, tc.wantRejected, result)
			}
			if tc.wantContains != "" && !strings.Contains(result.Output, tc.wantContains) {
				t.Fatalf("expected output to contain %q, got %q", tc.wantContains, result.Output)
			}
		})
	}
}

func TestWebFetchToolRejectsLocalhostBeforeDefaultPort(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("localhost URL should be rejected before network transport")
		return nil, nil
	}))

	result := tool.Run(context.Background(), map[string]any{"url": "http://localhost:8000/status"})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "bash with curl") {
		t.Fatalf("expected curl guidance, got %q", result.Output)
	}
	if strings.Contains(result.Output, "default port") {
		t.Fatalf("localhost should be classified before port validation, got %q", result.Output)
	}
}

func TestWebFetchToolRejectsNonDefaultPorts(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("non-default port should be rejected before network transport")
		return nil, nil
	}))

	for _, rawURL := range []string{
		"http://example.com:22/",
		"https://example.com:80/",
		"https://example.com:6379/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			result := tool.Run(context.Background(), map[string]any{"url": rawURL})
			if result.Status != StatusError {
				t.Fatalf("expected unsafe URL error, got %s: %s", result.Status, result.Output)
			}
			if !strings.Contains(result.Output, "default port") {
				t.Fatalf("expected default-port message, got %q", result.Output)
			}
		})
	}
}

func TestWebFetchToolRejectsHostnamesResolvingToPrivateAddresses(t *testing.T) {
	tool := newWebFetchToolWithClientAndResolver(
		webFetchTestClient(func(*http.Request) (*http.Response, error) {
			t.Fatal("private resolved host should be rejected before network transport")
			return nil, nil
		}),
		webFetchResolverFunc(func(_ context.Context, network string, host string) ([]netip.Addr, error) {
			if network != "ip" || host != "private.example" {
				t.Fatalf("unexpected lookup network=%q host=%q", network, host)
			}
			return []netip.Addr{netip.MustParseAddr("10.0.0.12")}, nil
		}),
	)

	result := tool.Run(context.Background(), map[string]any{"url": "https://private.example/status"})

	if result.Status != StatusError {
		t.Fatalf("expected private resolved host error, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "private network hosts are blocked") {
		t.Fatalf("expected private host message, got %q", result.Output)
	}
}

func TestWebFetchToolConfiguresDialTimeSafetyForDefaultTransport(t *testing.T) {
	tool, ok := NewWebFetchTool().(webFetchTool)
	if !ok {
		t.Fatalf("NewWebFetchTool returned %T, want webFetchTool", NewWebFetchTool())
	}

	client := tool.clientForRun()
	// The round tripper is the route-recording wrapper, not the bare transport:
	// it is what tells the dialer which dial is the one to the proxy the
	// transport picked for a given request. The guarded transport it wraps
	// stays reachable, and is what the rest of this test inspects.
	transport, ok := client.Transport.(*proxydial.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *proxydial.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("expected web_fetch transport to install a safe DialContext")
	}
	// REVERSED ON PURPOSE. The guarded fetch used to pin Proxy to nil as part of
	// the SSRF hardening, and this assertion held it there. #569 is what that
	// cost: on a machine that reaches the network only through a configured
	// forward proxy, every fetch failed outright. The proxy is honoured now, and
	// the wrapper hands the dialer the route chosen for each request so the one
	// dial it lets past the guard is the one to that proxy. The target is still
	// validated before the request, which the tests below pin.
	if transport.Proxy == nil {
		t.Fatal("expected web_fetch transport to honour the environment proxy; a machine behind a forward proxy cannot fetch at all without it (#569)")
	}
}

func TestWebFetchSafeDialRejectsPrivateRebindAddress(t *testing.T) {
	dialCalled := false
	dial := webFetchSafeDialContext(
		webFetchResolverFunc(func(_ context.Context, network string, host string) ([]netip.Addr, error) {
			if network != "ip" || host != "rebind.example" {
				t.Fatalf("unexpected lookup network=%q host=%q", network, host)
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}),
		webFetchDialFunc(func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("dial should not run")
		}),
	)

	_, err := dial(context.Background(), "tcp", "rebind.example:443")

	if err == nil || !strings.Contains(err.Error(), "loopback hosts are blocked") {
		t.Fatalf("expected loopback rejection, got %v", err)
	}
	if dialCalled {
		t.Fatal("dialer was called after unsafe DNS result")
	}
}

func TestWebFetchSafeDialPinsResolvedPublicAddress(t *testing.T) {
	var dialedAddress string
	stop := errors.New("stop after address capture")
	dial := webFetchSafeDialContext(
		webFetchResolverFunc(func(_ context.Context, network string, host string) ([]netip.Addr, error) {
			if network != "ip" || host != "public.example" {
				t.Fatalf("unexpected lookup network=%q host=%q", network, host)
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}),
		webFetchDialFunc(func(_ context.Context, _ string, address string) (net.Conn, error) {
			dialedAddress = address
			return nil, stop
		}),
	)

	_, err := dial(context.Background(), "tcp", "public.example:443")

	if !errors.Is(err, stop) {
		t.Fatalf("expected captured dial error, got %v", err)
	}
	if dialedAddress != "8.8.8.8:443" {
		t.Fatalf("dialed address = %q, want resolved IP address", dialedAddress)
	}
}

func TestWebFetchToolRejectsUnsafeRedirects(t *testing.T) {
	for _, location := range []string{
		"http://127.0.0.1/private",
		"http://100.64.0.1/internal",
	} {
		t.Run(location, func(t *testing.T) {
			tool := newWebFetchToolWithClient(webFetchTestClient(func(request *http.Request) (*http.Response, error) {
				response := webFetchTestResponse(request, http.StatusFound, "text/plain", "redirect")
				response.Header.Set("Location", location)
				return response, nil
			}))

			result := tool.Run(context.Background(), map[string]any{"url": "https://example.com/start"})

			if result.Status != StatusError {
				t.Fatalf("expected redirect safety error, got %s: %s", result.Status, result.Output)
			}
			if !strings.Contains(result.Output, "unsafe redirect URL") {
				t.Fatalf("expected unsafe redirect message, got %q", result.Output)
			}
		})
	}
}

func TestWebFetchToolRedactsContentType(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		return webFetchTestResponse(request, http.StatusOK, "text/plain; token=secret-token", "hello"), nil
	}))

	result := tool.Run(context.Background(), map[string]any{"url": "https://example.com/redact"})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if strings.Contains(result.Output, "secret-token") || strings.Contains(result.Meta["content_type"], "secret-token") {
		t.Fatalf("expected content type to be redacted, output=%q meta=%#v", result.Output, result.Meta)
	}
	if !strings.Contains(result.Output, "token=[REDACTED]") || !strings.Contains(result.Meta["content_type"], "token=[REDACTED]") {
		t.Fatalf("expected redacted token in content type, output=%q meta=%#v", result.Output, result.Meta)
	}
}

func TestWebFetchToolRejectsNonSuccessStatus(t *testing.T) {
	tool := newWebFetchToolWithClient(webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		return webFetchTestResponse(request, http.StatusNotFound, "text/plain", "missing page"), nil
	}))

	result := tool.Run(context.Background(), map[string]any{"url": "https://example.com/missing"})

	if result.Status != StatusError {
		t.Fatalf("expected status error, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "HTTP 404 Not Found") || !strings.Contains(result.Output, "missing page") {
		t.Fatalf("unexpected non-success output: %q", result.Output)
	}
}

func webFetchTestClient(handler func(*http.Request) (*http.Response, error)) *http.Client {
	return &http.Client{Transport: roundTripFunc(handler)}
}

func webFetchTestResponse(request *http.Request, statusCode int, contentType string, body string) *http.Response {
	response := &http.Response{
		StatusCode: statusCode,
		Status:     httpStatusLine(statusCode),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
	if contentType != "" {
		response.Header.Set("Content-Type", contentType)
	}
	return response
}

func httpStatusLine(statusCode int) string {
	return strings.TrimSpace(strings.Join([]string{strconv.Itoa(statusCode), http.StatusText(statusCode)}, " "))
}

func TestWebFetchRunWithSandboxAllowsUnderShellNetworkDeny(t *testing.T) {
	called := false
	client := webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		called = true
		return webFetchTestResponse(request, http.StatusOK, "text/plain", "ok"), nil
	})
	tool, ok := newWebFetchToolWithClientAndResolver(client, publicWebFetchResolver(t, "example.com")).(webFetchTool)
	if !ok {
		t.Fatalf("newWebFetchToolWithClientAndResolver returned %T", newWebFetchToolWithClientAndResolver(client, nil))
	}

	engine := zeroSandbox.NewEngine(zeroSandbox.EngineOptions{
		Policy: zeroSandbox.Policy{Mode: zeroSandbox.ModeEnforce, Network: zeroSandbox.NetworkDeny},
	})
	result := tool.RunWithSandbox(context.Background(), map[string]any{"url": "https://example.com"}, engine)
	if result.Status != StatusOK {
		t.Fatalf("web_fetch must run under shell network deny, got %q: %s", result.Status, result.Output)
	}
	if !called {
		t.Fatal("web_fetch transport must be called")
	}
}

func TestWebFetchRunWithSandboxAllowsUnderShellNetworkAllow(t *testing.T) {
	called := false
	client := webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		called = true
		return webFetchTestResponse(request, http.StatusOK, "text/plain", "ok"), nil
	})
	tool := newWebFetchToolWithClientAndResolver(client, publicWebFetchResolver(t, "evil.test")).(webFetchTool)

	engine := zeroSandbox.NewEngine(zeroSandbox.EngineOptions{
		Policy: zeroSandbox.Policy{Mode: zeroSandbox.ModeEnforce, Network: zeroSandbox.NetworkAllow},
	})
	result := tool.RunWithSandbox(context.Background(), map[string]any{"url": "https://evil.test"}, engine)
	if result.Status != StatusOK {
		t.Fatalf("web_fetch must run under shell network allow, got %q: %s", result.Status, result.Output)
	}
	if !called {
		t.Fatal("web_fetch transport must be called")
	}
}

func TestRegistryRoutesWebFetchThroughSandboxPath(t *testing.T) {
	called := false
	client := webFetchTestClient(func(request *http.Request) (*http.Response, error) {
		called = true
		return webFetchTestResponse(request, http.StatusOK, "text/plain", "ok"), nil
	})
	registry := NewRegistry()
	registry.Register(newWebFetchToolWithClientAndResolver(client, publicWebFetchResolver(t, "evil.test")))

	engine := zeroSandbox.NewEngine(zeroSandbox.EngineOptions{
		Policy: zeroSandbox.Policy{Mode: zeroSandbox.ModeEnforce, Network: zeroSandbox.NetworkDeny},
	})
	res := registry.RunWithOptions(context.Background(), "web_fetch", map[string]any{"url": "https://evil.test"}, RunOptions{
		Sandbox:           engine,
		PermissionGranted: true,
	})
	if res.Status != StatusOK {
		t.Fatalf("registry must route web_fetch through RunWithSandbox, got %q: %s", res.Status, res.Output)
	}
	if !called {
		t.Fatal("web_fetch transport must be called")
	}
}

func publicWebFetchResolver(t *testing.T, expectedHost string) webFetchResolver {
	t.Helper()
	return webFetchResolverFunc(func(_ context.Context, network string, host string) ([]netip.Addr, error) {
		if network != "ip" || host != expectedHost {
			t.Fatalf("unexpected lookup network=%q host=%q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
}
