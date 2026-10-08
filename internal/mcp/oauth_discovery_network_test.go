package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOAuthDiscoveryNetworkPolicyRejectsNonPublicAddresses(t *testing.T) {
	policy := oauthDiscoveryNetworkPolicy{}
	for _, value := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"169.254.169.254",
		"192.0.2.10",
		"224.0.0.1",
		"::1",
		"fc00::1",
		"fe80::1",
		"2001:db8::1",
	} {
		t.Run(value, func(t *testing.T) {
			if err := policy.validateAddress(netip.MustParseAddr(value)); err == nil {
				t.Fatalf("address %s should be rejected", value)
			}
		})
	}
	for _, value := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if err := policy.validateAddress(netip.MustParseAddr(value)); err != nil {
			t.Fatalf("public address %s rejected: %v", value, err)
		}
	}
	loopbackPolicy := oauthDiscoveryNetworkPolicy{allowLoopback: true}
	if err := loopbackPolicy.validateAddress(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatalf("explicit loopback resource should allow loopback discovery: %v", err)
	}
	if err := loopbackPolicy.validateAddress(netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Fatal("loopback development must not permit arbitrary private targets")
	}
}

func TestProtectedDiscoveryRejectsNonPublicOAuthEndpoints(t *testing.T) {
	for _, endpoint := range []struct {
		name     string
		metadata authServerMetadata
	}{
		{
			name: "authorization",
			metadata: authServerMetadata{
				AuthorizationEndpoint:        "http://127.0.0.1/authorize",
				protectAuthorizationEndpoint: true,
			},
		},
		{
			name: "token",
			metadata: authServerMetadata{
				TokenEndpoint:        "https://10.0.0.1/token",
				protectTokenEndpoint: true,
			},
		},
		{
			name: "registration",
			metadata: authServerMetadata{
				RegistrationEndpoint:        "https://169.254.169.254/register",
				protectRegistrationEndpoint: true,
			},
		},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			endpoint.metadata.protectedResourceURL = "https://mcp.example/mcp"
			if err := validateProtectedDiscoveredEndpoints(context.Background(), endpoint.metadata); err == nil {
				t.Fatalf("protected %s endpoint should be rejected", endpoint.name)
			}
		})
	}
}

func TestAdvertisedOAuthDiscoveryRejectsHostnameResolvingPrivateAtUseTime(t *testing.T) {
	var dialed atomic.Bool
	policy := oauthDiscoveryNetworkPolicy{
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.0.0.12")}, nil
		},
	}
	client := &http.Client{Transport: advertisedOAuthDiscoveryTransport{
		base: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed.Store(true)
			return nil, io.EOF
		}},
		policy: policy,
	}}

	_, err := client.Get("https://auth.example/.well-known/oauth-authorization-server")
	if err == nil || !strings.Contains(err.Error(), errUnsafeOAuthDiscoveryTarget.Error()) {
		t.Fatalf("error = %v, want private-resolution rejection", err)
	}
	if dialed.Load() {
		t.Fatal("private resolved address reached the dialer")
	}
}

func TestAdvertisedOAuthDiscoveryPinsResolvedAddress(t *testing.T) {
	const publicAddress = "93.184.216.34"
	var dialedAddress string
	policy := oauthDiscoveryNetworkPolicy{
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(publicAddress)}, nil
		},
	}
	baseTransport := &http.Transport{DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
		dialedAddress = address
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			reader := bufio.NewReader(serverConn)
			for {
				line, err := reader.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
		}()
		return clientConn, nil
	}}
	client := &http.Client{Transport: advertisedOAuthDiscoveryTransport{base: baseTransport, policy: policy}}

	response, err := client.Get("http://auth.example/metadata")
	if err != nil {
		t.Fatalf("GET pinned discovery target: %v", err)
	}
	response.Body.Close()
	if dialedAddress != net.JoinHostPort(publicAddress, "80") {
		t.Fatalf("dialed %q, want resolved address", dialedAddress)
	}
}

func TestPinnedOAuthDiscoveryPreservesHTTPSProxyFirstHop(t *testing.T) {
	for _, test := range []struct {
		name     string
		proxy    string
		firstHop string
	}{
		{name: "HTTP", proxy: "http://proxy.example:8080", firstHop: "proxy.example:8080"},
		{name: "HTTPS", proxy: "https://proxy.example:8443", firstHop: "proxy.example:8443"},
		{name: "SOCKS", proxy: "socks5://proxy.example:1080", firstHop: "proxy.example:1080"},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxyURL, err := url.Parse(test.proxy)
			if err != nil {
				t.Fatalf("parse proxy URL: %v", err)
			}
			var dialedAddress string
			baseTransport := &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
					dialedAddress = address
					return nil, errors.New("stop after observing first hop")
				},
			}
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://auth.example/metadata", nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}

			_, _ = roundTripPinnedOAuthDiscovery(request, baseTransport, netip.MustParseAddr("93.184.216.34"))
			if dialedAddress != test.firstHop {
				t.Fatalf("dialed %q, want %s proxy first hop", dialedAddress, test.name)
			}
		})
	}
}
