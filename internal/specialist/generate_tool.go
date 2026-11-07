package specialist

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/Twigpine/zero/internal/tools"
)

type GenerateTool struct {
	storage *Storage
}

type generateParameters struct {
	Name         string
	Description  string
	SystemPrompt string
	Location     Location
	Tools        []string
	Overwrite    bool
}

func NewGenerateTool(storage *Storage) *GenerateTool {
	return &GenerateTool{storage: storage}
}

func (tool *GenerateTool) Name() string {
	return "GenerateSpecialist"
}

func (tool *GenerateTool) Description() string {
	return "Create a project-local Zero specialist profile from a designed name, description, and system prompt."
}

func (tool *GenerateTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"name": {
				Type:        "string",
				Description: "Specialist identifier, such as api-reviewer. Derived from description when omitted.",
			},
			"description": {
				Type:        "string",
				Description: "Short user-facing description of the specialist.",
			},
			"system_prompt": {
				Type:        "string",
				Description: "Full instructions the specialist should follow.",
			},
			"location": {
				Type:        "string",
				Description: "Where to save the specialist. GenerateSpecialist is project-scoped.",
				Enum:        []string{"project"},
				Default:     "project",
			},
			"tools": {
				Type:        "array",
				Description: "Tool categories or tool ids for the specialist, such as read-only, edit, execute, or plan.",
				Items:       &tools.PropertySchema{Type: "string"},
			},
			"overwrite": {
				Type:        "boolean",
				Description: "Replace an existing specialist with the same name.",
				Default:     false,
			},
		},
		Required:             []string{"description"},
		AdditionalProperties: false,
	}
}

func (tool *GenerateTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectWrite,
		Permission:      tools.PermissionPrompt,
		Reason:          "Writes a project specialist profile inside the workspace.",
		AdvertiseInAuto: true,
	}
}

func (tool *GenerateTool) Run(ctx context.Context, args map[string]any) tools.Result {
	params, err := parseGenerateParameters(args)
	if err != nil {
		return taskError(err)
	}
	if tool.storage == nil {
		return taskError(fmt.Errorf("specialist storage is not configured"))
	}
	systemPrompt := strings.TrimSpace(params.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = defaultGeneratedSystemPrompt(params.Description)
	}
	manifest, err := tool.storage.Create(CreateInput{
		Name:         params.Name,
		Description:  params.Description,
		SystemPrompt: systemPrompt,
		Location:     params.Location,
		Tools:        params.Tools,
		Overwrite:    params.Overwrite,
	})
	if err != nil {
		return taskError(err)
	}
	return generateToolResult(manifest)
}

func generateToolResult(manifest Manifest) tools.Result {
	lines := []string{
		fmt.Sprintf("specialist: %s", manifest.Metadata.Name),
		fmt.Sprintf("location: %s", manifest.Location),
		fmt.Sprintf("path: %s", manifest.FilePath),
	}
	for _, warning := range manifest.Warnings {
		lines = append(lines, "warning: "+warning)
	}
	return tools.Result{
		Status: tools.StatusOK,
		Output: strings.Join(lines, "\n"),
		Meta: map[string]string{
			"name":     manifest.Metadata.Name,
			"location": string(manifest.Location),
			"path":     manifest.FilePath,
		},
	}
}

func parseGenerateParameters(args map[string]any) (generateParameters, error) {
	description, err := optionalTaskString(args, "description")
	if err != nil {
		return generateParameters{}, err
	}
	name, err := optionalTaskString(args, "name")
	if err != nil {
		return generateParameters{}, err
	}
	systemPrompt, err := optionalTaskString(args, "system_prompt")
	if err != nil {
		return generateParameters{}, err
	}
	location, err := optionalTaskString(args, "location")
	if err != nil {
		return generateParameters{}, err
	}
	toolsValue, err := optionalStringList(args, "tools")
	if err != nil {
		return generateParameters{}, err
	}
	overwrite, err := optionalTaskBool(args, "overwrite")
	if err != nil {
		return generateParameters{}, err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return generateParameters{}, fmt.Errorf("generate specialist requires description")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slugFromDescription(description)
	} else if !namePattern.MatchString(name) {
		return generateParameters{}, fmt.Errorf("invalid specialist name %q: use lowercase letters, numbers, and dashes", name)
	}
	locationValue, err := parseWritableLocation(location)
	if err != nil {
		return generateParameters{}, err
	}
	return generateParameters{
		Name:         name,
		Description:  description,
		SystemPrompt: strings.TrimSpace(systemPrompt),
		Location:     locationValue,
		Tools:        toolsValue,
		Overwrite:    overwrite,
	}, nil
}

func optionalStringList(args map[string]any, key string) ([]string, error) {
	if args == nil {
		return nil, nil
	}
	value, ok := args[key]
	if !ok || value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case []string:
		return trimStringList(typed), nil
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("parameter %s must be an array of strings", key)
			}
			values = append(values, text)
		}
		return trimStringList(values), nil
	case string:
		return SplitToolList(typed), nil
	default:
		return nil, fmt.Errorf("parameter %s must be an array of strings", key)
	}
}

func parseWritableLocation(value string) (Location, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "project":
		return LocationProject, nil
	default:
		return "", fmt.Errorf("generate specialist location must be project")
	}
}

func defaultGeneratedSystemPrompt(description string) string {
	description = strings.TrimSpace(description)
	return strings.Join([]string{
		"You are a focused Zero specialist.",
		"",
		"Purpose: " + description,
		"",
		"Stay within this purpose, use only the tools made available to you, and report concise findings or changes back to the parent agent.",
	}, "\n")
}

func slugFromDescription(description string) string {
	var builder strings.Builder
	lastDash := false
	for _, char := range strings.ToLower(description) {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
			lastDash = false
		case char >= '0' && char <= '9':
			if builder.Len() > 0 {
				builder.WriteRune(char)
				lastDash = false
			}
		case unicode.IsSpace(char) || char == '-' || char == '_':
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
		if builder.Len() >= 64 {
			break
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" || slug[0] < 'a' || slug[0] > 'z' {
		return "specialist"
	}
	return slug
}

// SplitToolList parses comma- or whitespace-separated specialist tool selections.
func SplitToolList(value string) []string {
	items := []string{}
	for _, item := range strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == ' ' || char == '\t' || char == '\n' || char == '\r'
	}) {
		item = strings.TrimSpace(item)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}
