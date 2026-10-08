package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
)

type applyPatchTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
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
	if isStructuredPatch(patch) {
		return tool.runStructuredPatch(applyRoot, relativeRoot, patch, options)
	}
	// Unified diffs are translated into the same operations and applied by
	// the same os.Root engine, so neither format opens a target by pathname
	// after validation (no check-to-use window) and git is not needed.
	operations, err := parseUnifiedPatch(patch)
	if err != nil {
		return errorResult("Error applying patch: " + err.Error())
	}
	return applyPatchOperations(applyRoot, relativeRoot, operations, options)
}

// changedFilesFromPatch extracts the unique, WORKSPACE-relative paths a patch
// touches, reusing the same per-line parser used for validation. Patch paths are
// relative to the apply cwd, so relativeRoot (the workspace-relative cwd, e.g.
// "sub/dir", or "." for the workspace root) is prefixed so callers get true
// workspace-relative paths regardless of cwd. When the apply cwd resolves to an
// extra write root, resolveScopedPath returns the absolute path as relativeRoot;
// in that case the entries in the returned slice are absolute paths, since
// workspace-relative would be ambiguous there.
func changedFilesFromPatch(relativeRoot string, patch string) []string {
	seen := map[string]bool{}
	var paths []string
	for _, path := range patchHeaderPaths(patch) {
		if path == "" || path == "/dev/null" {
			continue
		}
		workspacePath := path
		if relativeRoot != "" && relativeRoot != "." {
			workspacePath = filepath.ToSlash(filepath.Join(relativeRoot, path))
		}
		if seen[workspacePath] {
			continue
		}
		seen[workspacePath] = true
		paths = append(paths, workspacePath)
	}
	return paths
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

func validatePatchPaths(root string, patch string) error {
	for _, path := range patchHeaderPaths(patch) {
		if path == "" || path == "/dev/null" {
			continue
		}
		// Relative traversal is rejected here; an absolute path is checked by
		// resolveWorkspaceTargetPath, which only accepts one inside the root.
		if path == ".." || strings.HasPrefix(path, "../") {
			return fmt.Errorf("patch path %q must stay inside the workspace", path)
		}
		if _, _, err := resolveWorkspaceTargetPath(root, normalizePatchPathForRoot(root, path)); err != nil {
			return err
		}
	}
	return nil
}

// patchHeaderPaths returns the file paths declared in a unified diff's headers
// (`diff --git` and `---`/`+++` lines). It tracks hunk state by counting body
// lines from each `@@ -a,b +c,d @@` header, so a removed/added content line that
// merely begins with "--- "/"+++ " (e.g. the removal of a markdown line "-- x")
// is NOT mistaken for a file header. This mirrors how `git apply` parses hunks,
// so a line this skips is content git won't write to either — no security gap.
func patchHeaderPaths(patch string) []string {
	var paths []string
	oldRemaining, newRemaining := 0, 0
	inHunk := false
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		if inHunk && (oldRemaining > 0 || newRemaining > 0) {
			switch {
			case strings.HasPrefix(line, "-"):
				oldRemaining--
			case strings.HasPrefix(line, "+"):
				newRemaining--
			case strings.HasPrefix(line, "\\"):
				// "\ No newline at end of file" — not a content line.
			default: // context line (" ...") or a blank context line
				oldRemaining--
				newRemaining--
			}
			continue
		}
		inHunk = false
		switch {
		case strings.HasPrefix(line, "diff --git "):
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				paths = append(paths, stripPatchPrefix(fields[2]), stripPatchPrefix(fields[3]))
			}
		case strings.HasPrefix(line, "@@"):
			oldRemaining, newRemaining = parseHunkCounts(line)
			inHunk = oldRemaining > 0 || newRemaining > 0
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			if p := patchFileHeaderPath(line); p != "" && p != "/dev/null" {
				paths = append(paths, stripPatchPrefix(p))
			}
		}
	}
	return paths
}

func patchFileHeaderPath(line string) string {
	if len(line) < len("--- ") {
		return ""
	}
	rest := line[len("--- "):] // "--- " and "+++ " are both 4 bytes
	if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
		rest = rest[:tab]
	}
	return strings.TrimSpace(unquoteGitPath(rest))
}

// parseHunkCounts reads the old/new line counts from a "@@ -a,b +c,d @@" header.
// A missing count (e.g. "@@ -a +c @@") means 1 per unified-diff convention.
//
// Only the range section BETWEEN the opening and closing "@@" is parsed. A hunk
// header may carry a free-form section heading after the closing "@@" (e.g.
// "@@ -1,1 +1,1 @@ func foo()"), and that text can itself contain "+"/"-"
// tokens. Scanning the whole line would let a crafted heading like
// "@@ -1,1 +1,1 @@ +1,999999" overwrite the real count, keep the parser stuck in
// hunk mode, and swallow later "--- "/"+++ " file headers so they escape
// validatePatchPaths — a workspace-confinement bypass.
func parseHunkCounts(line string) (int, int) {
	_, rest, ok := strings.Cut(line, "@@")
	if !ok {
		return 0, 0
	}
	rangeSection := rest
	if before, _, ok := strings.Cut(rest, "@@"); ok {
		rangeSection = before // drop the section heading after the closing "@@"
	}
	old, next := 0, 0
	for _, field := range strings.Fields(rangeSection) {
		switch {
		case strings.HasPrefix(field, "-"):
			old = hunkCount(field[1:])
		case strings.HasPrefix(field, "+"):
			next = hunkCount(field[1:])
		}
	}
	return old, next
}

func hunkCount(spec string) int {
	if _, count, ok := strings.Cut(spec, ","); ok {
		if n, err := strconv.Atoi(count); err == nil {
			return n
		}
		return 0
	}
	return 1
}

// unquoteGitPath undoes git's C-style quoting of a diff path. Git wraps a path in
// double quotes and backslash-escapes special bytes (spaces, tabs, high bytes as
// octal) when it contains anything unusual; an unquoted path is returned as-is.
func unquoteGitPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if unquoted, err := strconv.Unquote(s); err == nil {
			return unquoted
		}
	}
	return s
}

func stripPatchPrefix(path string) string {
	path = strings.TrimSpace(path)
	// A unified-diff path carries exactly one of the a/ or b/ prefixes; strip a
	// single one so a real directory literally named "a" or "b" is preserved.
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		path = path[2:]
	}
	return filepath.ToSlash(path)
}
