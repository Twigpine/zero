package config

import (
	"fmt"
	"slices"
	"strings"
)

func mergeMCPConfig(dst *MCPConfig, src MCPConfig, canReenable bool) {
	if len(src.Servers) == 0 {
		return
	}
	if dst.Servers == nil {
		dst.Servers = map[string]MCPServerConfig{}
	}
	for name, server := range src.Servers {
		dst.Servers[name] = mergeMCPServer(dst.Servers[name], server, canReenable)
	}
}

// mergeProjectMCPConfig merges a project-scoped (lower-trust) MCP config into
// dst. It never re-enables a server the user explicitly disabled or disabled a
// server the user explicitly enabled: it merges with canReenable=false so the
// trust-boundary guard in mergeMCPServer keeps the user's decision authoritative.
func mergeProjectMCPConfig(dst *MCPConfig, src MCPConfig) error {
	if len(src.Servers) == 0 {
		return nil
	}
	if dst.Servers == nil {
		dst.Servers = map[string]MCPServerConfig{}
	}
	for name, server := range src.Servers {
		base := dst.Servers[name]
		candidate := mergeMCPServer(base, server, false)
		if projectMCPServerTargetChanges(base, server) && hasInheritedMCPCredentialMaterial(server, candidate) {
			return fmt.Errorf("project MCP server %q cannot override target while inheriting user credentials; set headers/env/envFrom/oauth explicitly or use a new server name", name)
		}
		candidate.ProjectConfigured = true
		dst.Servers[name] = candidate
	}
	return nil
}

// mergeMCPServer merges a later config layer's MCP server into the base. The
// canReenable flag marks the CLI-override scope, the only layer that is allowed
// to lift a sticky user-level disable/enable decision (see the disabled-handling
// block).
func mergeMCPServer(base MCPServerConfig, next MCPServerConfig, canReenable bool) MCPServerConfig {
	if strings.TrimSpace(next.Type) != "" {
		base.Type = next.Type
	}
	if strings.TrimSpace(next.Command) != "" {
		base.Command = next.Command
	}
	if next.Args != nil {
		base.Args = append([]string{}, next.Args...)
	}
	if next.Env != nil {
		base.Env = copyMCPStringMap(next.Env)
	}
	if next.EnvFrom != nil {
		base.EnvFrom = copyMCPStringMap(next.EnvFrom)
	}
	if strings.TrimSpace(next.URL) != "" {
		base.URL = next.URL
	}
	if next.Headers != nil {
		base.Headers = copyMCPStringMap(next.Headers)
	}
	if strings.TrimSpace(next.Auth) != "" {
		base.Auth = next.Auth
	}
	if next.OAuth != nil {
		base.OAuth = next.OAuth
	}
	// Capture the higher-trust scope's prior decision before folding in
	// next.disabledSet. The trust boundary must be evaluated against the state
	// the higher-trust layer actually left behind, not against next's own flag.
	baseDisabledSet := base.disabledSet
	baseDisabled := base.Disabled
	if next.disabledSet {
		base.disabledSet = true
	}
	if next.disabledSet || next.Disabled {
		// Trust boundary: once a higher-trust scope has explicitly set the
		// disabled flag, a lower-trust layer (project config) cannot override
		// that decision in either direction — the user's choice (whether to
		// enable or disable a server) wins over the repo. This blocks a repo
		// from re-enabling a server the user disabled, and from disabling a
		// server the user explicitly enabled. The only layer allowed to
		// override an explicit higher-scope decision is the CLI override scope
		// (canReenable=true).
		if canReenable || !baseDisabledSet || baseDisabled == next.Disabled {
			base.Disabled = next.Disabled
		}
	}
	if next.configured {
		base.configured = true
	}
	if next.ProjectConfigured {
		base.ProjectConfigured = true
	}
	return base
}

func projectMCPServerTargetChanges(base MCPServerConfig, next MCPServerConfig) bool {
	if strings.TrimSpace(next.Type) != "" && mcpServerTransportKind(base) != mcpServerTransportKind(mergeMCPServer(base, next, true)) {
		return true
	}
	if strings.TrimSpace(next.URL) != "" && strings.TrimSpace(next.URL) != strings.TrimSpace(base.URL) {
		return true
	}
	if strings.TrimSpace(next.Command) != "" && strings.TrimSpace(next.Command) != strings.TrimSpace(base.Command) {
		return true
	}
	if next.Args != nil && !slices.Equal(trimMCPArgs(next.Args), trimMCPArgs(base.Args)) {
		return true
	}
	return false
}

func mcpServerTransportKind(server MCPServerConfig) string {
	if typ := strings.TrimSpace(strings.ToLower(server.Type)); typ != "" {
		return typ
	}
	if strings.TrimSpace(server.URL) != "" {
		return "http"
	}
	return "stdio"
}

func hasInheritedMCPCredentialMaterial(project MCPServerConfig, candidate MCPServerConfig) bool {
	if project.Headers == nil && hasMCPStringMapMaterial(candidate.Headers) {
		return true
	}
	if project.Env == nil && hasMCPStringMapMaterial(candidate.Env) {
		return true
	}
	// A credential reference carries no value, but inheriting one still hands
	// the user's stored secret to whatever binary or endpoint the project layer
	// just pointed the server at.
	if project.EnvFrom == nil && hasMCPStringMapMaterial(candidate.EnvFrom) {
		return true
	}
	if project.OAuth == nil && hasMCPOAuthMaterial(candidate.OAuth) {
		return true
	}
	return false
}

func hasMCPStringMapMaterial(values map[string]string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func hasMCPOAuthMaterial(oauth *MCPOAuthConfig) bool {
	if oauth == nil {
		return false
	}
	if strings.TrimSpace(oauth.ClientID) != "" ||
		strings.TrimSpace(oauth.ClientSecret) != "" ||
		strings.TrimSpace(oauth.AuthorizationEndpoint) != "" ||
		strings.TrimSpace(oauth.TokenEndpoint) != "" ||
		strings.TrimSpace(oauth.RegistrationEndpoint) != "" ||
		strings.TrimSpace(oauth.IssuerURL) != "" {
		return true
	}
	for _, scope := range oauth.Scopes {
		if strings.TrimSpace(scope) != "" {
			return true
		}
	}
	return false
}

func trimMCPArgs(values []string) []string {
	if values == nil {
		return nil
	}
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func copyMCPStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
