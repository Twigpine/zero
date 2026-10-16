package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// parseUnifiedPatch converts a unified diff into the same operations a
// structured patch produces, so both formats are applied by Zero itself
// through an opened workspace root (descriptor-relative, no-follow writes)
// instead of handing pathnames to git apply after validation.
//
// Supported: file modifications, creations (--- /dev/null) and deletions
// (+++ /dev/null, verified against the content the hunks expect to remove),
// multiple hunks per file, "\ No newline at end of file"
// markers, a/ and b/ prefixes, git C-quoted paths, CRLF patches, and git's
// "rename from/to" and "copy from/to" headers (with or without hunks). Headers
// git emits around a hunk (diff --git, index, mode lines) are skipped; binary
// patches are rejected.
func parseUnifiedPatch(patch string) ([]structuredPatchOperation, error) {
	normalized := strings.TrimPrefix(strings.ReplaceAll(patch, "\r\n", "\n"), "\ufeff")
	lines := strings.Split(normalized, "\n")
	// The element after a final "\n" is the line terminator, not an empty
	// context line; counting it would let a truncated hunk look complete.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var operations []structuredPatchOperation
	var current *structuredPatchOperation
	var chunk *structuredPatchChunk
	var added []string // collected "+" lines for a file creation
	oldPath, newPath := "", ""
	pendingFrom, pendingHeader := "", ""
	pendingKind := structuredPatchUpdate
	diffOldPath, diffNewPath := "", ""
	diffOperands := ""
	diffPathsOK := false
	diffHeaderLine := 0
	// git's header-only forms: "deleted file mode" / "new file mode" after a
	// "diff --git" line with no ---/+++ pair describe an empty file.
	diffPath, headerOnly := "", byte(0) // headerOnly is 'd' (deleted) or 'n' (new)
	oldRemaining, newRemaining := 0, 0
	inHunk := false
	lastSide := byte(0) // '+' or '-' for the most recent content line

	finish := func() error {
		if current == nil {
			return nil
		}
		switch current.kind {
		case structuredPatchAdd:
			if len(added) > 0 {
				current.contents = strings.Join(added, "\n")
				if current.eofNewline != eofNewlineAbsent {
					current.contents += "\n"
				}
			}
		case structuredPatchUpdate, structuredPatchCopy:
			if !allStructuredPatchChunksHaveContent(current.chunks) {
				return fmt.Errorf("unified diff for %s has an empty hunk", current.path)
			}
			if len(current.chunks) == 0 && current.movePath == "" {
				return fmt.Errorf("unified diff for %s has no hunk lines", current.path)
			}
		case structuredPatchDelete:
			if len(current.chunks) == 0 || !allStructuredPatchChunksHaveContent(current.chunks) {
				return fmt.Errorf("unified deletion of %s must include the hunk with the content being removed", current.path)
			}
		}
		operations = append(operations, *current)
		current, chunk, added = nil, nil, nil
		return nil
	}
	flushHeaderOnly := func(line int) error {
		if headerOnly == 0 {
			return nil
		}
		if diffPath == "" {
			return fmt.Errorf("invalid unified diff at line %d: file mode header without a diff --git path", line)
		}
		if err := finish(); err != nil {
			return err
		}
		switch headerOnly {
		case 'd':
			operations = append(operations, structuredPatchOperation{kind: structuredPatchDelete, path: diffPath, line: line, verifyDelete: true})
		case 'n':
			operations = append(operations, structuredPatchOperation{kind: structuredPatchAdd, path: diffPath, line: line})
		}
		headerOnly, diffPath = 0, ""
		return nil
	}
	startFile := func(line int) error {
		// A ---/+++ pair means the file has hunks; it is not header-only.
		headerOnly, diffPath = 0, ""
		if oldPath == "" || newPath == "" {
			return fmt.Errorf("invalid unified diff at line %d: hunk before a ---/+++ header pair", line)
		}
		if diffHeaderLine != 0 {
			matches := false
			switch {
			case oldPath == "/dev/null":
				matches = newPath == diffNewPath
			case newPath == "/dev/null":
				matches = oldPath == diffOldPath
			default:
				matches = oldPath == diffOldPath && newPath == diffNewPath
			}
			if !matches {
				return fmt.Errorf("invalid unified diff at line %d: ---/+++ paths disagree with diff --git paths from line %d", line, diffHeaderLine)
			}
			diffHeaderLine = 0
		}
		// A ---/+++ pair after a rename/copy header names the same files; keep
		// accumulating that operation's hunks instead of starting a new one.
		if current != nil && current.movePath != "" && current.path == oldPath && (current.movePath == newPath || oldPath == newPath) {
			return nil
		}
		if err := finish(); err != nil {
			return err
		}
		op := structuredPatchOperation{line: line}
		switch {
		case oldPath == "/dev/null" && newPath == "/dev/null":
			return fmt.Errorf("invalid unified diff at line %d: both sides are /dev/null", line)
		case oldPath == "/dev/null":
			op.kind, op.path = structuredPatchAdd, newPath
		case newPath == "/dev/null":
			// A unified deletion states the content it expects to remove; keep
			// its hunks so the planner verifies them before deleting.
			op.kind, op.path, op.verifyDelete = structuredPatchDelete, oldPath, true
		default:
			op.kind, op.path = structuredPatchUpdate, oldPath
			if newPath != oldPath {
				op.movePath = newPath
			}
		}
		current = &op
		return nil
	}

	for index, raw := range lines {
		lineNumber := index + 1
		if inHunk && (oldRemaining > 0 || newRemaining > 0) {
			// A "--- " line followed by "+++ " and then a "@@" hunk header is
			// the next file's header pair, not a removed "-- x" line followed by
			// an added "++ y" line: an over-declared count would otherwise
			// swallow both and the hunk would later fail against the wrong
			// file. The "@@" requirement keeps a genuine adjacent pair intact.
			if strings.HasPrefix(raw, "--- ") && index+2 < len(lines) && strings.HasPrefix(lines[index+1], "+++ ") && strings.HasPrefix(lines[index+2], "@@") {
				return nil, fmt.Errorf("invalid unified diff at line %d: hunk ended before its declared line counts", lineNumber)
			}
			switch {
			case strings.HasPrefix(raw, "\\"):
				// "\ No newline at end of file" qualifies the previous line's side.
				if current != nil && lastSide == '+' {
					current.eofNewline = eofNewlineAbsent
				} else if current != nil && lastSide == '-' {
					current.oldNoNewline = true
					if current.eofNewline == eofNewlineKeep {
						current.eofNewline = eofNewlinePresent
					}
				}
				continue
			case strings.HasPrefix(raw, "-"):
				oldRemaining--
				lastSide = '-'
				if chunk != nil {
					chunk.old = append(chunk.old, raw[1:])
				}
			case strings.HasPrefix(raw, "+"):
				newRemaining--
				lastSide = '+'
				if current != nil && current.kind == structuredPatchAdd {
					added = append(added, raw[1:])
				} else if chunk != nil {
					chunk.new = append(chunk.new, raw[1:])
					chunk.newSourceOffsets = append(chunk.newSourceOffsets, -1)
				}
			default:
				content := raw
				if strings.HasPrefix(raw, " ") {
					content = raw[1:]
				} else if raw != "" {
					return nil, fmt.Errorf("invalid unified diff at line %d: hunk lines must start with ' ', '+', '-' or '\\'", lineNumber)
				}
				oldRemaining--
				newRemaining--
				lastSide = ' '
				if chunk != nil {
					offset := len(chunk.old)
					chunk.old = append(chunk.old, content)
					chunk.new = append(chunk.new, content)
					chunk.newSourceOffsets = append(chunk.newSourceOffsets, offset)
				}
			}
			continue
		}
		inHunk = false
		trimmed := strings.TrimSpace(raw)
		if oldRemaining > 0 || newRemaining > 0 {
			// The hunk ended before its declared counts were consumed. Report
			// it here rather than absorbing the next file's ---/+++ headers as
			// "-"/"+" content and failing later against the wrong file.
			return nil, fmt.Errorf("invalid unified diff at line %d: hunk ended before its declared line counts", lineNumber)
		}
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(raw, "\\"):
			// "\ No newline at end of file" directly after a hunk's last line.
			if current != nil && lastSide == '+' {
				current.eofNewline = eofNewlineAbsent
			} else if current != nil && lastSide == '-' {
				current.oldNoNewline = true
				if current.eofNewline == eofNewlineKeep {
					current.eofNewline = eofNewlinePresent
				}
			}
			continue
		case strings.HasPrefix(raw, "diff --git "):
			if err := flushHeaderOnly(lineNumber); err != nil {
				return nil, err
			}
			diffOperands = strings.TrimPrefix(raw, "diff --git ")
			diffOldPath, diffNewPath, diffPathsOK = sandbox.DiffGitPaths(diffOperands)
			diffOldPath, diffNewPath = filepath.ToSlash(diffOldPath), filepath.ToSlash(diffNewPath)
			diffHeaderLine = lineNumber
			if diffPathsOK {
				diffPath = diffNewPath
			} else {
				diffPath = ""
			}
			continue
		case strings.HasPrefix(raw, "deleted file mode "):
			headerOnly = 'd'
			continue
		case strings.HasPrefix(raw, "new file mode "):
			headerOnly = 'n'
			continue
		case strings.HasPrefix(raw, "index "), strings.HasPrefix(raw, "old mode "), strings.HasPrefix(raw, "new mode "):
			continue
		case strings.HasPrefix(raw, "similarity index "), strings.HasPrefix(raw, "dissimilarity index "):
			continue
		case strings.HasPrefix(raw, "rename from "), strings.HasPrefix(raw, "copy from "):
			pendingHeader = "rename"
			if strings.HasPrefix(raw, "copy from ") {
				pendingHeader = "copy"
			}
			from, ok := sandbox.ExtendedGitHeaderPath(strings.TrimPrefix(strings.TrimPrefix(raw, "rename from "), "copy from "))
			if !ok {
				return nil, fmt.Errorf("invalid unified diff at line %d: source path cannot be interpreted exactly", lineNumber)
			}
			pendingFrom = filepath.ToSlash(from)
			pendingKind = structuredPatchUpdate
			if pendingHeader == "copy" {
				pendingKind = structuredPatchCopy
			}
			if pendingFrom == "" {
				return nil, fmt.Errorf("invalid unified diff at line %d: missing source path", lineNumber)
			}
			if diffHeaderLine != 0 && diffPathsOK && pendingFrom != diffOldPath {
				return nil, fmt.Errorf("invalid unified diff at line %d: %s source disagrees with diff --git source from line %d", lineNumber, pendingHeader, diffHeaderLine)
			}
		case strings.HasPrefix(raw, "rename to "), strings.HasPrefix(raw, "copy to "):
			toHeader := "rename"
			if strings.HasPrefix(raw, "copy to ") {
				toHeader = "copy"
			}
			to, ok := sandbox.ExtendedGitHeaderPath(strings.TrimPrefix(strings.TrimPrefix(raw, "rename to "), "copy to "))
			if !ok {
				return nil, fmt.Errorf("invalid unified diff at line %d: destination path cannot be interpreted exactly", lineNumber)
			}
			to = filepath.ToSlash(to)
			if pendingFrom == "" || to == "" {
				return nil, fmt.Errorf("invalid unified diff at line %d: rename/copy destination without a source", lineNumber)
			}
			if pendingHeader != toHeader {
				return nil, fmt.Errorf("invalid unified diff at line %d: mismatched %s from/%s to headers", lineNumber, pendingHeader, toHeader)
			}
			if diffHeaderLine != 0 {
				matches := pendingFrom == diffOldPath && to == diffNewPath
				if !diffPathsOK {
					// Git's --no-prefix output does not quote ordinary spaces in
					// diff --git operands. Extended headers delimit those paths,
					// allowing their exact concatenation to resolve the ambiguity.
					matches = diffOperands == pendingFrom+" "+to
					if matches {
						diffOldPath, diffNewPath, diffPathsOK = pendingFrom, to, true
					}
				}
				if !matches {
					return nil, fmt.Errorf("invalid unified diff at line %d: %s paths disagree with diff --git paths from line %d", lineNumber, toHeader, diffHeaderLine)
				}
			}
			if err := finish(); err != nil {
				return nil, err
			}
			current = &structuredPatchOperation{kind: pendingKind, path: pendingFrom, movePath: to, line: lineNumber}
			oldPath, newPath, pendingFrom, pendingHeader = pendingFrom, to, "", ""
		case strings.HasPrefix(raw, "Binary files "), strings.HasPrefix(raw, "GIT binary patch"):
			return nil, fmt.Errorf("invalid unified diff at line %d: binary patches are not supported", lineNumber)
		case strings.HasPrefix(raw, "--- "):
			path, ok := patchFileHeaderPath(raw)
			if !ok {
				return nil, fmt.Errorf("invalid unified diff at line %d: path in --- header cannot be interpreted exactly", lineNumber)
			}
			oldPath = stripPatchPrefix(path)
			newPath = ""
			if oldPath == "" {
				return nil, fmt.Errorf("invalid unified diff at line %d: missing path in --- header", lineNumber)
			}
		case strings.HasPrefix(raw, "+++ "):
			path, ok := patchFileHeaderPath(raw)
			if !ok {
				return nil, fmt.Errorf("invalid unified diff at line %d: path in +++ header cannot be interpreted exactly", lineNumber)
			}
			newPath = stripPatchPrefix(path)
			if newPath == "" {
				return nil, fmt.Errorf("invalid unified diff at line %d: missing path in +++ header", lineNumber)
			}
			if err := startFile(lineNumber); err != nil {
				return nil, err
			}
		case strings.HasPrefix(raw, "@@"):
			if current == nil {
				return nil, fmt.Errorf("invalid unified diff at line %d: hunk before a ---/+++ header pair", lineNumber)
			}
			oldStart, oldCount, newCount, ok := parseHunkRange(raw)
			if !ok {
				return nil, fmt.Errorf("invalid unified diff at line %d: malformed hunk header", lineNumber)
			}
			// "\ No newline at end of file" describes the end of the file, so it
			// may only follow a file's final hunk; a marker before another hunk
			// would otherwise leak into that later hunk's result.
			if current.eofNewline != eofNewlineKeep {
				return nil, fmt.Errorf("invalid unified diff at line %d: \"\\ No newline at end of file\" must follow the last hunk of a file", lineNumber)
			}
			oldRemaining, newRemaining = oldCount, newCount
			inHunk = oldRemaining > 0 || newRemaining > 0
			lastSide = 0
			if current.kind != structuredPatchAdd {
				next := structuredPatchChunk{hasHint: true, hint: oldStart - 1}
				if oldCount == 0 {
					// A pure insertion's range names the line after which the
					// new lines go, so the insertion index is oldStart itself.
					next.hint = oldStart
				}
				if next.hint < 0 {
					next.hint = 0
				}
				current.chunks = append(current.chunks, next)
				chunk = &current.chunks[len(current.chunks)-1]
			}
		default:
			return nil, fmt.Errorf("invalid unified diff at line %d: unexpected %q", lineNumber, trimmed)
		}
	}
	// Strict at end of input on purpose: a hunk cut short (typically a model
	// response that stopped mid-patch) would otherwise apply as a partial
	// change — removals without their replacements — which is worse than a
	// clear error and a retry. Miscounted ranges with complete content are
	// still tolerated at every other boundary.
	if inHunk && (oldRemaining > 0 || newRemaining > 0) {
		return nil, fmt.Errorf("invalid unified diff at line %d: hunk ended before its declared line counts", len(lines))
	}
	if err := flushHeaderOnly(len(lines)); err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("unified diff contains no file changes")
	}
	return operations, nil
}

// parseHunkRange reads "@@ -a[,b] +c[,d] @@" and returns a, b and d; a missing
// count means 1 per unified-diff convention.
func parseHunkRange(line string) (oldStart, oldCount, newCount int, ok bool) {
	_, rest, found := strings.Cut(line, "@@")
	if !found {
		return 0, 0, 0, false
	}
	rangeSection := rest
	if before, _, found := strings.Cut(rest, "@@"); found {
		rangeSection = before
	}
	fields := strings.Fields(rangeSection)
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, 0, false
	}
	parse := func(spec string) (int, int, bool) {
		startText, countText, hasCount := strings.Cut(spec, ",")
		start, err := parseNonNegativeInt(startText)
		if err != nil {
			return 0, 0, false
		}
		count := 1
		if hasCount {
			if count, err = parseNonNegativeInt(countText); err != nil {
				return 0, 0, false
			}
		}
		return start, count, true
	}
	oldStart, oldCount, ok = parse(fields[0][1:])
	if !ok {
		return 0, 0, 0, false
	}
	_, newCount, ok = parse(fields[1][1:])
	if !ok {
		return 0, 0, 0, false
	}
	return oldStart, oldCount, newCount, true
}

func parseNonNegativeInt(text string) (int, error) {
	if text == "" {
		return 0, fmt.Errorf("empty number")
	}
	value := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", text)
		}
		value = value*10 + int(r-'0')
		if value > 1<<30 {
			return 0, fmt.Errorf("number too large: %q", text)
		}
	}
	return value, nil
}
