package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
)

func (m model) modelListText() string {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return "Models\nFailed to load model catalog: " + err.Error()
	}

	activeID := activeModelID(registry, m.modelName)
	models := registry.List(modelregistry.ListOptions{})
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Provider == models[j].Provider {
			return models[i].ID < models[j].ID
		}
		return models[i].Provider < models[j].Provider
	})
	modelLines := []string{}
	for _, model := range models {
		marker := " "
		if activeID != "" && model.ID == activeID {
			marker = "*"
		}
		modelLines = append(modelLines, fmt.Sprintf("%s %s (%s) - %s", marker, model.ID, model.Provider, model.DisplayName))
	}
	return renderCommandOutput(commandOutput{
		Title:  "Models",
		Status: commandStatusOK,
		Sections: []commandSection{
			{
				Title: "Active",
				Lines: []string{
					"Active model: " + displayValue(m.modelName, "none"),
					"provider: " + displayValue(m.providerName, "none"),
					"effort: " + m.effortDisplay(),
				},
			},
			{
				Title: "Available models:",
				Lines: modelLines,
			},
		},
		Hints: []string{"use /model <id> to switch this TUI session"},
	})
}

// modelContextWindow resolves the active model's context window (max input
// tokens) from the model registry to size agent-loop compaction. An unknown or
// custom model resolves to 0, leaving compaction DISABLED as a safe default.
// modelContextWindow resolves a model's exact context window (max input tokens) for
// the context gauges: the curated registry value, else a value learned from live
// provider discovery (so proxy/custom models like GPT-5 Codex, xAI, or Ollama-cloud
// get an accurate window once /model has discovered them), else 0 (unknown). The
// agent-run compaction path wraps this in modelregistry.AgentContextWindow to apply
// a positive fallback so compaction is enabled even for unknown models.
func (m model) modelContextWindow(modelName string) int {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return 0
	}
	if entry, ok := m.modelCatalog.Resolve(trimmed); ok && entry.ContextLimits.ContextWindow > 0 {
		return entry.ContextLimits.ContextWindow
	}
	// Live-discovered window, preferring the ACTIVE provider's models so a model ID
	// shared across providers resolves to the provider actually in use; only then
	// fall back to other providers that surfaced the same ID.
	if descriptor, ok := m.activeProviderDescriptor(); ok {
		if window := discoveredContextWindow(m.modelPickerLiveByProvider[descriptor.ID], trimmed); window > 0 {
			return window
		}
	}
	for _, models := range m.modelPickerLiveByProvider {
		if window := discoveredContextWindow(models, trimmed); window > 0 {
			return window
		}
	}
	// A custom/local Ollama model tag has no curated-catalog entry, and its
	// OpenAI-compatible /v1/models listing (the discoveredContextWindow source
	// above) doesn't carry context-window metadata at all — see
	// ollamaContextWindowDiscoveryCmd, the only other source for this.
	if window := m.ollamaContextWindowByModel[trimmed]; window > 0 {
		return window
	}
	// Unknown: report 0 so display gauges show no denominator. Compaction enablement
	// applies its own positive fallback via modelregistry.AgentContextWindow.
	return 0
}

// discoveredContextWindow returns the context window of the model whose ID matches
// name in a provider's live-discovered model list, or 0 when absent.
func discoveredContextWindow(models []providermodeldiscovery.Model, name string) int {
	for _, dm := range models {
		if strings.EqualFold(strings.TrimSpace(dm.ID), name) && dm.ContextWindow > 0 {
			return dm.ContextWindow
		}
	}
	return 0
}

func activeModelID(registry modelregistry.Registry, modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	if model, ok := registry.Get(modelName); ok {
		return model.ID
	}
	return strings.ToLower(modelName)
}
