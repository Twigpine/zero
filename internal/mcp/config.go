package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
)

type ServerType string

const (
	ServerTypeStdio ServerType = "stdio"
	ServerTypeHTTP  ServerType = "http"
	ServerTypeSSE   ServerType = "sse"
)

type Server struct {
	Name     string
	Type     ServerType
	Command  string
	Args     []string
	Env      map[string]string
	URL      string
	Headers  map[string]string
	Auth     string
	OAuth    *OAuthConfig
	Identity string
	// ProjectConfigured is true when project config touched this server. Runtime
	// credential lookup uses it to avoid reusing legacy user tokens by name.
	ProjectConfigured bool
	// UnconfiguredDefault is true when this server is one of Zero's built-in
	// defaults (e.g. keyless Exa) that the user never touched in their
	// config — no credentials, no overrides. Callers use it to avoid warning
	// loudly when a server nobody configured fails to connect.
	UnconfiguredDefault bool
}

func NormalizeConfig(cfg config.MCPConfig) ([]Server, error) {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	servers := make([]Server, 0, len(names))
	for _, name := range names {
		raw := cfg.Servers[name]
		if raw.Disabled {
			continue
		}
		server, err := normalizeServer(name, raw)
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func normalizeServer(name string, raw config.MCPServerConfig) (Server, error) {
	name = strings.TrimSpace(name)
	if err := ValidateServerName(name); err != nil {
		return Server{}, err
	}

	serverType := ServerType(strings.ToLower(strings.TrimSpace(raw.Type)))
	if serverType == "" {
		// When type is omitted, a URL makes the server use HTTP validation; otherwise
		// command-based stdio remains the default.
		if strings.TrimSpace(raw.URL) != "" {
			serverType = ServerTypeHTTP
		} else {
			serverType = ServerTypeStdio
		}
	}

	auth := strings.ToLower(strings.TrimSpace(raw.Auth))
	server := Server{
		Name:                name,
		Type:                serverType,
		Command:             strings.TrimSpace(raw.Command),
		Args:                trimStringSlice(raw.Args),
		Env:                 copyStringMap(raw.Env),
		URL:                 strings.TrimSpace(raw.URL),
		Headers:             copyStringMap(raw.Headers),
		Auth:                auth,
		OAuth:               normalizeOAuthConfig(raw.OAuth),
		ProjectConfigured:   raw.ProjectConfigured,
		UnconfiguredDefault: config.IsUnconfiguredDefault(name, raw),
	}

	switch server.Type {
	case ServerTypeStdio:
		if server.Command == "" {
			return Server{}, fmt.Errorf("MCP server %s requires command for stdio transport", server.Name)
		}
		if server.URL != "" {
			return Server{}, fmt.Errorf("MCP server %s url is only supported for http or sse transports", server.Name)
		}
		if len(server.Headers) > 0 {
			return Server{}, fmt.Errorf("MCP server %s headers are only supported for http or sse transports", server.Name)
		}
		if server.Auth != "" {
			return Server{}, fmt.Errorf("MCP server %s auth is only supported for http or sse transports", server.Name)
		}
	case ServerTypeHTTP, ServerTypeSSE:
		if server.URL == "" {
			return Server{}, fmt.Errorf("MCP server %s requires url for %s transport", server.Name, server.Type)
		}
		if server.Command != "" || len(server.Args) > 0 {
			return Server{}, fmt.Errorf("MCP server %s command and args are only supported for stdio transport", server.Name)
		}
		if len(server.Env) > 0 {
			return Server{}, fmt.Errorf("MCP server %s env is only supported for stdio transport", server.Name)
		}
		if err := validateHTTPURL(server.Name, server.URL); err != nil {
			return Server{}, err
		}
	default:
		return Server{}, fmt.Errorf("MCP server %s has unsupported type %q", server.Name, raw.Type)
	}

	if server.Auth != "" && server.Auth != ServerAuthOAuth {
		return Server{}, fmt.Errorf("MCP server %s has unsupported auth %q", server.Name, raw.Auth)
	}

	server.Identity = computeServerIdentity(server)
	return server, nil
}

// normalizeOAuthConfig converts a raw config OAuth block into the mcp package's
// OAuthConfig, trimming endpoint and identifier fields. Credential values are
// preserved verbatim.
func normalizeOAuthConfig(raw *config.MCPOAuthConfig) *OAuthConfig {
	if raw == nil {
		return nil
	}
	return &OAuthConfig{
		ClientID:              strings.TrimSpace(raw.ClientID),
		ClientSecret:          raw.ClientSecret,
		Scopes:                trimStringSlice(raw.Scopes),
		AuthorizationEndpoint: strings.TrimSpace(raw.AuthorizationEndpoint),
		TokenEndpoint:         strings.TrimSpace(raw.TokenEndpoint),
		RegistrationEndpoint:  strings.TrimSpace(raw.RegistrationEndpoint),
		IssuerURL:             strings.TrimSpace(raw.IssuerURL),
	}
}

func validateHTTPURL(serverName string, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("MCP server %s url must be a valid http or https URL", serverName)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("MCP server %s url must use http or https", serverName)
	}
	return nil
}

func computeServerIdentity(server Server) string {
	canonical := struct {
		Type    ServerType        `json:"type"`
		Command string            `json:"command,omitempty"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		URL     string            `json:"url,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
		Auth    string            `json:"auth,omitempty"`
		OAuth   *OAuthConfig      `json:"oauth,omitempty"`
	}{
		Type:    server.Type,
		Command: server.Command,
		Args:    append([]string{}, server.Args...),
		Env:     copyStringMap(server.Env),
		URL:     server.URL,
		Headers: copyStringMap(server.Headers),
		Auth:    server.Auth,
		OAuth:   server.OAuth,
	}
	// Marshal cannot fail for this canonical shape because it only contains
	// JSON-serializable primitive, slice, and map fields.
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	// The first 32 hex characters give a stable 128-bit identity while keeping
	// permission records compact.
	return hex.EncodeToString(sum[:])[:32]
}

func trimStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		trimmed = append(trimmed, value)
	}
	return trimmed
}

// copyStringMap trims and filters keys while preserving values verbatim. Env
// and header values may intentionally contain surrounding whitespace, unlike
// scalar fields such as Command and URL.
func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		copied[key] = value
	}
	if len(copied) == 0 {
		return nil
	}
	return copied
}
