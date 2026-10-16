package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
)

type applyPatchTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

type applyPatchPreparation struct {
	patch      string
	operations []structuredPatchOperation
	paths      []string
}

func prepareApplyPatchArguments(args map[string]any) (*applyPatchPreparation, error) {
	patch, err := aliasedStringArg(args, []string{"patch", "diff"}, "", true, false)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments for apply_patch: %w", err)
	}
	var operations []structuredPatchOperation
	if isStructuredPatch(patch) {
		operations, err = parseStructuredPatch(patch)
	} else {
		operations, err = parseUnifiedPatch(patch)
	}
	if err != nil {
		return nil, err
	}
	return &applyPatchPreparation{
		patch:      patch,
		operations: operations,
		paths:      structuredPatchOperationPaths(operations),
	}, nil
}

func preparedPatchPaths(prepared *applyPatchPreparation) []string {
	if prepared == nil {
		return nil
	}
	return prepared.paths
}

// ApplyPatchPaths derives preflight paths with the executor's parser. Invalid
// input returns nil so sandbox preflight fails closed; execution reports the
// detailed preparation error. Paths remain relative to the patch's cwd.
func ApplyPatchPaths(args map[string]any) []string {
	prepared, err := prepareApplyPatchArguments(args)
	if err != nil {
		return nil
	}
	return preparedPatchPaths(prepared)
}

func (applyPatchTool) isBuiltInApplyPatch() {}

// PrepareFreeformApplyPatchArguments converts native structured-patch input
// into the ordinary apply_patch argument map used by permission checks and tool
// execution. Absolute patch-header paths select a granted write root and are
// rewritten relative to that root; relative-only patches keep workspace-root
// behavior.
func PrepareFreeformApplyPatchArguments(tool Tool, patch string) (map[string]any, error) {
	preparer, ok := tool.(interface {
		prepareFreeformArguments(string) (map[string]any, error)
	})
	if !ok {
		return nil, fmt.Errorf("tool is not Zero's built-in apply_patch")
	}
	return preparer.prepareFreeformArguments(patch)
}

func (tool applyPatchTool) prepareFreeformArguments(patch string) (map[string]any, error) {
	args := map[string]any{"patch": patch}
	if !isStructuredPatch(patch) {
		return args, nil
	}

	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	selectedRoot := ""
	hasRelativeHeader := false
	for index, line := range lines {
		prefix := ""
		switch {
		case strings.HasPrefix(line, structuredAddFile):
			prefix = structuredAddFile
		case strings.HasPrefix(line, structuredDeleteFile):
			prefix = structuredDeleteFile
		case strings.HasPrefix(line, structuredUpdateFile):
			prefix = structuredUpdateFile
		case strings.HasPrefix(line, structuredMoveTo):
			prefix = structuredMoveTo
		default:
			continue
		}
		path := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if !filepath.IsAbs(path) {
			hasRelativeHeader = true
			continue
		}
		root, relative, err := tool.freeformPatchRoot(path)
		if err != nil {
			return nil, err
		}
		if selectedRoot != "" && selectedRoot != root {
			return nil, fmt.Errorf("freeform patch spans multiple write roots")
		}
		selectedRoot = root
		lines[index] = prefix + filepath.ToSlash(relative)
	}
	if selectedRoot != "" {
		roots, err := scopedRoots(tool.workspaceRoot, tool.scope)
		if err != nil {
			return nil, err
		}
		if hasRelativeHeader && selectedRoot != roots[0] {
			return nil, fmt.Errorf("freeform patch mixes workspace-relative paths with an extra write root")
		}
		args["cwd"] = selectedRoot
		args["patch"] = strings.Join(lines, "\n")
	}
	return args, nil
}

func (tool applyPatchTool) freeformPatchRoot(path string) (string, string, error) {
	roots, err := scopedRoots(tool.workspaceRoot, tool.scope)
	if err != nil {
		return "", "", err
	}
	var firstErr error
	for _, root := range roots {
		candidate := sandbox.NormalizePrefixForRoot(path, root)
		candidate, err = filepath.Abs(candidate)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		relative, err := filepath.Rel(root, candidate)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return root, filepath.ToSlash(relative), nil
		}
		if firstErr == nil {
			firstErr = outsideWorkspaceError(path)
		}
	}
	return "", "", firstErr
}

func NewScopedApplyPatchTool(workspaceRoot string, scope PathScope) Tool {
	return applyPatchTool{
		baseTool: baseTool{
			name:        "apply_patch",
			description: "Apply a multi-hunk or multi-file patch inside the workspace (or a granted extra write root). Structured format:\n*** Begin Patch\n*** Update File: src/app.js\n@@\n unchanged context line\n-removed line\n+added line\n*** End Patch\nUse \"*** Add File: path\" with \"+\" lines to create a file and \"*** Delete File: path\" to remove one; several sections may follow each other. A unified diff (---/+++ headers with a/ b/ prefixes, @@ hunks) is also accepted and applied in-process. Paths are workspace-relative; absolute paths inside the workspace are fine. For a single targeted change, edit_file is simpler.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"patch": {Type: "string", Description: "The structured (*** Begin Patch) or unified-diff patch text."},
					"cwd":   {Type: "string", Description: "Directory where the patch should be applied. Relative paths stay in the workspace; use an absolute path to target a granted extra write root. Defaults to workspace root.", Default: "."},
				},
				Required:             []string{"patch"},
				AdditionalProperties: false,
			},
			safety:       promptSafety(SideEffectWrite, "Applies patch hunks that can create, edit, or delete files."),
			capabilities: ToolCapabilities{Effect: EffectWorkspaceWrite, ThreadSafe: false, ResourceKeys: applyPatchResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool applyPatchTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.RunWithOptions(ctx, args, RunOptions{})
}

func (tool applyPatchTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	patch, err := aliasedStringArg(args, []string{"patch", "diff"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for apply_patch: " + err.Error())
	}
	cwd, err := stringArg(args, "cwd", ".", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for apply_patch: " + err.Error())
	}

	applyRoot, relativeRoot, err := resolveScopedPath(tool.workspaceRoot, tool.scope, cwd)
	if err != nil {
		return errorResult("Error applying patch: " + err.Error())
	}
	prepared := options.preparedApplyPatch
	if prepared == nil || prepared.patch != patch {
		prepared, err = prepareApplyPatchArguments(args)
		if err != nil {
			return errorResult("Error applying patch: " + err.Error())
		}
	}
	if err := validatePatchPaths(applyRoot, prepared.paths); err != nil {
		return errorResult("Error applying patch: " + err.Error())
	}
	return applyPatchOperations(applyRoot, relativeRoot, prepared.operations, options)
}

// normalizePatchPathForRoot resolves platform-level symlinks (macOS /var ->
// /private/var) in the prefix of an absolute patch path that lies outside the
// apply root, so a path the model copied from read_file compares equal to the
// symlink-resolved root. Relative paths are returned unchanged.
func normalizePatchPathForRoot(root string, path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return path
	}
	if evaluated, err := filepath.EvalSymlinks(resolvedRoot); err == nil {
		resolvedRoot = evaluated
	}
	return sandbox.NormalizePrefixForRoot(path, resolvedRoot)
}

func validatePatchPaths(root string, patchPaths []string) error {
	for _, path := range patchPaths {
		if path == "" || path == "/dev/null" {
			continue
		}
		// Relative traversal is rejected here; an absolute path is checked by
		// resolveWorkspaceTargetPath, which only accepts one inside the root.
		if path == ".." || strings.HasPrefix(path, "../") {
			return fmt.Errorf("patch path %q must stay inside the workspace", path)
		}
		absolute, _, err := resolveWorkspaceTargetPath(root, normalizePatchPathForRoot(root, path))
		if err != nil {
			return err
		}
		// The apply path refuses a protected credential per target inside
		// planStructuredPatch; repeat the lexical refusal here so a permission
		// preview never advertises the token as a writable target either.
		if err := protectedMutationDenied(absolute, root); err != nil {
			return err
		}
	}
	return nil
}

// patchFileHeaderPath reads a "--- "/"+++ " header through the same parser that
// produced patchPaths above. The executor must not re-interpret these bytes: a
// second reading that trims or unquotes differently would let the gate authorize
// `bridge-token ` while the executor opens the protected `bridge-token` beside
// it. Only the parser's own refusals are surfaced here.
func patchFileHeaderPath(line string) (string, bool) {
	return sandbox.PatchFileHeaderPath(line)
}

// stripPatchPrefix removes a single a/ or b/ prefix so a real directory named
// "a" or "b" is preserved. It preserves every other byte, including surrounding
// spaces, which are pathname data in an unquoted header operand.
func stripPatchPrefix(path string) string {
	return sandbox.StripPatchPrefix(path)
}
