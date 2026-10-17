package cli

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/specmode"
	"github.com/Twigpine/zero/internal/tools"
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func parseToolList(value string) []string {
	seen := map[string]bool{}
	tools := []string{}
	for _, name := range strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == ' ' || char == '\t' || char == '\n' || char == '\r'
	}) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tools = append(tools, name)
	}
	return tools
}

func validateExecToolFilters(options execOptions, registry *tools.Registry) error {
	for _, name := range append(append([]string{}, options.enabledTools...), options.disabledTools...) {
		if !toolNamePattern.MatchString(name) {
			return execUsageError{fmt.Sprintf("Invalid tool name %q.", name)}
		}
		// tool_search is registered later (only when deferral activates), so it is
		// not in the registry yet at validation time. Treat it as always-valid so an
		// operator can list it in --enabled-tools / --disabled-tools without a
		// spurious "Unknown tool" error.
		if _, ok := registry.Get(name); !ok && name != tools.ToolSearchToolName {
			return execUsageError{fmt.Sprintf("Unknown tool: %s", name)}
		}
	}
	disabled := map[string]bool{}
	for _, name := range options.disabledTools {
		disabled[name] = true
	}
	for _, name := range options.enabledTools {
		if disabled[name] {
			return execUsageError{fmt.Sprintf("Tool cannot be both enabled and disabled: %s", name)}
		}
	}
	if options.useSpec {
		if disabled[specmode.SubmitToolName] {
			return execUsageError{"--use-spec requires submit_spec; remove it from --disabled-tools."}
		}
		if len(options.enabledTools) > 0 && !toolListContains(options.enabledTools, specmode.SubmitToolName) {
			return execUsageError{"--use-spec requires submit_spec; include it in --enabled-tools."}
		}
	}
	return nil
}

func toolListContains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func resolveExecPermissionMode(options execOptions) (agent.PermissionMode, error) {
	if pm := strings.ToLower(strings.TrimSpace(options.permissionMode)); pm != "" {
		switch pm {
		case "plan":
			return agent.PermissionModePlan, nil
		case "spec-draft", "spec_draft":
			return agent.PermissionModeSpecDraft, nil
		case "auto":
			return agent.PermissionModeAuto, nil
		case "member", "member-auto", "member_auto":
			return agent.PermissionModeMemberAuto, nil
		case "ask":
			return agent.PermissionModeAsk, nil
		case "unsafe", "high":
			return agent.PermissionModeUnsafe, nil
		default:
			return "", execUsageError{fmt.Sprintf("Invalid permission mode %q. Expected plan, spec-draft, auto, member, ask, or unsafe.", options.permissionMode)}
		}
	}
	// Validate --auto first, regardless of --skip-permissions-unsafe, so an
	// invalid autonomy value is always rejected. (Previously the unsafe path
	// short-circuited before validation, letting "--auto bogus" slip through
	// whenever --skip-permissions-unsafe was also set.)
	var mode agent.PermissionMode
	switch strings.ToLower(strings.TrimSpace(options.autonomy)) {
	case "", "low", "medium":
		mode = agent.PermissionModeAuto
	case "member":
		// Internal autonomy for headless swarm/specialist members: Auto plus
		// advertised in-workspace mutators (see PermissionModeMemberAuto). The
		// swarm launcher sets this; it is not part of the public low|medium|high set.
		mode = agent.PermissionModeMemberAuto
	case "high":
		mode = agent.PermissionModeUnsafe
	default:
		return "", execUsageError{fmt.Sprintf("Invalid autonomy level %q. Expected low, medium, or high.", options.autonomy)}
	}
	if options.skipPermissionsUnsafe {
		return agent.PermissionModeUnsafe, nil
	}
	return mode, nil
}

func writeExecToolList(w io.Writer, registry *tools.Registry, options execOptions, permissionMode agent.PermissionMode) error {
	_, err := fmt.Fprintln(w, formatExecToolList(registry, options, permissionMode))
	return err
}

// writeExecToolListJSON emits the visible tool list as a single JSON object so
// `exec --list-tools -o json` honors the requested machine-readable format
// instead of falling through to the human-readable text listing.
func writeExecToolListJSON(w io.Writer, registry *tools.Registry, options execOptions, permissionMode agent.PermissionMode) error {
	visible := visibleExecTools(registry, options, permissionMode)
	infos := make([]map[string]any, 0, len(visible))
	for _, tool := range visible {
		safety := tool.Safety()
		infos = append(infos, map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"side_effect": string(safety.SideEffect),
			"permission":  string(safety.Permission),
		})
	}
	return writeJSONLine(w, map[string]any{
		"type":  "tools",
		"tools": infos,
	})
}

func formatExecToolList(registry *tools.Registry, options execOptions, permissionMode agent.PermissionMode) string {
	visible := visibleExecTools(registry, options, permissionMode)
	lines := []string{"Tools visible to model:"}
	for _, tool := range visible {
		safety := tool.Safety()
		lines = append(lines, fmt.Sprintf("  %s [%s/%s] - %s", tool.Name(), safety.SideEffect, safety.Permission, tool.Description()))
	}
	if len(visible) == 0 {
		lines = append(lines, "  (none)")
	}
	return strings.Join(lines, "\n")
}

func visibleExecTools(registry *tools.Registry, options execOptions, permissionMode agent.PermissionMode) []tools.Tool {
	all := registry.All()
	visible := []tools.Tool{}
	for _, tool := range all {
		if !agent.ToolVisible(tool, permissionMode, options.enabledTools, options.disabledTools) {
			continue
		}
		visible = append(visible, tool)
	}
	sort.Slice(visible, func(i, j int) bool {
		return visible[i].Name() < visible[j].Name()
	})
	return visible
}
