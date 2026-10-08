package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/tools"
)

type mcpToolListItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SideEffect  string `json:"sideEffect"`
	Permission  string `json:"permission"`
}

// registerMCPToolsForWorkspace resolves and registers the workspace's MCP servers.
// This spawns stdio servers, so it gates the project config layer behind the
// workspace-trust check: trustRoot is the ORIGINAL launch directory (resolved before
// any --worktree reassignment) so a worktree of a trusted repo inherits that trust.
// resolveTrust fails closed, so an empty trustRoot or a store-read error excludes the
// project layer and a cloned repo cannot spawn its ./.zero/config.json MCP servers.
//
// It returns a trustSkip alongside the runtime so the caller can fold the MCP gate
// into the one-line trust notice (mirroring the hooks and plugins chokepoints);
// otherwise a workspace whose only project config is MCP would be gated silently.
func registerMCPToolsForWorkspace(ctx context.Context, workspaceRoot string, registry *tools.Registry, deps appDeps, autonomy mcp.PermissionAutonomy, trustRoot string, runners ...*execution.Runner) (mcpToolRuntime, trustSkip, error) {
	options := mcp.RegisterOptions{}
	if len(runners) > 0 {
		options.Execution = runners[0]
	}
	return registerMCPToolsForWorkspaceWithOptions(ctx, workspaceRoot, registry, deps, autonomy, trustRoot, options)
}

func registerMCPToolsForWorkspaceWithOptions(ctx context.Context, workspaceRoot string, registry *tools.Registry, deps appDeps, autonomy mcp.PermissionAutonomy, trustRoot string, options mcp.RegisterOptions) (mcpToolRuntime, trustSkip, error) {
	excludeProject, trustCheckErrored := resolveTrust(trustRoot)
	skip := trustSkip{
		excludedProjectConfig: excludeProject && projectMCPConfigExists(workspaceRoot),
		trustCheckErrored:     trustCheckErrored,
	}
	cfg, err := deps.resolveMCPConfig(workspaceRoot, excludeProject)
	if err != nil {
		return nil, skip, err
	}
	if len(cfg.Servers) == 0 {
		return noopMCPRuntime{}, skip, nil
	}
	store, err := deps.newMCPStore()
	if err != nil {
		return nil, skip, err
	}
	options.PermissionStore = store
	options.Autonomy = autonomy
	options.WorkspaceRoot = workspaceRoot
	runtime, err := deps.registerMCPTools(ctx, registry, cfg, options)
	return runtime, skip, err
}

func execMCPAutonomy(options execOptions) mcp.PermissionAutonomy {
	if options.skipPermissionsUnsafe || strings.EqualFold(strings.TrimSpace(options.autonomy), "high") {
		return mcp.AutonomyHigh
	}
	if strings.EqualFold(strings.TrimSpace(options.autonomy), "medium") {
		return mcp.AutonomyMedium
	}
	return mcp.AutonomyLow
}

func mcpToolList(registry *tools.Registry) []mcpToolListItem {
	registered := registry.All()
	items := make([]mcpToolListItem, 0, len(registered))
	for _, tool := range registered {
		if !strings.HasPrefix(tool.Name(), "mcp_") {
			continue
		}
		safety := tool.Safety()
		items = append(items, mcpToolListItem{
			Name:        tool.Name(),
			Description: tool.Description(),
			SideEffect:  string(safety.SideEffect),
			Permission:  string(safety.Permission),
		})
	}
	sort.Slice(items, func(left int, right int) bool {
		return items[left].Name < items[right].Name
	})
	return items
}

func formatMCPToolList(items []mcpToolListItem) string {
	if len(items) == 0 {
		return "No MCP tools configured."
	}
	lines := []string{"MCP Tools:"}
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("  %s [%s/%s] - %s", item.Name, item.SideEffect, item.Permission, item.Description))
	}
	return strings.Join(lines, "\n")
}

func formatMCPServerList(servers map[string]config.MCPServerConfig) string {
	if len(servers) == 0 {
		return "No MCP servers configured."
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := []string{"MCP Servers:"}
	for _, name := range names {
		server := servers[name]
		state := "enabled"
		if server.Disabled {
			state = "disabled"
		}
		identity := strings.TrimSpace(server.Command)
		if identity == "" {
			identity = redactMCPURL(server.URL, "[REDACTED]")
		}
		lines = append(lines, fmt.Sprintf("  %s [%s] %s %s", name, server.Type, state, identity))
	}
	return strings.Join(lines, "\n")
}

func redactMCPURL(raw string, marker string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.User != nil {
		parsed.User = nil
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = redactMCPRawQuery(parsed.RawQuery, marker)
	}
	if parsed.Fragment != "" {
		parsed.Fragment = redactMCPRawQuery(parsed.Fragment, marker)
	}
	out := parsed.String()
	if strings.TrimSpace(out) == "" {
		return raw
	}
	return out
}

func redactMCPRawQuery(rawQuery string, marker string) string {
	parts := strings.Split(rawQuery, "&")
	for index, part := range parts {
		if part == "" {
			continue
		}
		key, _, hasValue := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		if !isSensitiveMCPDisplayKey(decodedKey) {
			continue
		}
		if hasValue {
			parts[index] = key + "=" + marker
		} else {
			parts[index] = key
		}
	}
	return strings.Join(parts, "&")
}

func isSensitiveMCPDisplayKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	if key == "key" {
		return true
	}
	for _, token := range []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "auth", "credential"} {
		if strings.Contains(key, token) {
			return true
		}
	}
	return false
}
