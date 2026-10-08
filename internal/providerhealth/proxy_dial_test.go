package providerhealth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/proxydial"
)

// connectivityClientWithProxy builds the real connectivity client and swaps in
// a proxy function, which is how the transport learns its route. The
// environment is left alone deliberately: http.ProxyFromEnvironment reads it
// once per process, so an environment-driven test would depend on test order.
func connectivityClientWithProxy(t *testing.T, resolver Resolver, proxyFor func(*http.Request) (*url.URL, error)) *http.Client {
	t.Helper()
	client := newConnectivityClient(3*time.Second, resolver, nil, false)
	wrapped, ok := client.Transport.(*proxydial.Transport)
	if !ok {
		t.Fatalf("transport = %T, want the route-recording wrapper; without it no dial can be recognised as the proxy dial", client.Transport)
	}
	wrapped.Proxy = proxyFor
	return client
}

// recordingProxy is a listener standing in for a local forward proxy. It
// records the first line it receives and hangs up.
func recordingProxy(t *testing.T) (*url.URL, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
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
	return proxyURL, arrived
}

// THE PROXY IS NOT THE TARGET, AND THE GUARD HAS TO KNOW THE DIFFERENCE.
//
// With HTTPS_PROXY set to a local forward proxy, the transport dials the proxy
// first and tunnels the request through it. This dialer refused that dial as a
// loopback address, so `zero doctor --connectivity` failed with "proxyconnect
// tcp: ... loopback hosts are blocked" for a target that had already been
// validated and was public (#569). Driven through the real client, so the route
// is the transport's own decision.
func TestConnectivityClientTunnelsThroughTheConfiguredProxy(t *testing.T) {
	proxyURL, arrived := recordingProxy(t)
	client := connectivityClientWithProxy(t,
		staticResolver{err: errors.New("resolver must not be called for a proxied request")},
		func(*http.Request) (*url.URL, error) { return proxyURL, nil })

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.example.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	select {
	case first := <-arrived:
		if line := strings.SplitN(first, "\r\n", 2)[0]; line != "CONNECT api.example.com:443 HTTP/1.1" {
			t.Fatalf("proxy received %q, want a CONNECT for the validated target", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the proxy never received a connection; request error: %v", err)
	}
}

// AND A DIRECT REQUEST STAYS GUARDED, EVEN WHEN ITS ORIGIN IS THE PROXY'S OWN
// ADDRESS.
//
// This is the NO_PROXY shape: the proxy function sends one host direct. An
// exemption derived from asking that function about a request of our own
// invention would answer "proxied" for the excluded host too, and this dial,
// whose address equals the configured proxy endpoint, would skip the resolver
// and reach whatever the name resolves to at connect time.
func TestDirectRequestToTheProxyEndpointIsStillGuarded(t *testing.T) {
	proxyURL, err := url.Parse("http://rebind.example:80")
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	resolver := countingResolver{
		onLookup: func() { lookups++ },
		addr:     netip.MustParseAddr("10.0.0.5"),
	}
	client := connectivityClientWithProxy(t, resolver, func(request *http.Request) (*url.URL, error) {
		if request.URL.Hostname() == "rebind.example" {
			return nil, nil
		}
		return proxyURL, nil
	})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://rebind.example/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)

	if lookups == 0 {
		t.Fatal("the direct dial skipped the safety resolver because its address matched the configured proxy endpoint")
	}
	var safety endpointSafetyError
	if !errors.As(err, &safety) {
		t.Fatalf("err = %v, want the private answer to be refused", err)
	}
}

// A proxy configured with a Unicode hostname is dialed by its IDNA-encoded
// name, and it is still the proxy: the request must reach it rather than be
// refused as an ordinary private address.
func TestUnicodeProxyHostnameIsStillTheProxy(t *testing.T) {
	dialed := ""
	client := connectivityClientWithProxy(t,
		staticResolver{err: errors.New("resolver must not be called for a proxied request")},
		func(*http.Request) (*url.URL, error) { return url.Parse("http://büro.example:3067") })
	wrapped := client.Transport.(*proxydial.Transport)
	guarded := wrapped.DialContext
	wrapped.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = address
		return guarded(ctx, network, address)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.example.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)

	if dialed != "xn--bro-hoa.example:3067" {
		t.Fatalf("SETUP INVALID: dialed %q, want the IDNA-encoded proxy endpoint", dialed)
	}
	var safety endpointSafetyError
	if errors.As(err, &safety) {
		t.Fatalf("the configured proxy was refused by the guard: %v", err)
	}
}

// Without a proxy the guard is exactly what it was: loopback is refused.
func TestSafeDialContextStillRefusesLoopbackWithoutAProxy(t *testing.T) {
	dial := safeDialContext(staticResolver{err: errors.New("resolver must not be called")}, false)
	conn, err := dial(context.Background(), "tcp", "127.0.0.1:3067")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("loopback was dialed with no proxy configured")
	}
	var safety endpointSafetyError
	if !errors.As(err, &safety) {
		t.Fatalf("err = %v, want endpointSafetyError", err)
	}
}

// A sibling loopback port is not the proxy, and the guard is unchanged for it.
func TestSafeDialContextRefusesALoopbackPortThatIsNotTheProxy(t *testing.T) {
	proxyURL, arrived := recordingProxy(t)
	client := connectivityClientWithProxy(t,
		staticResolver{err: errors.New("resolver must not be called")},
		func(request *http.Request) (*url.URL, error) {
			// Only the https probe is proxied; the http one below is direct and
			// happens to point at the neighbouring port.
			if request.URL.Scheme == "https" {
				return proxyURL, nil
			}
			return nil, nil
		})

	_, port, _ := net.SplitHostPort(proxyURL.Host)
	number, _ := strconv.Atoi(port)
	sibling := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(number+1)) + "/"
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, sibling, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	var safety endpointSafetyError
	if !errors.As(err, &safety) {
		t.Fatalf("err = %v, want a loopback port beside the proxy to stay blocked", err)
	}
	select {
	case first := <-arrived:
		t.Fatalf("the proxy received a connection it should not have: %q", first)
	default:
	}
}

// countingResolver answers with one address and reports each lookup, so a test
// can tell a skipped guard from a guard that ran and allowed the address.
type countingResolver struct {
	onLookup func()
	addr     netip.Addr
}

func (resolver countingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	if resolver.onLookup != nil {
		resolver.onLookup()
	}
	return []netip.Addr{resolver.addr}, nil
}
