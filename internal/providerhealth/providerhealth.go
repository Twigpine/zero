// Package providerhealth validates provider configuration and optionally probes
// the configured provider endpoint with a bounded, non-generating request.
package providerhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/proxydial"
	"github.com/Gitlawb/zero/internal/redaction"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Category string

const (
	CategoryConfig       Category = "config"
	CategoryUnsupported  Category = "unsupported"
	CategoryAuth         Category = "auth"
	CategoryRateLimit    Category = "rate_limit"
	CategoryNetwork      Category = "network"
	CategoryTimeout      Category = "timeout"
	CategoryProvider     Category = "provider_error"
	CategoryConnectivity Category = "connectivity"
)

const defaultTimeout = 5 * time.Second

type Options struct {
	Profile       config.ProviderProfile
	Connectivity  bool
	HTTPClient    *http.Client
	Resolver      Resolver
	OAuthResolver providerio.TokenResolver
	Timeout       time.Duration
	UserAgent     string
}

type Resolver interface {
	LookupNetIP(ctx context.Context, network string, host string) ([]netip.Addr, error)
}

type defaultResolver struct{}

func (defaultResolver) LookupNetIP(ctx context.Context, network string, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

type blockedPrefix struct {
	prefix netip.Prefix
	reason string
}

type embeddedIPv4Prefix struct {
	prefix     netip.Prefix
	byteOffset int
}

var blockedAddrPrefixes = []blockedPrefix{
	{prefix: netip.MustParsePrefix("0.0.0.0/8"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("10.0.0.0/8"), reason: "private network hosts are blocked"},
	{prefix: netip.MustParsePrefix("100.64.0.0/10"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("127.0.0.0/8"), reason: "loopback hosts are blocked"},
	{prefix: netip.MustParsePrefix("169.254.0.0/16"), reason: "link-local hosts are blocked"},
	{prefix: netip.MustParsePrefix("172.16.0.0/12"), reason: "private network hosts are blocked"},
	{prefix: netip.MustParsePrefix("192.0.0.0/24"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("192.0.2.0/24"), reason: "documentation hosts are blocked"},
	{prefix: netip.MustParsePrefix("192.88.99.0/24"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("192.168.0.0/16"), reason: "private network hosts are blocked"},
	{prefix: netip.MustParsePrefix("198.18.0.0/15"), reason: "benchmark network hosts are blocked"},
	{prefix: netip.MustParsePrefix("198.51.100.0/24"), reason: "documentation hosts are blocked"},
	{prefix: netip.MustParsePrefix("203.0.113.0/24"), reason: "documentation hosts are blocked"},
	{prefix: netip.MustParsePrefix("224.0.0.0/4"), reason: "multicast hosts are blocked"},
	{prefix: netip.MustParsePrefix("240.0.0.0/4"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("::/128"), reason: "unspecified hosts are blocked"},
	{prefix: netip.MustParsePrefix("::1/128"), reason: "loopback hosts are blocked"},
	{prefix: netip.MustParsePrefix("100::/64"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("2001::/23"), reason: "special-use hosts are blocked"},
	{prefix: netip.MustParsePrefix("2001:2::/48"), reason: "benchmark network hosts are blocked"},
	{prefix: netip.MustParsePrefix("2001:db8::/32"), reason: "documentation hosts are blocked"},
	{prefix: netip.MustParsePrefix("fc00::/7"), reason: "private network hosts are blocked"},
	{prefix: netip.MustParsePrefix("fe80::/10"), reason: "link-local hosts are blocked"},
	{prefix: netip.MustParsePrefix("ff00::/8"), reason: "multicast hosts are blocked"},
}

var embeddedIPv4Prefixes = []embeddedIPv4Prefix{
	{prefix: netip.MustParsePrefix("::/96"), byteOffset: 12},
	{prefix: netip.MustParsePrefix("64:ff9b::/96"), byteOffset: 12},
	{prefix: netip.MustParsePrefix("64:ff9b:1::/48"), byteOffset: 6},
	{prefix: netip.MustParsePrefix("2002::/16"), byteOffset: 2},
}

type endpointSafetyError struct {
	message string
}

func (err endpointSafetyError) Error() string {
	return err.message
}

type Result struct {
	Status       Status  `json:"status"`
	ProviderName string  `json:"providerName,omitempty"`
	ProviderKind string  `json:"providerKind,omitempty"`
	Model        string  `json:"model,omitempty"`
	APIModel     string  `json:"apiModel,omitempty"`
	BaseURL      string  `json:"baseURL,omitempty"`
	Checks       []Check `json:"checks"`
}

type Check struct {
	ID       string         `json:"id"`
	Label    string         `json:"label"`
	Status   Status         `json:"status"`
	Category Category       `json:"category,omitempty"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

func (result Result) Check(id string) *Check {
	for index := range result.Checks {
		if result.Checks[index].ID == id {
			return &result.Checks[index]
		}
	}
	return nil
}

func (result Result) PrimaryCheck() *Check {
	for index := range result.Checks {
		if result.Checks[index].Status == StatusFail {
			return &result.Checks[index]
		}
	}
	for index := range result.Checks {
		if result.Checks[index].Status == StatusWarn {
			return &result.Checks[index]
		}
	}
	if connectivity := result.Check("provider.connectivity"); connectivity != nil {
		return connectivity
	}
	if len(result.Checks) == 0 {
		return nil
	}
	return &result.Checks[0]
}

func Probe(ctx context.Context, options Options) Result {
	profile := options.Profile
	result := Result{
		Status:       StatusPass,
		ProviderName: redact(strings.TrimSpace(profile.Name), profile),
		ProviderKind: redact(strings.TrimSpace(string(profile.ProviderKind)), profile),
		Model:        redact(strings.TrimSpace(profile.Model), profile),
		BaseURL:      redactBaseURL(profile.BaseURL, profile),
		Checks:       []Check{},
	}

	if !config.HasProviderProfile(profile) {
		result.add(check("provider.config", "Provider config", StatusFail, CategoryConfig, "No LLM provider is configured.", nil, profile))
		return result.finalize()
	}
	if strings.TrimSpace(profile.Model) == "" {
		result.add(check("provider.config", "Provider config", StatusFail, CategoryConfig, fmt.Sprintf("Provider %s requires model.", providerName(profile)), nil, profile))
		return result.finalize()
	}
	result.add(check("provider.config", "Provider config", StatusPass, CategoryConfig, fmt.Sprintf("Provider config loaded for %s.", providerName(profile)), map[string]any{
		"name":     profile.Name,
		"provider": profile.ProviderKind,
		"baseURL":  profile.BaseURL,
		"model":    profile.Model,
	}, profile))

	if unsupported := unsupportedCatalogCheck(profile); unsupported != nil {
		result.add(*unsupported)
		return result.finalize()
	}

	metadata, err := providers.ResolveRuntimeMetadata(profile, providers.Options{})
	if err != nil {
		result.add(check("provider.runtime", "Provider runtime", StatusFail, CategoryConfig, "Provider runtime did not resolve: "+err.Error(), nil, profile))
		return result.finalize()
	}
	result.ProviderKind = redact(string(metadata.ProviderKind), profile)
	result.APIModel = redact(metadata.APIModel, profile)
	result.add(check("provider.runtime", "Provider runtime", StatusPass, CategoryConfig, fmt.Sprintf("Provider runtime resolves %s as %s.", providerName(profile), metadata.ProviderKind), map[string]any{
		"apiModel":     metadata.APIModel,
		"providerKind": metadata.ProviderKind,
	}, profile))

	oauthConfigured := options.OAuthResolver != nil
	if oauthConfigured {
		var resolveErr error
		profile, oauthConfigured, resolveErr = profileWithOAuthCredential(ctx, profile, options.OAuthResolver, false)
		if resolveErr != nil {
			result.add(check("provider.auth", "Provider auth", StatusFail, CategoryAuth, "Provider OAuth credential could not be resolved: "+resolveErr.Error(), map[string]any{
				"oauthLogin": true,
			}, profile))
			return result.finalize()
		}
	}
	if credentialRequired(profile, metadata.ProviderKind) && !hasCredential(profile) && !oauthConfigured {
		result.add(check("provider.auth", "Provider auth", StatusFail, CategoryAuth, fmt.Sprintf("Provider %s requires API credentials.", providerName(profile)), credentialDetails(profile), profile))
		return result.finalize()
	}
	if oauthConfigured {
		result.add(check("provider.auth", "Provider auth", StatusPass, CategoryAuth, fmt.Sprintf("Provider %s has OAuth credentials configured.", providerName(profile)), map[string]any{
			"oauthLogin": true,
		}, profile))
	} else if hasCredential(profile) {
		result.add(check("provider.auth", "Provider auth", StatusPass, CategoryAuth, fmt.Sprintf("Provider %s has credentials configured.", providerName(profile)), credentialDetails(profile), profile))
	} else {
		result.add(check("provider.auth", "Provider auth", StatusPass, CategoryAuth, fmt.Sprintf("Provider %s does not require API credentials.", providerName(profile)), credentialDetails(profile), profile))
	}

	if !options.Connectivity {
		return result.finalize()
	}
	result.add(connectivityCheck(ctx, profile, metadata.ProviderKind, options))
	return result.finalize()
}

func (result *Result) add(check Check) {
	result.Checks = append(result.Checks, check)
}

func (result Result) finalize() Result {
	status := StatusPass
	for _, check := range result.Checks {
		if check.Status == StatusFail {
			status = StatusFail
			break
		}
		if check.Status == StatusWarn {
			status = StatusWarn
		}
	}
	result.Status = status
	return result
}

func unsupportedCatalogCheck(profile config.ProviderProfile) *Check {
	if strings.TrimSpace(profile.CatalogID) == "" {
		return nil
	}
	descriptor, err := providercatalog.Require(profile.CatalogID)
	if err != nil {
		out := check("provider.runtime", "Provider runtime", StatusFail, CategoryConfig, err.Error(), nil, profile)
		return &out
	}
	if providercatalog.RuntimeSupported(descriptor) {
		return nil
	}
	out := check("provider.runtime", "Provider runtime", StatusFail, CategoryUnsupported, fmt.Sprintf("Provider %q uses transport %q: %s.", descriptor.ID, descriptor.Transport, providercatalog.RuntimeUnsupportedReason(descriptor)), map[string]any{
		"transport": descriptor.Transport,
	}, profile)
	return &out
}

func connectivityCheck(ctx context.Context, profile config.ProviderProfile, kind config.ProviderKind, options Options) Check {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, allowLoopbackOrPrivate, err := healthRequest(requestCtx, profile, kind, options)
	if err != nil {
		category := CategoryConfig
		var safety endpointSafetyError
		if errors.As(err, &safety) {
			category = CategoryNetwork
		}
		return check("provider.connectivity", "Provider connectivity", StatusFail, category, err.Error(), nil, profile)
	}
	client := options.HTTPClient
	if client == nil {
		client = newConnectivityClient(timeout, options.Resolver, sensitiveAuthHeaderNames(profile, kind), allowLoopbackOrPrivate)
	}
	response, err := client.Do(request)
	if err != nil {
		return classifyTransportError(err, profile)
	}
	defer func(body io.ReadCloser) {
		_ = body.Close()
	}(response.Body)
	if response.StatusCode == http.StatusUnauthorized && options.OAuthResolver != nil {
		refreshed, ok, refreshErr := profileWithOAuthCredential(requestCtx, options.Profile, options.OAuthResolver, true)
		if refreshErr != nil {
			return check("provider.connectivity", "Provider connectivity", StatusFail, CategoryAuth, "Provider OAuth credential refresh failed: "+refreshErr.Error(), map[string]any{
				"statusCode": response.StatusCode,
			}, profile)
		}
		if ok {
			_ = response.Body.Close()
			profile = refreshed
			request, allowLoopbackOrPrivate, err = healthRequest(requestCtx, profile, kind, options)
			if err != nil {
				return check("provider.connectivity", "Provider connectivity", StatusFail, CategoryConfig, err.Error(), nil, profile)
			}
			if options.HTTPClient == nil {
				client = newConnectivityClient(timeout, options.Resolver, sensitiveAuthHeaderNames(profile, kind), allowLoopbackOrPrivate)
			}
			response, err = client.Do(request)
			if err != nil {
				return classifyTransportError(err, profile)
			}
			defer func(body io.ReadCloser) {
				_ = body.Close()
			}(response.Body)
		}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return check("provider.connectivity", "Provider connectivity", StatusFail, CategoryProvider, "Provider response body could not be read: "+err.Error(), map[string]any{
			"statusCode": response.StatusCode,
			"endpoint":   request.URL.String(),
		}, profile)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return check("provider.connectivity", "Provider connectivity", StatusPass, CategoryConnectivity, fmt.Sprintf("Provider endpoint reachable (%d).", response.StatusCode), map[string]any{
			"statusCode": response.StatusCode,
			"endpoint":   request.URL.String(),
		}, profile)
	}

	category := CategoryProvider
	status := StatusFail
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		category = CategoryAuth
	case http.StatusTooManyRequests, 529:
		category = CategoryRateLimit
		status = StatusWarn
	}
	message := responseMessage(response.StatusCode, body)
	return check("provider.connectivity", "Provider connectivity", status, category, message, map[string]any{
		"statusCode": response.StatusCode,
		"endpoint":   request.URL.String(),
	}, profile)
}

func profileWithOAuthCredential(ctx context.Context, profile config.ProviderProfile, resolver providerio.TokenResolver, forceRefresh bool) (config.ProviderProfile, bool, error) {
	if resolver == nil {
		return profile, false, nil
	}
	header, value, ok, err := resolver(ctx, forceRefresh)
	if err != nil || !ok {
		return profile, false, err
	}
	if strings.TrimSpace(header) == "" {
		header = "Authorization"
	}
	profile.AuthHeader = header
	profile.AuthScheme = ""
	profile.AuthHeaderValue = value
	return profile, true, nil
}

func healthRequest(ctx context.Context, profile config.ProviderProfile, kind config.ProviderKind, options Options) (*http.Request, bool, error) {
	baseURL, err := resolvedBaseURL(profile, kind)
	if err != nil {
		return nil, false, err
	}
	endpoint := baseURL + healthPath(kind)
	if override, ok := overrideHealthEndpoint(profile, baseURL); ok {
		endpoint = override
	}
	// Loopback is permitted only when the user's OWN configured base_url is loopback
	// (a local provider like Ollama/LM Studio) — never for a redirect target. This is
	// the flag returned to the caller so the dial path applies the same policy. (AUDIT-H1)
	allowLoopbackOrPrivate := endpointIsLoopbackOrPrivate(endpoint)
	if err := validateEndpoint(ctx, endpoint, options.Resolver, allowLoopbackOrPrivate); err != nil {
		return nil, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	if options.UserAgent != "" {
		request.Header.Set("User-Agent", options.UserAgent)
	}
	applyAuth(request, profile, kind)
	return request, allowLoopbackOrPrivate, nil
}

func resolvedBaseURL(profile config.ProviderProfile, kind config.ProviderKind) (string, error) {
	baseURL := strings.TrimSpace(profile.BaseURL)
	switch kind {
	case config.ProviderKindOpenAI:
		return providerio.NormalizeBaseURL(baseURL, config.OpenAIBaseURL, "OpenAI")
	case config.ProviderKindAnthropic:
		return providerio.NormalizeBaseURL(baseURL, config.AnthropicBaseURL, "Anthropic")
	case config.ProviderKindGoogle:
		return providerio.NormalizeBaseURL(baseURL, config.GoogleBaseURL, "Google")
	case config.ProviderKindOpenAICompatible, config.ProviderKindAnthropicCompat:
		if baseURL == "" {
			return "", fmt.Errorf("%s provider %s requires baseURL for connectivity probing", kind, providerName(profile))
		}
		return providerio.NormalizeBaseURL(baseURL, "", string(kind))
	default:
		return "", fmt.Errorf("unsupported provider kind %q", kind)
	}
}

// validateEndpoint enforces the SSRF policy for the provider connectivity probe.
// allowLoopbackOrPrivate permits loopback hosts (localhost / 127.0.0.0/8 / ::1)
// and private-network hosts (192.168.0.0/16 / 10.0.0.0/8 / 172.16.0.0/12) — it
// is set true exclusively for the user's own explicitly-configured base_url
// (e.g. Ollama/LM Studio on the LAN), never for redirect targets.
func validateEndpoint(ctx context.Context, endpoint string, resolver Resolver, allowLoopbackOrPrivate bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("provider connectivity URL must use http or https")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("provider connectivity URL requires a host")
	}
	normalized := strings.TrimSuffix(strings.ToLower(host), ".")
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") {
		if allowLoopbackOrPrivate {
			return nil // user-configured local provider; loopback is intentional
		}
		return endpointSafetyError{message: "provider connectivity URL is unsafe: localhost hosts are blocked"}
	}
	if addr, err := netip.ParseAddr(normalized); err == nil {
		if reason := blockedAddrReason(addr); reason != "" && (!allowLoopbackOrPrivate || (!addr.IsLoopback() && !addr.IsPrivate())) {
			return endpointSafetyError{message: "provider connectivity URL is unsafe: " + reason}
		}
		return nil
	}
	if resolver == nil {
		resolver = defaultResolver{}
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return endpointSafetyError{message: "provider connectivity host could not be resolved safely: " + err.Error()}
	}
	if len(addrs) == 0 {
		return endpointSafetyError{message: "provider connectivity host resolved to no addresses"}
	}
	for _, addr := range addrs {
		if reason := blockedAddrReason(addr); reason != "" && (!allowLoopbackOrPrivate || (!addr.IsLoopback() && !addr.IsPrivate())) {
			return endpointSafetyError{message: "provider connectivity URL is unsafe: " + reason}
		}
	}
	return nil
}

// endpointIsLoopbackOrPrivate reports whether the configured endpoint's host is a loopback
// or private-network host the user typed themselves — a local provider on the LAN
// (e.g. 192.168.x.y / 10.x.y.z) or localhost. Used to decide whether the
// connectivity probe may relax address-blocking and reach the user's own
// explicitly-configured provider.
func endpointIsLoopbackOrPrivate(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(parsed.Hostname())), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback() || addr.IsPrivate()
	}
	return false
}

// maxConnectivityRedirects bounds redirect following so a chain of redirects
// cannot be used to amplify probing or stall the check.
const maxConnectivityRedirects = 5

// newConnectivityClient builds the HTTP client used for the default (no
// caller-supplied client) connectivity probe. It is hardened against SSRF:
//
//   - CheckRedirect re-validates every redirect target with the same address
//     rules as the initial endpoint, so a 3xx pointing at an internal address
//     (e.g. 169.254.169.254) is refused rather than followed blindly.
//   - the transport's DialContext re-resolves and validates the host at dial
//     time and then dials the validated IP literal, closing the TOCTOU window
//     between the pre-flight validateEndpoint check and the actual connection
//     (DNS rebinding).
func newConnectivityClient(timeout time.Duration, resolver Resolver, sensitiveHeaders []string, allowLoopbackOrPrivate bool) *http.Client {
	if resolver == nil {
		resolver = defaultResolver{}
	}
	transport, _ := http.DefaultTransport.(*http.Transport)
	if transport != nil {
		transport = transport.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.DialContext = safeDialContext(resolver, allowLoopbackOrPrivate)
	// WRAPPED, SO THE DIALER LEARNS THE ROUTE THIS TRANSPORT CHOSE. The wrapper
	// records the proxy selected for each request on that request, and the
	// dialer reads it back on the dial that request causes; installing the bare
	// transport instead would leave a proxied dial looking like a loopback
	// target and fail the probe (#569). The cloned default transport already
	// reads HTTPS_PROXY and friends; nothing new is enabled.
	return &http.Client{
		Timeout:   timeout,
		Transport: proxydial.Wrap(transport),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxConnectivityRedirects {
				return fmt.Errorf("provider connectivity exceeded %d redirects", maxConnectivityRedirects)
			}
			// A redirect that leaves the original host must not carry the provider
			// credential with it. Go's stdlib only auto-strips Authorization/Cookie/
			// WWW-Authenticate on a host change — NOT x-api-key, x-goog-api-key, or
			// arbitrary custom auth headers — so a 3xx to an attacker-controlled host
			// would otherwise receive the API key verbatim (credential exfil). Strip
			// every auth-bearing header on any host:port change (fail-safe). (AUDIT-M10)
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				for _, h := range sensitiveHeaders {
					req.Header.Del(h)
				}
			}
			// Redirects are NEVER allowed to reach loopback, even for a local provider:
			// allowLoopbackOrPrivate covers only the user's own configured base_url,
			// not a 3xx target (which could be attacker-controlled SSRF). (AUDIT-H1)
			return validateEndpoint(req.Context(), req.URL.String(), resolver, false)
		},
	}
}

// sensitiveAuthHeaderNames returns every request header applyAuth may set as a
// credential for this profile/kind: the kind's default auth header, the profile's
// custom auth header, and all CustomHeaders keys, plus the well-known auth headers
// as defense in depth. Used to scrub credentials before following a cross-host
// redirect on the health probe.
func sensitiveAuthHeaderNames(profile config.ProviderProfile, kind config.ProviderKind) []string {
	names := map[string]struct{}{
		"Authorization": {}, "Proxy-Authorization": {}, "Cookie": {},
		"x-api-key": {}, "x-goog-api-key": {}, "api-key": {},
	}
	switch kind {
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		names["x-api-key"] = struct{}{}
	case config.ProviderKindGoogle:
		names["x-goog-api-key"] = struct{}{}
	default:
		names["Authorization"] = struct{}{}
	}
	if h := strings.TrimSpace(profile.AuthHeader); h != "" {
		names[h] = struct{}{}
	}
	for k := range profile.CustomHeaders {
		if k = strings.TrimSpace(k); k != "" {
			names[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for k := range names {
		out = append(out, k)
	}
	return out
}

// safeDialContext returns a dial function that resolves the target host, refuses
// the connection if any resolved address is blocked, then dials the validated IP
// literal so the kernel cannot re-resolve to a different address after the check.
func safeDialContext(resolver Resolver, allowLoopbackOrPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		// THE PROXY IS NOT THE TARGET. With HTTPS_PROXY set to a local forward
		// proxy, the transport dials the proxy first and tunnels the request
		// through it, and this dialer used to refuse that dial as a loopback
		// address: "proxyconnect tcp: ... loopback hosts are blocked" for a
		// request whose real target had already been validated (#569). The
		// target validation is unchanged; only the dial the transport opens to
		// the proxy it picked for this very request is let past the guard, and a
		// direct dial is checked as it always was.
		if proxydial.IsProxyDial(ctx) {
			return dialer.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
			if reason := blockedAddrReason(addr); reason != "" && (!allowLoopbackOrPrivate || (!addr.IsLoopback() && !addr.IsPrivate())) {
				return nil, endpointSafetyError{message: "provider connectivity URL is unsafe: " + reason}
			}
			return dialer.DialContext(ctx, network, address)
		}
		addrs, err := resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, endpointSafetyError{message: "provider connectivity host could not be resolved safely: " + err.Error()}
		}
		if len(addrs) == 0 {
			return nil, endpointSafetyError{message: "provider connectivity host resolved to no addresses"}
		}
		for _, addr := range addrs {
			if reason := blockedAddrReason(addr); reason != "" && (!allowLoopbackOrPrivate || (!addr.IsLoopback() && !addr.IsPrivate())) {
				return nil, endpointSafetyError{message: "provider connectivity URL is unsafe: " + reason}
			}
		}
		// Every resolved address passed validation; dial them in order until one
		// connects so a dual-stack or multi-record provider isn't failed by a
		// single dead/unroutable address (e.g. an unreachable IPv6 first record).
		return dialValidatedAddrs(ctx, addrs, port, func(ctx context.Context, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		})
	}
}

// dialValidatedAddrs dials the already-validated addresses in order and returns
// the first connection that succeeds, so one dead address among a dual-stack or
// multi-record answer doesn't fail the whole probe. The last dial error is
// returned when every address fails.
func dialValidatedAddrs(ctx context.Context, addrs []netip.Addr, port string, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	var dialErr error
	for _, addr := range addrs {
		conn, err := dial(ctx, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}
	return nil, dialErr
}

func blockedAddrReason(addr netip.Addr) string {
	addr = addr.Unmap()
	for _, embedded := range embeddedIPv4Prefixes {
		if !embedded.prefix.Contains(addr) {
			continue
		}
		bytes := addr.As16()
		if embedded.byteOffset+4 > len(bytes) {
			continue
		}
		addr = netip.AddrFrom4([4]byte{
			bytes[embedded.byteOffset],
			bytes[embedded.byteOffset+1],
			bytes[embedded.byteOffset+2],
			bytes[embedded.byteOffset+3],
		})
		break
	}
	for _, blocked := range blockedAddrPrefixes {
		if blocked.prefix.Contains(addr) {
			return blocked.reason
		}
	}
	return ""
}

// overrideHealthEndpoint returns a provider-specific connectivity probe URL when
// the default {baseURL}+/models path does not exist. GitLawb OpenGateway is a
// smart-routing gateway whose flat /v1/models endpoint 404s by design ("Use
// /v1/<provider>/<path>"), so probe its public /health endpoint at the host root
// instead, which reports real reachability without a per-model call.
func overrideHealthEndpoint(profile config.ProviderProfile, baseURL string) (string, bool) {
	if profile.CatalogID != "gitlawb-opengateway" {
		return "", false
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host + "/health", true
}

func healthPath(kind config.ProviderKind) string {
	switch kind {
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		return "/v1/models"
	case config.ProviderKindGoogle:
		return "/v1beta/models"
	default:
		return "/models"
	}
}

func applyAuth(request *http.Request, profile config.ProviderProfile, kind config.ProviderKind) {
	switch kind {
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		request.Header.Set("anthropic-version", "2023-06-01")
		providerio.ApplyAuthHeaders(request, providerio.AuthHeaders{
			APIKey:            profile.APIKey,
			DefaultAuthHeader: "x-api-key",
			AuthHeader:        profile.AuthHeader,
			AuthScheme:        profile.AuthScheme,
			AuthHeaderValue:   profile.AuthHeaderValue,
			CustomHeaders:     profile.CustomHeaders,
		})
	case config.ProviderKindGoogle:
		providerio.ApplyAuthHeaders(request, providerio.AuthHeaders{
			APIKey:            profile.APIKey,
			DefaultAuthHeader: "x-goog-api-key",
			AuthHeader:        profile.AuthHeader,
			AuthScheme:        profile.AuthScheme,
			AuthHeaderValue:   profile.AuthHeaderValue,
			CustomHeaders:     profile.CustomHeaders,
		})
	default:
		providerio.ApplyAuthHeaders(request, providerio.AuthHeaders{
			APIKey:            profile.APIKey,
			DefaultAuthHeader: "Authorization",
			DefaultAuthScheme: "Bearer",
			AuthHeader:        profile.AuthHeader,
			AuthScheme:        profile.AuthScheme,
			AuthHeaderValue:   profile.AuthHeaderValue,
			CustomHeaders:     profile.CustomHeaders,
		})
	}
}

func classifyTransportError(err error, profile config.ProviderProfile) Check {
	category := CategoryNetwork
	if errors.Is(err, context.DeadlineExceeded) {
		category = CategoryTimeout
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			category = CategoryTimeout
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			if errors.Is(urlErr.Err, context.DeadlineExceeded) {
				category = CategoryTimeout
			}
		}
	}
	return check("provider.connectivity", "Provider connectivity", StatusFail, category, "Provider connectivity failed: "+err.Error(), nil, profile)
}

func responseMessage(statusCode int, body []byte) string {
	message := strings.TrimSpace(string(body))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Error.Message != "" {
			message = parsed.Error.Message
		} else if parsed.Message != "" {
			message = parsed.Message
		}
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return fmt.Sprintf("Provider endpoint returned %d: %s", statusCode, message)
}

func check(id string, label string, status Status, category Category, message string, details map[string]any, profile config.ProviderProfile) Check {
	out := Check{
		ID:       id,
		Label:    label,
		Status:   status,
		Category: category,
		Message:  redact(message, profile),
	}
	if len(details) > 0 {
		out.Details = map[string]any{}
		for key, value := range details {
			out.Details[key] = redactAny(key, value, profile)
		}
	}
	return out
}

func redactAny(key string, value any, profile config.ProviderProfile) any {
	if text, ok := value.(string); ok {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		if strings.Contains(normalizedKey, "url") || strings.Contains(normalizedKey, "endpoint") {
			return redactBaseURL(text, profile)
		}
		return redact(text, profile)
	}
	return value
}

func redactBaseURL(baseURL string, profile config.ProviderProfile) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err == nil && parsed.User != nil {
		parsed.User = nil
		baseURL = parsed.String()
	}
	return redact(baseURL, profile)
}

func redact(message string, profile config.ProviderProfile) string {
	secrets := providerSecrets(profile)
	return redaction.RedactString(providerio.Redact(message, secrets...), redaction.Options{
		ExtraSecretValues: secrets,
	})
}

func providerSecrets(profile config.ProviderProfile) []string {
	secrets := []string{profile.APIKey, profile.AuthHeaderValue}
	if fields := strings.Fields(profile.AuthHeaderValue); len(fields) == 2 {
		secrets = append(secrets, fields[1])
	}
	for _, value := range profile.CustomHeaders {
		if strings.TrimSpace(value) != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func credentialDetails(profile config.ProviderProfile) map[string]any {
	details := map[string]any{}
	if strings.TrimSpace(profile.APIKeyEnv) != "" {
		details["apiKeyEnv"] = profile.APIKeyEnv
	}
	if strings.TrimSpace(profile.AuthHeader) != "" {
		details["authHeader"] = profile.AuthHeader
	}
	return details
}

func hasCredential(profile config.ProviderProfile) bool {
	return strings.TrimSpace(profile.APIKey) != "" || strings.TrimSpace(profile.AuthHeaderValue) != ""
}

func credentialRequired(profile config.ProviderProfile, providerKind config.ProviderKind) bool {
	if strings.TrimSpace(profile.CatalogID) != "" {
		if descriptor, err := providercatalog.Require(profile.CatalogID); err == nil {
			return descriptor.RequiresAuth
		}
	}
	switch providerKind {
	case config.ProviderKindOpenAI, config.ProviderKindAnthropic, config.ProviderKindGoogle:
		return true
	default:
		return false
	}
}

func providerName(profile config.ProviderProfile) string {
	if strings.TrimSpace(profile.Name) != "" {
		return strings.TrimSpace(profile.Name)
	}
	if strings.TrimSpace(string(profile.ProviderKind)) != "" {
		return strings.TrimSpace(string(profile.ProviderKind))
	}
	return strings.TrimSpace(profile.Provider)
}
