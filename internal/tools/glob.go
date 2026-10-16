package tools

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
)

type globTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (globTool) outputCategory(map[string]any) outputCategory { return outputCategorySearch }

func NewGlobTool(workspaceRoot string) Tool {
	return NewScopedGlobTool(workspaceRoot, nil)
}

func NewScopedGlobTool(workspaceRoot string, scope PathScope) Tool {
	return globTool{
		baseTool: baseTool{
			name:        "glob",
			description: "Find files by glob pattern inside the workspace or an explicitly granted extra root.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"pattern":      {Type: "string", Description: `Glob pattern to match, for example "**/*.go".`},
					"cwd":          {Type: "string", Description: "Directory to scan. Relative paths stay in the workspace; use an absolute path to scan a granted extra root. Defaults to workspace root.", Default: "."},
					"limit":        {Type: "integer", Description: "Maximum matches to return.", Default: 100, Minimum: intPtr(1), Maximum: intPtr(1000)},
					"include_dirs": {Type: "boolean", Description: "Whether directory matches should be included.", Default: false},
				},
				Required:             []string{"pattern"},
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Finds matching paths without reading contents or modifying files."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true, ResourceKeys: scopedScanResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool globTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.runWith(ctx, args, sandboxReadExcluderWithin(nil, tool.workspaceRoot), true)
}

// RunWithOptions falls back to sandboxReadExcluderWithin, not a bare no-op
// excluder, when options.Sandbox is nil — see the matching comment on grepTool
// for why an engine-less call is a real production path this must still cover.
func (tool globTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	return tool.runWith(ctx, args, sandboxReadExcluderWithin(options.Sandbox, tool.workspaceRoot), false)
}

// RunWithSandbox runs glob while skipping subtrees the sandbox policy denies
// reads to (DenyRead). With no DenyRead configured the excluder is a no-op and
// behavior is unchanged.
func (tool globTool) RunWithSandbox(ctx context.Context, args map[string]any, engine *sandbox.Engine) Result {
	return tool.runWith(ctx, args, sandboxReadExcluder(engine), true)
}

func (tool globTool) runWith(ctx context.Context, args map[string]any, exclude readExcluder, directBudget bool) Result {
	pattern, err := aliasedStringArg(args, []string{"pattern", "glob", "match", "query", "expression"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for glob: " + err.Error())
	}
	// Optional with a "." default: treat an explicit empty cwd (a common
	// weak-model quirk) the same as the key being absent rather than erroring.
	cwd, err := aliasedStringArg(args, []string{"cwd", "dir", "directory", "path"}, ".", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for glob: " + err.Error())
	}
	if cwd == "" {
		cwd = "."
	}
	limit, err := intArg(args, "limit", 100, 1, 1000)
	if err != nil {
		return errorResult("Error: Invalid arguments for glob: " + err.Error())
	}
	includeDirs, err := boolArg(args, "include_dirs", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for glob: " + err.Error())
	}

	root, displayRoot, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, cwd)
	if err != nil {
		return errorResult("Error running glob " + fmt.Sprintf("%q", pattern) + ": " + err.Error())
	}
	matcher, err := compileGlob(pattern)
	if err != nil {
		return errorResult("Error running glob " + fmt.Sprintf("%q", pattern) + ": " + err.Error())
	}

	matches, err := scanGlob(ctx, root, displayRoot, matcher, includeDirs, exclude)
	if err != nil {
		if res, ok := searchCancelledResult("glob", err); ok {
			return res
		}
		return errorResult("Error running glob " + fmt.Sprintf("%q", pattern) + ": " + err.Error())
	}
	if len(matches) == 0 {
		return okResult("No matches found for " + pattern)
	}

	sort.Strings(matches)
	totalMatches := len(matches)
	truncated := totalMatches > limit
	if truncated {
		matches = matches[:limit]
	}
	output := strings.Join(matches, "\n")
	if truncated {
		output += fmt.Sprintf("\n\n[truncated: showing first %d of %d matches; increase limit or narrow cwd/pattern]", len(matches), totalMatches)
	}
	meta := map[string]string{}
	meta["pattern"] = pattern
	if truncated {
		meta["truncated"] = "true"
		meta["truncation_reason"] = "limit"
	}

	result := Result{
		Status:    StatusOK,
		Output:    output,
		Truncated: truncated,
		Meta:      meta,
	}
	if directBudget {
		return applyLegacyByteBudgetToResult(result, searchOutputBudgetBytes, "increase limit or narrow cwd/pattern")
	}
	return result
}

func scanGlob(ctx context.Context, root string, displayRoot string, matcher *regexp.Regexp, includeDirs bool, exclude readExcluder) ([]string, error) {
	matches := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		// Checked first, ahead of walkErr: an unscoped scan over a large tree can
		// run long enough that cancelling the run must stop the walk promptly
		// rather than visiting every remaining entry to completion.
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		if entry.IsDir() && shouldSkipDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() && exclude.dirExcluded(path) {
			// Skip a read-denied subtree (sandbox DenyRead) wholesale.
			return filepath.SkipDir
		}
		if entry.IsDir() && !includeDirs {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		normalized := filepath.ToSlash(relative)
		if !entry.IsDir() && shouldSkipWorkspaceFile(normalized) {
			return nil
		}
		// A read-denied path (even an includeDirs directory with a nested allow we
		// still descended into) must not appear in results.
		if exclude.fileExcluded(path) {
			return nil
		}
		if matcher.MatchString(normalized) {
			match := normalized
			// When cwd resolved to an extra (non-workspace) root, resolveScopedPath
			// returns an absolute displayRoot. Emit absolute matches there so the
			// agent can feed them straight back into read_file/edit_file; a bare
			// relative "foo.txt" would otherwise resolve against the workspace and
			// hit the wrong file when the same name exists in both roots.
			if filepath.IsAbs(displayRoot) {
				match = filepath.ToSlash(filepath.Join(displayRoot, relative))
			}
			matches = append(matches, match)
		}
		return nil
	})
	return matches, err
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	return regexp.Compile("^" + globToRegexp(filepath.ToSlash(pattern)) + "$")
}

func globToRegexp(pattern string) string {
	var builder strings.Builder
	for index := 0; index < len(pattern); index++ {
		char := pattern[index]
		switch char {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					builder.WriteString("(?:.*/)?")
				} else {
					builder.WriteString(".*")
				}
			} else {
				builder.WriteString("[^/]*")
			}
		case '?':
			builder.WriteString("[^/]")
		default:
			builder.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	return builder.String()
}
