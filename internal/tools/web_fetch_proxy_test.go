package tools

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/proxydial"
)

// webFetchTransportWithProxy builds the guarded web_fetch round tripper and
// swaps in a proxy function, which is how the transport learns its route. The
// environment is left alone deliberately: http.ProxyFromEnvironment reads it
// once per process, so an environment-driven test would depend on test order.
func webFetchTransportWithProxy(t *testing.T, resolver webFetchResolver, proxyFor func(*http.Request) (*url.URL, error)) *proxydial.Transport {
	t.Helper()
	roundTripper := webFetchSafeTransport(nil, resolver)
	wrapped, ok := roundTripper.(*proxydial.Transport)
	if !ok {
		t.Fatalf("transport = %T, want the route-recording wrapper; without it no dial can be recognised as the proxy dial", roundTripper)
	}
	wrapped.Proxy = proxyFor
	return wrapped
}

// THE PROXY IS NOT THE TARGET. With HTTPS_PROXY pointing at a local forward
// proxy, the transport dials the proxy and tunnels the request through it. The
// dialer used to refuse that dial as a loopback address, so every fetch on a
// machine behind such a proxy failed with "loopback hosts are blocked" for a
// public target that had already been validated (#569).
func TestWebFetchTunnelsThroughTheConfiguredProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	arrived := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 256)
		n, _ := conn.Read(buffer)
		arrived <- string(buffer[:n])
	}()
	proxyURL, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// A proxied fetch never resolves the target on this side; the CONNECT
	// carries the hostname and the proxy resolves it.
	transport := webFetchTransportWithProxy(t,
		webFetchResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("resolver must not be called for a proxied fetch")
		}),
		func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}

	_, err = client.Get("https://public.example/resource")
	select {
	case first := <-arrived:
		if line := strings.SplitN(first, "\r\n", 2)[0]; line != "CONNECT public.example:443 HTTP/1.1" {
			t.Fatalf("proxy received %q, want a CONNECT for the target", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the proxy never received a connection; request error: %v", err)
	}
}

// AND A DIRECT FETCH STAYS PINNED, EVEN WHEN ITS ORIGIN IS THE PROXY'S OWN
// ADDRESS.
//
// This is the NO_PROXY shape: the proxy function sends one host direct. An
// exemption derived from asking that function about a request of our own
// invention would answer "proxied" for the excluded host too, and this dial
// would skip the pin and reach whatever the name resolves to at connect time.
func TestWebFetchDirectRequestToTheProxyEndpointIsStillPinned(t *testing.T) {
	proxyURL, err := url.Parse("http://rebind.example:80")
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	transport := webFetchTransportWithProxy(t,
		webFetchResolverFunc(func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
			lookups++
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}),
		func(request *http.Request) (*url.URL, error) {
			if request.URL.Hostname() == "rebind.example" {
				return nil, nil
			}
			return proxyURL, nil
		})
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}

	_, err = client.Get("http://rebind.example/data")

	if lookups == 0 {
		t.Fatal("the direct dial skipped the address pin because its address matched the configured proxy endpoint")
	}
	if err == nil || !strings.Contains(err.Error(), "loopback hosts are blocked") {
		t.Fatalf("err = %v, want the private answer to be refused", err)
	}
}

// A proxy configured with a Unicode hostname is dialed by its IDNA-encoded
// name, and it is still the proxy.
func TestWebFetchUnicodeProxyHostnameIsStillTheProxy(t *testing.T) {
	dialed := ""
	transport := webFetchTransportWithProxy(t,
		webFetchResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("resolver must not be called for a proxied fetch")
		}),
		func(*http.Request) (*url.URL, error) { return url.Parse("http://büro.example:3067") })
	guarded := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = address
		return guarded(ctx, network, address)
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}

	_, err := client.Get("https://public.example/resource")

	if dialed != "xn--bro-hoa.example:3067" {
		t.Fatalf("SETUP INVALID: dialed %q, want the IDNA-encoded proxy endpoint", dialed)
	}
	if err != nil && strings.Contains(err.Error(), "hosts are blocked") {
		t.Fatalf("the configured proxy was refused by the guard: %v", err)
	}
}

// Without a proxy the guard is exactly what it was: loopback is refused and
// the dialer never runs.
func TestWebFetchSafeDialStillRefusesLoopbackWithoutAProxy(t *testing.T) {
	dialCalled := false
	dial := webFetchSafeDialContext(
		webFetchResolverFunc(func(_ context.Context, network string, host string) ([]netip.Addr, error) {
			t.Fatalf("resolver consulted for a loopback literal: network=%q host=%q", network, host)
			return nil, nil
		}),
		webFetchDialFunc(func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("dial should not run")
		}),
	)

	_, err := dial(context.Background(), "tcp", "127.0.0.1:3067")
	if err == nil || !strings.Contains(err.Error(), "loopback hosts are blocked") {
		t.Fatalf("expected loopback rejection, got %v", err)
	}
	if dialCalled {
		t.Fatal("loopback was dialed with no proxy configured")
	}
}
