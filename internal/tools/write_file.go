package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writePreimageBeforeObserve is a deterministic test hook. Production leaves
// it nil. Tests substitute the pathname after the write root is open and
// before existence or bytes are read, so a dissenting absolute-path resolution
// cannot be mistaken for the rooted object.
var writePreimageBeforeObserve func(absolutePath string)

type writeFileTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
	// readRooted reads an existing file through the granted write root. Tests
	// replace it to simulate an unreadable preimage; production uses
	// readRootedFile so bytes and identity come from the same descriptor.
	readRooted func(*os.Root, string) ([]byte, os.FileInfo, error)
}

func NewScopedWriteFileTool(workspaceRoot string, scope PathScope) Tool {
	return writeFileTool{
		baseTool: baseTool{
			name:        "write_file",
			description: "Create a new file, refusing to overwrite existing files unless overwrite is true.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"path":         {Type: "string", Description: "Absolute or relative path of the file to write."},
					"content":      {Type: "string", Description: "Full file contents to write."},
					"overwrite":    {Type: "boolean", Description: "Whether to allow overwriting an existing file.", Default: false},
					"bom":          {Type: "string", Enum: []string{"auto", "add", "remove"}, Default: "auto", Description: "Existing-file overwrites only: auto preserves an existing or supplied UTF-8 BOM; add/remove explicitly sets its presence. New files retain content bytes. Reapplied after optional formatting."},
					"line_endings": {Type: "string", Enum: []string{"auto", "lf", "crlf"}, Default: "auto", Description: "Existing-file overwrites only: auto preserves dominant existing endings (or supplied dominant CRLF); lf/crlf explicitly selects endings, independently of bom. New files retain content bytes. Reapplied after optional formatting."},
				},
				Required:             []string{"path", "content"},
				AdditionalProperties: false,
			},
			safety:       promptSafety(SideEffectWrite, "Creates or overwrites files."),
			capabilities: ToolCapabilities{Effect: EffectWorkspaceWrite, ThreadSafe: false, ResourceKeys: fileResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
		readRooted:    readRootedFile,
	}
}

func (tool writeFileTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.RunWithOptions(ctx, args, RunOptions{})
}

func (tool writeFileTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	requestedPath, err := aliasedStringArg(args, []string{"path", "file", "file_path", "filename"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_file: " + err.Error())
	}
	content, err := fileContentArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_file: " + err.Error())
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_file: " + err.Error())
	}
	bom, err := stringArg(args, "bom", "auto", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_file: " + err.Error())
	}
	if bom != "auto" && bom != "add" && bom != "remove" {
		return errorResult("Error: Invalid arguments for write_file: bom must be auto, add, or remove")
	}
	lineEndings, err := stringArg(args, "line_endings", "auto", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_file: " + err.Error())
	}
	if lineEndings != "auto" && lineEndings != "lf" && lineEndings != "crlf" {
		return errorResult("Error: Invalid arguments for write_file: line_endings must be auto, lf, or crlf")
	}

	absolutePath, relativePath, err := resolveScopedTargetPath(tool.workspaceRoot, tool.scope, requestedPath)
	if err != nil {
		return errorResult("Error writing file " + requestedPath + ": " + err.Error())
	}
	// Anchor observation and mutation on the granted write root. The pathname
	// checks above say what was REQUESTED; this handle enforces what actually
	// happens, so a parent directory swapped for an escaping symlink after
	// validation cannot redirect the create or overwrite. Existence and prior
	// bytes must come from the same handle: os.Stat on the absolute path stores
	// a pathname and, on Windows, fills volume and file index only when
	// os.SameFile re-opens that path, so a same-name substitution compares equal.
	root, rootedRelative, err := openScopedWriteRoot(tool.workspaceRoot, tool.scope, absolutePath)
	if err != nil {
		return errorResult("Error writing file " + relativePath + ": " + err.Error())
	}
	defer root.Close()
	if writePreimageBeforeObserve != nil {
		writePreimageBeforeObserve(absolutePath)
	}

	existed := false
	var priorInfo os.FileInfo
	if info, err := root.Stat(rootedRelative); err == nil {
		existed = true
		priorInfo = info
		if !overwrite {
			return errorResult("Error: " + relativePath + " already exists. Pass overwrite: true to replace it.")
		}
	} else if !os.IsNotExist(err) {
		return errorResult("Error writing file " + relativePath + ": " + err.Error())
	}

	// On overwrite, refuse to clobber a tracked file that changed on disk outside
	// Zero since it was last read — the new content was likely composed against a
	// stale view. The rooted read below is also the preimage: one descriptor-bound
	// snapshot supplies both the conflict check and the bytes passed to commit,
	// and those bytes carry what read_file normalizes away — a BOM, CRLF endings —
	// so the rewrite can keep them.
	readRooted := tool.readRooted
	if readRooted == nil {
		readRooted = readRootedFile
	}
	priorContent := ""
	priorContentKnown := !existed
	var encoding writeFileEncoding
	if existed {
		if options.FileTracker != nil && !options.FileTracker.SeenWhole(absolutePath) {
			return errorResult(fileUnseenMessage(relativePath))
		}
		current, info, rerr := readRooted(root, rootedRelative)
		if _, tracked := options.FileTracker.Version(absolutePath); tracked {
			// Fail CLOSED: if the tracked file can't be re-read to verify it, refuse
			// the overwrite rather than clobbering a file whose current state is
			// unknown (it may have been replaced or removed out from under us).
			if rerr != nil {
				return errorResult(fileConflictMessage(relativePath))
			}
			if cerr := options.FileTracker.CheckConflict(absolutePath, current); cerr != nil {
				return errorResult(fileConflictMessage(relativePath))
			}
		}
		// Fail CLOSED when an existing target cannot be read: those bytes are the
		// only evidence of the convention to restore, so overwriting without them
		// would write the model's normalized content over a CRLF/BOM file and
		// silently destroy exactly what this read exists to preserve.
		if rerr != nil {
			return errorResult("Error writing file " + relativePath + ": cannot read the existing file to preserve its line endings and BOM: " + rerr.Error())
		}
		// Prefer the identity from the opened object over root.Stat. Both
		// capture it at observation time; the opened object is the one whose
		// bytes were just read.
		priorInfo = info
		priorContent = string(current)
		priorContentKnown = true
		encoding = resolveWriteFileEncoding(current, content, bom, lineEndings)
		content = encoding.apply(content)
	}
	modelEquivalentContent := content

	if dir := filepath.Dir(rootedRelative); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return errorResult("Error writing file " + relativePath + ": " + err.Error())
		}
	}
	if err := recheckScopedWriteTarget(tool.workspaceRoot, tool.scope, requestedPath); err != nil {
		return errorResult("Error writing file " + relativePath + ": " + err.Error())
	}
	var expectedContent *string
	if priorContentKnown {
		expectedContent = &priorContent
	}
	if err := commitRootedFileContents(root, absolutePath, rootedRelative, priorInfo, expectedContent, content); err != nil {
		return errorResult("Error writing file " + relativePath + ": " + err.Error())
	}
	// Optional format-on-write (ZERO_FORMAT_ON_WRITE). Must run BEFORE the
	// FileTracker baseline: recording pre-format content would make the very
	// next edit look like an external modification and trip the conflict guard.
	restoreMode := os.FileMode(0o644)
	if priorInfo != nil {
		restoreMode = priorInfo.Mode().Perm()
	}
	formatting := maybeFormatWrittenFileScoped(ctx, tool.workspaceRoot, tool.scope, absolutePath, content, restoreMode)
	content = formatting.Content
	finalContentKnown := formatting.ContentKnown
	// Baseline the freshly written content so a later edit/overwrite in this
	// session compares against what is now on disk.
	newInfo := formatting.Info
	if newInfo == nil {
		newInfo, _ = root.Stat(rootedRelative)
	}
	// Encoding preservation must be the last thing to touch the bytes. Formatters
	// such as gofmt normalize endings to LF, so re-impose the same BOM and ending
	// decision on their output and publish it through the same rooted, guarded
	// commit. The result is then the preserved form of what the model sent
	// whenever the formatter changed nothing else, which keeps the whole-file
	// observation.
	if existed && finalContentKnown {
		if reencoded := encoding.apply(content); reencoded != content {
			if newInfo == nil {
				options.FileTracker.Forget(absolutePath)
				return errorResult("Error writing file " + relativePath + ": cannot restore line endings and BOM after formatting: the formatted file could not be inspected")
			}
			formatted := content
			if err := commitRootedFileContents(root, absolutePath, rootedRelative, newInfo, &formatted, reencoded); err != nil {
				options.FileTracker.Forget(absolutePath)
				return errorResult("Error writing file " + relativePath + ": cannot restore line endings and BOM after formatting: " + err.Error())
			}
			content = reencoded
			newInfo, _ = root.Stat(rootedRelative)
		}
	}
	if finalContentKnown {
		options.FileTracker.Record(absolutePath, []byte(content), newInfo)
	} else {
		options.FileTracker.Forget(absolutePath)
	}
	if finalContentKnown && content == modelEquivalentContent {
		options.FileTracker.RecordSeenRange(absolutePath, 1, trackedLineTotal(content), trackedLineTotal(content))
	}
	if !existed {
		options.FileTracker.RecordCreated(absolutePath)
	}

	verb := "Created"
	if existed {
		verb = "Overwrote"
	}
	// Report line count (not bytes): "Wrote 282 lines" reads as real work at a
	// glance, where a byte total is opaque noise.
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}
	summary := fmt.Sprintf("%s %s (%d lines).", verb, relativePath, lines)
	summary += formatting.notice(relativePath)
	if finalContentKnown {
		summary += inlineDiagnostics(ctx, options, absolutePath, relativePath)
	}
	result := okResult(summary)
	result.ChangedFiles = []string{relativePath}
	// Do not pretend an unreadable overwrite was a creation. The write may be
	// valid, but ACP only receives an exact before/after pair we actually saw.
	if priorContentKnown && finalContentKnown {
		if diff, ok := boundedFileDiff(absolutePath, priorContent, content, existed, true); ok {
			result.FileDiffs = []FileDiff{diff}
		} else if diffTextRevealsObfuscatedSecret(priorContent) || diffTextRevealsObfuscatedSecret(content) {
			result.Redacted = true
		}
	}
	// Card-only preview: a real unified diff (all-green for a create, red/green for
	// an overwrite) on Display.Preview. Output stays the summary, so the model never
	// re-reads the file — the rich preview costs zero model tokens.
	preview := ""
	if priorContentKnown && finalContentKnown {
		preview = boundedUnifiedDiff(relativePath, priorContent, content)
	}
	result.Display = Display{Summary: summary, Kind: "file", Preview: preview}
	return result
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// writeFileEncoding is the BOM and line-ending convention an overwrite
// publishes. It is decided once, from the existing bytes and the model's
// content, so formatter output can be brought back to the same convention
// without the formatter's own LF output looking like a request for LF.
type writeFileEncoding struct {
	bom  bool
	crlf bool
}

// resolveWriteFileEncoding restores byte-level features hidden by read_file's
// normalized text view. It keeps line endings consistent with the existing
// file, while still allowing an LF file to be explicitly replaced with
// consistently CRLF content. Explicit BOM and line-ending intent independently
// overrides this automatic behavior.
func resolveWriteFileEncoding(existing []byte, content, bom, lineEndings string) writeFileEncoding {
	encoding := writeFileEncoding{
		bom: bom == "add" || (bom == "auto" && (bytes.HasPrefix(existing, utf8BOM) || strings.HasPrefix(content, string(utf8BOM)))),
	}
	existingCRLF, existingLF := lineEndingCounts(existing)
	updatedCRLF, updatedLF := lineEndingCounts([]byte(content))
	encoding.crlf = existingCRLF > existingLF
	if !encoding.crlf && updatedCRLF > updatedLF {
		// Unlike LF returned by read_file, caller-supplied dominant CRLF is an
		// unambiguous request to change an LF file's convention.
		encoding.crlf = true
	}
	if lineEndings != "auto" {
		encoding.crlf = lineEndings == "crlf"
	}
	return encoding
}

// apply rewrites content into the convention. It is idempotent, so applying it
// to bytes that already follow the convention returns them unchanged.
func (encoding writeFileEncoding) apply(content string) string {
	updated := bytes.TrimPrefix([]byte(content), utf8BOM)
	if encoding.bom {
		updated = append(append([]byte(nil), utf8BOM...), updated...)
	}
	updated = bytes.ReplaceAll(updated, []byte("\r\n"), []byte("\n"))
	updated = bytes.ReplaceAll(updated, []byte("\r"), []byte("\n"))
	if encoding.crlf {
		updated = bytes.ReplaceAll(updated, []byte("\n"), []byte("\r\n"))
	}
	return string(updated)
}

func lineEndingCounts(content []byte) (crlf, loneLF int) {
	crlf = bytes.Count(content, []byte("\r\n"))
	return crlf, bytes.Count(content, []byte("\n")) - crlf
}

// fileContentArg reads the file body from "content" or a common alias that weaker
// models sometimes use instead (contents/text/body/data/file_content). It
// delegates to the shared aliasedStringArg so the present-but-non-string type
// error ("content must be a string") and the required-but-missing error
// ("content is required") stay consistent with every other tool. An empty string
// is allowed (writing an empty file), so allowEmpty is true.
func fileContentArg(args map[string]any) (string, error) {
	return aliasedStringArg(args, []string{"content", "contents", "text", "body", "data", "file_content"}, "", true, true)
}
