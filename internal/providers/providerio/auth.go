package providerio

import (
	"context"
	"net/http"
	"strings"

	"github.com/Twigpine/zero/internal/trace"
)

// TokenResolver yields a fresh OAuth credential for one request, or ok=false to
// fall back to the API key (e.g. there is no OAuth login for this provider).
// header is the header name to set ("" => Authorization); value is the full
// header value (for example "Bearer abc"). forceRefresh asks the resolver to
// bypass any cached token; it is set only on the single retry after a 401.
type TokenResolver func(ctx context.Context, forceRefresh bool) (header string, value string, ok bool, err error)

// SendWithAuthRetry issues a request authenticated with an OAuth bearer (when the
// resolver yields one) or the API key, retrying ONCE with a force-refreshed token
// after an upstream 401. A 401 means the server rejected the request (auth
// failed) WITHOUT processing a completion, so replaying it after a refresh is
// safe — the same "no duplicate billable work" guarantee SendWithRetry relies on
// for 429/503. With a nil resolver (or one that yields ok=false) this behaves
// exactly like SendWithRetry with API-key auth.
//
// base carries the API-key auth config (key + default header/scheme). setExtra
// (optional) sets the provider's non-auth headers on every attempt. Transient
// retries are handled by the inner SendWithRetry: 429/503/529 on their own
// budget, and provably pre-send transport failures (refused or timed-out
// connect, DNS, TLS handshake) on a separate, shorter one.
func SendWithAuthRetry(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	body []byte,
	base AuthHeaders,
	resolver TokenResolver,
	setExtra func(*http.Request),
	maxAttempts int,
) (*http.Response, error) {
	forceRefresh := false
	for authTry := 0; ; authTry++ {
		// Resolve auth BEFORE dispatching: a resolver error inside the request
		// callback would otherwise let SendWithRetry send an unauthenticated
		// request (leaking the path/body) before we return the error.
		headers := base
		if resolver != nil {
			// ProviderQueue captures pre-send auth-wait (OAuth token resolve). No
			// send-side semaphore exists today; a future request queue would also
			// accumulate here. nil recorder (untraced) is a no-op.
			queueSpan := trace.FromContext(ctx).Span(trace.SpanProviderQueue)
			header, value, ok, rerr := resolver(ctx, forceRefresh)
			queueSpan.End()
			if rerr != nil {
				return nil, rerr
			}
			if ok {
				headers = withBearer(base, header, value)
			}
		}
		response, err := SendWithRetry(ctx, client, method, url, body, func(request *http.Request) {
			if setExtra != nil {
				setExtra(request)
			}
			ApplyAuthHeaders(request, headers)
		}, maxAttempts)
		if err != nil {
			return response, err
		}
		// One retry on 401 with a forced token refresh (rejected request → safe).
		if resolver != nil && response.StatusCode == http.StatusUnauthorized && authTry == 0 {
			_ = response.Body.Close()
			forceRefresh = true
			continue
		}
		return response, nil
	}
}

// withBearer overrides base to use the OAuth credential: the given header
// (default Authorization) carries value verbatim, and the API key is cleared so
// the two auth methods can never both be sent.
func withBearer(base AuthHeaders, header, value string) AuthHeaders {
	if strings.TrimSpace(header) == "" {
		header = "Authorization"
	}
	base.APIKey = ""
	base.AuthHeader = header
	base.AuthScheme = ""
	base.AuthHeaderValue = value
	return base
}
