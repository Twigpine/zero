package modelregistry

import (
	"regexp"
	"strings"
)

// Match explicit multimodal local families, including common Ollama spellings.
// Gemma 3 requires a vision-capable size: 270M and 1B are text-only.
var localVisionFamily = regexp.MustCompile(`^(gemma-?3[-:](4b|12b|27b)|llama-?4|mistral-small-?3\.1|phi-?4-multimodal)($|[-:_.])`)

// SupportsVision reports whether the model identified by modelID accepts image
// input. A model in the curated catalog is authoritative (its declared
// capability wins). For a model the catalog does NOT know — a custom /
// openai-compatible / ollama id that will never be in the catalog — it falls
// back to recognizing well-known multimodal families by name, so a real vision
// model still accepts images. A name outside those families stays refused (the
// previous "cannot confirm → drop" behavior), so text-only models are unaffected.
//
// modelID is resolved through the registry's normal alias/pattern matching, so
// any spelling that Get accepts works here too. This helper is the single
// capability check shared by the headless (exec) and interactive (TUI) input
// surfaces; the warn-and-drop behavior lives at those call sites.
func SupportsVision(registry Registry, modelID string) bool {
	if registry.SupportsCapability(modelID, ModelCapabilityVision) {
		return true
	}
	// The catalog knows this model and it lacks vision: trust that — never let the
	// name heuristic override an authoritative "no".
	if _, known := registry.Get(modelID); known {
		return false
	}
	return VisionCapableByName(modelID)
}

// VisionCapableByName reports whether modelID names a known multimodal family.
// It is conservative: it matches the established vision families and leaves
// everything else refused, so a false "supported" is unlikely. Used only as a
// fallback for models absent from the curated catalog.
func VisionCapableByName(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
		id = id[slash+1:] // drop a "provider/" prefix
	}
	switch {
	case localVisionFamily.MatchString(id):
		return true
	case strings.Contains(id, "gemini"):
		return true // every Gemini model is multimodal
	case strings.Contains(id, "claude-3"), strings.Contains(id, "claude-4"),
		strings.Contains(id, "claude-opus"), strings.Contains(id, "claude-sonnet"),
		strings.Contains(id, "claude-haiku"):
		return true // Claude 3+ are multimodal
	case strings.Contains(id, "gpt-4o"), strings.Contains(id, "gpt-4.1"),
		strings.Contains(id, "gpt-4-turbo"), strings.Contains(id, "gpt-4-vision"),
		strings.Contains(id, "gpt-5"):
		return true // GPT-4o / 4.1 / 4-turbo / 5 are multimodal (gpt-oss is NOT matched)
	case id == "o1" || id == "o3" || strings.HasPrefix(id, "o1-") ||
		strings.HasPrefix(id, "o3-") || strings.HasPrefix(id, "o4-"):
		return true // OpenAI o-series accept images
	case strings.Contains(id, "grok-4"),
		strings.Contains(id, "grok") && mentionsVision(id):
		return true // Grok 4 (and grok vision variants) are multimodal
	case strings.Contains(id, "kimi-k2"):
		return true // Kimi-K2 and later are multimodal (kimi-for-coding is NOT)
	case strings.Contains(id, "qwen2-vl"), strings.Contains(id, "qwen2.5-vl"),
		strings.Contains(id, "qwen-vl"), strings.Contains(id, "qwenvl"):
		return true // Qwen VL series are multimodal
	case strings.Contains(id, "minimax-m3"):
		return true // MiniMax M3 accepts image input
	case strings.Contains(id, "llava"), strings.Contains(id, "pixtral"),
		strings.Contains(id, "internvl"), strings.Contains(id, "minicpm-v"),
		strings.Contains(id, "moondream"), strings.Contains(id, "bakllava"),
		strings.Contains(id, "-vl"), strings.Contains(id, "vl-"),
		mentionsVision(id):
		return true // common open multimodal families + *-vl / *-vision
	default:
		return false
	}
}

// mentionsVision reports whether id advertises vision via the word "vision",
// excluding negated forms (vision-less / no-vision): a model named for LACKING
// vision must not be treated as multimodal.
func mentionsVision(id string) bool {
	if !strings.Contains(id, "vision") {
		return false
	}
	switch {
	case strings.Contains(id, "vision-less"), strings.Contains(id, "visionless"),
		strings.Contains(id, "no-vision"), strings.Contains(id, "non-vision"),
		strings.Contains(id, "novision"), strings.Contains(id, "nonvision"):
		return false
	}
	return true
}
