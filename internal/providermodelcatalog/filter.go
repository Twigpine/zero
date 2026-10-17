package providermodelcatalog

import (
	"strings"

	"github.com/Twigpine/zero/internal/providercatalog"
)

// ModelIDAllowedForProvider reports whether a model ID is permitted for the
// given provider under provider-specific allow/block rules.
// For opencode-go-anthropic-compatible: only Qwen and MiniMax model IDs are allowed.
// Allows ANY model by default
func ModelIDAllowedForProvider(providerID, modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return false
	}
	switch providercatalog.NormalizeID(providerID) {
	case "opencode-go-anthropic-compatible":
		return strings.Contains(modelID, "qwen") || strings.Contains(modelID, "minimax")
	default:
		return true
	}
}

// FilterModelsForProvider filters a model slice by ModelIDAllowedForProvider.
func FilterModelsForProvider(providerID string, models []Model) []Model {
	result := make([]Model, 0, len(models))
	for _, model := range models {
		if ModelIDAllowedForProvider(providerID, model.ID) {
			result = append(result, model)
		}
	}
	return result
}

func IsCodingModel(model Model) bool {
	if IsKnownNonCodingModelID(model.ID) {
		return false
	}
	if len(model.OutputModalities) > 0 && !containsFold(model.OutputModalities, "text") {
		return false
	}
	if hasCodingTag(model.Tags) || model.ToolCall || model.Reasoning {
		return true
	}
	return LooksLikeCodingModelID(model.ID) || LooksLikeCodingModelID(model.Description)
}

func LooksLikeCodingModelID(id string) bool {
	normalized := strings.ToLower(strings.TrimSpace(id))
	if normalized == "" || IsKnownNonCodingModelID(normalized) {
		return false
	}
	for _, prefix := range []string{"o1", "o3", "o4", "o5"} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"-") {
			return true
		}
	}
	for _, term := range []string{
		"gpt", "claude", "sonnet", "opus", "haiku", "gemini", "gemma",
		"llama", "qwen", "deepseek", "kimi", "moonshot", "minimax",
		"mistral", "codestral", "devstral", "magistral", "ministral",
		"grok", "glm", "command", "nemotron", "mixtral", "coder",
		"code", "chat", "instruct", "reasoner", "reasoning", "mimo",
		"hy3", "tencent", "maverick", "scout", "bankr",
	} {
		if strings.Contains(normalized, term) {
			return true
		}
	}
	return false
}

func IsKnownNonCodingModelID(id string) bool {
	normalized := strings.ToLower(strings.TrimSpace(id))
	if normalized == "" {
		return false
	}
	for _, term := range []string{
		"audio", "dall-e", "deep-research", "embedding", "image",
		"moderation", "realtime", "rerank", "sora", "speech",
		"transcribe", "translate", "tts", "whisper",
	} {
		if strings.Contains(normalized, term) {
			return true
		}
	}
	return false
}

// IsSTTModel reports whether a model transcribes speech to text — the models
// IsKnownNonCodingModelID filters OUT of the coding picker, filtered back IN for
// the /stt-model picker.
//
// The reliable signal is the model's declared modalities, not its name: a
// transcriber consumes audio and emits text. Anything that emits audio is
// text-to-speech or speech-to-speech, never a transcriber, so audio output is a
// hard disqualifier. Only when a catalog entry carries no usable modality
// metadata (common for the Groq/OpenAI transcription models, which models.dev
// lists sparsely) do we fall back to a name heuristic.
func IsSTTModel(model Model) bool {
	audioIn := containsFold(model.InputModalities, "audio")
	audioOut := containsFold(model.OutputModalities, "audio")
	textOut := containsFold(model.OutputModalities, "text")

	// Emits audio → TTS or speech-to-speech, regardless of a transcription-like
	// name. This is what stops gpt-4o-audio-preview / *-realtime from slipping in.
	if audioOut {
		return false
	}
	// Consumes audio and emits text (or emits nothing explicit) → transcriber.
	if audioIn && (textOut || len(model.OutputModalities) == 0) {
		return true
	}
	// Declares input modalities but none is audio → cannot be speech-to-text,
	// whatever it is called.
	if len(model.InputModalities) > 0 && !audioIn {
		return false
	}
	// No usable modality metadata: fall back to the name heuristic.
	return IsSTTModelID(model.ID)
}

// IsSTTModelID is the name-only heuristic used when a catalog entry declares no
// modalities. It matches the known transcription model families and explicitly
// rejects text-to-speech names, so it never confuses a TTS model for a
// transcriber. Weaker than the modality signal in IsSTTModel — a fallback, not
// the primary test.
func IsSTTModelID(id string) bool {
	normalized := strings.ToLower(strings.TrimSpace(id))
	if normalized == "" {
		return false
	}
	// Text-to-speech / speech-synthesis names are never transcription.
	for _, tts := range []string{"tts", "text-to-speech"} {
		if strings.Contains(normalized, tts) {
			return false
		}
	}
	for _, term := range []string{
		"whisper", "transcribe", "transcription",
		"scribe", "parakeet", "canary", "moonshine",
		"sense-voice", "sensevoice", "wav2vec", "conformer",
	} {
		if strings.Contains(normalized, term) {
			return true
		}
	}
	return false
}

func hasCodingTag(tags []string) bool {
	for _, tag := range tags {
		normalized := strings.ToLower(strings.TrimSpace(tag))
		switch normalized {
		case "agentic", "chat", "code", "coder", "coding", "instruct", "reasoning", "tools":
			return true
		}
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
