package mcp

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const maxMCPRedirects = 10

func mcpHTTPClient(server Server, transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkMCPRedirect(server),
	}
}

func checkMCPRedirect(server Server) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxMCPRedirects {
			return fmt.Errorf("MCP %s server %s stopped after %d redirects", server.Type, server.Name, maxMCPRedirects)
		}
		if len(via) == 0 {
			return nil
		}
		if !sameMCPOrigin(via[0].URL, request.URL) {
			return fmt.Errorf("MCP %s server %s refused cross-origin redirect to %s", server.Type, server.Name, mcpOrigin(request.URL))
		}
		reapplyAIMLAPIRedirectHeaders(server, request, via[0])
		return nil
	}
}

func sameMCPOrigin(left *url.URL, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectiveMCPPort(left) == effectiveMCPPort(right)
}

func effectiveMCPPort(value *url.URL) string {
	if value == nil {
		return ""
	}
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func mcpOrigin(value *url.URL) string {
	if value == nil {
		return "<nil>"
	}
	host := value.Hostname()
	port := effectiveMCPPort(value)
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	if value.Scheme == "" {
		return host
	}
	return strings.ToLower(value.Scheme) + "://" + host
}
