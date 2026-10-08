package providercatalog

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type Transport string

const (
	TransportOpenAI              Transport = "openai"
	TransportAnthropic           Transport = "anthropic"
	TransportGoogle              Transport = "google"
	TransportBedrock             Transport = "bedrock"
	TransportVertex              Transport = "vertex"
	TransportOpenAICompatible    Transport = "openai-compatible"
	TransportOpenAICompat        Transport = TransportOpenAICompatible
	TransportAnthropicCompatible Transport = "anthropic-compatible"
	TransportAnthropicCompat     Transport = TransportAnthropicCompatible
)

type APIFormat string

const (
	APIFormatOpenAIResponses       APIFormat = "responses"
	APIFormatOpenAIChatCompletions APIFormat = "chat-completions"
	APIFormatAnthropicMessages     APIFormat = "messages"
	APIFormatGoogleGenerateContent APIFormat = "generate-content"
	APIFormatBedrockConverse       APIFormat = "bedrock-converse"
	APIFormatVertexGenerateContent APIFormat = "vertex-generate-content"
)

var ErrUnknownProvider = errors.New("unknown provider")

// AIMLAPIID is the provider catalog identifier for the aimlapi.com preset.
const AIMLAPIID = "aimlapi"

type Descriptor struct {
	ID                  string
	Name                string
	Transport           Transport
	DefaultBaseURL      string
	DefaultModel        string
	AuthEnvVars         []string
	CustomHeaders       map[string]string
	RequiresAuth        bool
	UsesAmbientAuth     bool
	Public              bool
	Local               bool
	SupportedAPIFormats []APIFormat
	Aliases             []string

	// Custom marks the "bring your own endpoint" catalog entries
	// (custom-openai-compatible, custom-anthropic-compatible). Unlike every other
	// descriptor, RequiresAuth here is just a template default for the credential
	// wizard step, not a fact about the user's actual endpoint — a custom target may
	// or may not need auth, so callers deciding whether a *saved profile* is missing
	// a credential must not treat RequiresAuth as authoritative for these entries.
	Custom bool

	// OAuth reports that this provider offers an in-app OAuth login that yields a
	// credential usable for model calls (browser PKCE and/or device code). Only
	// set for providers where this actually works (not subscription-via-proxy).
	OAuth bool
	// OAuthMintsKey reports that the OAuth flow returns/mints a normal API key
	// (e.g. OpenRouter) rather than a bearer token used directly.
	OAuthMintsKey bool
	// OAuthDeviceFlow reports that RFC 8628 device-code login is supported (for
	// headless / SSH use) in addition to the browser flow.
	OAuthDeviceFlow bool

	// Recommended marks a provider surfaced at the top and badged in
	// catalog-ordered lists and pickers. The recommended descriptors are the first
	// entries in the catalog, in order.
	Recommended bool
}

func RuntimeSupported(descriptor Descriptor) bool {
	switch descriptor.Transport {
	case TransportOpenAI, TransportOpenAICompatible, TransportAnthropic, TransportAnthropicCompatible, TransportGoogle:
		return true
	default:
		return false
	}
}

func RuntimeUnsupportedReason(descriptor Descriptor) string {
	if RuntimeSupported(descriptor) {
		return ""
	}
	switch descriptor.Transport {
	case TransportBedrock, TransportVertex:
		return "native adapter not implemented yet"
	default:
		return "provider transport not implemented yet"
	}
}

var descriptors = []Descriptor{
	// GitLawb OpenGateway — the recommended default. An OpenAI-compatible gateway
	// that smart-routes by model id across upstream providers (xiaomi-mimo,
	// minimax, qwen, google, nvidia, tencent, z-ai). Flat /v1/chat/completions
	// with a Bearer ogw_live_… key; listed first and badged in every picker.
	recommended(openAICompat("gitlawb-opengateway", "GitLawb OpenGateway", "https://opengateway.gitlawb.com/v1", "mimo-v2.5-pro", []string{"GITLAWB_OPENGATEWAY_API_KEY"}, "gitlawb opengateway", "opengateway")),
	// aimlapi.com — OpenAI-compatible aggregating gateway, also badged as
	// recommended (second, right after the OpenGateway default). The built-in TUI
	// onboarding can register/top up an account and save the issued key; the partner
	// headers attribute usage for the rebate.
	recommended(aimlapi()),
	openAI("openai", "OpenAI", "https://api.openai.com/v1", "gpt-4.1", []string{"OPENAI_API_KEY"}),
	anthropic("anthropic", "Anthropic", "https://api.anthropic.com", "claude-sonnet-4.5", []string{"ANTHROPIC_API_KEY"}),
	google("google", "Google", "https://generativelanguage.googleapis.com", "gemini-2.5-pro", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, "gemini"),
	openAICompat("ollama-cloud", "Ollama Cloud", "https://ollama.com/v1", "qwen3-coder:480b", []string{"OLLAMA_API_KEY"}, "ollama.com", "ollama cloud"),
	localOpenAI("ollama", "Ollama Local", "http://localhost:11434/v1", "llama3.1", "ollama local"),
	localOpenAI("lmstudio", "LM Studio", "http://localhost:1234/v1", "local-model", "lm-studio", "lm studio"),
	localOpenAI("atomic-chat-local", "Atomic Chat Local", "http://127.0.0.1:1337/v1", "local-model", "atomic chat local"),
	oauthProvider(openAICompat("openrouter", "OpenRouter", "https://openrouter.ai/api/v1", "openai/gpt-4.1", []string{"OPENROUTER_API_KEY"}), true, false),
	// Hugging Face Inference Providers — OpenAI-compatible router at
	// https://router.huggingface.co/v1 exposes hundreds of OSS models. OAuth
	// requires a one-time app registration at huggingface.co/settings/applications/new
	// (no client secret needed for "public" apps); the preset pre-fills scopes,
	// endpoints, and the OIDC issuer. Free tier has strict rate limits; Pro
	// removes them.
	oauthProvider(openAICompat("huggingface", "Hugging Face", "https://router.huggingface.co/v1", "meta-llama/Llama-3.3-70B-Instruct", []string{"HUGGINGFACE_API_KEY"}), false, true),
	// ChatGPT (Codex backend) — the bearer from a ChatGPT Plus/Pro subscription
	// OAuth login routes to chatgpt.com/backend-api/codex/responses (NOT
	// api.openai.com). The Codex path injects `originator: codex_cli_rs` and the
	// `chatgpt-account-id` claim as headers; see internal/providers/openai/codex.go.
	// AuthEnvVars is empty (no API-key path) and the OAuth preset is the only
	// way in; RequiresAuth is forced true so catalog consumers know the provider
	// needs an interactive login before a request will succeed.
	func() Descriptor {
		d := openAICompat("chatgpt", "ChatGPT", "https://chatgpt.com/backend-api/codex", "gpt-5.5", nil)
		d.RequiresAuth = true
		return oauthProvider(d, false, false)
	}(),
	openAICompat("groq", "Groq", "https://api.groq.com/openai/v1", "llama-3.3-70b-versatile", []string{"GROQ_API_KEY"}),
	openAICompat("deepseek", "DeepSeek", "https://api.deepseek.com/v1", "deepseek-chat", []string{"DEEPSEEK_API_KEY"}),
	openAICompat("together", "Together AI", "https://api.together.xyz/v1", "meta-llama/Llama-3.3-70B-Instruct-Turbo", []string{"TOGETHER_API_KEY"}),
	openAICompat("fireworks", "Fireworks AI", "https://api.fireworks.ai/inference/v1", "accounts/fireworks/models/kimi-k2p7-code", []string{"FIREWORKS_API_KEY"}, "fireworks-ai"),
	openAICompat("dashscope", "DashScope", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", "qwen-plus", []string{"DASHSCOPE_API_KEY", "QWEN_API_KEY"}, "qwen"),
	openAICompat("moonshot", "Moonshot AI", "https://api.moonshot.ai/v1", "kimi-k2-0905-preview", []string{"MOONSHOT_API_KEY"}, "kimi"),
	openAICompat("atlascloud", "Atlas Cloud", "https://api.atlascloud.ai/v1", "qwen/qwen3.5-flash", []string{"ATLASCLOUD_API_KEY"}, "atlas cloud", "atlas"),
	openAICompat("longcat", "LongCat", "https://api.longcat.chat/openai", "LongCat-2.0", []string{"LONGCAT_API_KEY"}, "meituan longcat", "meituan", "longcat-2.0"),
	openAICompat("nvidia-nim", "NVIDIA NIM", "https://integrate.api.nvidia.com/v1", "nvidia/llama-3.1-nemotron-70b-instruct", []string{"NVIDIA_API_KEY"}, "nvidia nim"),
	anthropicCompat("minimax", "MiniMax", "https://api.minimax.io/anthropic", "MiniMax-M3", []string{"MINIMAX_API_KEY"}, "mini-max", "mini_max"),
	anthropicCompat("minimaxi-cn", "MiniMax CN", "https://api.minimaxi.com/anthropic", "MiniMax-M3", []string{"MINIMAXI_API_KEY"}, "minimax cn", "minimax-cn"),
	openAICompat("mistral", "Mistral", "https://api.mistral.ai/v1", "mistral-large-latest", []string{"MISTRAL_API_KEY"}),
	openAICompat("github", "GitHub Models", "https://models.inference.ai.azure.com", "openai/gpt-4.1", []string{"GITHUB_TOKEN"}, "github-models"),
	transportDescriptor("bedrock", "Amazon Bedrock", TransportBedrock, "https://bedrock-runtime.${AWS_REGION}.amazonaws.com", "anthropic.claude-3-5-sonnet-20241022-v2:0", []string{"AWS_ACCESS_KEY_ID", "AWS_PROFILE"}, []APIFormat{APIFormatBedrockConverse}, true),
	transportDescriptor("vertex", "Vertex AI", TransportVertex, "https://aiplatform.googleapis.com", "gemini-2.5-pro", []string{"GOOGLE_APPLICATION_CREDENTIALS"}, []APIFormat{APIFormatVertexGenerateContent}, true),
	oauthProvider(openAICompat("xai", "xAI", "https://api.x.ai/v1", "grok-4", []string{"XAI_API_KEY"}), false, true),
	openAICompat("venice", "Venice AI", "https://api.venice.ai/api/v1", "qwen-2.5-qwq-32b", []string{"VENICE_API_KEY"}),
	openAICompat("xiaomi-mimo", "Xiaomi MiMo", "https://api.mimo.xiaomi.com/openai/v1", "mimo-vl", []string{"MIMO_API_KEY", "XIAOMI_API_KEY"}, "xiaomi mimo"),
	openAICompat("bankr", "Bankr", "https://api.bankr.bot/v1", "bankr-large", []string{"BANKR_API_KEY"}),
	openAICompat("zai", "Z.ai", "https://api.z.ai/api/paas/v4", "glm-4.5", []string{"ZAI_API_KEY"}, "z.ai"),
	openAICompat("zai-cn", "Z.ai CN", "https://open.bigmodel.cn/api/paas/v4", "glm-4.5", []string{"ZHIPU_API_KEY"}, "z.ai cn"),
	openAICompat("kilocode", "KiloCode", "https://api.kilo.ai/api/gateway", "anthropic/claude-sonnet-4.6", []string{"KILO_API_KEY"}, "kilo", "kilo gateway"),
	openAICompat("opencode", "OpenCode Zen", "https://opencode.ai/zen/v1", "deepseek-v4-flash", []string{"OPENCODE_API_KEY"}, "opencode zen"),
	openAICompat("opencode-go", "OpenCode Go", "https://opencode.ai/zen/go/v1", "deepseek-v4-pro", []string{"OPENCODE_API_KEY"}, "opencode go"),
	anthropicCompat("opencode-go-anthropic-compatible", "OpenCode Go Anthropic-compatible", "https://opencode.ai/zen/go", "minimax-m3", []string{"OPENCODE_API_KEY"}),
	openAICompat("atomic-chat", "Atomic Chat", "https://api.atomic.chat/v1", "gpt-4.1", []string{"ATOMIC_CHAT_API_KEY"}),
	// ChatGPT subscription via a local OAuth proxy. A ChatGPT (Plus/Pro) OAuth
	// token only works against ChatGPT's own backend (which is Cloudflare-gated to
	// the official client), so zero does not call it directly; instead point this
	// at a local proxy that holds the OAuth session and exposes an OpenAI-compatible
	// endpoint. Local (no API key — the proxy authenticates); override the base URL
	// for your proxy's port. See docs/oauth-subscriptions.md.
	localOpenAI("chatgpt-proxy", "ChatGPT (local OAuth proxy)", "http://localhost:10531/v1", "gpt-5", "chatgpt"),
	func() Descriptor {
		d := openAICompat("custom-openai-compatible", "Custom OpenAI-compatible", "https://example.invalid/v1", "custom-model", []string{"OPENAI_API_KEY"}, "custom openai compatible")
		d.Custom = true
		return d
	}(),
	func() Descriptor {
		d := anthropicCompat("custom-anthropic-compatible", "Custom Anthropic-compatible", "https://example.invalid/anthropic", "custom-model", []string{"ANTHROPIC_API_KEY"}, "custom anthropic compatible")
		d.Custom = true
		return d
	}(),
}

func All() []Descriptor {
	copied := make([]Descriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		copied = append(copied, cloneDescriptor(descriptor))
	}
	return copied
}

func Get(id string) (Descriptor, bool) {
	normalized := NormalizeID(id)
	for _, descriptor := range descriptors {
		if descriptor.ID == normalized {
			return cloneDescriptor(descriptor), true
		}
		for _, alias := range descriptor.Aliases {
			if NormalizeID(alias) == normalized {
				return cloneDescriptor(descriptor), true
			}
		}
	}
	return Descriptor{}, false
}

func Require(id string) (Descriptor, error) {
	normalized := NormalizeID(id)
	descriptor, ok := Get(normalized)
	if !ok {
		return Descriptor{}, fmt.Errorf("%w %q", ErrUnknownProvider, normalized)
	}
	return descriptor, nil
}

func NormalizeID(id string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(id)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastDash = false
		default:
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(builder.String(), "-")
}

func openAI(id string, name string, baseURL string, model string, env []string, aliases ...string) Descriptor {
	return Descriptor{
		ID:                  id,
		Name:                name,
		Transport:           TransportOpenAI,
		DefaultBaseURL:      baseURL,
		DefaultModel:        model,
		AuthEnvVars:         env,
		RequiresAuth:        true,
		SupportedAPIFormats: []APIFormat{APIFormatOpenAIResponses, APIFormatOpenAIChatCompletions},
		Aliases:             aliases,
	}
}

func anthropic(id string, name string, baseURL string, model string, env []string, aliases ...string) Descriptor {
	return Descriptor{
		ID:                  id,
		Name:                name,
		Transport:           TransportAnthropic,
		DefaultBaseURL:      baseURL,
		DefaultModel:        model,
		AuthEnvVars:         env,
		RequiresAuth:        true,
		SupportedAPIFormats: []APIFormat{APIFormatAnthropicMessages},
		Aliases:             aliases,
	}
}

func google(id string, name string, baseURL string, model string, env []string, aliases ...string) Descriptor {
	return Descriptor{
		ID:                  id,
		Name:                name,
		Transport:           TransportGoogle,
		DefaultBaseURL:      baseURL,
		DefaultModel:        model,
		AuthEnvVars:         env,
		RequiresAuth:        true,
		SupportedAPIFormats: []APIFormat{APIFormatGoogleGenerateContent},
		Aliases:             aliases,
	}
}

func aimlapi() Descriptor {
	descriptor := openAICompat(AIMLAPIID, "aimlapi.com", "https://api.aimlapi.com/v1", "anthropic/claude-sonnet-5", []string{"AIMLAPI_API_KEY"}, "aimlapi", "aiml api", "ai/ml api", "ai ml api")
	descriptor.CustomHeaders = map[string]string{
		"X-AIMLAPI-Partner-ID":          "part_62yQoGYDq4Yqnrj2R1iGrDNJ",
		"X-AIMLAPI-Integration-Repo":    "Gitlawb/zero",
		"X-AIMLAPI-Integration-Version": "zero",
	}
	// Onboarding is key-based only: the TUI sub-flow takes an existing key or an
	// email top-up (internal/aimlapi Paths A/B). Not flagged
	// OAuth-capable, so it does not appear in the wizard's "Sign in with OAuth" list.
	return descriptor
}

// recommended marks a descriptor as a recommended default so list/picker
// surfaces sort it to the top and render a provider-specific badge.
func recommended(descriptor Descriptor) Descriptor {
	descriptor.Recommended = true
	return descriptor
}

// oauthProvider marks a descriptor as OAuth-capable. mintsKey => the flow returns
// an API key (OpenRouter); deviceFlow => RFC 8628 device code is also supported.
func oauthProvider(descriptor Descriptor, mintsKey bool, deviceFlow bool) Descriptor {
	descriptor.OAuth = true
	descriptor.OAuthMintsKey = mintsKey
	descriptor.OAuthDeviceFlow = deviceFlow
	return descriptor
}

// OAuthProviders returns the catalog descriptors that support an in-app OAuth
// login, in catalog order. It is the single source of truth for the wizard's
// dedicated "Sign in with OAuth" provider list.
func OAuthProviders() []Descriptor {
	out := []Descriptor{}
	for _, descriptor := range descriptors {
		if descriptor.OAuth {
			out = append(out, cloneDescriptor(descriptor))
		}
	}
	return out
}

func localOpenAI(id string, name string, baseURL string, model string, aliases ...string) Descriptor {
	descriptor := openAICompat(id, name, baseURL, model, nil, aliases...)
	descriptor.RequiresAuth = false
	descriptor.Local = true
	return descriptor
}

func openAICompat(id string, name string, baseURL string, model string, env []string, aliases ...string) Descriptor {
	return Descriptor{
		ID:                  id,
		Name:                name,
		Transport:           TransportOpenAICompatible,
		DefaultBaseURL:      baseURL,
		DefaultModel:        model,
		AuthEnvVars:         env,
		RequiresAuth:        len(env) > 0,
		SupportedAPIFormats: []APIFormat{APIFormatOpenAIChatCompletions},
		Aliases:             aliases,
	}
}

func anthropicCompat(id string, name string, baseURL string, model string, env []string, aliases ...string) Descriptor {
	return Descriptor{
		ID:                  id,
		Name:                name,
		Transport:           TransportAnthropicCompatible,
		DefaultBaseURL:      baseURL,
		DefaultModel:        model,
		AuthEnvVars:         env,
		RequiresAuth:        len(env) > 0,
		SupportedAPIFormats: []APIFormat{APIFormatAnthropicMessages},
		Aliases:             aliases,
	}
}

func transportDescriptor(id string, name string, transport Transport, baseURL string, model string, env []string, formats []APIFormat, ambient bool) Descriptor {
	return Descriptor{
		ID:                  id,
		Name:                name,
		Transport:           transport,
		DefaultBaseURL:      baseURL,
		DefaultModel:        model,
		AuthEnvVars:         env,
		RequiresAuth:        len(env) > 0 || ambient,
		UsesAmbientAuth:     ambient,
		SupportedAPIFormats: append([]APIFormat{}, formats...),
	}
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.AuthEnvVars = append([]string{}, descriptor.AuthEnvVars...)
	descriptor.SupportedAPIFormats = append([]APIFormat{}, descriptor.SupportedAPIFormats...)
	descriptor.Aliases = append([]string{}, descriptor.Aliases...)
	if descriptor.CustomHeaders != nil {
		descriptor.CustomHeaders = copyStringMap(descriptor.CustomHeaders)
	}
	return descriptor
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
