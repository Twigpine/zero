package usage

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/sessions"
)

// The optional `resolvedModel` key is attribution only. A stored event whose
// key is not a string (an edited or foreign events.jsonl) must not discard the
// whole report: the key reads as absent and the token counts still add up.
func TestBuildReportNonStringResolvedModelReadsAsAbsent(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"
	meta := []sessions.Metadata{{SessionID: "s1", ModelID: "router/auto"}}
	for _, value := range []any{map[string]any{}, []any{"a"}, 17, 1.5, true, nil} {
		ev := usageEventExtras(t, "s1", 1, day, 8, 2, map[string]any{"resolvedModel": value})
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("resolvedModel=%#v: BuildReport: %v", value, err)
		}
		if report.Total.Requests != 1 || report.Total.TotalTokens != 10 {
			t.Fatalf("resolvedModel=%#v: total = %+v, want 1 request and 10 tokens", value, report.Total)
		}
		assertNoResolvedModelsJSON(t, report)
	}
}

// Two requested ids that share their first 256 bytes sanitize to the same
// label. Both must stay in separate groups, since pricing sees them apart.
func TestBuildReportKeepsDistinctRequestedModelsSharingCappedPrefix(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"
	prefix := strings.Repeat("a", 256)
	meta := []sessions.Metadata{
		{SessionID: "s1", ModelID: prefix + "1"},
		{SessionID: "s2", ModelID: prefix + "2"},
	}
	events := []sessions.Event{
		usageEventExtras(t, "s1", 1, day, 8, 2, map[string]any{"resolvedModel": "x/y"}),
		usageEventExtras(t, "s2", 1, day, 4, 1, map[string]any{"resolvedModel": "x/y"}),
	}
	report, err := BuildReport(events, meta, &registry, 10)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(report.ResolvedModels) != 2 {
		t.Fatalf("ResolvedModels = %+v, want two separate groups", report.ResolvedModels)
	}
	for _, entry := range report.ResolvedModels {
		if entry.Requests != 1 {
			t.Fatalf("group %+v merged requests, want 1 each", entry)
		}
	}
}
