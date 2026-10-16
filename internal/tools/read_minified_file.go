package tools

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/minify"
)

type readMinifiedFileTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (readMinifiedFileTool) outputCategory(map[string]any) outputCategory {
	return outputCategoryFile
}

func NewReadMinifiedFileTool(workspaceRoot string) Tool {
	return NewScopedReadMinifiedFileTool(workspaceRoot, nil)
}

func NewScopedReadMinifiedFileTool(workspaceRoot string, scope PathScope) Tool {
	return readMinifiedFileTool{
		baseTool: baseTool{
			name:        "read_minified_file",
			description: "Read source code in a token-efficient, language-aware form. Use for exploratory understanding of large or unfamiliar source when no exact edit is planned; use read_file directly for likely edit targets. Do not immediately reread the same file exactly unless a new need for exact text or line numbers appears.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"path":   {Type: "string", Description: "Source file path."},
					"offset": {Type: "integer", Description: "Optional 1-based source line to start from.", Minimum: intPtr(1)},
					"limit":  {Type: "integer", Description: "Optional maximum number of source lines to minify.", Minimum: intPtr(1)},
				},
				Required:             []string{"path"},
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Reads a minified view of file contents without modifying files."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true, ResourceKeys: fileResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool readMinifiedFileTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.run(args, RunOptions{}, true)
}

func (tool readMinifiedFileTool) RunWithOptions(_ context.Context, args map[string]any, options RunOptions) Result {
	return tool.run(args, options, false)
}

func (tool readMinifiedFileTool) run(args map[string]any, options RunOptions, directBudget bool) Result {
	requestedPath, err := aliasedStringArg(args, []string{"path", "file", "file_path", "filepath", "filename"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_minified_file: " + err.Error())
	}
	offset, err := intArg(args, "offset", 1, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_minified_file: " + err.Error())
	}
	limit, err := intArg(args, "limit", 0, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_minified_file: " + err.Error())
	}

	absolutePath, relativePath, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, requestedPath)
	if err != nil {
		return errorResult("Error reading file " + requestedPath + ": " + err.Error())
	}

	// protectedReadOpen binds the daemon-token check to the handle the content is
	// actually read from — see internal/tools/protected_credentials.go. This is
	// also the ONLY protection this tool has when called through the plain
	// registry API (Registry.Run, or RunWithOptions with no sandbox engine):
	// unlike read_file's sibling gate through Engine.Evaluate, nothing upstream
	// of this call ever checked the path at all.
	file, info, err := protectedReadOpen(absolutePath, tool.workspaceRoot)
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	content, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	// Record the raw whole-file baseline (matching read_file/edit_file) so a later
	// write can still detect an out-of-Zero modification — the minification only
	// affects what the model SEES, not the tracked on-disk state.
	options.FileTracker.Record(absolutePath, content, info)

	selected := selectSourceLines(content, offset, limit)
	if selected.pastEnd {
		return okResult(fmt.Sprintf("File: %s\n(offset %d is past the end of the file, which has %d lines)", relativePath, offset, selected.totalLines))
	}
	ranged := offset > 1 || limit > 0
	result := minify.File(relativePath, selected.content)
	if ranged {
		result = minify.ContextualFragment(relativePath, content, selected.content, selected.startByte)
	}
	rawLines := lineCount(string(selected.content))
	minLines := lineCount(result.Content)
	pct := 0
	if rawBytes := len(selected.content); rawBytes > 0 {
		if saved := rawBytes - len(result.Content); saved > 0 {
			pct = saved * 100 / rawBytes
		}
	}

	var header string
	if result.Applied {
		header = fmt.Sprintf("File: %s — minified %s view (comments stripped, no line numbers; %d→%d lines, ~%d%% fewer bytes). For exact text/comments or before editing, use read_file.",
			relativePath, result.Language, rawLines, minLines, pct)
	} else if ranged {
		header = fmt.Sprintf("File: %s — safe ranged view (whitespace normalized, no line numbers; %d→%d lines; context-sensitive stripping disabled because the range may begin inside a multiline construct). For exact text, use read_file.",
			relativePath, rawLines, minLines)
	} else {
		header = fmt.Sprintf("File: %s — whitespace-normalized view (no line numbers; %d→%d lines; full minification not available for this file type). For exact text, use read_file.",
			relativePath, rawLines, minLines)
	}

	rawBytes := len(selected.content)
	compactBytes := len(result.Content)
	savedTokens := 0
	if savedBytes := rawBytes - compactBytes; savedBytes > 0 {
		savedTokens = estimatedTokensFromBytes(savedBytes)
	}
	output := header + "\n\n" + result.Content
	meta := map[string]string{}
	meta["path"] = relativePath
	meta["mode"] = result.Language
	meta["compacted"] = strconv.FormatBool(result.Applied)
	meta["raw_bytes"] = strconv.Itoa(rawBytes)
	meta["compact_bytes"] = strconv.Itoa(compactBytes)
	meta["emitted_bytes"] = strconv.Itoa(len(output))
	meta["raw_lines"] = strconv.Itoa(rawLines)
	meta["emitted_lines"] = strconv.Itoa(minLines)
	meta["estimated_tokens_saved"] = strconv.Itoa(savedTokens)
	toolResult := Result{Status: StatusOK, Output: output, Meta: meta}
	if directBudget {
		return applyLegacyByteBudgetToResult(toolResult, readOutputBudgetBytes, "use offset/limit to request a smaller source range, or read_file when exact text is required")
	}
	return toolResult
}

type sourceSelection struct {
	content    []byte
	startByte  int
	totalLines int
	pastEnd    bool
}

func selectSourceLines(content []byte, offset, limit int) sourceSelection {
	if offset <= 1 && limit == 0 {
		return sourceSelection{content: content, totalLines: sourceLineCount(content)}
	}
	lines := strings.Split(string(content), "\n")
	totalLines := sourceLineCount(content)
	start := offset - 1
	if start >= totalLines {
		return sourceSelection{startByte: len(content), totalLines: totalLines, pastEnd: true}
	}
	startByte := 0
	for index := 0; index < start; index++ {
		startByte += len(lines[index]) + 1
	}
	end := totalLines
	if limit > 0 && limit < end-start {
		end = start + limit
	}
	return sourceSelection{
		content:    []byte(strings.Join(lines[start:end], "\n")),
		startByte:  startByte,
		totalLines: totalLines,
	}
}

func sourceLineCount(content []byte) int {
	// One implementation for reader and writer: the tracker contract lives on
	// trackedLineTotal, and this must never drift from it.
	return trackedLineTotal(string(content))
}

// lineCount reports the number of newline-separated lines in s (an empty string
// counts as 0 lines, matching how a reader perceives an empty file).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// trackedLineTotal reports a file's line count the way read_file reports it
// to the FileTracker (newline-terminated lines, plus an unterminated last
// line; an empty file is one line). Writers must record this same number:
// RecordSeenRange resets every observation when the total changes, so a
// writer that recorded lineCount (one higher for a trailing newline) made the
// next partial read_file wipe whole-file knowledge and refuse the next edit.
func trackedLineTotal(s string) int {
	if s == "" {
		return 1
	}
	total := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		total++
	}
	return total
}
