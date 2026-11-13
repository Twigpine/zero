package contextreport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/providers"
	"github.com/Twigpine/zero/internal/repomap"
	"github.com/Twigpine/zero/internal/tools"
)

const ContractV1 = "zero.context.report.v1"
const RuntimeGo = "go"

const (
	CategorySystemPrompt      = "system_prompt"
	CategoryProjectGuidelines = "project_guidelines"
	CategoryWorkspaceMap      = "workspace_map"
	CategoryTools             = "tools"
	CategoryFree              = "free"
)

var defaultProjectContextFiles = []string{"AGENTS.md", "ZERO.md", ".zero/AGENTS.md"}

const maxProjectContextBytes = 8 << 10
const maxWorkspaceMapContextBytes = 4 << 10

// toolDefinitionOverheadTokens approximates per-tool JSON/message framing.
const toolDefinitionOverheadTokens = 4

type Options struct {
	WorkspaceRoot       string
	Provider            config.ProviderProfile
	Registry            *tools.Registry
	ContextWindow       int
	DeferThreshold      int
	ProjectContextFiles []string
}

type Report struct {
	Contract             string     `json:"contract"`
	Runtime              string     `json:"runtime"`
	Root                 string     `json:"root"`
	ProviderName         string     `json:"providerName,omitempty"`
	ProviderKind         string     `json:"providerKind,omitempty"`
	ModelID              string     `json:"modelId,omitempty"`
	APIModel             string     `json:"apiModel,omitempty"`
	ContextWindow        int        `json:"contextWindow"`
	UsedTokens           int        `json:"usedTokens"`
	FreeTokens           int        `json:"freeTokens"`
	UsedFraction         float64    `json:"usedFraction"`
	ToolCount            int        `json:"toolCount"`
	AvailableToolCount   int        `json:"availableToolCount,omitempty"`
	DeferredToolCount    int        `json:"deferredToolCount,omitempty"`
	ProjectGuidelineFile string     `json:"projectGuidelineFile,omitempty"`
	Categories           []Category `json:"categories"`
}

type Category struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	Tokens  int     `json:"tokens"`
	Percent float64 `json:"percent"`
}

func Build(options Options) (Report, error) {
	root := strings.TrimSpace(options.WorkspaceRoot)
	if root == "" {
		root = "."
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	provider := options.Provider
	report := Report{
		Contract:     ContractV1,
		Runtime:      RuntimeGo,
		Root:         root,
		ProviderName: strings.TrimSpace(provider.Name),
		ModelID:      strings.TrimSpace(provider.Model),
	}
	if metadata, err := providers.ResolveRuntimeMetadata(provider, providers.Options{}); err == nil {
		report.ProviderKind = string(metadata.ProviderKind)
		report.APIModel = metadata.APIModel
		if report.ModelID == "" {
			report.ModelID = metadata.APIModel
		}
	}

	report.ContextWindow = options.ContextWindow
	if report.ContextWindow <= 0 {
		report.ContextWindow = contextWindowForModel(report.ModelID, report.APIModel)
	}

	categories := []Category{}
	basePrompt := systemPromptFootprint(root, report.ModelID)
	categories = append(categories, category(CategorySystemPrompt, "System prompt", estimateTextTokens(basePrompt), report.ContextWindow))

	projectGuidelines, guidelineFile := readProjectGuidelines(root, options.ProjectContextFiles)
	if guidelineFile != "" {
		report.ProjectGuidelineFile = guidelineFile
		categories = append(categories, category(CategoryProjectGuidelines, "Project guidelines", estimateTextTokens(projectGuidelines), report.ContextWindow))
	}

	if workspaceMap, err := workspaceMapFootprint(root); err != nil {
		categories = append(categories, category(CategoryWorkspaceMap, "Workspace map (error: "+err.Error()+")", 0, report.ContextWindow))
	} else if workspaceMap != "" {
		categories = append(categories, category(CategoryWorkspaceMap, "Workspace map", estimateTextTokens(workspaceMap), report.ContextWindow))
	}

	toolCount, availableTools, deferredTools, toolTokens := estimateRegistryTools(options.Registry, options.DeferThreshold)
	report.ToolCount = toolCount
	report.AvailableToolCount = availableTools
	report.DeferredToolCount = deferredTools
	if toolTokens > 0 {
		categories = append(categories, category(CategoryTools, "Tools", toolTokens, report.ContextWindow))
	}

	used := 0
	for _, cat := range categories {
		used += cat.Tokens
	}
	report.UsedTokens = used
	if report.ContextWindow > 0 {
		report.UsedFraction = float64(report.UsedTokens) / float64(report.ContextWindow)
		report.FreeTokens = report.ContextWindow - report.UsedTokens
		if report.FreeTokens < 0 {
			report.FreeTokens = 0
		}
	}
	categories = append(categories, category(CategoryFree, "Free", report.FreeTokens, report.ContextWindow))
	report.Categories = categories
	return report, nil
}

func workspaceMapFootprint(root string) (string, error) {
	snapshot, err := repomap.Scan(root, repomap.Options{MaxFiles: 300, MaxDepth: 5})
	if err != nil {
		return "", err
	}
	return repomap.RenderPrompt(snapshot, maxWorkspaceMapContextBytes), nil
}

func systemPromptFootprint(root string, modelID string) string {
	parts := []string{agent.BuildSystemPromptPreview(agent.Options{Model: modelID}), "runtime: go"}
	if root = strings.TrimSpace(root); root != "" {
		parts = append(parts, "workspace: "+root)
	}
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		parts = append(parts, "model: "+modelID)
	}
	return strings.Join(parts, "\n")
}

func Format(report Report) string {
	lines := []string{
		"Zero context report",
		"root: " + report.Root,
	}
	if report.ProviderName != "" {
		lines = append(lines, "provider: "+report.ProviderName)
	}
	if report.ModelID != "" {
		lines = append(lines, "model: "+report.ModelID)
	}
	if report.APIModel != "" {
		lines = append(lines, "api_model: "+report.APIModel)
	}
	if report.ContextWindow > 0 {
		lines = append(lines, fmt.Sprintf("usage: %s/%s tokens (%.1f%%)", compactNumber(report.UsedTokens), compactNumber(report.ContextWindow), report.UsedFraction*100))
	} else {
		lines = append(lines, fmt.Sprintf("usage: %s tokens (context window unknown)", compactNumber(report.UsedTokens)))
	}
	if report.ToolCount > 0 {
		toolLine := fmt.Sprintf("tools: %d exposed", report.ToolCount)
		if report.AvailableToolCount > report.ToolCount {
			toolLine += fmt.Sprintf(", %d available, %d deferred", report.AvailableToolCount, report.DeferredToolCount)
		}
		lines = append(lines, toolLine)
	}
	if report.ProjectGuidelineFile != "" {
		lines = append(lines, "project_guidelines: "+report.ProjectGuidelineFile)
	}
	lines = append(lines, "", "Categories:")
	for _, cat := range report.Categories {
		if report.ContextWindow > 0 {
			lines = append(lines, fmt.Sprintf("  %s: %s tokens (%.1f%%)", cat.Name, compactNumber(cat.Tokens), cat.Percent))
		} else {
			lines = append(lines, fmt.Sprintf("  %s: %s tokens", cat.Name, compactNumber(cat.Tokens)))
		}
	}
	return strings.Join(lines, "\n")
}

func contextWindowForModel(modelID string, apiModel string) int {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return 0
	}
	for _, candidate := range []string{modelID, apiModel} {
		if model, ok := registry.Get(candidate); ok {
			return model.ContextLimits.ContextWindow
		}
	}
	return 0
}

func category(key string, name string, tokens int, contextWindow int) Category {
	cat := Category{Key: key, Name: name, Tokens: maxInt(tokens, 0)}
	if contextWindow > 0 {
		cat.Percent = float64(cat.Tokens) / float64(contextWindow) * 100
	}
	return cat
}

func estimateRegistryTools(registry *tools.Registry, deferThreshold int) (int, int, int, int) {
	if registry == nil {
		return 0, 0, 0, 0
	}
	all := registry.All()
	sort.Slice(all, func(left int, right int) bool {
		return all[left].Name() < all[right].Name()
	})
	var visible []tools.Tool
	var deferred []tools.Tool
	for _, tool := range all {
		if tool.Safety().Permission == tools.PermissionDeny {
			continue
		}
		visible = append(visible, tool)
		if tools.IsDeferred(tool) {
			deferred = append(deferred, tool)
		}
	}
	active := deferThreshold > 0 && len(deferred) >= deferThreshold
	exposed := visible
	if active {
		exposed = make([]tools.Tool, 0, len(visible)-len(deferred)+1)
		for _, tool := range visible {
			if !tools.IsDeferred(tool) && tool.Name() != tools.ToolSearchToolName {
				exposed = append(exposed, tool)
			}
		}
		exposed = append(exposed, tools.NewToolSearchTool(registry))
	}

	total := 0
	for _, tool := range exposed {
		total += estimateTextTokens(tool.Name())
		description := tool.Description()
		if active && tool.Name() == tools.ToolSearchToolName {
			description = tools.BuildToolSearchDescription(deferred)
		}
		total += estimateTextTokens(description)
		if encoded, err := json.Marshal(tool.Parameters()); err == nil {
			total += estimateTextTokens(string(encoded))
		}
		total += toolDefinitionOverheadTokens
	}
	return len(exposed), len(visible), len(deferred), total
}

func readProjectGuidelines(root string, names []string) (string, string) {
	if len(names) == 0 {
		names = defaultProjectContextFiles
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		if len(content) > maxProjectContextBytes {
			content = content[:maxProjectContextBytes]
		}
		return content, filepath.ToSlash(name)
	}
	return "", ""
}

func estimateTextTokens(value string) int {
	if value == "" {
		return 0
	}
	// Share the agent's heuristic (non-whitespace bytes / 4) so the context
	// preview matches the scale the compaction loop uses and tracks the real
	// tokenizer far better than naive len/4. Non-empty text reports at least 1.
	tokens := agent.ApproxTextTokens(value)
	if tokens == 0 {
		return 1
	}
	return tokens
}

func compactNumber(value int) string {
	if value < 1000 {
		return fmt.Sprintf("%d", value)
	}
	if value < 1_000_000 {
		return trimFloat(float64(value)/1000) + "k"
	}
	return trimFloat(float64(value)/1_000_000) + "m"
}

func trimFloat(value float64) string {
	text := fmt.Sprintf("%.1f", value)
	return strings.TrimSuffix(text, ".0")
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
