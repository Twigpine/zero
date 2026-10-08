package providermodelcatalog

import (
	"reflect"
	"testing"

	"github.com/Gitlawb/zero/internal/providercatalog"
)

func TestModelsAreProviderScoped(t *testing.T) {
	tests := []struct {
		provider string
		want     []string
		notWant  []string
	}{
		{
			provider: "ollama-cloud",
			want:     []string{"qwen3-coder:480b", "gpt-oss:120b"},
			notWant:  []string{"llama3.1", "gpt-4.1", "openai/gpt-4.1"},
		},
		{
			provider: "ollama",
			want:     []string{"llama3.1", "qwen2.5-coder:32b"},
			notWant:  []string{"qwen3-coder:480b", "gpt-4.1", "gpt-5", "openai/gpt-4.1"},
		},
		{
			provider: "groq",
			want:     []string{"llama-3.3-70b-versatile", "openai/gpt-oss-120b"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5"},
		},
		{
			provider: "fireworks",
			want:     []string{"accounts/fireworks/models/kimi-k2p7-code", "accounts/fireworks/models/deepseek-v4-flash"},
			notWant:  []string{"gpt-4.1", "llama-3.3-70b-versatile"},
		},
		{
			provider: "chatgpt",
			want:     []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"},
			notWant:  []string{"gpt-5", "gpt-4.1", "openai/gpt-4.1"},
		},
		{
			provider: "aimlapi",
			want:     []string{"anthropic/claude-sonnet-5", "google/gemini-3.5-flash", "openai/gpt-5.5-2026-04-23", "qwen/qwen-3.7-max", "deepseek/deepseek-v4-pro"},
			notWant:  []string{"gpt-4o", "openai/gpt-4o", "anthropic/claude-opus-4.8", "x-ai/grok-4-5"},
		},
		{
			provider: "atlascloud",
			want:     []string{"qwen/qwen3.5-flash", "deepseek-ai/deepseek-v4-pro"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5", "openai/gpt-4.1"},
		},
		{
			provider: "mistral",
			want:     []string{"mistral-large-latest", "codestral-latest"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5"},
		},
		{
			provider: "minimaxi-cn",
			want:     []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.1"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5"},
		},
		{
			provider: "zai-cn",
			want:     []string{"glm-4.5", "glm-4.6", "glm-4.5-air", "glm-z1-air"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5"},
		},
		{
			provider: "gitlawb-opengateway",
			want: []string{
				"mimo-v2.5-pro", "tencent/hy3",
				"xiaomi/mimo-v2.5-pro", "xiaomi/mimo-v2.5", "minimax/minimax-m3", "qwen/qwen3.7-max",
				"google/gemini-3.1-flash-lite", "z-ai/glm-5.2", "nvidia/nemotron-3-ultra-550b-a55b:free",
			},
			notWant: []string{"openai/gpt-4.1", "claude-sonnet-4.5"},
		},
		{
			provider: "opencode-go-anthropic-compatible",
			want:     []string{"minimax-m3", "minimax-m2.7", "qwen3.7-plus", "qwen3.7-max"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5", "deepseek-chat"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			descriptor, ok := providercatalog.Get(tt.provider)
			if !ok {
				t.Fatalf("provider %q missing from catalog", tt.provider)
			}
			models := Models(descriptor)
			got := map[string]bool{}
			for _, model := range models {
				got[model.ID] = true
			}
			for _, want := range tt.want {
				if !got[want] {
					t.Fatalf("%s models missing %q; got %#v", tt.provider, want, modelIDs(models))
				}
			}
			for _, notWant := range tt.notWant {
				if got[notWant] {
					t.Fatalf("%s models should not include %q; got %#v", tt.provider, notWant, modelIDs(models))
				}
			}
		})
	}
}

func TestModelsDoNotAliasMutableCatalogState(t *testing.T) {
	descriptor, ok := providercatalog.Get("minimax")
	if !ok {
		t.Fatal("provider minimax missing from catalog")
	}
	first := Models(descriptor)
	if len(first) == 0 || len(first[0].InputModalities) == 0 {
		t.Fatal("expected MiniMax models with input modalities")
	}
	first[0].ID = "mutated"
	first[0].InputModalities[0] = "mutated"

	second := Models(descriptor)
	if second[0].ID == "mutated" || second[0].InputModalities[0] == "mutated" {
		t.Fatal("Models returned aliased mutable catalog state")
	}
}

func TestDedupeModelsDoesNotAliasCapabilitySlices(t *testing.T) {
	models := []Model{{
		ID:               "reasoner",
		ReasoningEfforts: []string{"low", "high"},
		ServiceTiers:     []string{"priority"},
	}}
	first := dedupeModels("reasoner", models)
	first[0].ReasoningEfforts[0] = "mutated"
	first[0].ServiceTiers[0] = "mutated"

	second := dedupeModels("reasoner", models)
	if got, want := second[0].ReasoningEfforts[0], "low"; got != want {
		t.Fatalf("reasoning efforts alias source state: got %q, want %q", got, want)
	}
	if got, want := second[0].ServiceTiers[0], "priority"; got != want {
		t.Fatalf("service tiers alias source state: got %q, want %q", got, want)
	}
}

func modelIDs(models []Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestMiniMaxModelsExposeCurrentCapabilities(t *testing.T) {
	descriptor, ok := providercatalog.Get("minimax")
	if !ok {
		t.Fatal("provider minimax missing from catalog")
	}
	models := Models(descriptor)
	if len(models) < 2 || models[0].ID != "MiniMax-M3" || models[1].ID != "MiniMax-M2.7" {
		t.Fatalf("MiniMax current models = %#v, want MiniMax-M3 followed by MiniMax-M2.7", modelIDs(models))
	}

	m3 := models[0]
	if m3.ContextWindow != 1_000_000 || !m3.ToolCall || !m3.Reasoning {
		t.Fatalf("MiniMax-M3 capabilities = %#v, want 1M context with tools and reasoning", m3)
	}
	if !reflect.DeepEqual(m3.InputModalities, []string{"text", "image", "video"}) ||
		!reflect.DeepEqual(m3.OutputModalities, []string{"text"}) {
		t.Fatalf("MiniMax-M3 modalities = input:%#v output:%#v", m3.InputModalities, m3.OutputModalities)
	}

	m27 := models[1]
	if m27.ContextWindow != 204_800 || !m27.ToolCall || !m27.Reasoning {
		t.Fatalf("MiniMax-M2.7 capabilities = %#v, want 204.8K context with tools and reasoning", m27)
	}
	if !reflect.DeepEqual(m27.InputModalities, []string{"text"}) ||
		!reflect.DeepEqual(m27.OutputModalities, []string{"text"}) {
		t.Fatalf("MiniMax-M2.7 modalities = input:%#v output:%#v", m27.InputModalities, m27.OutputModalities)
	}
}

// Pins the contract that MiniMax CN exposes exactly the same model IDs as the
// international provider; a divergence here is almost always a mistake.
func TestMiniMaxCNModelsMirrorInternational(t *testing.T) {
	intl, ok := providercatalog.Get("minimax")
	if !ok {
		t.Fatal("provider minimax missing from catalog")
	}
	cn, ok := providercatalog.Get("minimaxi-cn")
	if !ok {
		t.Fatal("provider minimaxi-cn missing from catalog")
	}
	intlModels := Models(intl)
	cnModels := Models(cn)
	if len(intlModels) != len(cnModels) {
		t.Fatalf("model count mismatch: international=%d china=%d", len(intlModels), len(cnModels))
	}
	intlIDs := map[string]string{}
	for _, m := range intlModels {
		intlIDs[m.ID] = m.Description
	}
	for _, m := range cnModels {
		wantDesc, ok := intlIDs[m.ID]
		if !ok {
			t.Fatalf("china provider exposes %q which is not in the international catalog", m.ID)
		}
		if m.Description != wantDesc {
			t.Fatalf("model %q description diverged: international=%q china=%q", m.ID, wantDesc, m.Description)
		}
	}
}

// Same contract as TestMiniMaxCNModelsMirrorInternational, for Z.ai: the
// international (api.z.ai) and China (open.bigmodel.cn) endpoints expose the
// same model lineup.
func TestZaiCNModelsMirrorInternational(t *testing.T) {
	intl, ok := providercatalog.Get("zai")
	if !ok {
		t.Fatal("provider zai missing from catalog")
	}
	cn, ok := providercatalog.Get("zai-cn")
	if !ok {
		t.Fatal("provider zai-cn missing from catalog")
	}
	intlModels := Models(intl)
	cnModels := Models(cn)
	if len(intlModels) != len(cnModels) {
		t.Fatalf("model count mismatch: international=%d china=%d", len(intlModels), len(cnModels))
	}
	intlIDs := map[string]string{}
	for _, m := range intlModels {
		intlIDs[m.ID] = m.Description
	}
	for _, m := range cnModels {
		wantDesc, ok := intlIDs[m.ID]
		if !ok {
			t.Fatalf("china provider exposes %q which is not in the international catalog", m.ID)
		}
		if m.Description != wantDesc {
			t.Fatalf("model %q description diverged: international=%q china=%q", m.ID, wantDesc, m.Description)
		}
	}
}
