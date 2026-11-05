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
// server is named. The key is resolved again for every request, redirect hops
// included (see reapplyAIMLAPIRedirectHeaders), through
// config.LoadAIMLAPIServiceKey, which in the same call decides whether it may
// be sent (no proxy profile, a single token), so a provider change applies to
// the next request and a withheld or invalid key means no Authorization
// header. It lives only in that request's headers: never in Server, its
// Identity (OAuth token binding), config.json, list output or errors (an
// error response to such a request is reported without its body). A
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

// aimlapiAutomaticHeaders are the headers applyAIMLAPIRequestHeaders may add.
var aimlapiAutomaticHeaders = [...]string{"Authorization", aimlapi.SourceHeaderName, aimlapi.PartnerHeaderName}

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

// reapplyAIMLAPIRedirectHeaders gives a redirect hop the automatic headers its
// own URL earns. net/http copies the first request's headers onto every hop
// without calling the header hooks, so without this a key or attribution added
// for the first URL would follow a same-origin hop that fails isAIMLAPIMCPURL
// (userinfo, say), and an eligible hop would reuse the first hop's key instead
// of resolving it again. The hop gets back the server's own values for these
// headers, then the same rule as any other request.
func reapplyAIMLAPIRedirectHeaders(server Server, request *http.Request, first *http.Request) {
	if request == nil || first == nil || !isAIMLAPIMCPURL(first.URL) {
		return
	}
	for _, name := range aimlapiAutomaticHeaders {
		request.Header.Del(name)
	}
	for name, value := range server.Headers {
		for _, automatic := range aimlapiAutomaticHeaders {
			if strings.EqualFold(name, automatic) {
				request.Header.Set(name, value)
			}
		}
	}
	applyAIMLAPIRequestHeaders(server, request)
}

// sentAIMLAPIServiceKey reports whether request went out carrying the key
// applyAIMLAPIRequestHeaders injects, as opposed to the server's own
// Authorization header or OAuth.
func sentAIMLAPIServiceKey(server Server, request *http.Request) bool {
	if request == nil || !isAIMLAPIMCPURL(request.URL) || strings.EqualFold(server.Auth, ServerAuthOAuth) || server.ProjectConfigured {
		return false
	}
	return request.Header.Get("Authorization") != "" && !hasUserAuthorizationHeader(server)
}

func hasUserAuthorizationHeader(server Server) bool {
	for name, value := range server.Headers {
		if strings.EqualFold(name, "Authorization") && value != "" {
			return true
		}
	}
	return false
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
	if server.ProjectConfigured || hasUserAuthorizationHeader(server) {
		return " (" + oauthHint + ")"
	}
	return fmt.Sprintf(" (set up the aimlapi.com provider with `zero providers setup aimlapi` or set AIMLAPI_API_KEY; automatic injection is also disabled for proxy profiles, OAuth, project config, and non-empty Authorization headers — see docs/EXTENDING.md#built-in-servers. To sign in instead, set \"auth\": \"oauth\" on this server and run `zero mcp oauth login %s`)", server.Name)
}
