package proxydial

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// errDialed ends the request at the dial, after the route decision has been
// observed. Nothing here needs a live server.
var errDialed = errors.New("dial recorded")

type dialRecord struct {
	address string
	proxied bool
}

// driveRequest sends one request through a wrapped transport whose dialer
// records what it was asked for and whether the context says this dial belongs
// to a proxied request.
func driveRequest(t *testing.T, target string, proxyFor func(*http.Request) (*url.URL, error)) dialRecord {
	t.Helper()
	var record dialRecord
	transport := &http.Transport{Proxy: proxyFor}
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		record = dialRecord{address: address, proxied: IsProxyDial(ctx)}
		return nil, errDialed
	}
	client := &http.Client{Transport: Wrap(transport)}
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); !errors.Is(err, errDialed) {
		t.Fatalf("%s: request ended before the dial: %v", target, err)
	}
	if record.address == "" {
		t.Fatalf("%s: the dialer was never called", target)
	}
	return record
}

func fixedProxy(raw string) func(*http.Request) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return func(*http.Request) (*url.URL, error) { return parsed, nil }
}

// THE DIAL TO A CONFIGURED PROXY IS THE ONE THAT IS EXEMPT. A proxied request
// dials the proxy, never the target, so the route on the context is what
// identifies the dial.
func TestProxiedRequestDialsTheProxyAndSaysSo(t *testing.T) {
	record := driveRequest(t, "https://api.example.com/v1/models", fixedProxy("http://127.0.0.1:3067"))
	if !record.proxied {
		t.Error("a request routed through a proxy did not reach the dialer as proxied")
	}
	if record.address != "127.0.0.1:3067" {
		t.Errorf("dial address = %q, want the proxy endpoint", record.address)
	}
}

// AND A DIRECT REQUEST IS NOT, EVEN WHEN ITS ORIGIN IS THE PROXY'S OWN ADDRESS.
//
// This is the NO_PROXY shape: the proxy function excludes one host, so a request
// to that host goes direct. Asking that same function about a request of our own
// invention would answer "proxied" (the invented host is not excluded) and the
// direct dial, whose address equals the proxy endpoint, would skip the guard
// while resolving to whatever the name resolves to at connect time.
func TestDirectRequestToTheProxyEndpointIsNotAProxyDial(t *testing.T) {
	proxyURL, err := url.Parse("http://rebind.example:80")
	if err != nil {
		t.Fatal(err)
	}
	noProxyForRebind := func(request *http.Request) (*url.URL, error) {
		if request.URL.Hostname() == "rebind.example" {
			return nil, nil
		}
		return proxyURL, nil
	}

	record := driveRequest(t, "http://rebind.example/data", noProxyForRebind)
	if record.address != "rebind.example:80" {
		t.Fatalf("SETUP INVALID: dial address = %q, want the direct origin, which is also the proxy endpoint", record.address)
	}
	if record.proxied {
		t.Error("a direct dial was reported as the proxy dial because its address matched the configured proxy")
	}

	// The same function still routes another host through that proxy, so the
	// exclusion above is a route difference and not a disabled proxy.
	if record := driveRequest(t, "http://elsewhere.example/data", noProxyForRebind); !record.proxied {
		t.Error("the configured proxy was not used for a host outside the exclusion")
	}
}

// The inverse mismatch: a callback that routes by host must not have its answer
// taken from some other host. A proxy selected only for the real target is
// honoured.
func TestHostDependentProxySelectionIsHonoured(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:3067")
	if err != nil {
		t.Fatal(err)
	}
	onlyForOneHost := func(request *http.Request) (*url.URL, error) {
		if request.URL.Hostname() == "api.example.com" {
			return proxyURL, nil
		}
		return nil, nil
	}
	if record := driveRequest(t, "https://api.example.com/v1", onlyForOneHost); !record.proxied || record.address != "127.0.0.1:3067" {
		t.Errorf("selected proxy not reached: %+v", record)
	}
	if record := driveRequest(t, "https://other.example/v1", onlyForOneHost); record.proxied {
		t.Error("an unproxied host was reported as proxied")
	}
}

// Scheme separation: an https-only proxy leaves an http request direct, and the
// decision recorded is the one made for that request's own scheme.
func TestSchemeSeparation(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:3067")
	if err != nil {
		t.Fatal(err)
	}
	httpsOnly := func(request *http.Request) (*url.URL, error) {
		if request.URL.Scheme == "https" {
			return proxyURL, nil
		}
		return nil, nil
	}
	if record := driveRequest(t, "https://api.example.com/v1", httpsOnly); !record.proxied {
		t.Error("the https proxy was not applied to an https request")
	}
	if record := driveRequest(t, "http://api.example.com/v1", httpsOnly); record.proxied {
		t.Error("an http request was treated as proxied by an https-only proxy")
	}
}

// A UNICODE PROXY HOSTNAME IS STILL THE PROXY. Go dials proxies by their
// IDNA-encoded name, so a matcher comparing the URL's own spelling would call
// this dial direct and refuse it as an ordinary private address. Nothing is
// compared here, so both spellings of one host are one host.
func TestUnicodeProxyHostnameIsRecognised(t *testing.T) {
	unicodeRecord := driveRequest(t, "https://api.example.com/v1", fixedProxy("http://büro.example:3067"))
	if !unicodeRecord.proxied {
		t.Error("a proxy configured with a Unicode hostname was not recognised as the proxy")
	}
	asciiRecord := driveRequest(t, "https://api.example.com/v1", fixedProxy("http://xn--bro-hoa.example:3067"))
	if !asciiRecord.proxied {
		t.Error("a proxy configured with the ASCII spelling was not recognised as the proxy")
	}
	// The two spellings name one endpoint, and it is the ASCII one that is
	// dialed. This is the mismatch a string comparison would have had to close.
	if unicodeRecord.address != asciiRecord.address {
		t.Errorf("dial addresses differ across spellings of one host: %q vs %q", unicodeRecord.address, asciiRecord.address)
	}
}

// A transport with no proxy leaves every dial direct.
func TestNoProxyMeansNoExemption(t *testing.T) {
	if record := driveRequest(t, "http://127.0.0.1:3067/", nil); record.proxied {
		t.Error("a dial was reported as proxied by a transport with no Proxy function")
	}
	if IsProxyDial(context.Background()) {
		t.Error("a context that never went through the wrapper reported a proxy dial")
	}
}

// THE DECISION IS PER REQUEST. A shared record of the last selected proxy would
// let a proxied request hand its exemption to a direct one running beside it.
func TestConcurrentRequestsKeepTheirOwnRoute(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:3067")
	if err != nil {
		t.Fatal(err)
	}
	byHost := func(request *http.Request) (*url.URL, error) {
		if request.URL.Hostname() == "proxied.example" {
			return proxyURL, nil
		}
		return nil, nil
	}

	var mutex sync.Mutex
	seen := map[string]bool{}
	transport := &http.Transport{Proxy: byHost}
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		mutex.Lock()
		seen[address] = IsProxyDial(ctx)
		mutex.Unlock()
		return nil, errDialed
	}
	client := &http.Client{Transport: Wrap(transport)}

	var group sync.WaitGroup
	for range 40 {
		for _, target := range []string{"http://proxied.example/", "http://direct.example/"} {
			group.Add(1)
			go func() {
				defer group.Done()
				request, _ := http.NewRequest(http.MethodGet, target, nil)
				_, _ = client.Do(request)
			}()
		}
	}
	group.Wait()

	mutex.Lock()
	defer mutex.Unlock()
	if !seen["127.0.0.1:3067"] {
		t.Error("the proxy dial was not recorded as proxied under concurrency")
	}
	if seen["direct.example:80"] {
		t.Error("a direct dial picked up another request's proxy exemption")
	}
	if len(seen) != 2 {
		t.Fatalf("SETUP INVALID: dialed %v, want exactly the proxy and the direct origin", seen)
	}
}
