package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"unicode/utf8"
)

const readFileByteChunkMax = 64 * 1024

// readFileLinePrefixSeparator follows the line number on every read_file line.
const readFileLinePrefixSeparator = "→"

type readFileTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (readFileTool) outputCategory(map[string]any) outputCategory { return outputCategoryFile }

func NewReadFileTool(workspaceRoot string) Tool {
	return NewScopedReadFileTool(workspaceRoot, nil)
}

func NewScopedReadFileTool(workspaceRoot string, scope PathScope) Tool {
	return readFileTool{
		baseTool: baseTool{
			name:        "read_file",
			description: "Read exact file text, each line prefixed with its line number and →. Use directly for small files likely to be edited, or when comments, formatting, or line numbers matter. Prefer read_minified_file only for exploratory understanding of large or unfamiliar code. Use offset and limit for a line range.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"path":        {Type: "string", Description: "File path."},
					"offset":      {Type: "integer", Description: "Optional 1-based source line to start from.", Minimum: intPtr(1)},
					"limit":       {Type: "integer", Description: "Optional number of source lines to return.", Minimum: intPtr(1)},
					"byte_offset": {Type: "integer", Description: "Optional zero-based offset for exact byte reads.", Minimum: intPtr(0)},
					"byte_limit":  {Type: "integer", Description: "Optional byte count for an exact byte read; use with byte_offset.", Minimum: intPtr(1), Maximum: intPtr(readFileByteChunkMax)},
				},
				Required:             []string{"path"},
				AdditionalProperties: false,
			},
			safety: readOnlySafety("Reads file contents without modifying files."),
			// ThreadSafe: FileTracker is mutex-guarded; concurrent reads of
			// distinct paths are safe. Same-path calls still serialize via
			// resource-key conflict detection in the agent parallel planner.
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true, ResourceKeys: fileResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool readFileTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.run(args, RunOptions{}, true)
}

func (tool readFileTool) RunWithOptions(_ context.Context, args map[string]any, options RunOptions) Result {
	return tool.run(args, options, false)
}

func (tool readFileTool) run(args map[string]any, options RunOptions, directBudget bool) Result {
	requestedPath, err := aliasedStringArg(args, []string{"path", "file", "file_path", "filepath", "filename"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_file: " + err.Error())
	}
	_, hasOffset := args["offset"]
	_, hasLimit := args["limit"]
	_, hasStartLine := args["start_line"]
	_, hasEndLine := args["end_line"]
	_, hasMaxLines := args["max_lines"]
	if hasOffset && hasStartLine {
		return errorResult("Error: Invalid arguments for read_file: use either offset or start_line, not both")
	}
	if hasLimit && (hasEndLine || hasMaxLines) {
		return errorResult("Error: Invalid arguments for read_file: use limit instead of end_line or max_lines, not both")
	}

	startKey := "offset"
	if !hasOffset && hasStartLine {
		startKey = "start_line"
	}
	startLine, err := intArg(args, startKey, 1, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_file: " + err.Error())
	}
	limit, err := intArg(args, "limit", 0, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_file: " + err.Error())
	}
	endLine, err := intArg(args, "end_line", 0, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_file: " + err.Error())
	}
	maxLines, err := intArg(args, "max_lines", 0, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for read_file: " + err.Error())
	}
	if hasLimit {
		const maxInt = int(^uint(0) >> 1)
		endLine = startLine + min(limit-1, maxInt-startLine)
	}
	hasLineRange := hasOffset || hasLimit || hasStartLine || hasEndLine || hasMaxLines
	_, hasByteOffset := args["byte_offset"]
	_, hasByteLimit := args["byte_limit"]
	byteMode := (hasByteOffset || hasByteLimit) && !hasLineRange
	byteOffset := 0
	byteLimit := readFileByteChunkMax
	if byteMode {
		byteOffset, err = intArg(args, "byte_offset", 0, 0, 0)
		if err != nil {
			return errorResult("Error: Invalid arguments for read_file: " + err.Error())
		}
		byteLimit, err = intArg(args, "byte_limit", readFileByteChunkMax, 1, readFileByteChunkMax)
		if err != nil {
			return errorResult("Error: Invalid arguments for read_file: " + err.Error())
		}
	}

	absolutePath, relativePath, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, requestedPath)
	if err != nil {
		return errorResult("Error reading file " + requestedPath + ": " + err.Error())
	}

	stats, err := scanReadFileStats(absolutePath, tool.workspaceRoot)
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	// Record the whole-file baseline (the raw bytes, matching what edit_file and
	// write_file read) so a later write can detect an out-of-Zero modification.
	// Stat is best-effort: a missing FileInfo only drops the diagnostic size/mtime,
	// not the authoritative content hash.
	options.FileTracker.RecordHash(absolutePath, stats.hash, stats.info)
	if byteMode {
		result, seenStart, seenEnd := renderReadFileBytes(absolutePath, relativePath, tool.workspaceRoot, stats.bytes, byteOffset, byteLimit)
		if result.Status == StatusOK && !result.Truncated && seenEnd > seenStart {
			if options.deferFileObservation {
				result.pendingFileObservation = &pendingFileObservation{
					path: absolutePath, output: result.Output, hash: stats.hash,
					start: seenStart, end: seenEnd, total: stats.bytes, byteMode: true,
				}
			} else {
				options.FileTracker.RecordSeenBytes(absolutePath, seenStart, seenEnd, stats.bytes)
				result.Meta["file_version"] = stats.hash
				result.Meta["seen_bytes"] = fmt.Sprintf("%d-%d", seenStart, seenEnd)
			}
		}
		return result
	}

	maxBytes := 0
	if directBudget {
		maxBytes = readOutputBudgetBytes
	}
	result := renderReadFileRange(absolutePath, relativePath, tool.workspaceRoot, stats.lines, startLine, endLine, maxLines, maxBytes)
	if result.Status == StatusOK && result.Meta["truncation_reason"] != "byte_budget" {
		seenStart, seenEnd := renderedReadRange(stats.lines, startLine, endLine, maxLines)
		if options.deferFileObservation {
			result.pendingFileObservation = &pendingFileObservation{
				path: absolutePath, output: result.Output, hash: stats.hash,
				start: seenStart, end: seenEnd, total: stats.lines,
			}
		} else {
			options.FileTracker.RecordSeenRange(absolutePath, seenStart, seenEnd, stats.lines)
			if result.Meta == nil {
				result.Meta = map[string]string{}
			}
			result.Meta["file_version"] = stats.hash
			result.Meta["seen_lines"] = fmt.Sprintf("%d-%d", seenStart, seenEnd)
		}
	}
	return result
}

func renderedReadRange(total, start, end, maxLines int) (int, int) {
	if start > total {
		return start, start
	}
	if end == 0 || end > total {
		end = total
	}
	if end < start {
		end = start
	}
	if maxLines > 0 && end-start+1 > maxLines {
		end = start + maxLines - 1
	}
	return start, end
}

func renderReadFileRange(absolutePath string, relativePath string, workspaceRoot string, total int, startLine int, endLine int, maxLines int, maxBytes int) Result {
	if startLine > total {
		return okResult(fmt.Sprintf("File: %s\n(offset %d is past the end of the file, which has %d lines)", relativePath, startLine, total))
	}
	if endLine == 0 || endLine > total {
		endLine = total
	}
	// A backwards range (end_line < start_line) is a caller slip, not a fatal
	// error. Recover the way an end_line past EOF is already recovered a few lines
	// up (clamped down to the last line): clamp end_line UP to start_line and read
	// just that one line. Erroring here instead cost the caller a whole retry for
	// an off-by-arithmetic mistake, and it was inconsistent — start_line past EOF
	// and end_line past EOF both recover gracefully; only this case hard-failed.
	// The recovery is surfaced in the output (rangeNote) so it is never silent: the
	// caller sees it read a different range than it asked for and why.
	rangeNote := ""
	if endLine < startLine {
		rangeNote = fmt.Sprintf("(note: end_line %d was before start_line %d, so only line %d was read — pass end_line >= start_line to read a wider range)", endLine, startLine, startLine)
		endLine = startLine
	}

	truncated := false
	selectedLines := endLine - startLine + 1
	if maxLines > 0 && selectedLines > maxLines {
		selectedLines = maxLines
		truncated = true
	}

	lastLine := startLine + selectedLines - 1
	header := fmt.Sprintf("File: %s (%d lines)", relativePath, total)
	if startLine != 1 || endLine != total || maxLines > 0 {
		header = fmt.Sprintf("File: %s (lines %d-%d of %d)", relativePath, startLine, lastLine, total)
	}

	// The shared registry boundary applies the file policy after redaction. Its
	// input still needs a bounded capture: retain the requested range's head and
	// tail instead of materializing an arbitrarily large file in memory. Direct
	// Tool.Run calls preserve the legacy prefix-only byte budget.
	var budgetedOutput *outputBudgetBuilder
	if maxBytes > 0 {
		budgetedOutput = newOutputBudgetBuilder(maxBytes, "use offset/limit to request a smaller line range")
	} else {
		budgetedOutput = newHeadTailOutputBudgetBuilder(readOutputBudgetBytes, "use offset/limit to request a smaller line range")
	}
	budgetedOutput.WriteString(header)
	budgetedOutput.WriteString("\n")
	if rangeNote != "" {
		budgetedOutput.WriteString(rangeNote)
		budgetedOutput.WriteString("\n")
	}
	budgetedOutput.WriteString("\n")
	if err := appendReadFileRange(budgetedOutput, absolutePath, workspaceRoot, startLine, selectedLines); err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	if truncated {
		// The Truncated flag alone is invisible to the model in the rendered
		// output, so it cannot tell a limit cut from a complete read. Make the
		// cut explicit and tell it how to continue.
		budgetedOutput.WriteString(fmt.Sprintf("\n\n[truncated: %d more line(s) in the requested range not shown; set offset=%d to continue]", endLine-lastLine, lastLine+1))
	}

	budgeted := budgetedOutput.Result()
	meta := map[string]string{}
	if maxBytes > 0 || budgeted.Truncated {
		for key, value := range outputBudgetMeta(budgeted) {
			meta[key] = value
		}
	}
	if truncated || budgeted.Truncated {
		meta["truncated"] = "true"
		if budgeted.Truncated {
			meta["truncation_reason"] = "byte_budget"
		} else {
			meta["truncation_reason"] = "limit"
		}
	}
	return Result{
		Status:    StatusOK,
		Output:    budgeted.Output,
		Truncated: truncated || budgeted.Truncated,
		Meta:      meta,
	}
}

type readFileStats struct {
	lines int
	bytes int
	hash  string
	info  os.FileInfo
}

func scanReadFileStats(path, workspaceRoot string) (readFileStats, error) {
	file, _, err := protectedReadOpen(path, workspaceRoot)
	if err != nil {
		return readFileStats{}, err
	}
	defer file.Close()

	hasher := sha256.New()
	reader := bufio.NewReader(file)
	lines := 0
	bytes := 0
	for {
		raw, _, err := readRawLine(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return readFileStats{}, err
		}
		if _, err := hasher.Write(raw); err != nil {
			return readFileStats{}, err
		}
		bytes += len(raw)
		lines++
	}
	if lines == 0 {
		lines = 1
	}
	info, _ := file.Stat()
	return readFileStats{lines: lines, bytes: bytes, hash: hex.EncodeToString(hasher.Sum(nil)), info: info}, nil
}

func renderReadFileBytes(path, relativePath, workspaceRoot string, total, requestedStart, limit int) (Result, int, int) {
	if requestedStart >= total {
		return okResult(fmt.Sprintf("File: %s\n(byte_offset %d is past the end of the file, which has %d bytes)", relativePath, requestedStart, total)), 0, 0
	}
	file, _, err := protectedReadOpen(path, workspaceRoot)
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error()), 0, 0
	}
	defer file.Close()

	want := min(limit, total-requestedStart)
	data := make([]byte, want)
	read, err := file.ReadAt(data, int64(requestedStart))
	if err != nil && err != io.EOF {
		return errorResult("Error reading file " + relativePath + ": " + err.Error()), 0, 0
	}
	data = data[:read]
	start := requestedStart
	for len(data) > 0 && data[0]&0xc0 == 0x80 {
		data = data[1:]
		start++
	}
	for trim := 0; !utf8.Valid(data) && trim < utf8.UTFMax && len(data) > 0; trim++ {
		data = data[:len(data)-1]
	}
	if !utf8.Valid(data) {
		return errorResult("Error reading file " + relativePath + ": exact byte mode requires UTF-8 text"), 0, 0
	}
	end := start + len(data)
	if end <= start {
		return errorResult("Error reading file " + relativePath + ": byte_limit is too small to return the next UTF-8 character"), 0, 0
	}

	header := fmt.Sprintf("File: %s (bytes %d-%d of %d)", relativePath, start, end-1, total)
	output := header + "\n\n" + string(data)
	if end < total {
		output += fmt.Sprintf("\n\n[continue with byte_offset=%d]", end)
	}
	return Result{Status: StatusOK, Output: output, Meta: map[string]string{"next_byte_offset": strconv.Itoa(end)}}, start, end
}

func appendReadFileRange(output *outputBudgetBuilder, path string, workspaceRoot string, startLine int, selectedLines int) error {
	file, _, err := protectedReadOpen(path, workspaceRoot)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 1
	emitted := 0
	for emitted < selectedLines {
		raw, ended, err := readRawLine(reader)
		if err == io.EOF {
			raw = nil
			ended = false
		} else if err != nil {
			return err
		}

		if lineNumber >= startLine {
			if emitted > 0 {
				output.WriteString("\n")
			}
			// Compact "N→" prefix: the padded "  N | " form cost ~19% of every
			// read (about 1.5k tokens per 800-line file) and was re-sent on each
			// later call; no consumer parses it, and models read "N→" natively.
			output.WriteString(strconv.Itoa(lineNumber))
			output.WriteString(readFileLinePrefixSeparator)
			output.WriteString(string(trimLineBreak(raw, ended)))
			emitted++
		}
		if err == io.EOF {
			break
		}
		lineNumber++
	}
	return nil
}
