package aimlapi

import (
	"net/http"
	"net/url"
	"strings"
)

const (
	// SourceHeaderName and SourceValue identify Zero as the calling agent on
	// requests to aimlapi.com services (the Toolbox and the MCP server).
	SourceHeaderName = "X-AIMLAPI-Source"
	SourceValue      = "agent/zero"

	// CanonicalInferenceBaseURL is the catalog inference endpoint. A provider
	// profile whose base URL differs is treated as a proxy: its key is never sent
	// to other aimlapi.com services.
	CanonicalInferenceBaseURL = "https://api.aimlapi.com/v1"
	// ToolboxBaseURL is the fixed AI/ML API Toolbox REST base. It is not
	// configurable, so the key and attribution can only reach tools.aimlapi.com.
	ToolboxBaseURL = "https://tools.aimlapi.com/v1/tools"
	// MCPServerURL is the AI/ML API MCP server (streamable HTTP).
	MCPServerURL = "https://mcp.aimlapi.com/mcp"
)

// attributedHosts are the only hostnames that receive the AI/ML API key or the
// attribution headers. Matching is exact; a suffix or substring match would
// also accept hosts such as tools.aimlapi.com.example.net.
var attributedHosts = [...]string{"api.aimlapi.com", "tools.aimlapi.com", "mcp.aimlapi.com"}

// IsAttributedURL reports whether target is an https URL on one of the
// aimlapi.com service hosts, on the default port and without userinfo.
func IsAttributedURL(target *url.URL) bool {
	if target == nil || !strings.EqualFold(target.Scheme, "https") || target.User != nil {
		return false
	}
	if port := target.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	for _, candidate := range attributedHosts {
		if host == candidate {
			return true
		}
	}
	return false
}

// ApplyAttributionHeaders sets the source and partner headers on header only
// when target is an aimlapi.com service URL; any other target is left as is.
func ApplyAttributionHeaders(header http.Header, target *url.URL) {
	if header == nil || !IsAttributedURL(target) {
		return
	}
	header.Set(SourceHeaderName, SourceValue)
	header.Set(PartnerHeaderName, ResolvePartnerID(""))
}
