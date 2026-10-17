package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// richMCPTool mimics a real MCP tool's schema (Exa/Firecrawl-style search): an
// object with several typed, described properties. Real MCP tools are this size
// or larger, which is why shipping their full schemas every turn is expensive.
type richMCPTool struct{ name string }

func (t richMCPTool) Name() string { return t.name }
func (t richMCPTool) Description() string {
	return "Search the web and return ranked, structured results with titles, URLs, snippets, and optional cleaned full-page content. Supports natural-language and keyword queries with domain and date filtering."
}
func (t richMCPTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Required:             []string{"query"},
		Properties: map[string]tools.PropertySchema{
			"query":                {Type: "string", Description: "The search query to run against the web index. Supports natural-language and keyword queries."},
			"num_results":          {Type: "integer", Description: "Maximum number of results to return (1-100). Defaults to 10."},
			"include_domains":      {Type: "array", Description: "Only return results from these domains.", Items: &tools.PropertySchema{Type: "string"}},
			"exclude_domains":      {Type: "array", Description: "Never return results from these domains.", Items: &tools.PropertySchema{Type: "string"}},
			"start_published_date": {Type: "string", Description: "ISO-8601 date; only results published on or after this date are returned."},
			"end_published_date":   {Type: "string", Description: "ISO-8601 date; only results published on or before this date are returned."},
			"category":             {Type: "string", Description: "Restrict results to a content category.", Enum: []string{"news", "research paper", "company", "tweet", "pdf"}},
			"include_text":         {Type: "boolean", Description: "Include cleaned full-page text for each result (slower, larger payload)."},
		},
	}
}
func (t richMCPTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectNetwork, Permission: tools.PermissionAllow}
}
func (t richMCPTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}
func (t richMCPTool) Deferred() bool { return true }

// TestDeferralTokenSavingsMeasurement quantifies the win from the lowered
// deferThreshold default: with a typical small MCP server (6 tools), threshold 10
// (the OLD default) kept deferral OFF so all six full schemas shipped every turn,
// while threshold 3 (the NEW default) collapses them to compact reminder lines.
func TestDeferralTokenSavingsMeasurement(t *testing.T) {
	const mcpToolCount = 6
	registry := tools.NewRegistry()
	for i := 0; i < mcpToolCount; i++ {
		registry.Register(richMCPTool{name: fmt.Sprintf("mcp__websearch__search_v%d", i)})
	}
	registry.Register(fakeToolSearchTool{})

	measure := func(threshold int) (int, int) {
		defs, reminder := partitionTools(registry, PermissionModeAuto, Options{DeferThreshold: threshold}, map[string]bool{})
		encoded, err := json.Marshal(defs)
		if err != nil {
			t.Fatalf("marshal tool defs: %v", err)
		}
		bytes := len(encoded) + len(reminder)
		return bytes, bytes / 4 // ~tokens (approx 4 chars/token)
	}

	eagerBytes, eagerTok := measure(10) // 6 < 10 -> inactive -> all schemas eager
	deferBytes, deferTok := measure(3)  // 6 >= 3 -> active  -> compact reminder
	savedPct := 100 * float64(eagerBytes-deferBytes) / float64(eagerBytes)

	t.Logf("MCP tools in set: %d", mcpToolCount)
	t.Logf("threshold 10 (OLD, eager schemas): %d bytes (~%d tokens) per turn", eagerBytes, eagerTok)
	t.Logf("threshold  3 (NEW, deferred lines): %d bytes (~%d tokens) per turn", deferBytes, deferTok)
	t.Logf("SAVED: ~%d tokens per turn (%.0f%% smaller tool payload)", eagerTok-deferTok, savedPct)

	if deferBytes >= eagerBytes {
		t.Fatalf("deferral should shrink the tool payload: eager=%d defer=%d", eagerBytes, deferBytes)
	}
	if savedPct < 50 {
		t.Fatalf("expected a large reduction, got only %.0f%%", savedPct)
	}
}
