package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/minify"
)

const readMinifiedMaxLoadBytes = 512 * 1024

const (
	readMinifiedDefaultLineLimit = 2000
	readMinifiedMaxLineRunes     = 2000
	readMinifiedMaxWindowBytes   = readMinifiedMaxLoadBytes
	readMinifiedTruncLimit       = "limit"
	readMinifiedTruncByteBudget  = "byte_budget"
	readMinifiedTruncLineClamp   = "line_clamp"
)

// Small files are loaded completely. Large files are streamed as bounded
// pages. Reaching a deep offset may scan a sequential prefix, and binary
// detection covers bytes read or discarded while reaching and emitting it.

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
			description: "Read source code in a token-efficient, language-aware form. Use for exploratory understanding of large or unfamiliar source when no exact edit is planned; use read_file directly for likely edit targets. Do not immediately reread the same file exactly unless a new need for exact text or line numbers appears. Defaults to at most 2000 source lines; large files are not loaded in full. Deep offsets may scan sequentially to reach the requested line.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"path":   {Type: "string", Description: "Source file path."},
					"offset": {Type: "integer", Description: "Optional 1-based source line to start from.", Minimum: intPtr(1)},
					"limit":  {Type: "integer", Description: "Optional maximum number of source lines to minify (default 2000 when omitted).", Minimum: intPtr(1)},
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
	return tool.run(ctx, args, RunOptions{}, true)
}

func (tool readMinifiedFileTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	return tool.run(ctx, args, options, false)
}

func (tool readMinifiedFileTool) run(ctx context.Context, args map[string]any, options RunOptions, directBudget bool) Result {
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
	explicitLimit := limit > 0
	if !explicitLimit {
		limit = readMinifiedDefaultLineLimit
	}

	absolutePath, relativePath, err := resolveScopedReadPath(tool.workspaceRoot, tool.scope, requestedPath)
	if err != nil {
		return errorResult("Error reading file " + requestedPath + ": " + err.Error())
	}
	file, err := openReadableRegularFile(absolutePath)
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}

	classification, err := classifyMinifiedPrefix(ctx, file)
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	if classification.binary {
		return binaryMinifiedResult(relativePath, classification.mime)
	}

	loaded, err := loadMinifiedFile(ctx, file, info.Size(), offset, limit)
	if err != nil {
		return errorResult("Error reading file " + relativePath + ": " + err.Error())
	}
	if loaded.partial {
		if loaded.window.containsNUL || bytesContainMinifiedNUL(loaded.window.content) {
			return binaryMinifiedResult(relativePath, "application/octet-stream")
		}
	} else if bytesContainMinifiedNUL(loaded.content) {
		return binaryMinifiedResult(relativePath, "application/octet-stream")
	}

	var content []byte
	var selected sourceSelection
	var sourceTotal int
	var windowHitLimit bool
	var partialLoad bool
	var lineClamps int
	var loadNote string
	if loaded.partial {
		partialLoad = true
		window := loaded.window
		if window.pastEnd {
			return okResult(fmt.Sprintf("File: %s\n(offset %d is past the end of the file, which has %d lines)", relativePath, offset, window.totalLines))
		}
		content = window.content
		sourceTotal = window.totalLines
		windowHitLimit = window.hitLimit
		lineClamps = window.clampedLines
		selected = sourceSelection{content: content, totalLines: sourceTotal, emitted: window.emitted}
		loadNote = fmt.Sprintf("(note: file is %d bytes; streamed %d source line(s) from offset %d; skipped %d source line(s) before the page; the remainder was not scanned)", info.Size(), window.emitted, offset, window.prefixLinesSkipped)
	} else {
		content = loaded.content
		options.FileTracker.Record(absolutePath, content, info)
		selected = selectSourceLines(content, offset, limit)
		if selected.pastEnd {
			return okResult(fmt.Sprintf("File: %s\n(offset %d is past the end of the file, which has %d lines)", relativePath, offset, selected.totalLines))
		}
		if len(content) == 0 {
			return Result{Status: StatusOK, Output: fmt.Sprintf("File: %s is empty", relativePath), Meta: map[string]string{"empty": "true", "path": relativePath}}
		}
		sourceTotal = selected.totalLines
	}

	selectedSourceLines := selected.emitted
	truncatedByWindow := windowHitLimit
	selectionIncomplete := selected.startByte > 0 || selected.emitted < selected.totalLines
	if !partialLoad && sourceTotal > 0 {
		start := offset
		if start < 1 {
			start = 1
		}
		truncatedByWindow = start-1+selectedSourceLines < sourceTotal
	}
	truncated := truncatedByWindow || lineClamps > 0
	ranged := offset > 1 || explicitLimit || partialLoad || selectionIncomplete || truncated

	var minified minify.Result
	if partialLoad || lineClamps > 0 {
		minified = minify.File("_window.txt", selected.content)
	} else if ranged {
		minified = minify.ContextualFragment(relativePath, content, selected.content, selected.startByte)
	} else {
		minified = minify.File(relativePath, selected.content)
	}

	rawLines := lineCount(string(selected.content))
	minLines := lineCount(minified.Content)
	pct := 0
	if rawBytes := len(selected.content); rawBytes > 0 && len(minified.Content) < rawBytes {
		pct = (rawBytes - len(minified.Content)) * 100 / rawBytes
	}
	header := minifiedHeader(relativePath, minified, rawLines, minLines, pct, ranged || partialLoad)
	if loadNote != "" {
		header += "\n" + loadNote
	}
	if lineClamps > 0 {
		header += fmt.Sprintf("\n(note: %d line(s) exceeded %d characters and were clamped; use read_file with byte_offset/byte_limit for exact bytes)", lineClamps, readMinifiedMaxLineRunes)
	}
	if truncatedByWindow {
		header += fmt.Sprintf("\n[truncated: more source lines remain; set offset=%d to continue]", offset+selectedSourceLines)
	}

	rawBytes := len(selected.content)
	compactBytes := len(minified.Content)
	savedTokens := 0
	if savedBytes := rawBytes - compactBytes; savedBytes > 0 {
		savedTokens = estimatedTokensFromBytes(savedBytes)
	}
	output := header + "\n\n" + minified.Content
	meta := map[string]string{
		"path":                   relativePath,
		"mode":                   minified.Language,
		"compacted":              strconv.FormatBool(minified.Applied),
		"raw_bytes":              strconv.Itoa(rawBytes),
		"compact_bytes":          strconv.Itoa(compactBytes),
		"emitted_bytes":          strconv.Itoa(len(output)),
		"raw_lines":              strconv.Itoa(rawLines),
		"emitted_lines":          strconv.Itoa(minLines),
		"source_lines_emitted":   strconv.Itoa(selectedSourceLines),
		"requested_limit":        strconv.Itoa(limit),
		"estimated_tokens_saved": strconv.Itoa(savedTokens),
	}
	if partialLoad {
		meta["partial_load"] = "true"
	}
	if sourceTotal > 0 {
		if windowHitLimit {
			meta["source_total_lines_min"] = strconv.Itoa(sourceTotal)
		} else {
			meta["source_total_lines"] = strconv.Itoa(sourceTotal)
		}
	}
	if partialLoad && loaded.window.prefixLinesSkipped > 0 {
		meta["prefix_lines_skipped"] = strconv.Itoa(loaded.window.prefixLinesSkipped)
		meta["prefix_bytes_scanned"] = strconv.Itoa(loaded.window.prefixBytesScanned)
	}
	if !explicitLimit {
		meta["default_line_limit"] = strconv.Itoa(limit)
	}
	if truncated {
		meta["truncated"] = "true"
		switch {
		case loaded.window.hitByteLimit:
			meta["truncation_reason"] = readMinifiedTruncByteBudget
		case truncatedByWindow && lineClamps > 0:
			meta["truncation_reason"] = readMinifiedTruncLimit + "+" + readMinifiedTruncLineClamp
		case lineClamps > 0:
			meta["truncation_reason"] = readMinifiedTruncLineClamp
		default:
			meta["truncation_reason"] = readMinifiedTruncLimit
		}
		if lineClamps > 0 {
			meta["clamped_lines"] = strconv.Itoa(lineClamps)
		}
	}

	result := Result{Status: StatusOK, Output: output, Truncated: truncated, Meta: meta}
	if directBudget {
		return applyLegacyByteBudgetToResult(result, readOutputBudgetBytes, "use offset/limit to request a smaller source range, or read_file when exact text is required")
	}
	return result
}

func minifiedHeader(path string, result minify.Result, rawLines, minLines, pct int, ranged bool) string {
	if result.Applied {
		return fmt.Sprintf("File: %s — minified %s view (comments stripped, no line numbers; %d→%d lines, ~%d%% fewer bytes). For exact text/comments or before editing, use read_file.", path, result.Language, rawLines, minLines, pct)
	}
	if ranged {
		return fmt.Sprintf("File: %s — safe ranged view (whitespace normalized, no line numbers; %d→%d lines; context-sensitive stripping disabled because the range may begin inside a multiline construct). For exact text, use read_file.", path, rawLines, minLines)
	}
	return fmt.Sprintf("File: %s — whitespace-normalized view (no line numbers; %d→%d lines; full minification not available for this file type). For exact text, use read_file.", path, rawLines, minLines)
}

type minifiedLoad struct {
	content []byte
	window  fileLineWindow
	partial bool
}

func loadMinifiedFile(ctx context.Context, r io.ReadSeeker, snapshotSize int64, offset, limit int) (minifiedLoad, error) {
	if err := ctx.Err(); err != nil {
		return minifiedLoad{}, err
	}
	if snapshotSize > int64(readMinifiedMaxLoadBytes) {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return minifiedLoad{}, err
		}
		window, err := readFileLineWindow(ctx, r, offset, limit)
		return minifiedLoad{window: window, partial: true}, err
	}
	content, err := readAllContextLimited(ctx, r, readMinifiedMaxLoadBytes+1)
	if err != nil {
		return minifiedLoad{}, err
	}
	if len(content) <= readMinifiedMaxLoadBytes {
		return minifiedLoad{content: content}, nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return minifiedLoad{}, err
	}
	window, err := readFileLineWindow(ctx, r, offset, limit)
	return minifiedLoad{window: window, partial: true}, err
}

func readAllContextLimited(ctx context.Context, r io.Reader, max int) ([]byte, error) {
	return io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: r}, int64(max)))
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

type windowReader struct {
	ctx              context.Context
	r                io.Reader
	remaining        int64
	limited          bool
	syntheticEOF     bool
	probeDone        bool
	moreData         bool
	probeContainsNUL bool
}

func (r *windowReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.limited {
		if r.remaining == 0 {
			if !r.probeDone {
				r.probeDone = true
				var probe [1]byte
				n, probeErr := r.r.Read(probe[:])
				if n > 0 {
					r.moreData = true
					r.probeContainsNUL = probe[0] == 0
				}
				if probeErr != nil && probeErr != io.EOF {
					return 0, probeErr
				}
			}
			r.syntheticEOF = true
			return 0, io.EOF
		}
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
	}
	n, err := r.r.Read(p)
	if r.limited {
		r.remaining -= int64(n)
	}
	return n, err
}

func (r *windowReader) limit(bytes int64) {
	r.remaining = bytes
	r.limited = true
	r.syntheticEOF = false
	r.probeDone = false
	r.moreData = false
	r.probeContainsNUL = false
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type fileLineWindow struct {
	content            []byte
	totalLines         int
	emitted            int
	pastEnd            bool
	hitLimit           bool
	hitByteLimit       bool
	clampedLines       int
	containsNUL        bool
	prefixLinesSkipped int
	prefixBytesScanned int
}

func readFileLineWindow(ctx context.Context, r io.Reader, offset, limit int) (fileLineWindow, error) {
	if offset < 1 {
		offset = 1
	}
	if limit < 1 {
		limit = readMinifiedDefaultLineLimit
	}
	source := &windowReader{ctx: ctx, r: r}
	reader := bufio.NewReader(source)
	var out strings.Builder
	lineNumber := 0
	emitted := 0
	clampedLines := 0
	prefixLinesSkipped := 0
	prefixBytesScanned := 0
	containsNUL := false
	maxKeep := readMinifiedMaxLineRunes * 4
	for {
		if err := ctx.Err(); err != nil {
			return fileLineWindow{}, err
		}
		if lineNumber+1 == offset && !source.limited {
			// The byte budget applies only to the selected page. Reaching a deep
			// offset still scans a sequential prefix; bufio bounds buffered memory,
			// not cumulative prefix I/O.
			source.limit(int64(readMinifiedMaxWindowBytes + 1))
		}
		raw, ended, clipped, lineContainsNUL, lineBytesScanned, err := readRawLineLimited(reader, maxKeep)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fileLineWindow{}, err
		}
		lineNumber++
		containsNUL = containsNUL || lineContainsNUL || source.probeContainsNUL
		if lineNumber < offset {
			prefixLinesSkipped++
			prefixBytesScanned += lineBytesScanned
			continue
		}
		body := trimLineBreak(raw, ended)
		clampedBody, runeClamped := clampMinifiedRunes(string(body), readMinifiedMaxLineRunes)
		separatorBytes := 0
		if emitted > 0 {
			separatorBytes = 1
		}
		if out.Len()+separatorBytes+len(clampedBody) > readMinifiedMaxWindowBytes {
			return fileLineWindow{content: []byte(out.String()), totalLines: lineNumber, emitted: emitted, hitLimit: true, hitByteLimit: true, clampedLines: clampedLines, containsNUL: containsNUL, prefixLinesSkipped: prefixLinesSkipped, prefixBytesScanned: prefixBytesScanned}, nil
		}
		if emitted > 0 {
			out.WriteByte('\n')
		}
		if clipped || runeClamped {
			clampedLines++
		}
		out.WriteString(clampedBody)
		emitted++
		if source.syntheticEOF {
			return fileLineWindow{content: []byte(out.String()), totalLines: lineNumber, emitted: emitted, hitLimit: source.moreData, hitByteLimit: source.moreData, clampedLines: clampedLines, containsNUL: containsNUL, prefixLinesSkipped: prefixLinesSkipped, prefixBytesScanned: prefixBytesScanned}, nil
		}
		if emitted >= limit {
			_, peekErr := reader.Peek(1)
			if peekErr != nil && peekErr != io.EOF {
				return fileLineWindow{}, peekErr
			}
			if peekErr == nil || source.moreData {
				return fileLineWindow{content: []byte(out.String()), totalLines: offset + emitted - 1, emitted: emitted, hitLimit: true, clampedLines: clampedLines, containsNUL: containsNUL, prefixLinesSkipped: prefixLinesSkipped, prefixBytesScanned: prefixBytesScanned}, nil
			}
			break
		}
		if !ended {
			break
		}
	}
	total := lineNumber
	if total == 0 {
		total = 1
	}
	if offset > total {
		return fileLineWindow{totalLines: total, pastEnd: true, containsNUL: containsNUL, prefixLinesSkipped: prefixLinesSkipped, prefixBytesScanned: prefixBytesScanned}, nil
	}
	return fileLineWindow{content: []byte(out.String()), totalLines: total, emitted: emitted, clampedLines: clampedLines, containsNUL: containsNUL, prefixLinesSkipped: prefixLinesSkipped, prefixBytesScanned: prefixBytesScanned}, nil
}

func clampMinifiedRunes(s string, max int) (string, bool) {
	if valid, trimmed := trimInvalidUTF8Suffix(s); trimmed {
		s = valid
		if max <= 0 || len([]rune(s)) <= max {
			return s, true
		}
	}
	if max <= 0 {
		return s, false
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return string(runes[:max]) + "…", true
}

func trimInvalidUTF8Suffix(s string) (string, bool) {
	for index := 0; index < len(s); {
		runeValue, size := utf8.DecodeRuneInString(s[index:])
		if runeValue == utf8.RuneError && size == 1 {
			return s[:index], true
		}
		index += size
	}
	return s, false
}

type sourceSelection struct {
	content    []byte
	startByte  int
	totalLines int
	emitted    int
	pastEnd    bool
}

func selectSourceLines(content []byte, offset, limit int) sourceSelection {
	total := sourceLineCount(content)
	if offset <= 1 && (limit == 0 || limit >= total) {
		return sourceSelection{content: content, totalLines: total, emitted: total}
	}
	lines := strings.Split(string(content), "\n")
	totalLines := total
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
	return sourceSelection{content: []byte(strings.Join(lines[start:end], "\n")), startByte: startByte, totalLines: totalLines, emitted: end - start}
}

func sourceLineCount(content []byte) int {
	// One implementation for reader and writer: the tracker contract lives on
	// trackedLineTotal, and this must never drift from it.
	return trackedLineTotal(string(content))
}

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

type minifiedClassification struct {
	binary bool
	mime   string
}

func classifyMinifiedPrefix(ctx context.Context, file *os.File) (minifiedClassification, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return minifiedClassification{}, err
	}
	buf := make([]byte, 512)
	n, err := io.ReadFull(&contextReader{ctx: ctx, r: file}, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return minifiedClassification{}, err
	}
	buf = buf[:n]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return minifiedClassification{}, err
	}
	mime := http.DetectContentType(buf)
	return minifiedClassification{binary: bytesContainMinifiedNUL(buf) || knownBinaryMinifiedMIME(mime), mime: mime}, nil
}

func bytesContainMinifiedNUL(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0
}

func knownBinaryMinifiedMIME(mime string) bool {
	if strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return true
	}
	switch mime {
	case "application/zip", "application/pdf", "application/gzip", "application/x-gzip", "application/x-7z-compressed", "application/x-rar-compressed", "application/wasm", "application/octet-stream":
		return true
	default:
		return false
	}
}

func binaryMinifiedResult(path, mime string) Result {
	if mime == "" {
		mime = "application/octet-stream"
	}
	return Result{Status: StatusOK, Output: fmt.Sprintf("File: %s looks binary (%s); not shown as text. Use a specialized tool or convert it first.", path, mime), Meta: map[string]string{"binary": "true", "mime": mime, "path": path}}
}
