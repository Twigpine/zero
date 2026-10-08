package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ToolSearchToolName is the canonical registry name of the deferred-tool loader.
// It is exported so the agent loop, CLI filter validation, and the partition can
// reference the loader without re-stating the literal (keeping all the gates that
// must treat it specially in agreement).
const ToolSearchToolName = "tool_search"

// toolSearchMaxKeywordMatches caps how many deferred tools a bare-keyword query
// loads in one call, keeping the returned schemas bounded.
const toolSearchMaxKeywordMatches = 10

// toolSearchTool lets the model pull a deferred tool's full schema on demand.
// It holds the live registry (like escalate_model holds the model registry) so
// it can resolve names against the currently registered deferred-eligible tools.
type toolSearchTool struct {
	baseTool
	registry *Registry
}

// NewToolSearchTool builds the tool_search tool over the given registry. The
// tool is informational/no-side-effect and is advertised even in auto mode so
// the model can always discover withheld tools.
func NewToolSearchTool(registry *Registry) Tool {
	return toolSearchTool{
		baseTool: baseTool{
			name:        ToolSearchToolName,
			description: BuildToolSearchDescription(nil),
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"query": {
						Type:        "string",
						Description: "Either \"select:Name1,Name2\" for exact tool names, or space-separated keywords to match tool names and descriptions.",
					},
				},
				Required:             []string{"query"},
				AdditionalProperties: false,
			},
			safety: Safety{
				SideEffect:      SideEffectNone,
				Permission:      PermissionAllow,
				Reason:          "Lists and loads already-registered tool schemas; performs no side effects.",
				AdvertiseInAuto: true,
			},
			// Reads registry schemas only; no workspace mutation.
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: false},
		},
		registry: registry,
	}
}

func (tool toolSearchTool) cloneForRegistry(registry *Registry) Tool {
	tool.registry = registry
	return tool
}

// Run satisfies the Tool interface; actual dispatch goes through RunWithOptions.
func (tool toolSearchTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.RunWithOptions(ctx, args, RunOptions{})
}

func (tool toolSearchTool) RunWithOptions(_ context.Context, args map[string]any, options RunOptions) Result {
	query, err := stringArg(args, "query", "", true)
	if err != nil {
		return errorResult("Error: Invalid arguments for tool_search: " + err.Error())
	}
	query = strings.TrimSpace(query)

	// Honor the run's operator filters AND the run's permission-mode visibility
	// so an operator-hidden OR mode-hidden deferred tool is invisible to
	// tool_search: it never resolves via select:, never ranks for a keyword
	// query, and is omitted from the no-match listing. The mode filter matters
	// even though tool_search itself is denied at dispatch in plan/spec-draft
	// today (its own Safety carries no side effect, so it fails the same
	// advertisement gate) — it is defense-in-depth so a deferred write/mutator
	// tool's name, description, and schema can never leak through this loader
	// if that outer gate is ever loosened independently of this one.
	deferred := tool.visibleDeferredTools(options.EnabledTools, options.DisabledTools, options.PermissionMode)

	var matches []Tool
	if rest, ok := strings.CutPrefix(query, "select:"); ok {
		matches = tool.resolveExact(rest, deferred)
	} else {
		matches = tool.rankByKeyword(query, deferred)
	}

	// Names the model asked for that it ALREADY has eagerly. tool_search only
	// loads DEFERRED tools, so a request for an eager tool (e.g. Task or
	// update_plan) otherwise falls through to a misleading "no tools matched" —
	// a common confusion that leaves weaker models looping. Redirect the model to
	// call those tools directly instead.
	alreadyAvailable := tool.eagerToolsForQuery(query, options.EnabledTools, options.DisabledTools, options.PermissionMode)

	if len(matches) == 0 {
		if len(alreadyAvailable) > 0 {
			return okResult(alreadyAvailableMessage(alreadyAvailable))
		}
		return okResult(tool.noMatchMessage(query, deferred))
	}

	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match.Name())
	}
	output := renderLoadedTools(matches)
	if len(alreadyAvailable) > 0 {
		output += "\n\n" + alreadyAvailableMessage(alreadyAvailable)
	}
	return Result{
		Status: StatusOK,
		Output: output,
		Meta:   map[string]string{"load_tools": strings.Join(names, ",")},
	}
}

// visibleDeferredTools returns the registry's deferred-eligible tools that pass
// the operator allow/deny filters and the run's permission-mode visibility,
// sorted by name so keyword ranking and listings stay deterministic. A
// nil/empty filter pair admits every deferred tool on the operator axis (the
// pre-filter behavior); permissionMode "" admits every deferred tool on the
// mode axis (matches every mode but plan/spec-draft, which never restricted
// tool_search's candidates before this filter existed). The allow/deny
// semantics mirror the agent's ToolAllowedByFilters: denied if in disabled; if
// enabled is non-empty, the tool must be listed in it.
func (tool toolSearchTool) visibleDeferredTools(enabled []string, disabled []string, permissionMode string) []Tool {
	var deferred []Tool
	if tool.registry != nil {
		for _, candidate := range tool.registry.All() {
			if !ModelVisible(candidate) {
				continue
			}
			if !IsDeferred(candidate) {
				continue
			}
			if !toolAllowedByFilters(candidate.Name(), enabled, disabled) {
				continue
			}
			if !toolAdvertisedForPermissionMode(candidate, permissionMode) {
				continue
			}
			deferred = append(deferred, candidate)
		}
	}
	sort.Slice(deferred, func(left, right int) bool {
		return deferred[left].Name() < deferred[right].Name()
	})
	return deferred
}

// visibleEagerToolNames returns the names of tools the model ALREADY has in its
// list this run: registered, NOT deferred, passing the operator filters and the
// run's permission-mode visibility, and not tool_search itself. These never
// need (and never resolve through) tool_search.
func (tool toolSearchTool) visibleEagerToolNames(enabled []string, disabled []string, permissionMode string) map[string]bool {
	names := map[string]bool{}
	if tool.registry == nil {
		return names
	}
	for _, candidate := range tool.registry.All() {
		if !ModelVisible(candidate) {
			continue
		}
		if IsDeferred(candidate) || candidate.Name() == ToolSearchToolName {
			continue
		}
		if !toolAllowedByFilters(candidate.Name(), enabled, disabled) {
			continue
		}
		if !toolAdvertisedForPermissionMode(candidate, permissionMode) {
			continue
		}
		names[candidate.Name()] = true
	}
	return names
}

// eagerToolsForQuery returns, sorted, the already-available (eager) tool names a
// tool_search query refers to: exact names for a "select:" query, or eager tools
// whose name matches a keyword otherwise. Used to steer the model back to calling
// a tool it already has instead of fruitlessly searching for it.
func (tool toolSearchTool) eagerToolsForQuery(query string, enabled []string, disabled []string, permissionMode string) []string {
	eager := tool.visibleEagerToolNames(enabled, disabled, permissionMode)
	if len(eager) == 0 {
		return nil
	}
	hit := map[string]bool{}
	if rest, ok := strings.CutPrefix(query, "select:"); ok {
		for _, raw := range strings.Split(rest, ",") {
			if name := strings.TrimSpace(raw); name != "" && eager[name] {
				hit[name] = true
			}
		}
	} else {
		keywords := strings.Fields(strings.ToLower(query))
		for name := range eager {
			lower := strings.ToLower(name)
			for _, keyword := range keywords {
				if strings.Contains(lower, keyword) {
					hit[name] = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(hit))
	for name := range hit {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// alreadyAvailableMessage tells the model the named tools are already in its list
// and must be called directly — tool_search is only for deferred tools.
func alreadyAvailableMessage(names []string) string {
	subject, verb := names[0], "is"
	if len(names) > 1 {
		subject, verb = strings.Join(names, ", "), "are"
	}
	return subject + " " + verb + " already in your tool list — call " +
		map[bool]string{true: "them", false: "it"}[len(names) > 1] +
		" directly. tool_search only loads deferred tools, not tools you already have."
}

// toolAllowedByFilters mirrors agent.ToolAllowedByFilters (kept here to avoid an
// import cycle: the agent package imports tools, not the other way around). A
// name is denied when it appears in disabled; when enabled is non-empty, the
// name must appear in it to be allowed.
func toolAllowedByFilters(name string, enabled []string, disabled []string) bool {
	if len(enabled) > 0 && !containsName(enabled, name) {
		return false
	}
	return !containsName(disabled, name)
}

// PlanMode and SpecDraftMode are the string values of agent.PermissionModePlan
// and agent.PermissionModeSpecDraft. RunOptions.PermissionMode is already a
// plain string (set from agent.PermissionMode via string(permissionMode)), so
// this package owns the literals rather than importing the agent type (which
// would create an import cycle). agent.ToolAdvertised delegates plan/spec-draft
// decisions here so tool_search and the dispatch gate stay identical.
const (
	PlanMode      = "plan"
	SpecDraftMode = "spec-draft"
)

// ToolAdvertisedForPermissionMode is the single source of truth for whether a
// tool may be advertised under a given permission mode string. agent.ToolAdvertised
// calls this for plan/spec-draft after its own PermissionDeny short-circuit;
// tool_search applies it to deferred candidates so a mutator cannot leak through
// load_tools. Modes other than plan/spec-draft only apply the PermissionDeny
// gate here (auto/member-auto/ask/unsafe advertisement rules stay in agent).
func ToolAdvertisedForPermissionMode(tool Tool, permissionMode string) bool {
	// A denied tool is never advertised in any mode (mirrors agent.ToolAdvertised).
	if tool.Safety().Permission == PermissionDeny {
		return false
	}
	switch permissionMode {
	case PlanMode:
		if tool.Name() == "lsp_navigate" {
			return false
		}
		safety := tool.Safety()
		return safety.SideEffect == SideEffectRead && safety.Permission == PermissionAllow
	case SpecDraftMode:
		// Never trust control-tool names alone (a re-registered spoof can carry
		// arbitrary Safety).
		safety := tool.Safety()
		switch tool.Name() {
		case "update_plan":
			return false
		case "ask_user":
			return safety.SideEffect == SideEffectRead && safety.Permission == PermissionAllow
		case "submit_spec":
			return safety.SideEffect == SideEffectWrite && safety.Permission == PermissionAllow
		}
		return safety.SideEffect == SideEffectRead && safety.Permission == PermissionAllow
	default:
		return true
	}
}

// toolAdvertisedForPermissionMode is the unexported name used by tool_search
// internals; keep it as a thin alias so call sites stay short.
func toolAdvertisedForPermissionMode(tool Tool, permissionMode string) bool {
	return ToolAdvertisedForPermissionMode(tool, permissionMode)
}

func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// resolveExact maps a comma-separated name list (the part after "select:") to
// the matching deferred tools, preserving the model's order and skipping blanks
// and unknown names.
func (tool toolSearchTool) resolveExact(list string, deferred []Tool) []Tool {
	byName := make(map[string]Tool, len(deferred))
	for _, candidate := range deferred {
		byName[candidate.Name()] = candidate
	}
	var matches []Tool
	seen := make(map[string]bool)
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		if candidate, ok := byName[name]; ok {
			seen[name] = true
			matches = append(matches, candidate)
		}
	}
	return matches
}

// rankByKeyword scores deferred tools by case-insensitive substring match on the
// name (weighted higher) then the description, and returns the top matches.
func (tool toolSearchTool) rankByKeyword(query string, deferred []Tool) []Tool {
	keywords := strings.Fields(strings.ToLower(query))
	if len(keywords) == 0 {
		return nil
	}
	type scored struct {
		tool  Tool
		score int
		order int
	}
	var ranked []scored
	for index, candidate := range deferred {
		name := strings.ToLower(candidate.Name())
		desc := strings.ToLower(candidate.Description())
		nameSquashed := squashSeparators(name)
		score := 0
		for _, keyword := range keywords {
			switch {
			case strings.Contains(name, keyword):
				score += 2
			case strings.Contains(nameSquashed, squashSeparators(keyword)):
				// Separator-insensitive fallback: "webfetch" matches "web_fetch".
				// Ranked below an exact substring so precise queries still win.
				score++
			}
			if strings.Contains(desc, keyword) {
				score++
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{tool: candidate, score: score, order: index})
		}
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].score != ranked[right].score {
			return ranked[left].score > ranked[right].score
		}
		return ranked[left].order < ranked[right].order
	})
	if len(ranked) > toolSearchMaxKeywordMatches {
		ranked = ranked[:toolSearchMaxKeywordMatches]
	}
	matches := make([]Tool, 0, len(ranked))
	for _, entry := range ranked {
		matches = append(matches, entry.tool)
	}
	return matches
}

// squashSeparators drops separators so "web_fetch", "web-fetch", and "web fetch"
// all normalize to "webfetch". It lets a query missing the separators still match
// a tool name, the common way a model mistypes a tool ("webfetch").
func squashSeparators(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', ' ', '.':
			return -1
		}
		return r
	}, s)
}

// noMatchMessage reports that nothing loaded and names the available deferred
// tools so the model can retry with a valid select: query.
func (tool toolSearchTool) noMatchMessage(query string, deferred []Tool) string {
	if len(deferred) == 0 {
		return "No deferred tools are available to load."
	}
	names := make([]string, 0, len(deferred))
	for _, candidate := range deferred {
		names = append(names, candidate.Name())
	}
	return "No tools matched \"" + query + "\". Available tools: " + strings.Join(names, ", ") +
		`. Retry with query "select:Name1,Name2" using exact names.`
}

// renderLoadedTools lists each loaded tool's name, full description, and full
// input schema (pretty-printed JSON) so the model has the complete spec inline.
func renderLoadedTools(matches []Tool) string {
	var builder strings.Builder
	builder.WriteString("Loaded ")
	if len(matches) == 1 {
		builder.WriteString("1 tool")
	} else {
		builder.WriteString(strconv.Itoa(len(matches)))
		builder.WriteString(" tools")
	}
	builder.WriteString(". Full schemas follow; call them on the next turn.\n")
	for _, match := range matches {
		builder.WriteString("\n## ")
		builder.WriteString(match.Name())
		builder.WriteString("\n")
		builder.WriteString(match.Description())
		builder.WriteString("\n")
		schemaJSON, err := json.MarshalIndent(match.Parameters(), "", "  ")
		if err != nil {
			builder.WriteString("(schema unavailable)\n")
			continue
		}
		builder.WriteString("input schema:\n")
		builder.Write(schemaJSON)
		builder.WriteString("\n")
	}
	return builder.String()
}
