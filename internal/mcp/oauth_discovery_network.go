package mcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

var errUnsafeOAuthDiscoveryTarget = errors.New("mcp oauth: refused non-public server-advertised discovery target")

var nonPublicOAuthDiscoveryPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

type oauthDiscoveryNetworkPolicy struct {
	allowLoopback bool
	lookupNetIP   func(context.Context, string, string) ([]netip.Addr, error)
}

type advertisedOAuthDiscoveryTransport struct {
	base   http.RoundTripper
	policy oauthDiscoveryNetworkPolicy
}

// newAdvertisedOAuthDiscoveryClient binds server-advertised discovery to the
// public network. Loopback targets remain available only when the operator
// explicitly configured a loopback MCP resource.
func newAdvertisedOAuthDiscoveryClient(client *http.Client, resourceURL string) (*http.Client, error) {
	policy, err := newOAuthDiscoveryNetworkPolicy(resourceURL)
	if err != nil {
		return nil, err
	}
	copied := withoutDiscoveryRedirects(client)
	transport := copied.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copied.Transport = advertisedOAuthDiscoveryTransport{base: transport, policy: policy}
	return copied, nil
}

func newOAuthDiscoveryNetworkPolicy(resourceURL string) (oauthDiscoveryNetworkPolicy, error) {
	parsed, err := url.Parse(strings.TrimSpace(resourceURL))
	if err != nil || parsed.Hostname() == "" {
		return oauthDiscoveryNetworkPolicy{}, errors.New("mcp oauth: invalid MCP resource URL for discovery policy")
	}
	return oauthDiscoveryNetworkPolicy{allowLoopback: isExplicitOAuthLoopbackHost(parsed.Hostname())}, nil
}

func validateAdvertisedOAuthDiscoveryURL(resourceURL string, targetURL string) error {
	policy, err := newOAuthDiscoveryNetworkPolicy(resourceURL)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Hostname() == "" {
		return errUnsafeOAuthDiscoveryTarget
	}
	return policy.validateLiteralHost(parsed.Hostname())
}

func (transport advertisedOAuthDiscoveryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("mcp oauth: discovery request has no URL")
	}
	if err := transport.policy.validateLiteralHost(request.URL.Hostname()); err != nil {
		return nil, err
	}

	baseTransport, ok := transport.base.(*http.Transport)
	if !ok {
		return nil, errors.New("mcp oauth: cannot enforce discovery network policy with a custom HTTP transport")
	}
	addresses, err := transport.policy.resolve(request.Context(), request.URL.Hostname())
	if err != nil {
		return nil, err
	}
	var lastErr error
	for index, address := range addresses {
		attempt := request
		if index > 0 && request.Body != nil {
			if request.GetBody == nil {
				break
			}
			body, bodyErr := request.GetBody()
			if bodyErr != nil {
				return nil, bodyErr
			}
			attempt = request.Clone(request.Context())
			attempt.Body = body
		}
		response, roundTripErr := roundTripPinnedOAuthDiscovery(attempt, baseTransport, address)
		if roundTripErr == nil {
			return response, nil
		}
		lastErr = roundTripErr
	}
	if lastErr == nil {
		lastErr = errUnsafeOAuthDiscoveryTarget
	}
	return nil, lastErr
}

func roundTripPinnedOAuthDiscovery(request *http.Request, base *http.Transport, address netip.Addr) (*http.Response, error) {
	pinnedRequest := request.Clone(request.Context())
	pinnedURL := *request.URL
	originalHost := request.URL.Hostname()
	port := request.URL.Port()
	if port == "" {
		switch strings.ToLower(request.URL.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return nil, errUnsafeOAuthDiscoveryTarget
		}
	}
	pinnedURL.Host = net.JoinHostPort(address.String(), port)
	pinnedRequest.URL = &pinnedURL
	if pinnedRequest.Host == "" {
		pinnedRequest.Host = request.URL.Host
	}

	pinnedTransport := base.Clone()
	pinnedTransport.DisableKeepAlives = true
	var proxyURL *url.URL
	if base.Proxy != nil {
		var err error
		proxyURL, err = base.Proxy(request)
		if err != nil {
			return nil, fmt.Errorf("mcp oauth: resolve discovery proxy: %w", err)
		}
		if proxyURL == nil {
			pinnedTransport.Proxy = nil
		} else {
			pinnedTransport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	if pinnedTransport.TLSClientConfig == nil {
		pinnedTransport.TLSClientConfig = &tls.Config{ServerName: originalHost} //nolint:gosec // standard verification remains enabled
	} else {
		pinnedTransport.TLSClientConfig = pinnedTransport.TLSClientConfig.Clone()
		pinnedTransport.TLSClientConfig.ServerName = originalHost
	}
	// A custom TLS dialer must not resolve the advertised hostname again after
	// the policy has pinned it. For an HTTPS proxy, however, that proxy remains
	// the TLS first hop and the transport sends CONNECT to the pinned target.
	dialContext := pinnedTransport.DialContext
	if dialContext == nil {
		dialContext = (&net.Dialer{}).DialContext
	}
	pinnedAddress := net.JoinHostPort(address.String(), port)
	targetTLSConfig := pinnedTransport.TLSClientConfig
	pinnedTransport.DialTLSContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		if proxyURL != nil && strings.EqualFold(proxyURL.Scheme, "https") {
			proxyTLSConfig := targetTLSConfig.Clone()
			proxyTLSConfig.ServerName = proxyURL.Hostname()
			return dialOAuthDiscoveryTLS(ctx, dialContext, network, address, proxyTLSConfig)
		}
		return dialOAuthDiscoveryTLS(ctx, dialContext, network, pinnedAddress, targetTLSConfig)
	}
	return pinnedTransport.RoundTrip(pinnedRequest)
}

func dialOAuthDiscoveryTLS(ctx context.Context, dialContext func(context.Context, string, string) (net.Conn, error), network string, address string, config *tls.Config) (net.Conn, error) {
	connection, err := dialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tlsConnection := tls.Client(connection, config)
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		connection.Close()
		return nil, err
	}
	return tlsConnection, nil
}

func (policy oauthDiscoveryNetworkPolicy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		address = address.Unmap()
		if err := policy.validateAddress(address); err != nil {
			return nil, err
		}
		return []netip.Addr{address}, nil
	}

	lookupNetIP := policy.lookupNetIP
	if lookupNetIP == nil {
		lookupNetIP = net.DefaultResolver.LookupNetIP
	}
	addresses, err := lookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, errors.New("mcp oauth: resolve server-advertised discovery target failed")
	}
	if len(addresses) == 0 {
		return nil, errors.New("mcp oauth: server-advertised discovery target resolved to no addresses")
	}
	explicitLoopback := isExplicitOAuthLoopbackHost(host)
	resolved := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if explicitLoopback && !address.IsLoopback() {
			return nil, errUnsafeOAuthDiscoveryTarget
		}
		if err := policy.validateAddress(address); err != nil {
			return nil, err
		}
		resolved = append(resolved, address)
	}
	return resolved, nil
}

func (policy oauthDiscoveryNetworkPolicy) validateLiteralHost(host string) error {
	normalized := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if normalized == "" {
		return errUnsafeOAuthDiscoveryTarget
	}
	if isCloudMetadataHostname(normalized) {
		return errUnsafeOAuthDiscoveryTarget
	}
	if isExplicitOAuthLoopbackHost(normalized) && !policy.allowLoopback {
		return errUnsafeOAuthDiscoveryTarget
	}
	if address, err := netip.ParseAddr(normalized); err == nil {
		return policy.validateAddress(address.Unmap())
	}
	return nil
}

func (policy oauthDiscoveryNetworkPolicy) validateAddress(address netip.Addr) error {
	if policy.allowLoopback && address.IsLoopback() {
		return nil
	}
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return errUnsafeOAuthDiscoveryTarget
	}
	for _, prefix := range nonPublicOAuthDiscoveryPrefixes {
		if prefix.Contains(address) {
			return errUnsafeOAuthDiscoveryTarget
		}
	}
	return nil
}

func isExplicitOAuthLoopbackHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") {
		return true
	}
	address, err := netip.ParseAddr(normalized)
	return err == nil && address.Unmap().IsLoopback()
}

func isCloudMetadataHostname(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "metadata", "metadata.google.internal", "metadata.azure.internal":
		return true
	default:
		return false
	}
}
