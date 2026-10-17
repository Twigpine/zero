package tools

import "github.com/Twigpine/zero/internal/localcontrol"

// BuiltinCatalog returns every built-in tool that Zero can expose to the model
// from this package (core + control + optional local-control helpers).
//
// It is the source of truth for metadata coverage tests: every tool returned
// here must declare an explicit Effect other than EffectUnknown.
//
// Not included (fail-closed EffectUnknown by design):
//   - MCP tools (internal/mcp registryTool)
//   - Plugin tools (internal/plugins pluginTool)
//   - Specialist tools registered outside this package (Task, Generate, …)
//
// workspaceRoot is used only to construct path-scoped tools; it need not exist.
func BuiltinCatalog(workspaceRoot string) []Tool {
	if workspaceRoot == "" {
		workspaceRoot = "."
	}
	var all []Tool
	all = append(all, CoreReadOnlyToolsScoped(workspaceRoot, nil)...)
	all = append(all, CoreWriteToolsScoped(workspaceRoot, nil)...)
	all = append(all, CoreShellToolsScoped(workspaceRoot, nil)...)
	// Network tools: always include web_fetch; always include web_search for
	// classification even when no backend is configured at runtime (registration
	// remains conditional in CoreNetworkTools).
	all = append(all, NewWebFetchTool(), NewWebSearchTool())
	all = append(all, NewEscalateModelTool())

	// tool_search needs a live registry of the tools above.
	reg := NewRegistry()
	for _, tool := range all {
		reg.Register(tool)
	}
	search := NewToolSearchTool(reg)
	reg.Register(search)
	all = append(all, search)

	// Local-control tools are feature-gated at registration time but always
	// exist as built-in implementations with fixed classifications.
	all = append(all, NewLocalBrowserTools(localcontrol.BrowserOptions{})...)
	all = append(all, NewLocalDesktopTools(localcontrol.DesktopOptions{})...)
	all = append(all, NewLocalTerminalTools(localcontrol.TerminalOptions{})...)
	all = append(all, NewLocalControlArtifactTools(LocalControlArtifactOptions{})...)
	return all
}

// ValidateBuiltinCatalog returns diagnostics for every catalog tool that is
// missing or inconsistently classified. Empty means full coverage.
//
// Duplicate builtin names are always rejected: ResourceKeys is a function and
// cannot be compared, so partial metadata comparison would miss conflict-scope
// drift between two same-name tools.
//
// Validation uses constructor-declared capabilities (before ThreadSafe
// normalization) so a mutator wired with ThreadSafe=true is still reported.
// Runtime callers continue to use CapabilitiesOf, which normalizes.
func ValidateBuiltinCatalog(workspaceRoot string) []string {
	var problems []string
	seen := map[string]struct{}{}
	for _, tool := range BuiltinCatalog(workspaceRoot) {
		name := tool.Name()
		if _, ok := seen[name]; ok {
			problems = append(problems, name+": duplicate builtin name in catalog")
			continue
		}
		seen[name] = struct{}{}

		// Runtime view: must not remain Unknown after normalization.
		runtimeCaps := CapabilitiesOf(tool)
		if runtimeCaps.Effect == EffectUnknown {
			problems = append(problems, name+": built-in tool remains EffectUnknown (metadata required)")
		}

		// Declaration view: catch invalid ThreadSafe on mutators that
		// CapabilitiesOf would silently clear.
		declared := declaredCapabilitiesOf(tool)
		problems = append(problems, ValidateCapabilities(name, declared)...)
	}
	return problems
}

// declaredCapabilitiesOf returns constructor metadata when available; otherwise
// falls back to CapabilitiesOf (already normalized).
func declaredCapabilitiesOf(tool Tool) ToolCapabilities {
	if tool == nil {
		return UnknownCapabilities()
	}
	if d, ok := tool.(declaredCapabilityProvider); ok {
		return d.declaredCapabilities()
	}
	return CapabilitiesOf(tool)
}
