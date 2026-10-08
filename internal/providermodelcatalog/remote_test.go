package providermodelcatalog

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/providercatalog"
)

func TestParseModelsDevProviderScopesAndMapsMetadata(t *testing.T) {
	body := []byte(`{
		"openai": {
			"models": {
				"gpt-4.1": {
					"id": "gpt-4.1",
					"name": "GPT-4.1",
					"tool_call": true,
					"reasoning": false,
					"limit": {"context": 1048576, "output": 32768},
					"cost": {"input": 2, "output": 8},
					"modalities": {"input": ["text", "image"], "output": ["text"]}
				},
				"gpt-image-1": {
					"id": "gpt-image-1",
					"name": "GPT Image",
					"modalities": {"input": ["text", "image"], "output": ["image"]}
				},
				"text-embedding-3-large": {
					"id": "text-embedding-3-large",
					"name": "Embedding model"
				},
				"whisper-1": {
					"id": "whisper-1",
					"name": "Whisper"
				}
			}
		},
		"anthropic": {
			"models": {
				"claude-sonnet-4.5": {"id": "claude-sonnet-4.5"}
			}
		}
	}`)

	models, err := ParseModelsDevProvider(body, "openai")
	if err != nil {
		t.Fatalf("ParseModelsDevProvider returned error: %v", err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "gpt-4.1" {
		t.Fatalf("models = %#v, want only coding-capable OpenAI model", got)
	}
	model := models[0]
	if model.ID != "gpt-4.1" || model.Description != "GPT-4.1" {
		t.Fatalf("model identity = %#v, want GPT-4.1 metadata", model)
	}
	if model.ContextWindow != 1048576 || !model.ToolCall || model.Reasoning {
		t.Fatalf("model capabilities = %#v, want context/tools without reasoning", model)
	}
	if model.InputCost != 2 || model.OutputCost != 8 {
		t.Fatalf("model cost = %#v, want input/output pricing", model)
	}
	if strings.Join(model.InputModalities, ",") != "text,image" || strings.Join(model.OutputModalities, ",") != "text" {
		t.Fatalf("model modalities = %#v/%#v, want text,image -> text", model.InputModalities, model.OutputModalities)
	}
	if model.Source != "models.dev" {
		t.Fatalf("model source = %q, want models.dev", model.Source)
	}
}

func TestParseOpenGatewayCatalogSupportsRichModelJSON(t *testing.T) {
	body := []byte(`{
		"models": [
			{
				"id": "minimax-m3",
				"name": "MiniMax M3",
				"description": "agentic coding route",
				"context_window": 262144,
				"tool_call": true,
				"reasoning": true,
				"tags": ["coding", "free"],
				"cost": {"input": 0, "output": 0}
			},
			{
				"id": "tencent/hy3",
				"name": "Tencent HY3",
				"description": "free Tencent chat route",
				"context_window": 262144,
				"tools": true,
				"tags": ["free"]
			},
			{
				"id": "auto",
				"name": "Auto (smart routing)",
				"description": "picks the cheapest capable model"
			},
			{
				"id": "whisper-1",
				"name": "Whisper"
			}
		]
	}`)

	models, err := ParseOpenGatewayCatalog(body)
	if err != nil {
		t.Fatalf("ParseOpenGatewayCatalog returned error: %v", err)
	}
	// Gateway list is trusted: keep auto + coding routes; drop known non-coding.
	// Sorted by description label (agentic…, free…, picks…).
	if got := strings.Join(modelIDs(models), ","); got != "minimax-m3,tencent/hy3,auto" {
		t.Fatalf("models = %#v, want gateway live models including auto", got)
	}
	var model Model
	for _, entry := range models {
		if entry.ID == "minimax-m3" {
			model = entry
			break
		}
	}
	if model.ID != "minimax-m3" || model.Description != "agentic coding route" {
		t.Fatalf("gateway model = %#v, want rich description", model)
	}
	if model.ContextWindow != 262144 || !model.ToolCall || !model.Reasoning {
		t.Fatalf("gateway capabilities = %#v, want context/tools/reasoning", model)
	}
	if strings.Join(model.Tags, ",") != "coding,free" {
		t.Fatalf("gateway tags = %#v, want coding/free", model.Tags)
	}
	if model.Source != "opengateway" {
		t.Fatalf("gateway source = %q, want opengateway", model.Source)
	}
	var hy3 Model
	for _, entry := range models {
		if entry.ID == "tencent/hy3" {
			hy3 = entry
			break
		}
	}
	if hy3.ID != "tencent/hy3" || hy3.ContextWindow != 262144 || !hy3.ToolCall {
		t.Fatalf("gateway HY3 model = %#v, want Tencent HY3 with context/tools", hy3)
	}
}

func TestParseOpenRouterCatalogMapsLiveMetadata(t *testing.T) {
	body := []byte(`{
		"data": [
			{
				"id": "anthropic/claude-sonnet-4.5",
				"name": "Anthropic: Claude Sonnet 4.5",
				"description": "A long marketing blurb that should not win the picker label.",
				"context_length": 200000,
				"pricing": {
					"prompt": "0.000003",
					"completion": "0.000015"
				},
				"architecture": {
					"input_modalities": ["text", "image"],
					"output_modalities": ["text"]
				},
				"supported_parameters": ["tools", "tool_choice", "reasoning", "temperature"]
			},
			{
				"id": "vendor/image-only",
				"name": "Image Only",
				"architecture": {
					"input_modalities": ["text"],
					"output_modalities": ["image"]
				}
			},
			{
				"id": "openai/gpt-4o",
				"name": "OpenAI: GPT-4o",
				"context_length": 128000,
				"supported_parameters": ["tools"]
			}
		]
	}`)

	models, err := ParseOpenRouterCatalog(body)
	if err != nil {
		t.Fatalf("ParseOpenRouterCatalog returned error: %v", err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "anthropic/claude-sonnet-4.5,openai/gpt-4o" {
		t.Fatalf("models = %#v, want coding OpenRouter models only", got)
	}
	var claude Model
	for _, entry := range models {
		if entry.ID == "anthropic/claude-sonnet-4.5" {
			claude = entry
			break
		}
	}
	if claude.Description != "Anthropic: Claude Sonnet 4.5" {
		t.Fatalf("description = %q, want short display name", claude.Description)
	}
	if claude.ContextWindow != 200000 || !claude.ToolCall || !claude.Reasoning {
		t.Fatalf("claude capabilities = %#v, want context/tools/reasoning from live payload", claude)
	}
	if strings.Join(claude.InputModalities, ",") != "text,image" || strings.Join(claude.OutputModalities, ",") != "text" {
		t.Fatalf("claude modalities = %#v/%#v", claude.InputModalities, claude.OutputModalities)
	}
	// Live pricing.prompt is USD per token; Model costs are USD per million tokens.
	if claude.InputCost != 3 || claude.OutputCost != 15 {
		t.Fatalf("claude costs = %f/%f, want 3/15 (per-million, matching models.dev)", claude.InputCost, claude.OutputCost)
	}
	if claude.Source != "openrouter" {
		t.Fatalf("source = %q, want openrouter", claude.Source)
	}
}

func TestParsePricingString(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0.000003", 3},
		{" 0.000015 ", 15},
		{"-1", 0},
		{"0", 0},
		{"", 0},
		{"not-a-number", 0},
		{"Inf", 0},
		{"+Inf", 0},
		{"-Inf", 0},
		{"NaN", 0},
	}
	for _, tc := range cases {
		if got := parsePricingString(tc.in); got != tc.want {
			t.Fatalf("parsePricingString(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPublicLiveCatalog(t *testing.T) {
	if !PublicLiveCatalog("openrouter") || !PublicLiveCatalog("gitlawb-opengateway") {
		t.Fatal("openrouter and gitlawb-opengateway should advertise a public live catalog")
	}
	if PublicLiveCatalog("openai") {
		t.Fatal("openai should not be treated as a public live catalog provider")
	}
}

func TestModelsDevProviderIDMapsZeroAliases(t *testing.T) {
	tests := map[string]string{
		"chatgpt":      "openai",
		"github":       "github-models",
		"moonshot":     "moonshotai",
		"nvidia-nim":   "nvidia",
		"xiaomi-mimo":  "xiaomi",
		"dashscope":    "alibaba",
		"ollama-cloud": "ollama-cloud",
		"zai-cn":       "zai",
		"minimaxi-cn":  "minimax",
		"fireworks":    "fireworks-ai",
	}
	for zeroID, want := range tests {
		provider, ok := providercatalog.Get(zeroID)
		if !ok {
			t.Fatalf("provider %q missing from catalog", zeroID)
		}
		if got := ModelsDevProviderID(provider); got != want {
			t.Fatalf("ModelsDevProviderID(%q) = %q, want %q", zeroID, got, want)
		}
	}
}
