package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Gitlawb/zero/internal/pathjail"
	"github.com/Gitlawb/zero/internal/sandbox"
)

const (
	structuredPatchBegin  = "*** Begin Patch"
	structuredPatchEnd    = "*** End Patch"
	structuredAddFile     = "*** Add File: "
	structuredDeleteFile  = "*** Delete File: "
	structuredUpdateFile  = "*** Update File: "
	structuredMoveTo      = "*** Move to: "
	structuredEndOfFile   = "*** End of File"
	structuredHunkContext = "@@ "
)

type structuredPatchKind uint8

const (
	structuredPatchAdd structuredPatchKind = iota
	structuredPatchDelete
	structuredPatchUpdate
	// structuredPatchCopy creates movePath from path's content (plus any
	// hunks) and keeps path; produced by a unified diff's "copy from/to".
	structuredPatchCopy
)

type structuredPatchOperation struct {
	kind     structuredPatchKind
	path     string
	movePath string
	contents string
	chunks   []structuredPatchChunk
	line     int
	// eofNewline lets a unified diff's "\ No newline at end of file" marker
	// force the trailing-newline state of the result; structured patches keep
	// the file's existing state.
	eofNewline eofNewlineMode
	// verifyDelete marks a deletion that carries the expected old content
	// (a unified diff's "+++ /dev/null" hunks, or git's header-only form for
	// an empty file): the removed lines must equal the current file byte for
	// byte, otherwise the deletion is stale and refused. A structured
	// "*** Delete File" means "delete this path" and has no chunks.
	verifyDelete bool
	// oldNoNewline records a unified diff's "\ No newline at end of file"
	// on the removed side, i.e. the old content did not end with a newline.
	oldNoNewline bool
}

type eofNewlineMode uint8

const (
	eofNewlineKeep eofNewlineMode = iota
	eofNewlinePresent
	eofNewlineAbsent
)

type structuredPatchChunk struct {
	context          string
	hasContext       bool
	old              []string
	new              []string
	newSourceOffsets []int
	endOfFile        bool
	// hint is the 0-based line the hunk is expected at (from a unified diff's
	// "@@ -a,b" range). When the expected lines match there it is used as-is;
	// otherwise the hunk is located by content like a structured hunk.
	hint    int
	hasHint bool
}

type structuredPatchTarget struct {
	requested string
	absolute  string
	relative  string
}

type structuredPatchChange struct {
	kind       structuredPatchKind
	from       structuredPatchTarget
	to         structuredPatchTarget
	before     string
	after      string
	mode       os.FileMode
	beforeInfo os.FileInfo
}

// unifiedHunkRangePattern recognises a unified-diff range header ("-12,4 +12,6",
// optionally followed by "@@ heading") written inside a structured hunk marker.
// The line numbers carry no information for the context-anchored grammar, so
// the range is dropped and only an explicit heading is kept as the anchor.
var unifiedHunkRangePattern = regexp.MustCompile(`^-\d+(?:,\d+)?\s+\+\d+(?:,\d+)?\s*(?:@@\s*(.*))?$`)

const structuredPatchFormatHint = ` (format: "*** Begin Patch", then "*** Update File: path" / "*** Add File: path" / "*** Delete File: path" sections whose hunks start with "@@" and use " " context, "-" removed and "+" added lines, then "*** End Patch")`

// structuredPatchMarker classifies a line as the "begin" or "end" marker of a
// structured patch, or "" when it is neither. The classifier is owned by the
// sandbox package so the boundary check and the tool accept exactly the same
// spellings; a strict byte-equal match here once made every patch from some
// models fail on line 1 and pushed them into whole-file rewrites.
func structuredPatchMarker(line string) string {
	return sandbox.StructuredPatchMarker(line)
}

// structuredHunkAnchor normalises the text after a hunk's "@@ " marker. A
// unified-diff range is dropped (its optional heading survives as the anchor);
// anything else is used verbatim.
func structuredHunkAnchor(context string) string {
	context = strings.TrimSpace(context)
	if match := unifiedHunkRangePattern.FindStringSubmatch(context); match != nil {
		return strings.TrimSpace(match[1])
	}
	return context
}

func isStructuredPatch(patch string) bool {
	return sandbox.IsStructuredPatch(patch)
}

func (tool applyPatchTool) runStructuredPatch(applyRoot, relativeRoot, patch string, options RunOptions) Result {
	operations, err := parseStructuredPatch(patch)
	if err != nil {
		return errorResult("Error applying patch: " + err.Error())
	}
	return applyPatchOperations(applyRoot, relativeRoot, operations, options)
}

// applyPatchOperations applies parsed operations (from either patch format)
// through an opened workspace root: every stat, read, create and write is
// descriptor-relative and refuses to follow a link out of the root, so there
// is no pathname check-to-use window between validation and write.
func applyPatchOperations(applyRoot, relativeRoot string, operations []structuredPatchOperation, options RunOptions) Result {
	workspace, err := os.OpenRoot(applyRoot)
	if err != nil {
		return errorResult("Error applying patch: " + err.Error())
	}
	defer workspace.Close()
	changes, err := planStructuredPatch(workspace, operations, options.FileTracker)
	if err != nil {
		return errorResult("Error applying patch: " + err.Error())
	}
	wholeBefore := make(map[string]bool, len(changes))
	if options.FileTracker != nil {
		for _, change := range changes {
			if change.kind == structuredPatchUpdate || change.kind == structuredPatchCopy {
				wholeBefore[change.from.absolute] = options.FileTracker.SeenWhole(change.from.absolute)
			}
		}
	}
	applyOutcome, err := applyStructuredPatchChanges(workspace, relativeRoot, changes, options.FileTracker)
	if err != nil {
		result := errorResult("Error applying patch: " + err.Error())
		result.ChangedFiles = changedFilesFromStructuredPatch(relativeRoot, applyOutcome.committed)
		result.FileDiffs = fileDiffsFromStructuredPatch(relativeRoot, applyOutcome.committed)
		result.Redacted = structuredPatchContainsObfuscatedSecret(applyOutcome.committed)
		result.Display = Display{Summary: result.Output, Kind: "diff", Preview: structuredPatchPreview(applyOutcome.committed)}
		return result
	}

	for _, change := range changes {
		switch change.kind {
		case structuredPatchDelete:
			options.FileTracker.Forget(change.from.absolute)
		case structuredPatchAdd:
			recordStructuredPatchFile(options.FileTracker, change.to.absolute, true, true)
		case structuredPatchUpdate:
			wasWhole := wholeBefore[change.from.absolute]
			options.FileTracker.Forget(change.from.absolute)
			recordStructuredPatchFile(options.FileTracker, change.to.absolute, false, wasWhole)
		case structuredPatchCopy:
			// The source is untouched; the destination inherits only what the
			// model had actually seen of the source.
			recordStructuredPatchFile(options.FileTracker, change.to.absolute, true, wholeBefore[change.from.absolute])
		}
	}

	summary := "Patch applied successfully."
	if relativeRoot != "." {
		summary = "Patch applied successfully in " + relativeRoot + "."
	}
	result := okResult(summary)
	result.ChangedFiles = changedFilesFromStructuredPatch(relativeRoot, changes)
	result.FileDiffs = fileDiffsFromStructuredPatch(relativeRoot, changes)
	result.Redacted = structuredPatchContainsObfuscatedSecret(changes)
	result.Display = Display{Summary: summary, Kind: "diff", Preview: structuredPatchPreview(changes)}
	return result
}

func structuredPatchContainsObfuscatedSecret(changes []structuredPatchChange) bool {
	for _, change := range changes {
		if diffTextRevealsObfuscatedSecret(change.before) || diffTextRevealsObfuscatedSecret(change.after) {
			return true
		}
	}
	return false
}

func fileDiffsFromStructuredPatch(_ string, changes []structuredPatchChange) []FileDiff {
	const maxToolResultFileDiffs = 64
	diffs := make([]FileDiff, 0, len(changes)*2)
	usedBytes := 0
	appendGroup := func(group ...FileDiff) bool {
		if len(group) == 0 || len(diffs)+len(group) > maxToolResultFileDiffs {
			return false
		}
		groupBytes := 0
		for _, diff := range group {
			groupBytes += len(diff.OldText) + len(diff.NewText)
		}
		if usedBytes+groupBytes > maxToolResultFileDiffBytes {
			return false
		}
		diffs = append(diffs, group...)
		usedBytes += groupBytes
		return true
	}
	makeDiff := func(path, before, after string, oldExists, newExists bool) (FileDiff, bool) {
		return boundedFileDiff(path, before, after, oldExists, newExists)
	}
	for _, change := range changes {
		var group []FileDiff
		switch {
		case change.kind == structuredPatchDelete:
			if diff, ok := makeDiff(change.from.absolute, change.before, "", true, false); ok {
				group = append(group, diff)
			}
		case change.kind == structuredPatchAdd:
			if diff, ok := makeDiff(change.to.absolute, "", change.after, false, true); ok {
				group = append(group, diff)
			}
		case change.kind == structuredPatchCopy && change.from.absolute != change.to.absolute:
			// A copy leaves its source unchanged; the destination is a create.
			if diff, ok := makeDiff(change.to.absolute, "", change.after, false, true); ok {
				group = append(group, diff)
			}
		case change.kind == structuredPatchUpdate && change.from.absolute != change.to.absolute:
			// A move is two filesystem changes, not a destination overwrite.
			from, fromOK := makeDiff(change.from.absolute, change.before, "", true, false)
			to, toOK := makeDiff(change.to.absolute, "", change.after, false, true)
			if fromOK && toOK {
				group = append(group, from, to)
			}
		default:
			if diff, ok := makeDiff(change.to.absolute, change.before, change.after, true, true); ok {
				group = append(group, diff)
			}
		}
		if len(group) > 0 && !appendGroup(group...) {
			break
		}
	}
	return diffs
}

func parseStructuredPatch(patch string) ([]structuredPatchOperation, error) {
	normalized := strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(patch, "\r\n", "\n"), "\ufeff"))
	if normalized == "" {
		return nil, fmt.Errorf("structured patch is empty")
	}
	lines := strings.Split(normalized, "\n")
	if structuredPatchMarker(lines[0]) != "begin" {
		return nil, fmt.Errorf("the first line of a structured patch must be %q%s", structuredPatchBegin, structuredPatchFormatHint)
	}
	if structuredPatchMarker(lines[len(lines)-1]) != "end" {
		return nil, fmt.Errorf("the last line of a structured patch must be %q%s", structuredPatchEnd, structuredPatchFormatHint)
	}

	var operations []structuredPatchOperation
	for index := 1; index < len(lines)-1; {
		header := strings.TrimSpace(lines[index])
		lineNumber := index + 1
		if header == "" {
			index++
			continue
		}
		switch {
		case strings.HasPrefix(header, structuredAddFile):
			path, err := structuredPatchPath(header, structuredAddFile, lineNumber)
			if err != nil {
				return nil, err
			}
			op := structuredPatchOperation{kind: structuredPatchAdd, path: path, line: lineNumber}
			index++
			var content []string
			for index < len(lines)-1 && !isStructuredPatchHeader(lines[index]) {
				if !strings.HasPrefix(lines[index], "+") {
					return nil, structuredPatchLineError(index+1, "added-file contents must start with '+'")
				}
				content = append(content, strings.TrimPrefix(lines[index], "+"))
				index++
			}
			if len(content) > 0 {
				op.contents = strings.Join(content, "\n") + "\n"
			}
			operations = append(operations, op)
		case strings.HasPrefix(header, structuredDeleteFile):
			path, err := structuredPatchPath(header, structuredDeleteFile, lineNumber)
			if err != nil {
				return nil, err
			}
			operations = append(operations, structuredPatchOperation{kind: structuredPatchDelete, path: path, line: lineNumber})
			index++
		case strings.HasPrefix(header, structuredUpdateFile):
			path, err := structuredPatchPath(header, structuredUpdateFile, lineNumber)
			if err != nil {
				return nil, err
			}
			op := structuredPatchOperation{kind: structuredPatchUpdate, path: path, line: lineNumber}
			index++
			if index < len(lines)-1 && strings.HasPrefix(strings.TrimSpace(lines[index]), structuredMoveTo) {
				op.movePath, err = structuredPatchPath(strings.TrimSpace(lines[index]), structuredMoveTo, index+1)
				if err != nil {
					return nil, err
				}
				index++
			}
			for index < len(lines)-1 && !isStructuredPatchHeader(lines[index]) {
				line := lines[index]
				trimmed := strings.TrimSpace(line)
				if trimmed == structuredEndOfFile {
					if len(op.chunks) == 0 || (!hasStructuredPatchContent(op.chunks[len(op.chunks)-1])) {
						return nil, structuredPatchLineError(index+1, "*** End of File must follow an update hunk")
					}
					op.chunks[len(op.chunks)-1].endOfFile = true
					index++
					continue
				}
				if trimmed == "@@" || strings.HasPrefix(trimmed, structuredHunkContext) {
					chunk := structuredPatchChunk{}
					rest := ""
					if trimmed != "@@" {
						rest = strings.TrimPrefix(trimmed, structuredHunkContext)
					}
					if anchor := structuredHunkAnchor(rest); anchor != "" {
						chunk.context = anchor
						chunk.hasContext = true
					}
					op.chunks = append(op.chunks, chunk)
					index++
					continue
				}
				if len(op.chunks) == 0 {
					op.chunks = append(op.chunks, structuredPatchChunk{})
				}
				chunk := &op.chunks[len(op.chunks)-1]
				switch {
				case line == "":
					sourceOffset := len(chunk.old)
					chunk.old = append(chunk.old, "")
					chunk.new = append(chunk.new, "")
					chunk.newSourceOffsets = append(chunk.newSourceOffsets, sourceOffset)
				case strings.HasPrefix(line, " "):
					content := strings.TrimPrefix(line, " ")
					sourceOffset := len(chunk.old)
					chunk.old = append(chunk.old, content)
					chunk.new = append(chunk.new, content)
					chunk.newSourceOffsets = append(chunk.newSourceOffsets, sourceOffset)
				case strings.HasPrefix(line, "+"):
					chunk.new = append(chunk.new, strings.TrimPrefix(line, "+"))
					chunk.newSourceOffsets = append(chunk.newSourceOffsets, -1)
				case strings.HasPrefix(line, "-"):
					chunk.old = append(chunk.old, strings.TrimPrefix(line, "-"))
				default:
					return nil, structuredPatchLineError(index+1, "update lines must start with ' ', '+', or '-'")
				}
				index++
			}
			if len(op.chunks) == 0 || !allStructuredPatchChunksHaveContent(op.chunks) {
				return nil, structuredPatchLineError(lineNumber, "update file hunk is empty")
			}
			operations = append(operations, op)
		default:
			return nil, structuredPatchLineError(lineNumber, "expected an Add File, Delete File, or Update File header")
		}
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("structured patch contains no file operations")
	}
	return operations, nil
}

func structuredPatchPath(header, prefix string, line int) (string, error) {
	path := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if path == "" {
		return "", structuredPatchLineError(line, "file path is required")
	}
	return path, nil
}

func isStructuredPatchHeader(line string) bool {
	return structuredPatchMarker(line) == "end" || strings.HasPrefix(line, structuredAddFile) || strings.HasPrefix(line, structuredDeleteFile) || strings.HasPrefix(line, structuredUpdateFile)
}

func structuredPatchLineError(line int, message string) error {
	return fmt.Errorf("invalid structured patch at line %d: %s", line, message)
}

func hasStructuredPatchContent(chunk structuredPatchChunk) bool {
	return len(chunk.old) > 0 || len(chunk.new) > 0
}

func allStructuredPatchChunksHaveContent(chunks []structuredPatchChunk) bool {
	for _, chunk := range chunks {
		if !hasStructuredPatchContent(chunk) {
			return false
		}
	}
	return true
}

func structuredPatchOperationPaths(operations []structuredPatchOperation) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, operation := range operations {
		for _, path := range []string{operation.path, operation.movePath} {
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}

func planStructuredPatch(root *os.Root, operations []structuredPatchOperation, tracker *FileTracker) ([]structuredPatchChange, error) {
	seen := make(map[string]struct{})
	var changes []structuredPatchChange
	for _, operation := range operations {
		from, err := resolveStructuredPatchTarget(root.Name(), operation.path)
		if err != nil {
			return nil, err
		}
		to := from
		if operation.movePath != "" {
			to, err = resolveStructuredPatchTarget(root.Name(), operation.movePath)
			if err != nil {
				return nil, err
			}
		}
		targets := []structuredPatchTarget{from}
		if to.absolute != from.absolute {
			targets = append(targets, to)
		}
		for _, target := range targets {
			if _, exists := seen[target.absolute]; exists {
				return nil, fmt.Errorf("structured patch changes %q more than once", target.relative)
			}
			seen[target.absolute] = struct{}{}
		}

		change := structuredPatchChange{kind: operation.kind, from: from, to: to, mode: 0o644}
		switch operation.kind {
		case structuredPatchAdd:
			if _, err := root.Lstat(to.relative); err == nil {
				return nil, fmt.Errorf("cannot add %s because it already exists", to.relative)
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			change.after = operation.contents
		case structuredPatchDelete, structuredPatchUpdate, structuredPatchCopy:
			content, info, err := readRootedFile(root, from.relative)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", from.relative, err)
			}
			change.mode = info.Mode()
			change.beforeInfo = info
			if err := tracker.CheckConflict(from.absolute, content); err != nil {
				return nil, fmt.Errorf("%s", fileConflictMessage(from.relative))
			}
			change.before = string(content)
			if operation.kind == structuredPatchDelete && operation.verifyDelete {
				if err := verifyUnifiedDeletion(change.before, from.relative, operation); err != nil {
					return nil, err
				}
			}
			if operation.kind == structuredPatchUpdate || operation.kind == structuredPatchCopy {
				updated, err := applyStructuredPatchUpdate(change.before, from.relative, operation.chunks)
				if err != nil {
					return nil, err
				}
				change.after = applyEOFNewline(updated, operation.eofNewline)
				if operation.kind == structuredPatchCopy && from.absolute == to.absolute {
					return nil, fmt.Errorf("cannot copy %s onto itself", from.relative)
				}
				if from.absolute != to.absolute {
					if _, err := root.Lstat(to.relative); err == nil {
						return nil, fmt.Errorf("cannot move %s to %s because the destination already exists", from.relative, to.relative)
					} else if !os.IsNotExist(err) {
						return nil, err
					}
				}
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func resolveStructuredPatchTarget(root, path string) (structuredPatchTarget, error) {
	// Relative traversal is rejected outright. An absolute path is allowed when
	// it resolves inside the apply root (models routinely echo the absolute path
	// they were shown by read_file); resolveWorkspaceTargetPath rejects anything
	// that lands outside.
	if path == ".." || strings.HasPrefix(filepath.ToSlash(path), "../") {
		return structuredPatchTarget{}, fmt.Errorf("patch path %q must stay inside the workspace", path)
	}
	absolute, relative, err := resolveWorkspaceTargetPath(root, normalizePatchPathForRoot(root, path))
	if err != nil {
		return structuredPatchTarget{}, err
	}
	return structuredPatchTarget{requested: path, absolute: absolute, relative: relative}, nil
}

func applyStructuredPatchUpdate(content, path string, chunks []structuredPatchChunk) (string, error) {
	lineEnding := structuredPatchLineEnding(content)
	if lineEnding == "\r\n" {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	lines := strings.Split(content, "\n")
	trailingNewline := content != "" && len(lines) > 0 && lines[len(lines)-1] == ""
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	replacements := make([]structuredReplacement, 0, len(chunks))
	lineIndex := 0
	for _, chunk := range chunks {
		if chunk.hasContext {
			index, ambiguous := findStructuredPatchSequence(lines, []string{chunk.context}, lineIndex, false)
			if ambiguous {
				return "", fmt.Errorf("context %q is ambiguous in %s; provide a more specific hunk header", chunk.context, path)
			}
			if index < 0 {
				return "", fmt.Errorf("could not find context %q in %s", chunk.context, path)
			}
			lineIndex = index + 1
		}
		if len(chunk.old) == 0 {
			// A context anchor identifies the line immediately before a pure
			// insertion. Only an explicit end-of-file hunk (or an unanchored
			// insertion) belongs at the file end. A unified diff names the
			// insertion point in its range instead; a zero-context hunk (as
			// from `git diff -U0`) has nothing to verify the position against,
			// so it is position-trusting by design — the same semantics git
			// apply uses without fuzz. Hunks that carry context are verified.
			start := lineIndex
			if chunk.hasHint {
				start = max(chunk.hint, lineIndex)
			} else if chunk.endOfFile || !chunk.hasContext {
				start = len(lines)
			}
			if start > len(lines) {
				start = len(lines)
			}
			replacement, err := structuredPatchReplacement(chunk, lines, start)
			if err != nil {
				return "", fmt.Errorf("building replacement for %s: %w", path, err)
			}
			replacements = append(replacements, structuredReplacement{start: start, replacement: replacement})
			lineIndex = start
			continue
		}
		var index int
		ambiguous := false
		if chunk.hasHint && chunk.hint >= lineIndex && sequenceMatchesAt(lines, chunk.old, chunk.hint) {
			index = chunk.hint
		} else {
			index, ambiguous = findStructuredPatchSequence(lines, chunk.old, lineIndex, chunk.endOfFile)
		}
		if ambiguous {
			return "", fmt.Errorf("expected lines are ambiguous in %s; provide more surrounding context:\n%s", path, strings.Join(chunk.old, "\n"))
		}
		if index < 0 {
			return "", fmt.Errorf("could not find expected lines in %s:\n%s", path, strings.Join(chunk.old, "\n"))
		}
		replacement, err := structuredPatchReplacement(chunk, lines, index)
		if err != nil {
			return "", fmt.Errorf("building replacement for %s: %w", path, err)
		}
		replacements = append(replacements, structuredReplacement{start: index, length: len(chunk.old), replacement: replacement})
		lineIndex = index + len(chunk.old)
	}
	for index := 1; index < len(replacements); index++ {
		previous := replacements[index-1]
		current := replacements[index]
		if current.start < previous.start+previous.length {
			return "", fmt.Errorf("overlapping or out-of-order hunks in %s; provide non-overlapping context", path)
		}
	}
	for index := len(replacements) - 1; index >= 0; index-- {
		replacement := replacements[index]
		lines = append(lines[:replacement.start], append(replacement.replacement, lines[replacement.start+replacement.length:]...)...)
	}
	if len(lines) == 0 {
		return "", nil
	}
	updated := strings.Join(lines, lineEnding)
	if trailingNewline {
		updated += lineEnding
	}
	return updated, nil
}

// verifyUnifiedDeletion checks, byte for byte, that the lines a unified
// deletion removes are exactly the file's current content. A deletion is not
// recoverable, so none of the whitespace tolerance used to locate update
// hunks applies here; only the file's own line-ending style is normalised.
// With no hunks (git's header-only form) the file must be empty.
func verifyUnifiedDeletion(current, path string, operation structuredPatchOperation) error {
	var removed []string
	for _, chunk := range operation.chunks {
		if len(chunk.new) > 0 {
			return fmt.Errorf("deletion of %s must not add lines", path)
		}
		removed = append(removed, chunk.old...)
	}
	expected := strings.Join(removed, "\n")
	if len(removed) > 0 && !operation.oldNoNewline {
		expected += "\n"
	}
	actual := current
	if structuredPatchLineEnding(current) == "\r\n" {
		actual = strings.ReplaceAll(current, "\r\n", "\n")
	}
	if actual != expected {
		if len(removed) == 0 {
			return fmt.Errorf("deletion of %s expects an empty file but it has content; include the removed lines in the patch", path)
		}
		return fmt.Errorf("deletion of %s does not match its current content; the removed lines must equal the whole file", path)
	}
	return nil
}

// sequenceMatchesAt reports whether wanted appears verbatim at lines[start:].
func sequenceMatchesAt(lines, wanted []string, start int) bool {
	if start < 0 || start+len(wanted) > len(lines) {
		return false
	}
	for offset, line := range wanted {
		if lines[start+offset] != line {
			return false
		}
	}
	return true
}

// applyEOFNewline forces or strips a single trailing line ending when a
// unified diff's end-of-file marker asked for it.
func applyEOFNewline(content string, mode eofNewlineMode) string {
	switch mode {
	case eofNewlinePresent:
		if content != "" && !strings.HasSuffix(content, "\n") {
			return content + structuredPatchLineEnding(content)
		}
	case eofNewlineAbsent:
		content = strings.TrimSuffix(content, "\n")
		content = strings.TrimSuffix(content, "\r")
	}
	return content
}

func structuredPatchReplacement(chunk structuredPatchChunk, source []string, start int) ([]string, error) {
	if len(chunk.newSourceOffsets) != len(chunk.new) {
		return nil, fmt.Errorf("context mapping has %d entries for %d output lines", len(chunk.newSourceOffsets), len(chunk.new))
	}
	replacement := append([]string(nil), chunk.new...)
	for outputIndex, sourceOffset := range chunk.newSourceOffsets {
		if sourceOffset < 0 {
			continue
		}
		sourceIndex := start + sourceOffset
		if sourceIndex < 0 || sourceIndex >= len(source) {
			return nil, fmt.Errorf("context source index %d is outside the matched file", sourceIndex)
		}
		replacement[outputIndex] = source[sourceIndex]
	}
	return replacement, nil
}

func structuredPatchLineEnding(content string) string {
	if strings.Contains(content, "\r\n") && !strings.Contains(strings.ReplaceAll(content, "\r\n", ""), "\n") {
		return "\r\n"
	}
	return "\n"
}

type structuredReplacement struct {
	start       int
	length      int
	replacement []string
}

func findStructuredPatchSequence(lines, wanted []string, start int, endOfFile bool) (int, bool) {
	if len(wanted) == 0 {
		return start, false
	}
	if len(wanted) > len(lines) {
		return -1, false
	}
	searchStart := start
	if endOfFile {
		searchStart = len(lines) - len(wanted)
		if searchStart < start {
			searchStart = start
		}
	}
	for _, equal := range []func(string, string) bool{
		func(left, right string) bool { return left == right },
		func(left, right string) bool {
			return strings.TrimRight(left, " \t") == strings.TrimRight(right, " \t")
		},
		func(left, right string) bool { return strings.TrimSpace(left) == strings.TrimSpace(right) },
	} {
		match := -1
		for index := searchStart; index+len(wanted) <= len(lines); index++ {
			matched := true
			for offset, line := range wanted {
				if !equal(lines[index+offset], line) {
					matched = false
					break
				}
			}
			if matched {
				if match >= 0 {
					return -1, true
				}
				match = index
			}
		}
		if match >= 0 {
			return match, false
		}
	}
	return -1, false
}

type structuredPatchApplyOutcome struct {
	committed       []structuredPatchChange
	incompletePaths []string
}

type structuredPatchChangeOutcome struct {
	completed       []structuredPatchChange
	incompletePaths []string
}

func applyStructuredPatchChanges(root *os.Root, relativeRoot string, changes []structuredPatchChange, tracker *FileTracker) (structuredPatchApplyOutcome, error) {
	// committed lists, in order, the paths whose change reached disk before a
	// later change failed, so the caller (and the model) knows exactly which
	// files now hold the patched content and which were never touched.
	var outcome structuredPatchApplyOutcome
	for _, change := range changes {
		changeOutcome, err := applyStructuredPatchChange(root, change)
		outcome.committed = append(outcome.committed, changeOutcome.completed...)
		outcome.incompletePaths = append(outcome.incompletePaths, changeOutcome.incompletePaths...)
		if err != nil {
			forgetStructuredPatchFiles(tracker, changes)
			committedPaths := changedFilesFromStructuredPatch(relativeRoot, outcome.committed)
			unverifiedPaths := appendUniqueStructuredPatchPaths(nil, relativeRoot, outcome.incompletePaths)
			if len(committedPaths) > 0 || len(unverifiedPaths) > 0 {
				parts := []string{"patch was partially applied"}
				if len(committedPaths) > 0 {
					parts = append(parts, "already committed: "+strings.Join(committedPaths, ", "))
				}
				if len(unverifiedPaths) > 0 {
					parts = append(parts, "published but unverified: "+strings.Join(unverifiedPaths, ", "))
				}
				parts = append(parts, "the remaining files are unchanged")
				if len(unverifiedPaths) > 0 {
					parts = append(parts, "re-read the listed files before retrying")
				} else {
					parts = append(parts, "re-read the committed files before retrying")
				}
				return outcome, fmt.Errorf("%w; %s", err, strings.Join(parts, "; "))
			}
			return outcome, err
		}
	}
	return outcome, nil
}

// completedStructuredPatchEffect turns a partially completed compound change
// into the exact filesystem sub-effect that is still verifiable after the
// error. In particular, a failed move may have published its destination while
// leaving the source in place; that is a destination creation, not a move.
func completedStructuredPatchEffect(root *os.Root, change structuredPatchChange) (structuredPatchChange, bool) {
	if change.to.relative == "" {
		return structuredPatchChange{}, false
	}
	content, _, err := readRootedFile(root, change.to.relative)
	if err != nil || string(content) != change.after {
		return structuredPatchChange{}, false
	}
	switch change.kind {
	case structuredPatchAdd, structuredPatchCopy:
		return structuredPatchChange{
			kind:  structuredPatchAdd,
			to:    change.to,
			after: change.after,
			mode:  change.mode,
		}, true
	case structuredPatchUpdate:
		if change.from.absolute != change.to.absolute {
			return structuredPatchChange{
				kind:  structuredPatchAdd,
				to:    change.to,
				after: change.after,
				mode:  change.mode,
			}, true
		}
	}
	return structuredPatchChange{}, false
}

func incompleteStructuredPatchWrite(root *os.Root, change structuredPatchChange, published bool) structuredPatchChangeOutcome {
	if !published {
		return structuredPatchChangeOutcome{}
	}
	if completed, ok := completedStructuredPatchEffect(root, change); ok {
		return structuredPatchChangeOutcome{completed: []structuredPatchChange{completed}}
	}
	if change.to.relative != "" {
		return structuredPatchChangeOutcome{incompletePaths: []string{change.to.relative}}
	}
	return structuredPatchChangeOutcome{}
}

func appendUniqueStructuredPatchPaths(existing []string, relativeRoot string, paths []string) []string {
	seen := make(map[string]bool, len(existing)+len(paths))
	for _, path := range existing {
		seen[path] = true
	}
	for _, path := range paths {
		if relativeRoot != "" && relativeRoot != "." {
			path = filepath.ToSlash(filepath.Join(relativeRoot, path))
		}
		if path != "" && !seen[path] {
			seen[path] = true
			existing = append(existing, path)
		}
	}
	return existing
}

func forgetStructuredPatchFiles(tracker *FileTracker, changes []structuredPatchChange) {
	if tracker == nil {
		return
	}
	for _, change := range changes {
		tracker.Forget(change.from.absolute)
		if change.to.absolute != change.from.absolute {
			tracker.Forget(change.to.absolute)
		}
	}
}

// structuredPatchBeforeCommit, when set, runs before each change's pre-commit
// recheck. Tests use it to alter the filesystem between planning and commit
// deterministically; it is nil in production.
var structuredPatchBeforeCommit func(change structuredPatchChange)

// structuredPatchBeforeRename runs after a same-path update has staged and
// closed its replacement but before the final preimage recheck and rename.
// Tests use it to reproduce a competing writer deterministically.
var structuredPatchBeforeRename func(change structuredPatchChange)

// structuredPatchRemove is a deterministic test seam for failures after a
// move destination has been published. Production removes through the opened
// workspace root.
var structuredPatchRemove = func(root *os.Root, name string) error { return root.Remove(name) }

func applyStructuredPatchChange(root *os.Root, change structuredPatchChange) (structuredPatchChangeOutcome, error) {
	if structuredPatchBeforeCommit != nil {
		structuredPatchBeforeCommit(change)
	}
	// The planner validated the source against the bytes it read; re-read
	// through the same root immediately before committing so a change made
	// by another process in between is refused rather than overwritten or
	// removed.
	if change.kind != structuredPatchAdd {
		if err := recheckStructuredPatchPreimage(root, change); err != nil {
			return structuredPatchChangeOutcome{}, err
		}
	}
	switch change.kind {
	case structuredPatchDelete:
		if err := structuredPatchRemove(root, change.from.relative); err != nil {
			return structuredPatchChangeOutcome{}, fmt.Errorf("deleting %s: %w", change.from.relative, err)
		}
		return structuredPatchChangeOutcome{completed: []structuredPatchChange{change}}, nil
	case structuredPatchAdd, structuredPatchCopy:
		var beforePublish func() error
		if change.kind == structuredPatchCopy {
			beforePublish = structuredPatchPrePublishGuard(root, change)
		}
		published, err := writeStructuredPatchFile(root, change.to, change.after, change.mode, true, beforePublish)
		if err != nil {
			return incompleteStructuredPatchWrite(root, change, published), err
		}
		return structuredPatchChangeOutcome{completed: []structuredPatchChange{change}}, nil
	case structuredPatchUpdate:
		moving := change.from.absolute != change.to.absolute
		published, err := writeStructuredPatchFile(root, change.to, change.after, change.mode, moving, structuredPatchPrePublishGuard(root, change))
		if err != nil {
			return incompleteStructuredPatchWrite(root, change, published), err
		}
		if moving {
			if err := structuredPatchRemove(root, change.from.relative); err != nil {
				return incompleteStructuredPatchWrite(root, change, true), fmt.Errorf("removing moved source %s: %w", change.from.relative, err)
			}
		}
		return structuredPatchChangeOutcome{completed: []structuredPatchChange{change}}, nil
	}
	return structuredPatchChangeOutcome{}, fmt.Errorf("unsupported structured patch operation")
}

func recheckStructuredPatchPreimage(root *os.Root, change structuredPatchChange) error {
	current, info, err := readRootedFile(root, change.from.relative)
	if err != nil {
		return fmt.Errorf("re-reading %s before commit: %w", change.from.relative, err)
	}
	if (change.beforeInfo != nil && !os.SameFile(change.beforeInfo, info)) || string(current) != change.before {
		return fmt.Errorf("%s changed on disk between planning and commit; re-read it and retry", change.from.relative)
	}
	return nil
}

func structuredPatchPrePublishGuard(root *os.Root, change structuredPatchChange) func() error {
	return func() error {
		if change.kind == structuredPatchUpdate && change.from.absolute == change.to.absolute && structuredPatchBeforeRename != nil {
			structuredPatchBeforeRename(change)
		}
		return recheckStructuredPatchPreimage(root, change)
	}
}

func writeStructuredPatchFile(root *os.Root, target structuredPatchTarget, content string, mode os.FileMode, createOnly bool, beforePublish func() error) (bool, error) {
	parent := filepath.Dir(target.relative)
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return false, fmt.Errorf("creating parent directory for %s: %w", target.relative, err)
	}
	temp, tempName, err := pathjail.CreateTemp(root, parent, "zero-patch", ".tmp")
	if err != nil {
		return false, fmt.Errorf("writing %s: %w", target.relative, err)
	}
	defer func() { _ = removeStructuredPatchTemp(root, tempName) }()
	if _, err := temp.WriteString(content); err != nil {
		_ = temp.Close()
		return false, fmt.Errorf("writing %s: %w", target.relative, err)
	}
	if err := temp.Chmod(mode.Perm()); err != nil {
		_ = temp.Close()
		return false, fmt.Errorf("writing %s: %w", target.relative, err)
	}
	if err := temp.Close(); err != nil {
		return false, fmt.Errorf("writing %s: %w", target.relative, err)
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return false, err
		}
	}
	if createOnly {
		return structuredPatchPublishNoReplace(root, tempName, target.relative, mode)
	}
	if err := root.Rename(tempName, target.relative); err != nil {
		return false, fmt.Errorf("writing %s: %w", target.relative, err)
	}
	return true, nil
}

func publishStructuredPatchNoReplace(root *os.Root, source, target string, mode os.FileMode) (bool, error) {
	return publishStructuredPatchNoReplaceWith(root, source, target, mode, root.Link)
}

// structuredPatchPublishNoReplace is a deterministic test seam for failures
// after a create-only target has become visible. Production uses the atomic
// no-replace publisher above.
var structuredPatchPublishNoReplace = publishStructuredPatchNoReplace

func publishStructuredPatchNoReplaceWith(root *os.Root, source, target string, mode os.FileMode, link func(string, string) error) (bool, error) {
	if linkErr := link(source, target); linkErr == nil {
		cleanupErr := removeStructuredPatchTemp(root, source)
		chmodErr := root.Chmod(target, mode.Perm())
		if cleanupErr != nil || chmodErr != nil {
			return true, fmt.Errorf("publishing %s: %w", target, errors.Join(cleanupErr, chmodErr))
		}
		return true, nil
	} else if errors.Is(linkErr, os.ErrExist) {
		return false, fmt.Errorf("creating %s: %w", target, linkErr)
	} else {
		committed, copyErr := copyStructuredPatchNoReplace(root, source, target, mode)
		if copyErr != nil {
			return committed, fmt.Errorf("creating %s without overwrite: hard link failed: %w; exclusive-copy fallback failed: %w", target, linkErr, copyErr)
		}
		return committed, nil
	}
}

func copyStructuredPatchNoReplace(root *os.Root, source, target string, mode os.FileMode) (bool, error) {
	return copyStructuredPatchNoReplaceWith(root, source, target, mode, io.Copy, removeStructuredPatchTemp)
}

func copyStructuredPatchNoReplaceWith(
	root *os.Root,
	source, target string,
	mode os.FileMode,
	copyFile func(io.Writer, io.Reader) (int64, error),
	cleanup func(*os.Root, string) error,
) (bool, error) {
	sourceFile, err := root.Open(source)
	if err != nil {
		return false, err
	}
	sourceClosed := false
	defer func() {
		if !sourceClosed {
			_ = sourceFile.Close()
		}
	}()
	targetFile, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	committed := true
	cleanupTarget := func(cause error) (bool, error) {
		_ = targetFile.Close()
		if cleanupErr := cleanup(root, target); cleanupErr == nil {
			return false, cause
		} else {
			return committed, errors.Join(cause, fmt.Errorf("cleaning partial target: %w", cleanupErr))
		}
	}
	if _, err := copyFile(targetFile, sourceFile); err != nil {
		return cleanupTarget(err)
	}
	if err := sourceFile.Close(); err != nil {
		sourceClosed = true
		return cleanupTarget(err)
	}
	sourceClosed = true
	if err := targetFile.Chmod(mode.Perm()); err != nil {
		return cleanupTarget(err)
	}
	if err := targetFile.Close(); err != nil {
		if cleanupErr := cleanup(root, target); cleanupErr == nil {
			return false, err
		} else {
			return true, errors.Join(err, fmt.Errorf("cleaning target after close failure: %w", cleanupErr))
		}
	}
	if err := cleanup(root, source); err != nil {
		return true, fmt.Errorf("removing published source: %w", err)
	}
	return true, nil
}

func removeStructuredPatchTemp(root *os.Root, name string) error {
	chmodErr := root.Chmod(name, 0o600)
	if os.IsNotExist(chmodErr) {
		return nil
	}
	removeErr := root.Remove(name)
	if os.IsNotExist(removeErr) {
		removeErr = nil
	}
	if removeErr == nil {
		return nil
	}
	return errors.Join(chmodErr, removeErr)
}

func recordStructuredPatchFile(tracker *FileTracker, absolute string, created, seenWhole bool) {
	if tracker == nil {
		return
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		tracker.Forget(absolute)
		return
	}
	info, _ := os.Stat(absolute)
	tracker.Record(absolute, content, info)
	if seenWhole {
		lines := trackedLineTotal(string(content))
		tracker.RecordSeenRange(absolute, 1, lines, lines)
	}
	if created {
		tracker.RecordCreated(absolute)
	}
}

func changedFilesFromStructuredPatch(relativeRoot string, changes []structuredPatchChange) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, change := range changes {
		var targets []structuredPatchTarget
		switch change.kind {
		case structuredPatchDelete:
			targets = []structuredPatchTarget{change.from}
		case structuredPatchUpdate:
			targets = []structuredPatchTarget{change.to}
			if change.from.absolute != change.to.absolute {
				targets = append([]structuredPatchTarget{change.from}, targets...)
			}
		default:
			// Adds and copies mutate only their destination; the copy source is
			// evidence for the operation, not a changed file.
			targets = []structuredPatchTarget{change.to}
		}
		for _, target := range targets {
			path := target.relative
			if relativeRoot != "" && relativeRoot != "." {
				path = filepath.ToSlash(filepath.Join(relativeRoot, path))
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func structuredPatchPreview(changes []structuredPatchChange) string {
	var preview strings.Builder
	for _, change := range changes {
		path := change.to.relative
		if change.kind == structuredPatchDelete {
			path = change.from.relative
		}
		before, after := change.before, change.after
		if change.kind == structuredPatchDelete {
			after = ""
		}
		diff := boundedUnifiedDiff(path, before, after)
		if diff == "" {
			continue
		}
		if preview.Len()+len(diff)+1 > maxToolPreviewBytes {
			break
		}
		if preview.Len() > 0 {
			preview.WriteByte('\n')
		}
		preview.WriteString(diff)
	}
	return capPreviewDiff(preview.String())
}
