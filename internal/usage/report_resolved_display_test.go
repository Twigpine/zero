package usage

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/sessions"
)

func usageEventExtras(t *testing.T, sessionID string, sequence int, createdAt string, prompt, completion int, extra map[string]any) sessions.Event {
	t.Helper()
	payload := map[string]any{
		"promptTokens":     prompt,
		"completionTokens": completion,
		"totalTokens":      prompt + completion,
	}
	for k, v := range extra {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return sessions.Event{
		SessionID: sessionID,
		Sequence:  sequence,
		Type:      sessions.EventUsage,
		CreatedAt: createdAt,
		Payload:   raw,
	}
}

func assertNoResolvedModelsJSON(t *testing.T, report Report) {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if bytes.Contains(data, []byte(`"resolvedModels"`)) {
		t.Fatalf("JSON contains resolvedModels: %s", data)
	}
	if len(report.ResolvedModels) != 0 {
		t.Fatalf("ResolvedModels = %+v, want empty", report.ResolvedModels)
	}
}

func TestD2_1_20_BuildReportAggregatesResolvedModelsWithoutRegistryPrice(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"
	events := []sessions.Event{
		usageEventExtras(t, "s1", 1, day, 8, 2, map[string]any{
			"totalTokens":   10,
			"resolvedModel": "deepseek/deepseek-r1:free",
		}),
		usageEventExtras(t, "s1", 2, day, 4, 1, map[string]any{
			"totalTokens":   5,
			"resolvedModel": "deepseek/deepseek-r1:free",
		}),
	}
	meta := []sessions.Metadata{{SessionID: "s1", ModelID: "router/auto"}}

	report, err := BuildReport(events, meta, &registry, 10)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if report.Total.Requests != 2 {
		t.Fatalf("Total.Requests = %d, want 2", report.Total.Requests)
	}
	want := []ResolvedModelUsage{{
		RequestedModel: "router/auto",
		ResolvedModel:  "deepseek/deepseek-r1:free",
		Requests:       2,
		TotalTokens:    15,
	}}
	if !reflect.DeepEqual(report.ResolvedModels, want) {
		t.Fatalf("ResolvedModels = %+v, want %+v", report.ResolvedModels, want)
	}
}

func TestD2_1_21_BuildReportNothingToShow(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"
	baseMeta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{name: "absent", payload: nil},
		{name: "empty", payload: map[string]any{"resolvedModel": ""}},
		{name: "equal-requested", payload: map[string]any{"resolvedModel": "gpt-test"}},
		{name: "only-removable", payload: map[string]any{"resolvedModel": "\x00\u200b"}},
		{name: "split-nul", payload: map[string]any{"resolvedModel": "gpt-\x00test"}},
		{name: "split-bidi", payload: map[string]any{"resolvedModel": "gpt-\u202etest"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := tc.payload
			if extra == nil {
				extra = map[string]any{}
			}
			ev := usageEventExtras(t, "s1", 1, day, 10, 8, extra)
			report, err := BuildReport([]sessions.Event{ev}, baseMeta, &registry, 10)
			if err != nil {
				t.Fatalf("BuildReport: %v", err)
			}
			assertNoResolvedModelsJSON(t, report)
		})
	}
}

func TestD2_1_22_BuildReportSanitizesResolvedModelOnRead(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"

	t.Run("escaped-json", func(t *testing.T) {
		lit := "\u001b[31mx\u0085\u202ey"
		raw := []byte(`{"promptTokens":1,"completionTokens":1,"totalTokens":2,"resolvedModel":"\u001b[31mx\u0085\u202ey"}`)
		var payload usageEventPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if payload.ResolvedModel != lit {
			t.Fatalf("precondition ResolvedModel = %q", payload.ResolvedModel)
		}
		ev := sessions.Event{SessionID: "s1", Sequence: 1, Type: sessions.EventUsage, CreatedAt: day, Payload: raw}
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.ResolvedModels) != 1 || report.ResolvedModels[0].ResolvedModel != "[31mxy" {
			t.Fatalf("ResolvedModels = %+v, want resolved [31mxy", report.ResolvedModels)
		}
	})

	t.Run("raw-utf8", func(t *testing.T) {
		rawVal := "\x1b[31mx\u0085\u202ey"
		ev := usageEventExtras(t, "s1", 1, day, 1, 1, map[string]any{"resolvedModel": rawVal})
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.ResolvedModels) != 1 || report.ResolvedModels[0].ResolvedModel != "[31mxy" {
			t.Fatalf("ResolvedModels = %+v, want resolved [31mxy", report.ResolvedModels)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		sent := strings.Repeat("\u6a21", 1_000_000)
		ev := usageEventExtras(t, "s1", 1, day, 1, 1, map[string]any{"resolvedModel": sent})
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.ResolvedModels) != 1 {
			t.Fatalf("ResolvedModels = %+v, want one entry", report.ResolvedModels)
		}
		got := report.ResolvedModels[0].ResolvedModel
		if len(got) > 256 || len(got) != 255 {
			t.Fatalf("len(ResolvedModel) = %d, want 255", len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("ResolvedModel is not valid UTF-8")
		}
	})

	t.Run("requested-model-sanitized", func(t *testing.T) {
		ev := usageEventExtras(t, "s1", 1, day, 1, 1, map[string]any{"resolvedModel": "x/y"})
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt\u202e-test"}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.ResolvedModels) != 1 || report.ResolvedModels[0].RequestedModel != "gpt-test" {
			t.Fatalf("ResolvedModels = %+v, want RequestedModel gpt-test", report.ResolvedModels)
		}
	})
}

func TestD2_1_23_BuildReportPrecedenceAndMixedLines(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"

	t.Run("a-escalation-equals-resolved", func(t *testing.T) {
		ev := usageEventExtras(t, "s1", 1, day, 10, 8, map[string]any{
			"model":         "gpt-4.1",
			"resolvedModel": "gpt-4.1",
		})
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		assertNoResolvedModelsJSON(t, report)
	})

	t.Run("b-escalation-differs", func(t *testing.T) {
		ev := usageEventExtras(t, "s1", 1, day, 10, 8, map[string]any{
			"model":         "gpt-4.1",
			"resolvedModel": "x",
		})
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.ResolvedModels) != 1 || report.ResolvedModels[0].RequestedModel != "gpt-4.1" {
			t.Fatalf("ResolvedModels = %+v, want RequestedModel gpt-4.1", report.ResolvedModels)
		}
	})

	t.Run("c-mixed-bucket", func(t *testing.T) {
		events := []sessions.Event{
			usageEvent(t, "s1", 1, day, 10, 8),
			usageEvent(t, "s1", 2, day, 5, 4),
			usageEventExtras(t, "s1", 3, day, 3, 2, map[string]any{"resolvedModel": "x/y"}),
		}
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: "gpt-test"}}
		report, err := BuildReport(events, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.Buckets) != 1 || report.Buckets[0].Requests != 3 {
			t.Fatalf("Buckets = %+v, want one bucket with 3 requests", report.Buckets)
		}
		if len(report.ResolvedModels) != 1 || report.ResolvedModels[0].Requests != 1 {
			t.Fatalf("ResolvedModels = %+v, want one entry with Requests 1", report.ResolvedModels)
		}
	})

	t.Run("d-stable-sort", func(t *testing.T) {
		specs := []struct {
			session, model, resolved string
			tokens                   int
		}{
			{"s1", "gpt-test", "x/y", 11},
			{"s2", "gpt-test", "deepseek/a", 22},
			{"s3", "a-model", "z", 33},
		}
		build := func(reverse bool) (Report, error) {
			var events []sessions.Event
			var meta []sessions.Metadata
			order := specs
			if reverse {
				order = []struct {
					session, model, resolved string
					tokens                   int
				}{specs[2], specs[1], specs[0]}
			}
			seq := 1
			for _, s := range order {
				events = append(events, usageEventExtras(t, s.session, seq, day, s.tokens/2, s.tokens-s.tokens/2, map[string]any{
					"totalTokens":   s.tokens,
					"resolvedModel": s.resolved,
				}))
				meta = append(meta, sessions.Metadata{SessionID: s.session, ModelID: s.model})
				seq++
			}
			return BuildReport(events, meta, &registry, 10)
		}
		forward, err := build(false)
		if err != nil {
			t.Fatalf("BuildReport forward: %v", err)
		}
		backward, err := build(true)
		if err != nil {
			t.Fatalf("BuildReport backward: %v", err)
		}
		if !reflect.DeepEqual(forward.ResolvedModels, backward.ResolvedModels) {
			t.Fatalf("order changed result:\nforward %+v\nbackward %+v", forward.ResolvedModels, backward.ResolvedModels)
		}
		want := []ResolvedModelUsage{
			{RequestedModel: "a-model", ResolvedModel: "z", Requests: 1, TotalTokens: 33},
			{RequestedModel: "gpt-test", ResolvedModel: "deepseek/a", Requests: 1, TotalTokens: 22},
			{RequestedModel: "gpt-test", ResolvedModel: "x/y", Requests: 1, TotalTokens: 11},
		}
		if !reflect.DeepEqual(forward.ResolvedModels, want) {
			t.Fatalf("ResolvedModels = %+v, want %+v", forward.ResolvedModels, want)
		}
	})

	t.Run("e-empty-requested", func(t *testing.T) {
		ev := usageEventExtras(t, "s1", 1, day, 5, 4, map[string]any{"resolvedModel": "x"})
		meta := []sessions.Metadata{{SessionID: "s1", ModelID: ""}}
		report, err := BuildReport([]sessions.Event{ev}, meta, &registry, 10)
		if err != nil {
			t.Fatalf("BuildReport: %v", err)
		}
		if len(report.ResolvedModels) != 1 || report.ResolvedModels[0].RequestedModel != "" {
			t.Fatalf("ResolvedModels = %+v, want empty RequestedModel", report.ResolvedModels)
		}
	})
}

func TestD2_1_24_BuildReportTotalsUnchangedWithResolvedModelKey(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	const day = "2026-06-01T09:00:00Z"
	netLOC := 42

	// Event sets covering D2.1-20 through D2.1-23 scenarios.
	withResolved := []sessions.Event{
		usageEventExtras(t, "r1", 1, day, 8, 2, map[string]any{"totalTokens": 10, "resolvedModel": "deepseek/deepseek-r1:free"}),
		usageEventExtras(t, "r1", 2, day, 4, 1, map[string]any{"totalTokens": 5, "resolvedModel": "deepseek/deepseek-r1:free"}),
		usageEventExtras(t, "g1", 1, day, 10, 8, map[string]any{"resolvedModel": "gpt-test"}),
		usageEventExtras(t, "g2", 1, day, 1, 1, map[string]any{"resolvedModel": "\u001b[31mx\u0085\u202ey"}),
		usageEventExtras(t, "e1", 1, day, 10, 8, map[string]any{"model": "gpt-4.1", "resolvedModel": "x"}),
		usageEventExtras(t, "m1", 1, day, 10, 8, nil),
		usageEventExtras(t, "m1", 2, day, 5, 4, nil),
		usageEventExtras(t, "m1", 3, day, 3, 2, map[string]any{"resolvedModel": "x/y"}),
		usageEventExtras(t, "u1", 1, day, 5, 4, map[string]any{"resolvedModel": "x"}),
	}
	meta := []sessions.Metadata{
		{SessionID: "r1", ModelID: "router/auto"},
		{SessionID: "g1", ModelID: "gpt-test"},
		{SessionID: "g2", ModelID: "gpt-test"},
		{SessionID: "e1", ModelID: "gpt-test"},
		{SessionID: "m1", ModelID: "gpt-test"},
		{SessionID: "u1", ModelID: ""},
	}

	stripResolved := func(events []sessions.Event) []sessions.Event {
		out := make([]sessions.Event, len(events))
		for i, ev := range events {
			out[i] = ev
			if len(ev.Payload) == 0 {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			delete(payload, "resolvedModel")
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			out[i].Payload = raw
		}
		return out
	}

	with, err := BuildReport(withResolved, meta, &registry, netLOC)
	if err != nil {
		t.Fatalf("BuildReport with: %v", err)
	}
	plain, err := BuildReport(stripResolved(withResolved), meta, &registry, netLOC)
	if err != nil {
		t.Fatalf("BuildReport plain: %v", err)
	}

	if with.Total != plain.Total {
		t.Fatalf("Total differs: with %+v plain %+v", with.Total, plain.Total)
	}
	if len(with.Buckets) != len(plain.Buckets) {
		t.Fatalf("bucket count %d vs %d", len(with.Buckets), len(plain.Buckets))
	}
	for i := range with.Buckets {
		if with.Buckets[i] != plain.Buckets[i] {
			t.Fatalf("bucket[%d] with %+v plain %+v", i, with.Buckets[i], plain.Buckets[i])
		}
	}
	if with.NetLOC != plain.NetLOC || with.NetLOCPositive != plain.NetLOCPositive ||
		with.TokensPerNetLOC != plain.TokensPerNetLOC || with.CostPerNetLOC != plain.CostPerNetLOC ||
		with.LOCEstimated != plain.LOCEstimated || with.CostEstimated != plain.CostEstimated {
		t.Fatalf("LOC/cost fields differ:\nwith: %+v\nplain: %+v", with, plain)
	}
}
