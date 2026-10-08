package agent

import (
	"strings"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type contextPlannerConfig struct {
	contextWindow  int
	promptCacheKey string
	serviceTier    string
	promptParts    systemPromptParts
}

// contextPlanner is the single seam for constructing model-visible requests.
// Planning is deterministic and does not retrieve content, execute tools, or
// change permissions. The initial implementation deliberately preserves every
// message and tool definition while making composition and cache drift
// inspectable; later selection policies must continue to cross this seam.
type contextPlanner struct {
	config          contextPlannerConfig
	previousPrefix  prefixFingerprint
	hasPrevious     bool
	toolSnapshotKey string
	toolSnapshot    []zeroruntime.ToolDefinition
}

type contextPlan struct {
	Request           zeroruntime.CompletionRequest
	Breakdown         ContextBreakdown
	PrefixFingerprint prefixFingerprint
}

func newContextPlanner(config contextPlannerConfig) *contextPlanner {
	return &contextPlanner{config: config}
}

// Plan returns a provider request snapshot plus content-free accounting.
// It intentionally performs no relevance filtering: preserving current model
// capability is the baseline contract for future planner policies.
func (planner *contextPlanner) Plan(messages []zeroruntime.Message, toolDefs []zeroruntime.ToolDefinition, reasoningEffort string) contextPlan {
	return planner.plan(messages, toolDefs, reasoningEffort, planner.config.promptParts)
}

// planWithPromptParts plans a request whose stable system sections differ from
// the planner's configured main-run prompt, such as a compaction summary call.
func (planner *contextPlanner) planWithPromptParts(messages []zeroruntime.Message, toolDefs []zeroruntime.ToolDefinition, reasoningEffort string, parts systemPromptParts) contextPlan {
	return planner.plan(messages, toolDefs, reasoningEffort, parts)
}

func (planner *contextPlanner) plan(messages []zeroruntime.Message, toolDefs []zeroruntime.ToolDefinition, reasoningEffort string, parts systemPromptParts) contextPlan {
	requestMessages := copyMessages(messages)
	requestSystemPrompt := leadingSystemContent(requestMessages)
	if parts.prompt != requestSystemPrompt {
		// Component boundaries from a different request would make the detailed
		// invalidation reason misleading. Retain the exact request prefix and
		// classify it conservatively as an unstructured system prompt instead.
		parts = systemPromptParts{prompt: requestSystemPrompt}
	}
	fingerprint := computePrefixFingerprint(buildPromptSubstringsFromParts(parts, toolDefs))
	request := zeroruntime.CompletionRequest{
		Messages:        requestMessages,
		Tools:           planner.snapshotTools(toolDefs, fingerprint),
		ReasoningEffort: reasoningEffort,
		ServiceTier:     planner.config.serviceTier,
		PromptCacheKey:  planner.config.promptCacheKey,
	}
	breakdown := MeasureContext(request.Messages, request.Tools, planner.config.contextWindow)
	breakdown.CompletePrefixHash = fingerprint.CompletePrefixHash
	var previous *prefixFingerprint
	if planner.hasPrevious {
		previous = &planner.previousPrefix
	}
	breakdown.PrefixInvalidationReason = explainPrefixChange(previous, fingerprint)
	planner.previousPrefix = fingerprint
	planner.hasPrevious = true
	return contextPlan{
		Request:           request,
		Breakdown:         breakdown,
		PrefixFingerprint: fingerprint,
	}
}

// snapshotTools freezes provider-visible tool definitions when their semantic
// fingerprint changes. Stable turns reuse the frozen schemas instead of
// recursively cloning every map and slice for every request.
func (planner *contextPlanner) snapshotTools(toolDefs []zeroruntime.ToolDefinition, fingerprint prefixFingerprint) []zeroruntime.ToolDefinition {
	key := fingerprint.ToolsHash + "\x00" + fingerprint.SchemaHash
	if planner.toolSnapshotKey != key {
		planner.toolSnapshot = copyToolDefinitions(toolDefs)
		planner.toolSnapshotKey = key
	}
	return planner.toolSnapshot
}

func copyToolDefinitions(toolDefs []zeroruntime.ToolDefinition) []zeroruntime.ToolDefinition {
	if toolDefs == nil {
		return nil
	}
	copied := make([]zeroruntime.ToolDefinition, len(toolDefs))
	for index, definition := range toolDefs {
		copied[index] = definition
		copied[index].Parameters = copySchemaMap(definition.Parameters)
		if definition.Format != nil {
			format := *definition.Format
			copied[index].Format = &format
		}
	}
	return copied
}

func copySchemaMap(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	copied := make(map[string]any, len(schema))
	for key, value := range schema {
		copied[key] = copySchemaValue(value)
	}
	return copied
}

func copySchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return copySchemaMap(typed)
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = copySchemaValue(item)
		}
		return copied
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func leadingSystemContent(messages []zeroruntime.Message) string {
	contents := make([]string, 0, 1)
	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleSystem {
			break
		}
		contents = append(contents, message.Content)
	}
	return strings.Join(contents, "\n\n")
}

func explainPrefixChange(previous *prefixFingerprint, current prefixFingerprint) string {
	if previous == nil {
		return "initial"
	}
	if previous.CompletePrefixHash == current.CompletePrefixHash {
		return "unchanged"
	}
	reasons := make([]string, 0, 3)
	if previous.SystemPromptHash != current.SystemPromptHash {
		parts := make([]string, 0, 4)
		if previous.BaseInstructionsHash != current.BaseInstructionsHash {
			parts = append(parts, "base_instructions")
		}
		if previous.ConfirmationPolicyHash != current.ConfirmationPolicyHash {
			parts = append(parts, "confirmation_policy")
		}
		if previous.ProjectContextHash != current.ProjectContextHash {
			parts = append(parts, "project_context")
		}
		if previous.SkillsHash != current.SkillsHash {
			parts = append(parts, "skills")
		}
		if len(parts) == 0 {
			parts = append(parts, "system_prompt")
		}
		reasons = append(reasons, parts...)
	}
	if previous.ToolsHash != current.ToolsHash {
		reasons = append(reasons, "tools")
	}
	if previous.SchemaHash != current.SchemaHash {
		reasons = append(reasons, "schema")
	}
	if len(reasons) == 0 {
		return "prefix_changed"
	}
	return strings.Join(reasons, ",")
}
