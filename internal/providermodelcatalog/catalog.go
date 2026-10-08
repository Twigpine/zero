package providermodelcatalog

import (
	"strings"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

type Model struct {
	ID                     string
	Description            string
	ContextWindow          int
	ToolCall               bool
	Reasoning              bool
	ReasoningEfforts       []string
	DefaultReasoningEffort string
	ServiceTiers           []string
	DefaultServiceTier     string
	InputModalities        []string
	OutputModalities       []string
	InputCost              float64
	OutputCost             float64
	Tags                   []string
	Source                 string
}

const minimaxModelSource = "https://platform.minimax.io/docs/api-reference/api-overview"

// Shared by both "minimax" and "minimaxi-cn"; Models() rebuilds a fresh slice
// per call, so the shared backing slice cannot be mutated by callers.
// Flat cost fields stay unset because MiniMax-M3 pricing is tiered and this
// catalog model shape cannot represent those tiers without losing information.
var minimaxCuratedModels = []Model{
	{
		ID:               "MiniMax-M3",
		Description:      "catalog default",
		ContextWindow:    1_000_000,
		ToolCall:         true,
		Reasoning:        true,
		InputModalities:  []string{"text", "image", "video"},
		OutputModalities: []string{"text"},
		Source:           minimaxModelSource,
	},
	{
		ID:               "MiniMax-M2.7",
		Description:      "agentic coding model",
		ContextWindow:    204_800,
		ToolCall:         true,
		Reasoning:        true,
		InputModalities:  []string{"text"},
		OutputModalities: []string{"text"},
		Source:           minimaxModelSource,
	},
	{ID: "MiniMax-M2.1", Description: "agentic coding model"},
}

// Shared by both "zai" (international, api.z.ai) and "zai-cn" (China,
// open.bigmodel.cn); same model lineup on both endpoints.
var zaiCuratedModels = []Model{
	{ID: "glm-4.5", Description: "catalog default"},
	{ID: "glm-4.5-air", Description: "fast model"},
	{ID: "glm-4.6", Description: "latest general model"},
	{ID: "glm-z1-air", Description: "reasoning model"},
}

var curatedModels = map[string][]Model{
	// aimlapi.com aggregates every upstream provider behind one OpenAI-compatible
	// endpoint, so the curated defaults are a cross-provider "best of" flagship
	// list. The live /v1/models discovery (when a key is present) replaces this;
	// these are the offline/pre-key fallback and the picker's default order.
	"aimlapi": {
		{ID: "anthropic/claude-sonnet-5", Description: "catalog default (Anthropic Claude Sonnet 5)"},
		{ID: "google/gemini-3.5-flash", Description: "Google Gemini Flash 3.5"},
		{ID: "openai/gpt-5.5-2026-04-23", Description: "OpenAI GPT-5.5"},
		{ID: "qwen/qwen-3.7-max", Description: "Qwen 3.7 Max"},
		{ID: "deepseek/deepseek-v4-pro", Description: "DeepSeek V4"},
	},
	"ollama-cloud": {
		{ID: "qwen3-coder:480b", Description: "catalog default"},
		{ID: "gpt-oss:120b", Description: "agentic coding model"},
		{ID: "deepseek-v4-pro", Description: "coding model"},
		{ID: "minimax-m3", Description: "agentic coding model"},
		{ID: "kimi-k2.6", Description: "coding model"},
		{ID: "devstral-2:123b", Description: "coding model"},
	},
	"ollama": {
		{ID: "llama3.1", Description: "catalog default"},
		{ID: "qwen2.5-coder:32b", Description: "local coding model"},
		{ID: "deepseek-coder-v2:16b", Description: "local coding model"},
		{ID: "codellama:13b", Description: "local coding model"},
	},
	"lmstudio": {
		{ID: "local-model", Description: "catalog default"},
		{ID: "qwen2.5-coder-32b-instruct", Description: "local coding model"},
		{ID: "deepseek-coder-v2-lite-instruct", Description: "local coding model"},
		{ID: "llama-3.1-8b-instruct", Description: "local chat model"},
	},
	"groq": {
		{ID: "llama-3.3-70b-versatile", Description: "catalog default"},
		{ID: "openai/gpt-oss-120b", Description: "large open-weight model"},
		{ID: "openai/gpt-oss-20b", Description: "fast open-weight model"},
		{ID: "deepseek-r1-distill-llama-70b", Description: "reasoning model"},
		{ID: "qwen/qwen3-32b", Description: "coding-capable model"},
	},
	"openrouter": {
		{ID: "openai/gpt-4.1", Description: "catalog default"},
		{ID: "anthropic/claude-sonnet-4.5", Description: "coding model"},
		{ID: "google/gemini-2.5-pro", Description: "long-context model"},
		{ID: "minimax/minimax-m2.1", Description: "agentic coding model"},
		{ID: "deepseek/deepseek-chat", Description: "coding model"},
	},
	"chatgpt": {
		{ID: "gpt-5.5", Description: "recommended Codex model"},
		{ID: "gpt-5.4", Description: "strong Codex model"},
		{ID: "gpt-5.4-mini", Description: "fast Codex model"},
		{ID: "gpt-5.3-codex-spark", Description: "research preview fast Codex model"},
	},
	"deepseek": {
		{ID: "deepseek-chat", Description: "catalog default"},
		{ID: "deepseek-reasoner", Description: "reasoning model"},
	},
	"together": {
		{ID: "meta-llama/Llama-3.3-70B-Instruct-Turbo", Description: "catalog default"},
		{ID: "Qwen/Qwen2.5-Coder-32B-Instruct", Description: "coding model"},
		{ID: "deepseek-ai/DeepSeek-R1", Description: "reasoning model"},
		{ID: "meta-llama/Llama-4-Maverick-17B-128E-Instruct-FP8", Description: "multimodal model"},
	},
	"fireworks": {
		// Serverless-only curated set: ordinary API-key setups hit /inference/v1
		// and cannot use on-demand deployment IDs.
		{ID: "accounts/fireworks/models/kimi-k2p7-code", Description: "catalog default"},
		{ID: "accounts/fireworks/models/deepseek-v4-flash", Description: "fast coding model"},
		{ID: "accounts/fireworks/models/gpt-oss-120b", Description: "general model"},
		{ID: "accounts/fireworks/models/deepseek-v4-pro", Description: "reasoning model"},
	},
	"dashscope": {
		{ID: "qwen-plus", Description: "catalog default"},
		{ID: "qwen-max", Description: "strong general model"},
		{ID: "qwen-coder-plus", Description: "coding model"},
		{ID: "qwen3-coder-plus", Description: "coding model"},
	},
	"moonshot": {
		{ID: "kimi-k2-0905-preview", Description: "catalog default"},
		{ID: "kimi-k2-turbo-preview", Description: "fast coding model"},
		{ID: "moonshot-v1-128k", Description: "long-context model"},
	},
	"atlascloud": {
		{ID: "qwen/qwen3.5-flash", Description: "catalog default"},
		{ID: "deepseek-ai/deepseek-v4-pro", Description: "reasoning model"},
	},
	"nvidia-nim": {
		{ID: "nvidia/llama-3.1-nemotron-70b-instruct", Description: "catalog default"},
		{ID: "meta/llama-3.1-70b-instruct", Description: "general model"},
		{ID: "mistralai/mixtral-8x7b-instruct-v0.1", Description: "mixture model"},
	},
	"minimax":     minimaxCuratedModels,
	"minimaxi-cn": minimaxCuratedModels,
	"mistral": {
		{ID: "mistral-large-latest", Description: "catalog default"},
		{ID: "codestral-latest", Description: "coding model"},
		{ID: "mistral-medium-latest", Description: "balanced model"},
		{ID: "ministral-8b-latest", Description: "small fast model"},
		{ID: "magistral-medium-latest", Description: "reasoning model"},
	},
	"github": {
		{ID: "openai/gpt-4.1", Description: "catalog default"},
		{ID: "openai/gpt-4o", Description: "multimodal model"},
		{ID: "openai/o3-mini", Description: "reasoning model"},
		{ID: "mistral-ai/codestral-2501", Description: "coding model"},
	},
	"xai": {
		{ID: "grok-4", Description: "catalog default"},
		{ID: "grok-3", Description: "general model"},
		{ID: "grok-3-mini", Description: "fast reasoning model"},
		{ID: "grok-code-fast-1", Description: "coding model"},
	},
	"venice": {
		{ID: "qwen-2.5-qwq-32b", Description: "catalog default"},
		{ID: "llama-3.3-70b", Description: "general model"},
		{ID: "deepseek-r1-671b", Description: "reasoning model"},
	},
	"xiaomi-mimo": {
		{ID: "mimo-vl", Description: "catalog default"},
		{ID: "mimo-v2.5-pro-ultraspeed", Description: "fast model"},
	},
	"bankr": {
		{ID: "bankr-large", Description: "catalog default"},
	},
	"zai":    zaiCuratedModels,
	"zai-cn": zaiCuratedModels,
	// OpenGateway smart-routes by model id across its upstream providers
	// (see /health: xiaomi-mimo, minimax, qwen, google, nvidia, tencent, z-ai).
	// These are the curated coding defaults; the gateway accepts any model its
	// upstreams expose, so users can also type an id the picker doesn't list.
	"gitlawb-opengateway": {
		{ID: "mimo-v2.5-pro", Description: "catalog default (Xiaomi MiMo)"},
		{ID: "xiaomi/mimo-v2.5-pro", Description: "Xiaomi MiMo V2.5 Pro"},
		{ID: "mimo-v2.5-pro-ultraspeed", Description: "fast model (Xiaomi MiMo)"},
		{ID: "xiaomi/mimo-v2.5", Description: "multimodal model (Xiaomi)"},
		{ID: "tencent/hy3", Description: "free Tencent HY3 model"},
		{ID: "MiniMax-M3", Description: "MiniMax model"},
		{ID: "minimax/minimax-m3", Description: "MiniMax M3 model"},
		{ID: "qwen-plus", Description: "Qwen model"},
		{ID: "qwen/qwen3.7-max", Description: "Qwen flagship coding model"},
		{ID: "gemini-2.5-pro", Description: "long-context model (Google)"},
		{ID: "google/gemini-3.1-flash-lite", Description: "Google Gemini 3.1 Flash Lite"},
		{ID: "glm-4.6", Description: "Z.ai model"},
		{ID: "z-ai/glm-5.2", Description: "GLM coding & reasoning model"},
		{ID: "nvidia/llama-3.1-nemotron-70b-instruct", Description: "NVIDIA NIM model"},
		{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Description: "free Nemotron 3 Ultra reasoning MoE"},
	},
	"atomic-chat": {
		{ID: "gpt-4.1", Description: "catalog default"},
		{ID: "gpt-4o-mini", Description: "fast model"},
	},
	// atomic-chat-local has no curated list: discover the user-loaded model
	// from the runtime's /v1/models endpoint.
	"opencode-go-anthropic-compatible": {
		{ID: "minimax-m3", Description: "MiniMax M3: default"},
		{ID: "minimax-m2.7", Description: "MiniMax M2.7: coding model"},
		{ID: "qwen3.7-plus", Description: "Qwen 3.7 Plus: balanced model"},
		{ID: "qwen3.7-max", Description: "Qwen 3.7 Max: strong model"},
	},
	"custom-openai-compatible": {
		{ID: "custom-model", Description: "custom endpoint model"},
	},
	"custom-anthropic-compatible": {
		{ID: "custom-model", Description: "custom endpoint model"},
	},
}

func Models(provider providercatalog.Descriptor) []Model {
	if models, ok := curatedModels[provider.ID]; ok {
		return FilterModelsForProvider(provider.ID, dedupeModels(provider.DefaultModel, models))
	}
	models := registryModels(provider)
	if len(models) > 0 {
		return FilterModelsForProvider(provider.ID, dedupeModels(provider.DefaultModel, models))
	}
	return FilterModelsForProvider(provider.ID, dedupeModels(provider.DefaultModel, nil))
}

func registryModels(provider providercatalog.Descriptor) []Model {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil
	}
	models := []Model{}
	for _, entry := range registry.List(modelregistry.ListOptions{}) {
		if !modelMatchesProvider(entry, provider) {
			continue
		}
		models = append(models, Model{
			ID:            entry.ID,
			Description:   entry.DisplayName,
			ContextWindow: entry.ContextLimits.ContextWindow,
		})
		if len(models) >= 8 {
			break
		}
	}
	return models
}

func dedupeModels(defaultModel string, models []Model) []Model {
	result := []Model{}
	seen := map[string]bool{}
	add := func(model Model) {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" || seen[model.ID] {
			return
		}
		if model.Description == "" {
			model.Description = "catalog model"
		}
		model.InputModalities = append([]string{}, model.InputModalities...)
		model.OutputModalities = append([]string{}, model.OutputModalities...)
		model.ReasoningEfforts = append([]string{}, model.ReasoningEfforts...)
		model.ServiceTiers = append([]string{}, model.ServiceTiers...)
		model.Tags = append([]string{}, model.Tags...)
		seen[model.ID] = true
		result = append(result, model)
	}
	defaultEntry := Model{ID: defaultModel, Description: "catalog default"}
	for _, model := range models {
		if strings.TrimSpace(model.ID) == strings.TrimSpace(defaultModel) {
			defaultEntry = model
			defaultEntry.Description = "catalog default"
			break
		}
	}
	add(defaultEntry)
	for _, model := range models {
		add(model)
	}
	return result
}

func modelMatchesProvider(model modelregistry.ModelEntry, provider providercatalog.Descriptor) bool {
	switch provider.Transport {
	case providercatalog.TransportOpenAI:
		return model.Provider == modelregistry.ProviderOpenAI
	case providercatalog.TransportAnthropic, providercatalog.TransportAnthropicCompatible:
		return model.Provider == modelregistry.ProviderAnthropic
	case providercatalog.TransportGoogle:
		return model.Provider == modelregistry.ProviderGoogle
	default:
		return false
	}
}
