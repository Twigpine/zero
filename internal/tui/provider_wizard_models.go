package tui

import (
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/providercatalog"
	"github.com/Twigpine/zero/internal/providermodelcatalog"
)

func providerWizardModelOptions(provider providercatalog.Descriptor) []providerWizardModel {
	catalogModels := providermodelcatalog.Models(provider)
	models := make([]providerWizardModel, 0, len(catalogModels))
	for _, model := range catalogModels {
		models = append(models, providerWizardModel{
			ID:          model.ID,
			Description: model.Description,
			Meta:        providerWizardModelMeta(model.ContextWindow, model.ToolCall, model.Reasoning, model.InputCost, model.OutputCost, model.Tags),
		})
	}
	return models
}

func providerWizardModelMeta(contextWindow int, toolCall bool, reasoning bool, inputCost float64, outputCost float64, tags []string) string {
	parts := []string{}
	if ctx := formatContextWindow(contextWindow); ctx != "" {
		parts = append(parts, ctx+" ctx")
	}
	if toolCall {
		parts = append(parts, "tools")
	}
	if reasoning {
		parts = append(parts, "reasoning")
	}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			parts = append(parts, tag)
		}
		if len(parts) >= 5 {
			break
		}
	}
	if inputCost > 0 || outputCost > 0 {
		parts = append(parts, fmt.Sprintf("$%g/%g", inputCost, outputCost))
	}
	return strings.Join(parts, " · ")
}
