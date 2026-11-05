package mcp

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/config"
)

// The AI/ML API MCP server authenticates with the user's AI/ML API key.
//
// Invariant: the key and the attribution headers are attached only to an
// outgoing request whose own URL is https://mcp.aimlapi.com on the default port
// with no userinfo (exact hostname, see aimlapi.IsAttributedURL), whatever the
// server is named. The key is resolved again for every request through
// config.LoadAIMLAPIServiceKey, which in the same call decides whether it may
// be sent (no proxy profile, a single token), so a provider change applies to
// the next request and a withheld or invalid key means no Authorization
// header. It lives only in that request's headers: never in Server, its
// Identity (OAuth token binding), config.json, list output or errors. A
// user-configured Authorization header, OAuth, or a project-touched server
// always goes without it.
var loadAIMLAPIServiceKey = config.LoadAIMLAPIServiceKey

var aimlapiMCPHost = func() string {
	parsed, err := url.Parse(aimlapi.MCPServerURL)
	if err != nil {
		panic(err)
	}
	return parsed.Hostname()
}()

func isAIMLAPIMCPURL(target *url.URL) bool {
	return aimlapi.IsAttributedURL(target) && strings.EqualFold(target.Hostname(), aimlapiMCPHost)
}

// applyAIMLAPIRequestHeaders runs after the server's own headers are set.
func applyAIMLAPIRequestHeaders(server Server, request *http.Request) {
	if request == nil || !isAIMLAPIMCPURL(request.URL) {
		return
	}
	aimlapi.ApplyAttributionHeaders(request.Header, request.URL)
	if request.Header.Get("Authorization") != "" || strings.EqualFold(server.Auth, ServerAuthOAuth) || server.ProjectConfigured {
		return
	}
	key, err := loadAIMLAPIServiceKey()
	if err != nil || key == "" {
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
}

// aimlapiUnauthorizedHint explains how to authenticate when the AI/ML API MCP
// server rejects a request without OAuth. It names no key material.
func aimlapiUnauthorizedHint(server Server, status int) string {
	if status != http.StatusUnauthorized || strings.EqualFold(server.Auth, ServerAuthOAuth) {
		return ""
	}
	target, err := url.Parse(server.URL)
	if err != nil || !isAIMLAPIMCPURL(target) {
		return ""
	}
	oauthHint := fmt.Sprintf("To sign in instead, set \"auth\": \"oauth\" on this server and run `zero mcp oauth login %s`", server.Name)
	if server.ProjectConfigured {
		return " (" + oauthHint + ")"
	}
	for name, value := range server.Headers {
		if strings.EqualFold(name, "Authorization") && value != "" {
			return " (" + oauthHint + ")"
		}
	}
	return fmt.Sprintf(" (set up the aimlapi.com provider with `zero providers setup aimlapi` or set AIMLAPI_API_KEY; automatic injection is also disabled for proxy profiles, OAuth, project config, and non-empty Authorization headers — see docs/EXTENDING.md#built-in-servers. To sign in instead, set \"auth\": \"oauth\" on this server and run `zero mcp oauth login %s`)", server.Name)
}
