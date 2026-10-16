package tools

import (
	"context"
	"os"

	"github.com/Gitlawb/zero/internal/imageinput"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// ViewImageToolName is the canonical registry name of the image viewer.
const ViewImageToolName = "view_image"

// NewViewImageTool builds the workspace-scoped image viewer.
func NewViewImageTool(workspaceRoot string) Tool {
	return NewScopedViewImageTool(workspaceRoot, nil)
}

// NewScopedViewImageTool builds the image viewer bound to a path scope.
//
// The scope is not optional decoration. imageinput.LoadFile resolves a relative
// path by joining it to the workspace root and otherwise takes the path as
// given, so on its own it would happily read outside the workspace. Every path
// goes through resolveScopedReadPath first, exactly like read_file, so this tool
// grants no reach that read_file does not already have.
func NewScopedViewImageTool(workspaceRoot string, scope PathScope) Tool {
	return viewImageTool{
		baseTool: baseTool{
			name: ViewImageToolName,
			description: "Look at an image file. Use this to see a screenshot, diagram, mockup or photo " +
				"the user mentioned but did not attach, or one a previous tool wrote to disk. " +
				"Accepts PNG, JPEG, GIF and WebP up to 10 MiB.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"path": {Type: "string", Description: "Path of the image to look at."},
				},
				Required:             []string{"path"},
				AdditionalProperties: false,
			},
			safety: readOnlySafety("Reads an image file without modifying anything."),
			// Same shape as read_file: reads one file, mutates nothing, and is
			// safe to run concurrently.
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true, ResourceKeys: fileResourceKeys},
		},
		workspaceRoot: workspaceRoot,
		scope:         scope,
	}
}

type viewImageTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (tool viewImageTool) Run(_ context.Context, args map[string]any) Result {
	requestedPath, err := aliasedStringArg(args, []string{"path", "file", "file_path", "filepath", "filename", "image"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for " + ViewImageToolName + ": " + err.Error())
	}
	absolutePath, relativePath, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, requestedPath)
	if err != nil {
		return errorResult("Error: " + err.Error())
	}
	// Preserve the loader's early non-regular-file rejection (notably FIFOs).
	// The consumed handle is validated again below; this is only a fast check.
	info, err := os.Stat(absolutePath)
	if err != nil {
		return errorResult("Error viewing " + relativePath + ": " + err.Error())
	}
	if !info.Mode().IsRegular() {
		return errorResult("Error viewing " + relativePath + ": image file must be a regular file")
	}
	file, _, err := ProtectedReadOpen(absolutePath, tool.workspaceRoot)
	if err != nil {
		return errorResult("Error viewing " + relativePath + ": " + err.Error())
	}
	defer file.Close()
	image, err := imageinput.LoadOpenFile(file, absolutePath, tool.workspaceRoot)
	if err != nil {
		// The loader's errors already name the problem (missing, too large,
		// unsupported type) and are safe to show: they carry the path the caller
		// asked for, which it already knows.
		return errorResult("Error viewing " + relativePath + ": " + err.Error())
	}
	return Result{
		Status: StatusOK,
		// The text says what arrived; the image itself reaches the model through
		// the agent loop, which delivers Images on a following user message.
		Output: "Viewing " + relativePath + " (" + image.MediaType + ").",
		Images: []zeroruntime.ImageBlock{image},
		Meta:   map[string]string{"media_type": image.MediaType},
	}
}
