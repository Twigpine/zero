// Package proxydial answers one question for an SSRF-guarded dialer: is this
// dial the connection dial of a request the transport routed through a forward
// proxy?
//
// A guarded transport validates the TARGET of a request before dialing, and its
// dialer refuses loopback, private and link-local addresses so a redirect or a
// DNS rebind cannot walk the request onto the local network. When a forward
// proxy is configured, the transport dials the PROXY first and tunnels the
// request through it, and a local proxy is by far the common shape:
// HTTPS_PROXY=http://127.0.0.1:3067. The dialer then sees 127.0.0.1, refuses it,
// and the user gets "loopback hosts are blocked" for a request whose real target
// was validated and public (#569).
//
// The proxy address is not the request target, and the guard was never about it.
// It is configuration the user set on purpose, and the transport is going to dial
// it for every request whether the guard likes it or not. So that dial is let
// through, and only that dial.
//
// THE EXEMPTION FOLLOWS THE TRANSPORT'S OWN ROUTE DECISION, NOT A GUESS AT IT.
// Asking the Proxy function a question of one's own invention answers for a
// request the transport never made: with NO_PROXY excluding a host, the real
// request to that host goes direct while a synthetic probe still comes back
// proxied, and a direct dial whose address happens to equal the proxy endpoint
// would then skip the guard entirely. Host-dependent proxy callbacks make the
// mismatch general, and no amount of care over the probe's spelling closes it,
// because the two calls are about different requests.
//
// So the decision is recorded where it is made, on the request, and read back on
// the dial that request causes. Transport.RoundTrip asks the wrapped transport's
// Proxy function about the actual request, stores the answer in that request's
// context, and hands it on; the transport derives the dial context from the
// request context, so the dialer reads back the route this very connection is
// being opened for. Per-request, so concurrent requests cannot overwrite each
// other, and unforgeable from outside the package: the context key is private.
package proxydial

import (
	"context"
	"net/http"
	"net/url"
)

// selectedProxyKey addresses the route decision inside a request context. It is
// unexported so nothing outside this package can plant one.
type selectedProxyKey struct{}

// Transport records the proxy its wrapped transport selects for each request in
// that request's context, so a guarded DialContext installed on the same
// transport can tell the dial to a proxy from a direct dial.
//
// The embedded transport stays reachable: callers configure Proxy, DialContext
// and the rest on it exactly as before, and tests can read them back.
type Transport struct {
	*http.Transport
}

// Wrap returns a Transport around transport. The result is the http.RoundTripper
// to install on the client; installing the bare transport instead leaves every
// dial looking direct, which is safe (the guard applies) but reopens #569.
func Wrap(transport *http.Transport) *Transport {
	return &Transport{Transport: transport}
}

// RoundTrip resolves the route for this request and passes the decision down to
// the dialer through the request context.
func (transport *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.Transport.Proxy != nil {
		// A Proxy error is left for the transport to report: it fails the
		// request before dialing, so there is no dial to classify. The function
		// is called once here and once by the transport, which is what a
		// host-dependent callback already expects from redirects and retries.
		if proxyURL, err := transport.Transport.Proxy(request); err == nil && proxyURL != nil {
			request = request.WithContext(context.WithValue(request.Context(), selectedProxyKey{}, proxyURL))
		}
	}
	return transport.Transport.RoundTrip(request)
}

// IsProxyDial reports whether ctx belongs to a request this package's Transport
// routed through a proxy, which makes the dial about to happen the dial to that
// proxy.
//
// THE ADDRESS IS NOT COMPARED, AND DOES NOT NEED TO BE. A transport opens
// exactly one connection dial per connection, and when a proxy is selected that
// dial goes to the proxy; the target is reached through it, never dialed. So the
// route decision on the context already identifies the dial, and matching the
// address against the proxy URL on top of it could only ever disagree with the
// transport, which is what a punycoded Unicode proxy hostname does: Go dials
// xn--bro-hoa.example while the URL still spells buero as the user wrote it.
func IsProxyDial(ctx context.Context) bool {
	proxyURL, _ := ctx.Value(selectedProxyKey{}).(*url.URL)
	return proxyURL != nil
}
