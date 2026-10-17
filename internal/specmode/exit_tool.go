package specmode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/tools"
)

const (
	SubmitToolName            = "submit_spec"
	ControlSpecReviewRequired = "spec_review_required"
)

type SubmitTool struct {
	workspaceRoot string
	now           func() time.Time
}

func NewSubmitTool(workspaceRoot string, now func() time.Time) SubmitTool {
	return SubmitTool{workspaceRoot: workspaceRoot, now: now}
}

func (tool SubmitTool) Name() string {
	return SubmitToolName
}

func (tool SubmitTool) Description() string {
	return "Save the completed implementation spec and stop for user review before implementation."
}

func (tool SubmitTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"title": {
				Type:        "string",
				Description: "Short 3-6 word title for the spec.",
			},
			"plan": {
				Type:        "string",
				Description: "Complete markdown implementation spec.",
			},
		},
		Required:             []string{"title", "plan"},
		AdditionalProperties: false,
	}
}

func (tool SubmitTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect: tools.SideEffectWrite,
		Permission: tools.PermissionAllow,
		Reason:     "Writes a spec markdown file under the workspace .zero/specs directory and stops for review.",
	}
}

func (tool SubmitTool) Run(_ context.Context, args map[string]any) tools.Result {
	title, err := requiredString(args, "title")
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: Invalid arguments for submit_spec: " + err.Error()}
	}
	plan, err := requiredString(args, "plan")
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: Invalid arguments for submit_spec: " + err.Error()}
	}
	saved, err := SaveDraft(SaveOptions{
		WorkspaceRoot: tool.workspaceRoot,
		Title:         title,
		Plan:          plan,
		Now:           tool.now,
	})
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: Failed to save spec: " + err.Error()}
	}
	output := fmt.Sprintf("Spec saved for review: %s", saved.RelativePath)
	return tools.Result{
		Status: tools.StatusOK,
		Output: output,
		Meta: map[string]string{
			"control":      ControlSpecReviewRequired,
			"specId":       saved.ID,
			"specTitle":    saved.Title,
			"specFilePath": saved.Path,
			"relativePath": saved.RelativePath,
		},
		ChangedFiles: []string{saved.RelativePath},
		Display:      tools.Display{Summary: output, Kind: "file"},
	}
}

func requiredString(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s is required", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return text, nil
}
