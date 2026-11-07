package specialist

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/modelregistry"
)

type Location string

const (
	LocationBuiltin Location = "builtin"
	LocationUser    Location = "user"
	LocationProject Location = "project"
)

type Metadata struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Extends         string   `json:"extends,omitempty"`
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoningEffort,omitempty"`
	Tools           []string `json:"tools,omitempty"`
}

type Manifest struct {
	Metadata      Metadata  `json:"metadata"`
	SystemPrompt  string    `json:"systemPrompt"`
	ResolvedTools []string  `json:"resolvedTools,omitempty"`
	Location      Location  `json:"location"`
	FilePath      string    `json:"filePath"`
	LastModified  time.Time `json:"lastModified,omitempty"`
	Warnings      []string  `json:"warnings,omitempty"`
}

type Summary struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Extends         string   `json:"extends,omitempty"`
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoningEffort,omitempty"`
	Tools           []string `json:"tools,omitempty"`
	ResolvedTools   []string `json:"resolvedTools,omitempty"`
	Location        Location `json:"location"`
	FilePath        string   `json:"filePath"`
	Warnings        []string `json:"warnings,omitempty"`
}

type Paths struct {
	UserDir    string `json:"userDir"`
	ProjectDir string `json:"projectDir,omitempty"`
}

type LoadOptions struct {
	Paths Paths
}

type LoadResult struct {
	Paths       Paths      `json:"paths"`
	Specialists []Manifest `json:"specialists"`
	Warnings    []string   `json:"warnings,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// specialistFilesMu prevents Zero-managed loads from observing a specialist
// mutation in progress. This is especially important on Windows, where
// ReplaceFileW can briefly leave the destination name absent. It cannot
// synchronize external editors or other Zero processes.
var specialistFilesMu sync.RWMutex

var knownMetadataKeys = map[string]bool{
	"name":            true,
	"description":     true,
	"extends":         true,
	"model":           true,
	"reasoningEffort": true,
	"tools":           true,
}

var toolCategories = map[string][]string{
	"read-only": {"read_file", "read_minified_file", "view_image", "list_directory", "grep", "glob"},
	"edit":      {"read_file", "read_minified_file", "view_image", "list_directory", "grep", "glob", "write_file", "edit_file", "apply_patch"},
	"execute":   {"read_file", "read_minified_file", "view_image", "list_directory", "grep", "glob", "exec_command", "write_stdin", "bash"},
	"plan":      {"update_plan"},
}

var forbiddenToolNames = map[string]bool{
	"Task":               true,
	"TaskOutput":         true,
	"TaskStop":           true,
	"GenerateSpecialist": true,
}

// defaultToolSelection keeps omitted tools conservative until runtime task
// execution has its own permission policy.
var defaultToolSelection = []string{"read-only"}

var knownToolNames = map[string]bool{
	"read_file":           true,
	"read_minified_file":  true,
	"view_image":          true,
	"list_directory":      true,
	"glob":                true,
	"grep":                true,
	"lsp_navigate":        true,
	"skill":               true,
	"ask_user":            true,
	"request_permissions": true,
	"write_file":          true,
	"edit_file":           true,
	"apply_patch":         true,
	"update_plan":         true,
	"exec_command":        true,
	"write_stdin":         true,
	"bash":                true,
	"web_fetch":           true,
	"web_search":          true,
}

func DefaultPaths(workspaceRoot string) (Paths, error) {
	userConfigDir, err := config.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	paths := Paths{
		UserDir: filepath.Join(userConfigDir, "zero", "specialists"),
	}
	if strings.TrimSpace(workspaceRoot) != "" {
		paths.ProjectDir = filepath.Join(filepath.Clean(workspaceRoot), ".zero", "specialists")
	}
	return paths, nil
}

func Load(options LoadOptions) (LoadResult, error) {
	paths := options.Paths
	if strings.TrimSpace(paths.UserDir) == "" {
		resolved, err := DefaultPaths("")
		if err != nil {
			return LoadResult{}, err
		}
		paths.UserDir = resolved.UserDir
	}
	specialistFilesMu.RLock()
	defer specialistFilesMu.RUnlock()

	manifests := Builtins()
	warnings := []string{}
	projectManifests, projectWarnings, err := loadDirectory(paths.ProjectDir, LocationProject)
	if err != nil {
		return LoadResult{}, err
	}
	warnings = append(warnings, projectWarnings...)
	manifests = append(manifests, projectManifests...)
	userManifests, userWarnings, err := loadDirectory(paths.UserDir, LocationUser)
	if err != nil {
		return LoadResult{}, err
	}
	warnings = append(warnings, userWarnings...)
	manifests = append(manifests, userManifests...)

	merged := mergeByName(manifests)
	resolved, err := resolveExtends(merged)
	if err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Paths: paths, Specialists: resolved, Warnings: warnings}, nil
}

func Find(result LoadResult, name string) (Manifest, bool) {
	name = strings.TrimSpace(name)
	for _, manifest := range result.Specialists {
		if manifest.Metadata.Name == name {
			return manifest, true
		}
	}
	return Manifest{}, false
}

func Summaries(manifests []Manifest) []Summary {
	summaries := make([]Summary, 0, len(manifests))
	for _, manifest := range manifests {
		summaries = append(summaries, Summary{
			Name:            manifest.Metadata.Name,
			Description:     manifest.Metadata.Description,
			Extends:         manifest.Metadata.Extends,
			Model:           manifest.Metadata.Model,
			ReasoningEffort: manifest.Metadata.ReasoningEffort,
			Tools:           append([]string(nil), manifest.Metadata.Tools...),
			ResolvedTools:   append([]string(nil), manifest.ResolvedTools...),
			Location:        manifest.Location,
			FilePath:        manifest.FilePath,
			Warnings:        append([]string(nil), manifest.Warnings...),
		})
	}
	return summaries
}

func ParseMarkdown(content string) (Manifest, error) {
	frontmatter, body, err := splitFrontmatter(content)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := parseFrontmatter(frontmatter)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := manifestFromRaw(raw)
	if err != nil {
		return Manifest{}, err
	}
	manifest.SystemPrompt = strings.TrimSpace(body)
	for key := range raw {
		if !knownMetadataKeys[key] {
			manifest.Warnings = append(manifest.Warnings, "unknown frontmatter key: "+key)
		}
	}
	if manifest.SystemPrompt == "" {
		if manifest.Metadata.Description != "" {
			manifest.SystemPrompt = manifest.Metadata.Description
			manifest.Warnings = append(manifest.Warnings, "using description as system prompt")
		} else if manifest.Metadata.Extends == "" {
			return Manifest{}, fmt.Errorf("system prompt cannot be empty")
		}
	}
	if err := Validate(&manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Validate(manifest *Manifest) error {
	manifest.Metadata.Name = strings.TrimSpace(manifest.Metadata.Name)
	manifest.Metadata.Description = strings.TrimSpace(manifest.Metadata.Description)
	manifest.Metadata.Extends = strings.TrimSpace(manifest.Metadata.Extends)
	manifest.Metadata.Model = strings.TrimSpace(manifest.Metadata.Model)
	manifest.Metadata.ReasoningEffort = strings.TrimSpace(manifest.Metadata.ReasoningEffort)
	if manifest.Metadata.Name == "" {
		return fmt.Errorf("specialist name is required")
	}
	if !namePattern.MatchString(manifest.Metadata.Name) {
		return fmt.Errorf("invalid specialist name %q: use lowercase letters, numbers, and dashes", manifest.Metadata.Name)
	}
	if manifest.Metadata.Description == "" && manifest.Metadata.Extends == "" {
		return fmt.Errorf("specialist %q requires a description", manifest.Metadata.Name)
	}
	if manifest.Metadata.Extends != "" && !namePattern.MatchString(manifest.Metadata.Extends) {
		return fmt.Errorf("invalid base specialist name %q: use lowercase letters, numbers, and dashes", manifest.Metadata.Extends)
	}
	if manifest.Metadata.Model != "" {
		registry, err := modelregistry.DefaultRegistry()
		if err != nil {
			return fmt.Errorf("load model registry: %w", err)
		}
		modelID, ok := registry.ResolveID(manifest.Metadata.Model)
		if !ok {
			return fmt.Errorf("specialist %q references unknown model %q", manifest.Metadata.Name, manifest.Metadata.Model)
		}
		manifest.Metadata.Model = modelID
	}
	if manifest.Metadata.ReasoningEffort != "" {
		effort := strings.ToLower(manifest.Metadata.ReasoningEffort)
		if !modelregistry.ValidReasoningEffort(modelregistry.ReasoningEffort(effort)) {
			return fmt.Errorf("specialist %q references unknown reasoning effort %q", manifest.Metadata.Name, manifest.Metadata.ReasoningEffort)
		}
		manifest.Metadata.ReasoningEffort = effort
	}
	resolved, err := ResolveTools(manifest.Metadata.Tools)
	if err != nil {
		return fmt.Errorf("specialist %q: %w", manifest.Metadata.Name, err)
	}
	manifest.ResolvedTools = resolved
	return nil
}

func ResolveTools(selection []string) ([]string, error) {
	if len(selection) == 0 {
		selection = defaultToolSelection
	}
	resolved := []string{}
	seen := map[string]bool{}
	for _, item := range selection {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if forbiddenToolNames[item] {
			return nil, fmt.Errorf("forbidden specialist tool %q", item)
		}
		tools, ok := toolCategories[item]
		if !ok {
			if !knownToolNames[item] {
				return nil, fmt.Errorf("unknown tool or category %q", item)
			}
			tools = []string{item}
		}
		for _, tool := range tools {
			if seen[tool] {
				continue
			}
			seen[tool] = true
			resolved = append(resolved, tool)
		}
	}
	sort.Strings(resolved)
	return resolved, nil
}

func splitFrontmatter(content string) (string, string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", strings.TrimSpace(normalized), nil
	}
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.Join(lines[1:index], "\n"), strings.Join(lines[index+1:], "\n"), nil
		}
	}
	return "", "", fmt.Errorf("frontmatter is missing closing ---")
}

func parseFrontmatter(frontmatter string) (map[string]any, error) {
	raw := map[string]any{}
	if strings.TrimSpace(frontmatter) == "" {
		return raw, nil
	}
	lines := strings.Split(frontmatter, "\n")
	seen := map[string]int{}
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid frontmatter line %d", index+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("invalid empty frontmatter key on line %d", index+1)
		}
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate frontmatter key %q on line %d; first seen on line %d", key, index+1, previous)
		}
		seen[key] = index + 1
		if value == "" && isListKey(key) {
			values, next, err := parseBlockList(lines, index+1)
			if err != nil {
				return nil, err
			}
			raw[key] = values
			index = next - 1
			continue
		}
		if isListKey(key) {
			values, err := parseInlineList(value)
			if err != nil {
				return nil, fmt.Errorf("%s must be an array", key)
			}
			raw[key] = values
			continue
		}
		raw[key] = unquote(value)
	}
	return raw, nil
}

func manifestFromRaw(raw map[string]any) (Manifest, error) {
	metadata := Metadata{}
	for key, value := range raw {
		switch key {
		case "name":
			metadata.Name = stringValue(value)
		case "description":
			metadata.Description = stringValue(value)
		case "extends":
			metadata.Extends = stringValue(value)
		case "model":
			metadata.Model = stringValue(value)
		case "reasoningEffort":
			metadata.ReasoningEffort = stringValue(value)
		case "tools":
			values, ok := value.([]string)
			if !ok {
				return Manifest{}, fmt.Errorf("tools must be an array")
			}
			metadata.Tools = values
		}
	}
	return Manifest{Metadata: metadata}, nil
}

func isListKey(key string) bool {
	return key == "tools"
}

func parseBlockList(lines []string, start int) ([]string, int, error) {
	values := []string{}
	index := start
	for ; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			break
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if value == "" {
			return nil, index, fmt.Errorf("empty list item on frontmatter line %d", index+1)
		}
		values = append(values, unquote(value))
	}
	return values, index, nil
}

func parseInlineList(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("not an inline array")
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	if inner == "" {
		return []string{}, nil
	}
	values := []string{}
	reader := csv.NewReader(strings.NewReader(inner))
	reader.TrimLeadingSpace = true
	fields, err := reader.Read()
	if err != nil {
		return nil, err
	}
	for _, part := range fields {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		values = append(values, unquote(item))
	}
	return values, nil
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		if value[0] == '"' {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return strings.TrimSpace(unquoted)
			}
		}
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func loadDirectory(dir string, location Location) ([]Manifest, []string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read specialist directory %s: %w", dir, err)
	}
	manifests := []Manifest{}
	warnings := []string{}
	for _, entry := range entries {
		// Only *.md is a specialist. Everything else in the directory is
		// deliberately invisible here, including the two kinds of sibling files an
		// interrupted overwrite can leave: a .specialist-*.tmp replacement that was
		// never published, and a .zero-replace-*.backup holding an original whose
		// rollback failed on Windows. Neither is a manifest, and guessing that one
		// of them is would be worse than the gap. The recovery for a backup that
		// really does hold the last good copy is a manual rename, spelled out both
		// in the fsutil error that reports it and in docs/SPECIALISTS.md.
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("skipped symlink specialist manifest: %s", filepath.Join(dir, entry.Name())))
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped unreadable specialist manifest %s: %s", path, err))
			continue
		}
		manifest, err := ParseMarkdown(string(data))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped invalid specialist manifest %s: %s", path, err))
			continue
		}
		manifest.Location = location
		manifest.FilePath = path
		if info, err := entry.Info(); err == nil {
			manifest.LastModified = info.ModTime()
		}
		manifests = append(manifests, manifest)
	}
	return manifests, warnings, nil
}

func mergeByName(manifests []Manifest) []Manifest {
	byName := map[string]Manifest{}
	for _, manifest := range manifests {
		byName[manifest.Metadata.Name] = manifest
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	merged := make([]Manifest, 0, len(names))
	for _, name := range names {
		merged = append(merged, byName[name])
	}
	return merged
}

func resolveExtends(manifests []Manifest) ([]Manifest, error) {
	byName := map[string]Manifest{}
	for _, manifest := range manifests {
		byName[manifest.Metadata.Name] = manifest
	}
	cache := map[string]Manifest{}
	resolved := make([]Manifest, 0, len(manifests))
	for _, manifest := range manifests {
		item, err := resolveManifestExtends(manifest.Metadata.Name, byName, cache, map[string]bool{})
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, item)
	}
	return resolved, nil
}

func resolveManifestExtends(name string, byName map[string]Manifest, cache map[string]Manifest, stack map[string]bool) (Manifest, error) {
	if manifest, ok := cache[name]; ok {
		return manifest, nil
	}
	manifest, ok := byName[name]
	if !ok {
		return Manifest{}, fmt.Errorf("specialist %q not found", name)
	}
	if stack[name] {
		return Manifest{}, fmt.Errorf("cycle detected in specialist extends chain at %q", name)
	}
	stack[name] = true
	defer delete(stack, name)

	baseName := strings.TrimSpace(manifest.Metadata.Extends)
	if baseName == "" {
		cache[name] = manifest
		return manifest, nil
	}
	base, ok := byName[baseName]
	if !ok {
		return Manifest{}, fmt.Errorf("base specialist %q for %q not found", baseName, manifest.Metadata.Name)
	}
	base, err := resolveManifestExtends(base.Metadata.Name, byName, cache, stack)
	if err != nil {
		return Manifest{}, err
	}
	merged := mergeExtends(base, manifest)
	if err := Validate(&merged); err != nil {
		return Manifest{}, err
	}
	cache[name] = merged
	return merged, nil
}

func mergeExtends(base Manifest, child Manifest) Manifest {
	merged := child
	if merged.Metadata.Description == "" {
		merged.Metadata.Description = base.Metadata.Description
	}
	if merged.Metadata.Model == "" {
		merged.Metadata.Model = base.Metadata.Model
	}
	if merged.Metadata.ReasoningEffort == "" {
		merged.Metadata.ReasoningEffort = base.Metadata.ReasoningEffort
	}
	if len(merged.Metadata.Tools) == 0 {
		merged.Metadata.Tools = append([]string(nil), base.Metadata.Tools...)
	} else {
		merged.Metadata.Tools = append([]string(nil), merged.Metadata.Tools...)
	}
	switch {
	case strings.TrimSpace(base.SystemPrompt) == "":
		merged.SystemPrompt = strings.TrimSpace(merged.SystemPrompt)
	case strings.TrimSpace(merged.SystemPrompt) == "":
		merged.SystemPrompt = strings.TrimSpace(base.SystemPrompt)
	default:
		merged.SystemPrompt = strings.TrimSpace(base.SystemPrompt) + "\n\n" + strings.TrimSpace(merged.SystemPrompt)
	}
	merged.Warnings = append(append([]string(nil), base.Warnings...), child.Warnings...)
	return merged
}
