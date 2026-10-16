package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
)

type grepTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (grepTool) outputCategory(map[string]any) outputCategory { return outputCategorySearch }

type grepMatch struct {
	file string
	line int
	text string
	hits int
}

var errGrepLimitReached = errors.New("grep head limit reached")

func NewGrepTool(workspaceRoot string) Tool {
	return NewScopedGrepTool(workspaceRoot, nil)
}

func NewScopedGrepTool(workspaceRoot string, scope PathScope) Tool {
	return grepTool{
		baseTool: baseTool{
			name:        "grep",
			description: "Search file contents with a regular expression inside the workspace or an explicitly granted extra root.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"pattern":          {Type: "string", Description: "Regular expression pattern to search for."},
					"path":             {Type: "string", Description: "Directory or file to search. Relative paths stay in the workspace; use an absolute path to search a granted extra root. Defaults to workspace root.", Default: "."},
					"glob":             {Type: "string", Description: `Optional glob filter, for example "**/*.go".`},
					"output_mode":      {Type: "string", Description: "Output mode.", Enum: []string{"content", "files_with_matches", "count"}, Default: "content"},
					"case_insensitive": {Type: "boolean", Description: "Case insensitive search.", Default: false},
					"head_limit":       {Type: "integer", Description: "Maximum content results to return.", Default: 50, Minimum: intPtr(1)},
				},
				Required:             []string{"pattern"},
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Searches file paths and matching lines without modifying files."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true, ResourceKeys: scopedScanResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool grepTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.runWith(ctx, args, sandboxReadExcluderWithin(nil, tool.workspaceRoot), true)
}

// RunWithOptions falls back to sandboxReadExcluderWithin rather than a bare
// no-op excluder when options.Sandbox is nil. Registry.Run funnels into this
// with an empty RunOptions, which is a real production path (MCP tool
// dispatch that predates a Sandbox engine, and any future caller of the plain
// registry API) — without the fallback, grep could return the bridge token's
// contents from a workspace search whenever no engine happened to be passed,
// even though list_directory's equivalent engine-less path already protects it.
func (tool grepTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	return tool.runWith(ctx, args, sandboxReadExcluderWithin(options.Sandbox, tool.workspaceRoot), false)
}

// RunWithSandbox runs the search while skipping subtrees the sandbox policy
// denies reads to (DenyRead), so grep never surfaces content from a read-denied
// path. With no DenyRead configured the excluder is a no-op and behavior is
// unchanged.
func (tool grepTool) RunWithSandbox(ctx context.Context, args map[string]any, engine *sandbox.Engine) Result {
	return tool.runWith(ctx, args, sandboxReadExcluder(engine), true)
}

func (tool grepTool) runWith(ctx context.Context, args map[string]any, exclude readExcluder, directBudget bool) Result {
	pattern, err := aliasedStringArg(args, []string{"pattern", "query", "regex", "search", "expression"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}
	// Optional with a "." default: treat an explicit empty path (a common
	// weak-model quirk) the same as the key being absent rather than erroring.
	targetPath, err := aliasedStringArg(args, []string{"path", "dir", "directory"}, ".", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}
	if targetPath == "" {
		targetPath = "."
	}
	globPattern, err := stringArg(args, "glob", "", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}
	outputMode, err := stringArg(args, "output_mode", "content", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}
	if outputMode != "content" && outputMode != "files_with_matches" && outputMode != "count" {
		return errorResult("Error: Invalid arguments for grep: output_mode must be content, files_with_matches, or count")
	}
	caseInsensitive, err := boolArg(args, "case_insensitive", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}
	shortInsensitive, err := boolArg(args, "-i", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}
	headLimit, err := intArg(args, "head_limit", 50, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for grep: " + err.Error())
	}

	if caseInsensitive || shortInsensitive {
		pattern = "(?i)" + pattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return errorResult("Error running grep: " + err.Error())
	}

	target, displayRoot, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, targetPath)
	if err != nil {
		return errorResult("Error running grep: " + err.Error())
	}

	// Resolve the workspace root through symlinks ONCE so (a) confinement checks
	// and (b) Rel computations both use the canonical root. tool.workspaceRoot is
	// only Abs-normalized (no EvalSymlinks); using it directly would produce
	// "../"-laden relative paths when the root itself lives under a symlink (e.g.
	// macOS /tmp -> /private/tmp) and would not catch files that resolve outside.
	// When a scope is present, pick the scope root that contains the resolved
	// target so that confineGrepFile computes correct relative paths.
	resolvedRoot, err := resolveGrepRoot(tool.workspaceRoot, tool.scope, target)
	if err != nil {
		return errorResult("Error running grep: " + err.Error())
	}

	var globMatcher *regexp.Regexp
	if globPattern != "" {
		globMatcher, err = compileGlob(globPattern)
		if err != nil {
			return errorResult("Error running grep: " + err.Error())
		}
	}

	absolutePaths := filepath.IsAbs(displayRoot)
	switch outputMode {
	case "count":
		collector := &grepCountCollector{}
		if err := scanGrepMatches(ctx, resolvedRoot, target, globMatcher, exclude, absolutePaths, exactGrepLineMatcher(compiled), collector.collect); err != nil {
			if res, ok := searchCancelledResult("grep", err); ok {
				return res
			}
			return errorResult("Error running grep: " + err.Error())
		}
		return applyDirectSearchBudget(collector.result(), directBudget, "narrow path/glob/pattern to continue")
	case "files_with_matches":
		collector := &grepFileListCollector{}
		if err := scanGrepMatches(ctx, resolvedRoot, target, globMatcher, exclude, absolutePaths, presenceGrepLineMatcher(compiled), collector.collect); err != nil {
			if res, ok := searchCancelledResult("grep", err); ok {
				return res
			}
			return errorResult("Error running grep: " + err.Error())
		}
		return applyDirectSearchBudget(collector.result(), directBudget, "narrow path/glob/pattern to continue")
	default:
		collector := &grepContentCollector{headLimit: headLimit}
		if err := scanGrepMatches(ctx, resolvedRoot, target, globMatcher, exclude, absolutePaths, presenceGrepLineMatcher(compiled), collector.collect); err != nil {
			if res, ok := searchCancelledResult("grep", err); ok {
				return res
			}
			return errorResult("Error running grep: " + err.Error())
		}
		return applyDirectSearchBudget(collector.result(), directBudget, "narrow path/glob/pattern or increase head_limit")
	}
}

func applyDirectSearchBudget(result Result, directBudget bool, hint string) Result {
	if !directBudget {
		return result
	}
	return applyLegacyByteBudgetToResult(result, searchOutputBudgetBytes, hint)
}

// resolveGrepRoot picks the scope root whose EvalSymlinks-resolved path contains
// the already-resolved target, so that confineGrepFile computes correct
// workspace-relative paths even when the target lives in an extra root.
// Falls back to EvalSymlinks(workspaceRoot) when no scoped root matches.
func resolveGrepRoot(workspaceRoot string, scope PathScope, resolvedTarget string) (string, error) {
	roots, err := scopedRoots(workspaceRoot, scope)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolved, resolvedTarget)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
			return resolved, nil
		}
	}
	// Fall back to the workspace root.
	return filepath.EvalSymlinks(workspaceRoot)
}

// confineGrepFile resolves a candidate file through symlinks and returns its
// clean, slash-separated path RELATIVE to the (already symlink-resolved) root.
// It returns ok=false when the resolved file escapes the workspace root, so a
// symlink inside the workspace that points outside is never searched/returned —
// mirroring resolveWorkspaceTargetPath / read_file confinement. resolvedRoot must
// already be EvalSymlinks-resolved so the Rel result is "../"-free for in-root
// files even when the root lives under a symlink.
func confineGrepFile(resolvedRoot string, path string) (string, string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", false
	}
	relative, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return "", "", false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", "", false
	}
	// Return the symlink-resolved absolute path too: callers must read THAT (not
	// the unresolved input) so a symlink swap between this check and the read
	// cannot escape the workspace boundary.
	return filepath.ToSlash(relative), resolved, true
}

func walkGrepFiles(ctx context.Context, resolvedRoot string, target string, globMatcher *regexp.Regexp, exclude readExcluder, visit func(string) error) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		relative, _, ok := confineGrepFile(resolvedRoot, target)
		if !ok {
			return nil
		}
		if shouldSkipWorkspaceFile(relative) {
			return nil
		}
		if exclude.fileExcluded(target) {
			return nil
		}
		// A single explicit file is matched by its base name so a pattern like
		// "*.go" applies regardless of how deep the file sits under the workspace.
		if globMatcher == nil || globMatcher.MatchString(filepath.Base(target)) {
			return visit(target)
		}
		return nil
	}

	return filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		// Checked first, ahead of walkErr: an unscoped search over a large tree
		// (e.g. a broad parent directory, not just the workspace) can run long
		// enough that cancelling the run must stop the walk promptly rather than
		// visiting every remaining entry to completion.
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == target {
				return walkErr
			}
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == target {
			return nil
		}
		if entry.IsDir() && shouldSkipDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			// Skip a read-denied subtree (sandbox DenyRead) wholesale.
			if exclude.dirExcluded(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if exclude.fileExcluded(path) {
			return nil
		}
		// Confine each candidate through symlinks: a symlink inside the workspace
		// pointing to a file OUTSIDE the root must be skipped, not searched.
		relative, _, ok := confineGrepFile(resolvedRoot, path)
		if !ok {
			return nil
		}
		if shouldSkipWorkspaceFile(relative) {
			return nil
		}
		// Match the glob against the path relative to the SEARCH directory, not the
		// workspace root, so "*.go" with path="subdir" matches subdir/a.go (as
		// "a.go") — matching ripgrep's --glob semantics. Falls back to the
		// workspace-relative path if Rel fails.
		globPath := relative
		if rel, relErr := filepath.Rel(target, path); relErr == nil {
			globPath = filepath.ToSlash(rel)
		}
		if globMatcher == nil || globMatcher.MatchString(globPath) {
			if err := visit(path); err != nil {
				return err
			}
		}
		return nil
	})
}

type grepLineMatcher func([]byte) (int, bool)

func presenceGrepLineMatcher(compiled *regexp.Regexp) grepLineMatcher {
	return func(line []byte) (int, bool) {
		if !compiled.Match(line) {
			return 0, false
		}
		return 1, true
	}
}

func exactGrepLineMatcher(compiled *regexp.Regexp) grepLineMatcher {
	return func(line []byte) (int, bool) {
		matches := compiled.FindAllIndex(line, -1)
		if len(matches) == 0 {
			return 0, false
		}
		return len(matches), true
	}
}

func scanGrepMatches(ctx context.Context, resolvedRoot string, target string, globMatcher *regexp.Regexp, exclude readExcluder, absolutePaths bool, matcher grepLineMatcher, emit func(grepMatch) bool) error {
	err := walkGrepFiles(ctx, resolvedRoot, target, globMatcher, exclude, func(file string) error {
		return scanGrepFile(ctx, resolvedRoot, absolutePaths, file, exclude, matcher, emit)
	})
	if errors.Is(err, errGrepLimitReached) {
		return nil
	}
	return err
}

func scanGrepFile(ctx context.Context, resolvedRoot string, absolutePaths bool, file string, exclude readExcluder, matcher grepLineMatcher, emit func(grepMatch) bool) error {
	// Re-confine at read time (defense-in-depth) AND to compute the clean
	// workspace-relative path used in output.
	relative, resolvedPath, ok := confineGrepFile(resolvedRoot, file)
	if !ok {
		return nil
	}
	if shouldSkipWorkspaceFile(relative) {
		return nil
	}
	fileLabel := relative
	if absolutePaths {
		// Extra-root search: report the absolute, symlink-resolved path
		// confineGrepFile already validated, so a bare workspace-relative
		// name can't resolve under the workspace and hit the wrong file when
		// the same name exists in both roots.
		fileLabel = filepath.ToSlash(resolvedPath)
	}

	// Read the symlink-RESOLVED path that confineGrepFile validated, not the
	// raw candidate, so a symlink swapped in after the check can't escape.
	handle, err := os.Open(resolvedPath)
	if err != nil {
		return nil
	}
	defer handle.Close()

	// The walk-time exclusion above inspected a PATHNAME; this open selects the
	// object. A workspace writer can replace an ordinary candidate with a
	// symlink or hard link to the protected credential in between, so the
	// decision that actually authorizes the scan has to come from this handle's
	// own metadata — the same binding protectedReadOpen and MCP resources/read
	// use. The earlier check stays as a walk pruning optimization.
	info, err := handle.Stat()
	if err != nil || exclude.openedFileExcluded(resolvedPath, handle, info) {
		return nil
	}

	reader := bufio.NewReader(handle)
	lineNumber := 1
	sawLine := false
	lastEnded := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, ended, err := readRawLine(reader)
		if err == io.EOF {
			if !sawLine || lastEnded {
				return emitGrepLine(matcher, fileLabel, lineNumber, nil, emit)
			}
			return nil
		}
		if err != nil {
			return nil
		}
		sawLine = true
		lastEnded = ended
		line := trimTrailingCarriageReturns(trimLineBreak(raw, ended))
		if err := emitGrepLine(matcher, fileLabel, lineNumber, line, emit); err != nil {
			return err
		}
		lineNumber++
	}
}

func trimTrailingCarriageReturns(line []byte) []byte {
	for len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func emitGrepLine(matcher grepLineMatcher, fileLabel string, lineNumber int, line []byte, emit func(grepMatch) bool) error {
	hits, ok := matcher(line)
	if !ok {
		return nil
	}
	if !emit(grepMatch{
		file: fileLabel,
		line: lineNumber,
		text: string(line),
		hits: hits,
	}) {
		return errGrepLimitReached
	}
	return nil
}

type grepCountCollector struct {
	hits int
}

func (collector *grepCountCollector) collect(match grepMatch) bool {
	collector.hits += match.hits
	return true
}

func (collector *grepCountCollector) result() Result {
	return okResult(fmt.Sprintf("%d matches found", collector.hits))
}

type grepFileListCollector struct {
	files []string
	seen  map[string]bool
}

func (collector *grepFileListCollector) collect(match grepMatch) bool {
	if collector.seen == nil {
		collector.seen = map[string]bool{}
	}
	if collector.seen[match.file] {
		return true
	}
	collector.seen[match.file] = true
	collector.files = append(collector.files, match.file)
	return true
}

func (collector *grepFileListCollector) result() Result {
	if len(collector.files) == 0 {
		return okResult("No matches found.")
	}
	sort.Strings(collector.files)
	return Result{Status: StatusOK, Output: strings.Join(collector.files, "\n")}
}

type grepContentCollector struct {
	headLimit   int
	matches     []grepMatch
	matchesSeen int
	truncated   bool
}

func (collector *grepContentCollector) collect(match grepMatch) bool {
	collector.matchesSeen++
	if len(collector.matches) < collector.headLimit {
		collector.matches = append(collector.matches, match)
		return true
	}
	collector.truncated = true
	return false
}

func (collector *grepContentCollector) result() Result {
	if collector.matchesSeen == 0 {
		return okResult("No matches found.")
	}
	lines := make([]string, 0, len(collector.matches))
	for _, match := range collector.matches {
		lines = append(lines, fmt.Sprintf("%s:%d: %s", match.file, match.line, match.text))
	}
	truncated := collector.truncated
	output := strings.Join(lines, "\n")
	if truncated {
		output += fmt.Sprintf("\n\n[truncated: showing first %d matches; narrow path/glob/pattern or increase head_limit]", len(lines))
	}
	meta := map[string]string{}
	if truncated {
		meta["truncated"] = "true"
		meta["truncation_reason"] = "head_limit"
	}
	return Result{
		Status:    StatusOK,
		Output:    output,
		Truncated: truncated,
		Meta:      meta,
	}
}
