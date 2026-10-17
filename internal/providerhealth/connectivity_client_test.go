package providerhealth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
)

func TestDialValidatedAddrsFallsBackPastDeadAddress(t *testing.T) {
	addrs := []netip.Addr{
		netip.MustParseAddr("203.0.113.1"), // dead first record
		netip.MustParseAddr("203.0.113.2"), // reachable second record
	}
	var dialed []string
	dial := func(_ context.Context, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if address == "203.0.113.2:443" {
			return stubConn{}, nil
		}
		return nil, errors.New("connection refused")
	}

	conn, err := dialValidatedAddrs(context.Background(), addrs, "443", dial)
	if err != nil {
		t.Fatalf("expected fallback to the reachable address, got err %v", err)
	}
	_ = conn.Close()
	if len(dialed) != 2 || dialed[0] != "203.0.113.1:443" || dialed[1] != "203.0.113.2:443" {
		t.Fatalf("expected both addresses dialed in order, got %v", dialed)
	}
}

func TestDialValidatedAddrsReturnsLastErrorWhenAllFail(t *testing.T) {
	addrs := []netip.Addr{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2")}
	firstErr := errors.New("first unreachable")
	lastErr := errors.New("last unreachable")
	attempts := 0
	dial := func(_ context.Context, _ string) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			return nil, firstErr
		}
		return nil, lastErr
	}
	_, err := dialValidatedAddrs(context.Background(), addrs, "443", dial)
	if err == nil {
		t.Fatal("expected an error when every address fails to dial")
	}
	// Distinct per-attempt errors lock in the "last error wins" contract — a
	// regression that returned the first failure would otherwise pass.
	if !errors.Is(err, lastErr) {
		t.Fatalf("err = %v, want the last attempt's error", err)
	}
	if attempts != len(addrs) {
		t.Fatalf("dialed %d addresses, want %d (every address tried)", attempts, len(addrs))
	}
}

type stubConn struct{ net.Conn }

func (stubConn) Close() error { return nil }

func TestSafeDialContextRejectsResolvedPrivateAddress(t *testing.T) {
	dial := safeDialContext(staticResolver{addr: netip.MustParseAddr("10.0.0.5")}, false)

	conn, err := dial(context.Background(), "tcp", "api.example.com:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("dialed a host that resolved to a private address")
	}
	var safety endpointSafetyError
	if !errors.As(err, &safety) {
		t.Fatalf("err = %v, want endpointSafetyError", err)
	}
}

func TestSafeDialContextRejectsLiteralLinkLocalAddress(t *testing.T) {
	// The cloud metadata address must be refused even when supplied as a literal,
	// without consulting the resolver and without opening a socket.
	dial := safeDialContext(staticResolver{err: errors.New("resolver must not be called")}, false)

	conn, err := dial(context.Background(), "tcp", "169.254.169.254:80")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("dialed a link-local literal address")
	}
	var safety endpointSafetyError
	if !errors.As(err, &safety) {
		t.Fatalf("err = %v, want endpointSafetyError", err)
	}
}

func TestConnectivityClientRefusesRedirectToBlockedHost(t *testing.T) {
	client := newConnectivityClient(5*time.Second, staticResolver{addr: netip.MustParseAddr("169.254.169.254")}, nil, false)
	if client.CheckRedirect == nil {
		t.Fatal("default connectivity client has no CheckRedirect guard")
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://metadata.internal/latest", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Fatal("CheckRedirect allowed a redirect to a metadata address")
	}
}

func TestConnectivityClientRefusesTooManyRedirects(t *testing.T) {
	client := newConnectivityClient(5*time.Second, staticResolver{addr: netip.MustParseAddr("93.184.216.34")}, nil, false)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.example.com/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	via := make([]*http.Request, maxConnectivityRedirects)
	if err := client.CheckRedirect(req, via); err == nil {
		t.Fatalf("CheckRedirect allowed more than %d redirects", maxConnectivityRedirects)
	}
}

func TestConnectivityClientStripsCredentialsOnCrossHostRedirect(t *testing.T) {
	// A 3xx to a different host must not carry the provider credential. Go's stdlib
	// strips Authorization/Cookie but NOT x-api-key/x-goog-api-key/custom headers,
	// so the CheckRedirect guard must delete them on a host change (AUDIT-M10).
	sensitive := sensitiveAuthHeaderNames(config.ProviderProfile{
		AuthHeader:    "x-custom-token",
		CustomHeaders: map[string]string{"x-tenant-secret": "s3cr3t"},
	}, config.ProviderKindAnthropic)
	client := newConnectivityClient(5*time.Second, staticResolver{addr: netip.MustParseAddr("93.184.216.34")}, sensitive, false)

	orig, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	redirect, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://evil.example.net/v1/models", nil)
	for _, h := range []string{"x-api-key", "x-custom-token", "x-tenant-secret", "Authorization"} {
		redirect.Header.Set(h, "leak-me")
	}
	if err := client.CheckRedirect(redirect, []*http.Request{orig}); err != nil {
		t.Fatalf("cross-host redirect to a public host should be followed (with creds stripped), got %v", err)
	}
	for _, h := range []string{"x-api-key", "x-custom-token", "x-tenant-secret", "Authorization"} {
		if v := redirect.Header.Get(h); v != "" {
			t.Errorf("credential header %q leaked across host redirect: %q", h, v)
		}
	}

	// Same-host redirect (only the path changes) must keep the credential.
	same, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.anthropic.com/v1/other", nil)
	same.Header.Set("x-api-key", "keep-me")
	if err := client.CheckRedirect(same, []*http.Request{orig}); err != nil {
		t.Fatalf("same-host redirect rejected: %v", err)
	}
	if same.Header.Get("x-api-key") != "keep-me" {
		t.Error("same-host redirect must not strip the credential")
	}
}

func TestValidateEndpointAllowsLoopbackOnlyForLocalProvider(t *testing.T) {
	ctx := context.Background()
	r := staticResolver{addr: netip.MustParseAddr("127.0.0.1")}

	// AUDIT-H1: a user-configured local provider base_url (loopback) must pass when
	// allowLoopback=true, so `zero setup ollama --verify` / doctor / providers check work.
	for _, ep := range []string{"http://localhost:11434/api/tags", "http://127.0.0.1:11434/v1/models", "http://[::1]:1234/v1/models"} {
		if err := validateEndpoint(ctx, ep, r, true); err != nil {
			t.Errorf("local provider endpoint %q should be allowed with allowLoopback=true, got %v", ep, err)
		}
		if !endpointIsLoopbackOrPrivate(ep) {
			t.Errorf("endpointIsLoopbackOrPrivate(%q) = false, want true", ep)
		}
	}

	// Loopback stays BLOCKED without the flag (e.g. a redirect target).
	if err := validateEndpoint(ctx, "http://localhost:11434/api/tags", r, false); err == nil {
		t.Error("loopback must remain blocked when allowLoopback=false (redirects / non-local)")
	}

	// allowLoopbackOrPrivate relaxes loopback AND private-network ranges — but
	// link-local and other special ranges (cloud metadata, documentation) stay
	// blocked even for a local provider config.
	for _, ep := range []string{"http://169.254.169.254/latest/meta-data", "http://192.0.2.1:8080/v1"} {
		if err := validateEndpoint(ctx, ep, staticResolver{addr: netip.MustParseAddr("169.254.169.254")}, true); err == nil {
			t.Errorf("special-use address %q must stay blocked even with flag=true", ep)
		}
		if endpointIsLoopbackOrPrivate(ep) {
			t.Errorf("endpointIsLoopbackOrPrivate(%q) = true, want false", ep)
		}
	}

	// Private-network addresses (192.168.x.y / 10.x.y.z / 172.16.x.y) are
	// now allowed when the flag is true — the user is pointing at their own
	// LAN box (e.g. llama.cpp on another local machine).
	for _, ep := range []string{"http://192.168.1.100:8080/v1", "http://10.0.0.5:8080/v1", "http://172.16.0.10:8080/v1"} {
		if err := validateEndpoint(ctx, ep, staticResolver{addr: netip.MustParseAddr("192.168.1.100")}, true); err != nil {
			t.Errorf("private-network address %q should be allowed with flag=true, got %v", ep, err)
		}
		if !endpointIsLoopbackOrPrivate(ep) {
			t.Errorf("endpointIsLoopbackOrPrivate(%q) = false, want true", ep)
		}
	}
}

func TestConnectivityClientLoopbackRedirectStillBlocked(t *testing.T) {
	// AUDIT-H1: even a local-provider probe (allowLoopback=true) must NOT follow a
	// redirect to loopback — that would be SSRF via a 3xx, not the user's base_url.
	client := newConnectivityClient(5*time.Second, staticResolver{addr: netip.MustParseAddr("127.0.0.1")}, nil, true)
	orig, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:11434/api/tags", nil)
	redirect, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:9999/secret", nil)
	if err := client.CheckRedirect(redirect, []*http.Request{orig}); err == nil {
		t.Fatal("a redirect to loopback must be rejected even when the probe allows a loopback base_url")
	}
}
