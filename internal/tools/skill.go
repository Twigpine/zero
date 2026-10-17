package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/skills"
)

// skillTool lets the model pull a reusable instruction "skill" into context on
// demand (PRD F15). It reads the skills directory itself via the internal/skills
// loader and returns the named skill's markdown body as its Output, so the model
// can opt into reusable guidance only when relevant. It is read-only.
type skillTool struct {
	baseTool
	dir string
}

// NewSkillTool builds the skill tool. An empty dir resolves to the standard
// skills data directory (skills.DefaultDir); pass an explicit dir in tests.
func NewSkillTool(dir string) *skillTool {
	if strings.TrimSpace(dir) == "" {
		dir = skills.DefaultDir(nil)
	}
	return &skillTool{
		dir: dir,
		baseTool: baseTool{
			name: "skill",
			description: "Load a named Zero skill and return its instructions as the tool output. " +
				"Skills are reusable, on-demand instruction sets (e.g. project conventions or confirmation policies). " +
				"Call this when a relevant skill exists; an unknown name returns the list of available skills.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"name": {
						Type:        "string",
						Description: "The name of the skill to load.",
					},
					"skill": {
						Type:        "string",
						Description: "Alias for name; supply either name or skill.",
					},
				},
				// Intentionally no strict Required: the tool needs exactly one of
				// name/skill, which Run enforces via aliasedStringArg. Declaring both
				// here keeps the alias usable under schema validators that reject
				// unknown keys (AdditionalProperties:false).
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Reads a local skill file; gathers reusable instructions only."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true},
		},
	}
}

// Run loads the named skill and returns its Content. Unknown names return a
// clear error listing the available skill names so the model can self-correct.
func (tool *skillTool) Run(_ context.Context, args map[string]any) Result {
	name, err := aliasedStringArg(args, []string{"name", "skill"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for skill: " + err.Error())
	}

	loaded, err := skills.Load(tool.dir)
	if err != nil {
		return errorResult("Error: failed to load skills: " + err.Error())
	}
	if len(loaded) == 0 {
		return errorResult(fmt.Sprintf("Error: no skills are available (looked in %s).", tool.dir))
	}

	names := make([]string, 0, len(loaded))
	for _, skill := range loaded {
		if skill.Name == name {
			return okResult(skill.Content)
		}
		names = append(names, skill.Name)
	}
	return errorResult(fmt.Sprintf("Error: unknown skill %q. Available skills: %s.", name, strings.Join(names, ", ")))
}
