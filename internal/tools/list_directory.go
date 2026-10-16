package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type listDirectoryTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (listDirectoryTool) outputCategory(map[string]any) outputCategory {
	return outputCategorySearch
}

func NewListDirectoryTool(workspaceRoot string) Tool {
	return NewScopedListDirectoryTool(workspaceRoot, nil)
}

func NewScopedListDirectoryTool(workspaceRoot string, scope PathScope) Tool {
	return listDirectoryTool{
		baseTool: baseTool{
			name:        "list_directory",
			description: "List files and directories in a workspace path with optional recursion.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"path":      {Type: "string", Description: "Directory to list. Defaults to workspace root.", Default: "."},
					"recursive": {Type: "boolean", Description: "Whether to list recursively.", Default: false},
					"max_depth": {Type: "integer", Description: "Maximum recursion depth when recursive is true.", Default: 2, Minimum: intPtr(1), Maximum: intPtr(5)},
				},
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Lists directory entries without modifying files."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true, ResourceKeys: directoryResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool listDirectoryTool) Run(_ context.Context, args map[string]any) Result {
	return tool.run(args, sandboxReadExcluderWithin(nil, tool.workspaceRoot), true)
}

// Registry.Run funnels into RunWithOptions with empty options, so a nil engine
// here is the MCP / legacy path rather than a test shape. Listing a directory
// must not disclose the bridge token's filename merely because the caller
// arrived without a sandbox — see sandboxReadExcluderWithin.
func (tool listDirectoryTool) RunWithOptions(_ context.Context, args map[string]any, options RunOptions) Result {
	return tool.run(args, sandboxReadExcluderWithin(options.Sandbox, tool.workspaceRoot), false)
}

func (tool listDirectoryTool) run(args map[string]any, exclude readExcluder, directBudget bool) Result {
	// Optional with a "." default: treat an explicit empty path (a common
	// weak-model quirk) the same as the key being absent rather than erroring.
	requestedPath, err := aliasedStringArg(args, []string{"path", "directory", "dir"}, ".", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for list_directory: " + err.Error())
	}
	if requestedPath == "" {
		requestedPath = "."
	}
	recursive, err := boolArg(args, "recursive", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for list_directory: " + err.Error())
	}
	maxDepth, err := intArg(args, "max_depth", 2, 1, 5)
	if err != nil {
		return errorResult("Error: Invalid arguments for list_directory: " + err.Error())
	}
	if !recursive {
		maxDepth = 0
	}

	absolutePath, relativePath, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, requestedPath)
	if err != nil {
		return errorResult("Error listing directory " + requestedPath + ": " + err.Error())
	}

	entries, err := listDirectoryEntries(absolutePath, 0, maxDepth, exclude)
	if err != nil {
		return errorResult("Error listing directory " + relativePath + ": " + err.Error())
	}
	if len(entries) == 0 {
		return okResult("Directory is empty: " + relativePath)
	}
	output := "Contents of " + relativePath + ":\n\n" + strings.Join(entries, "\n")
	result := Result{Status: StatusOK, Output: output}
	if directBudget {
		return applyLegacyByteBudgetToResult(result, searchOutputBudgetBytes, "use path, recursive=false, or a smaller max_depth to narrow the listing")
	}
	return result
}

func listDirectoryEntries(path string, depth int, maxDepth int, exclude readExcluder) ([]string, error) {
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	sort.Slice(dirEntries, func(left int, right int) bool {
		if dirEntries[left].IsDir() != dirEntries[right].IsDir() {
			return dirEntries[left].IsDir()
		}
		return dirEntries[left].Name() < dirEntries[right].Name()
	})

	results := make([]string, 0, len(dirEntries))
	for _, entry := range dirEntries {
		if entry.IsDir() && shouldSkipDirectory(entry.Name()) {
			continue
		}
		entryPath := filepath.Join(path, entry.Name())
		if entry.IsDir() && exclude.dirExcluded(entryPath) {
			continue
		}

		indent := strings.Repeat("  ", depth)
		if entry.IsDir() {
			// A denied directory with a nested AllowRead cannot be pruned, but the
			// denied directory entry itself must still stay out of the listing.
			if !exclude.fileExcluded(entryPath) {
				results = append(results, indent+entry.Name()+"/")
			}
			if depth < maxDepth {
				children, err := listDirectoryEntries(entryPath, depth+1, maxDepth, exclude)
				if err == nil {
					results = append(results, children...)
				}
			}
			continue
		}
		if exclude.fileExcluded(entryPath) {
			continue
		}

		results = append(results, fmt.Sprintf("%s%s", indent, entry.Name()))
	}

	return results, nil
}
