package tools

// Fuzzy fallback matching for edit_file. When the model's old_string fails to
// match byte-for-byte (drifted indentation, collapsed whitespace, escaped
// characters), these replacers propose candidate spans that differ only by the
// normalization each strategy explicitly handles. They never infer a match
// from similar but different content.
//
// Strategy cascade ported from opencode's edit tool, whose replacers were in
// turn distilled from Cline's diff-apply evals and gemini-cli's edit corrector.

import (
	"errors"
	"regexp"
	"strings"
)

// editReplacer proposes candidate spans for find inside content. Candidates
// are re-validated by fuzzyEditMatch before use.
type editReplacer func(content, find string) []string

var (
	errEditFuzzyNotFound  = errors.New("no fuzzy match for old_string")
	errEditFuzzyAmbiguous = errors.New("fuzzy match for old_string is ambiguous")
)

// fuzzyEditMatch runs the replacer cascade and returns the exact span of
// content to replace. When replaceAll is false the span must be unique in
// content; an ambiguous candidate is skipped in favor of later candidates and
// only reported if nothing unique is found. A span wildly larger than
// old_string is refused outright rather than risking a destructive edit.
func fuzzyEditMatch(content, find string, replaceAll bool) (string, error) {
	replacers := []editReplacer{
		lineTrimmedReplacer,
		whitespaceNormalizedReplacer,
		indentationFlexibleReplacer,
		escapeNormalizedReplacer,
		trimmedBoundaryReplacer,
	}
	found := false
	for _, replacer := range replacers {
		// Collect the replacer's DISTINCT candidate spans that literally occur in
		// content. Two or more distinct spans from one strategy (e.g. duplicate
		// blocks at different indentation, each occurring once) mean the model's
		// intent is genuinely ambiguous — silently editing the first would be a
		// wrong-span write, so it is rejected instead.
		var candidates []string
		seen := map[string]bool{}
		for _, search := range replacer(content, find) {
			if search == "" || seen[search] {
				continue
			}
			seen[search] = true
			if !strings.Contains(content, search) {
				continue
			}
			candidates = append(candidates, search)
		}
		if len(candidates) == 0 {
			continue
		}
		found = true
		if !replaceAll && len(candidates) > 1 {
			return "", errEditFuzzyAmbiguous
		}
		search := candidates[0]
		if isDisproportionateEditMatch(search, find) {
			return "", errors.New("refusing replacement because the matched span is much larger than old_string; re-read the file and provide the full exact old_string for the intended replacement")
		}
		if replaceAll {
			return search, nil
		}
		if strings.Index(content, search) == strings.LastIndex(content, search) {
			return search, nil
		}
		// The single candidate occurs at multiple positions; a later, stricter
		// strategy may still resolve a unique span, so keep cascading.
	}
	if !found {
		return "", errEditFuzzyNotFound
	}
	return "", errEditFuzzyAmbiguous
}

// isDisproportionateEditMatch guards against a fuzzy replacer matching a span
// far larger than the text the model asked to replace.
func isDisproportionateEditMatch(search, find string) bool {
	findLines := strings.Count(find, "\n") + 1
	searchLines := strings.Count(search, "\n") + 1
	limit := findLines + 3
	if findLines*2 > limit {
		limit = findLines * 2
	}
	if searchLines >= limit {
		return true
	}
	if findLines == 1 {
		return false
	}
	searchTrimmed := len(strings.TrimSpace(search))
	findTrimmed := len(strings.TrimSpace(find))
	byteLimit := findTrimmed + 500
	if findTrimmed*4 > byteLimit {
		byteLimit = findTrimmed * 4
	}
	return searchTrimmed > byteLimit
}

// splitFindLines splits find into lines, dropping the trailing empty line a
// trailing newline produces so windows align with real content lines.
func splitFindLines(find string) []string {
	lines := strings.Split(find, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineTrimmedReplacer matches when every line equals the corresponding
// old_string line after trimming surrounding whitespace.
func lineTrimmedReplacer(content, find string) []string {
	contentLines := strings.Split(content, "\n")
	findLines := splitFindLines(find)
	if len(findLines) == 0 {
		return nil
	}
	var candidates []string
	for i := 0; i+len(findLines) <= len(contentLines); i++ {
		matches := true
		for j := range findLines {
			if strings.TrimSpace(contentLines[i+j]) != strings.TrimSpace(findLines[j]) {
				matches = false
				break
			}
		}
		if matches {
			candidates = append(candidates, strings.Join(contentLines[i:i+len(findLines)], "\n"))
		}
	}
	return candidates
}

var editWhitespaceRun = regexp.MustCompile(`\s+`)

func normalizeEditWhitespace(text string) string {
	return strings.TrimSpace(editWhitespaceRun.ReplaceAllString(text, " "))
}

// whitespaceNormalizedReplacer matches after collapsing all whitespace runs to
// a single space: full lines, sub-line spans (via a word-boundary regex), and
// multi-line windows.
func whitespaceNormalizedReplacer(content, find string) []string {
	normalizedFind := normalizeEditWhitespace(find)
	if normalizedFind == "" {
		return nil
	}
	contentLines := strings.Split(content, "\n")
	var candidates []string
	var subLinePattern *regexp.Regexp
	for _, line := range contentLines {
		normalizedLine := normalizeEditWhitespace(line)
		if normalizedLine == normalizedFind {
			candidates = append(candidates, line)
			continue
		}
		if !strings.Contains(normalizedLine, normalizedFind) {
			continue
		}
		if subLinePattern == nil {
			words := strings.Fields(find)
			quoted := make([]string, len(words))
			for i, word := range words {
				quoted[i] = regexp.QuoteMeta(word)
			}
			pattern, err := regexp.Compile(strings.Join(quoted, `\s+`))
			if err != nil {
				continue
			}
			subLinePattern = pattern
		}
		if match := subLinePattern.FindString(line); match != "" {
			candidates = append(candidates, match)
		}
	}

	findLines := strings.Split(find, "\n")
	if len(findLines) > 1 {
		for i := 0; i+len(findLines) <= len(contentLines); i++ {
			block := strings.Join(contentLines[i:i+len(findLines)], "\n")
			if normalizeEditWhitespace(block) == normalizedFind {
				candidates = append(candidates, block)
			}
		}
	}
	return candidates
}

// stripCommonIndentation removes the minimum leading-whitespace width shared
// by all non-empty lines, so blocks match regardless of their nesting depth.
func stripCommonIndentation(text string) string {
	lines := strings.Split(text, "\n")
	minIndent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent <= 0 {
		return text
	}
	stripped := make([]string, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			stripped[i] = line
			continue
		}
		stripped[i] = line[minIndent:]
	}
	return strings.Join(stripped, "\n")
}

func indentationFlexibleReplacer(content, find string) []string {
	normalizedFind := stripCommonIndentation(find)
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")
	var candidates []string
	for i := 0; i+len(findLines) <= len(contentLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if stripCommonIndentation(block) == normalizedFind {
			candidates = append(candidates, block)
		}
	}
	return candidates
}

var editEscapeSequence = regexp.MustCompile("\\\\(n|t|r|'|\"|`|\\\\|\\n|\\$)")

// unescapeEditString undoes one level of string escaping (\n, \t, \", \\, a
// backslash-newline continuation, \$) — the model sometimes reproduces file
// content as it appeared inside a quoted string literal.
func unescapeEditString(text string) string {
	return editEscapeSequence.ReplaceAllStringFunc(text, func(match string) string {
		switch match[1:] {
		case "n":
			return "\n"
		case "t":
			return "\t"
		case "r":
			return "\r"
		case "\n":
			return "\n"
		default:
			// ', ", `, \, $ all unescape to themselves.
			return match[1:]
		}
	})
}

func escapeNormalizedReplacer(content, find string) []string {
	unescapedFind := unescapeEditString(find)
	var candidates []string
	if strings.Contains(content, unescapedFind) {
		candidates = append(candidates, unescapedFind)
	}
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(unescapedFind, "\n")
	for i := 0; i+len(findLines) <= len(contentLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if unescapeEditString(block) == unescapedFind {
			candidates = append(candidates, block)
		}
	}
	return candidates
}

// trimmedBoundaryReplacer tolerates stray leading/trailing whitespace (often
// blank lines) around an otherwise-exact old_string.
func trimmedBoundaryReplacer(content, find string) []string {
	trimmedFind := strings.TrimSpace(find)
	if trimmedFind == find || trimmedFind == "" {
		return nil
	}
	var candidates []string
	if strings.Contains(content, trimmedFind) {
		candidates = append(candidates, trimmedFind)
	}
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(find, "\n")
	for i := 0; i+len(findLines) <= len(contentLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if strings.TrimSpace(block) == trimmedFind {
			candidates = append(candidates, block)
		}
	}
	return candidates
}

// adaptReplacementToSpan re-shapes the model's replacement to the span a
// tolerant matcher resolved. When old_string only matched after normalization,
// new_string was written at old_string's (wrong) shape, so applying it raw
// would strip the file's indentation or drop a trailing CR:
//
//  1. Uniform re-indent: when every span line equals delta + the corresponding
//     find line's indentation (the line-trimmed / indentation-flexible shapes),
//     the same delta is prepended to every non-blank replacement line. Any
//     line that breaks the uniform-delta relationship disables the shift.
//  2. Trailing CR: a span from a CRLF file ends mid-line at "\r" (candidates
//     are built by joining lines split on "\n"); the replacement gets the same
//     trailing "\r" so the file's CRLF pairs stay intact.
func adaptReplacementToSpan(span, find, replacement string) string {
	if delta, ok := uniformIndentDelta(span, find); ok && delta != "" {
		lines := strings.Split(replacement, "\n")
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lines[i] = delta + line
		}
		replacement = strings.Join(lines, "\n")
	}
	if strings.HasSuffix(span, "\r") && !strings.HasSuffix(replacement, "\r") {
		replacement += "\r"
	}
	return replacement
}

// uniformIndentDelta returns the indentation prefix that, prepended to every
// non-blank find line, yields the corresponding span line's indentation. ok is
// false when line counts differ, any line pair disagrees on the delta, or the
// span is not simply a uniformly deeper-indented copy of find.
func uniformIndentDelta(span, find string) (string, bool) {
	spanLines := strings.Split(span, "\n")
	findLines := splitFindLines(find)
	if len(spanLines) != len(findLines) {
		return "", false
	}
	leadingWhitespace := func(line string) string {
		return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	delta := ""
	haveDelta := false
	for i := range findLines {
		spanLine := strings.TrimSuffix(spanLines[i], "\r")
		findLine := strings.TrimSuffix(findLines[i], "\r")
		if strings.TrimSpace(spanLine) == "" && strings.TrimSpace(findLine) == "" {
			continue
		}
		spanIndent := leadingWhitespace(spanLine)
		findIndent := leadingWhitespace(findLine)
		if !strings.HasSuffix(spanIndent, findIndent) {
			return "", false
		}
		lineDelta := spanIndent[:len(spanIndent)-len(findIndent)]
		if !haveDelta {
			delta = lineDelta
			haveDelta = true
			continue
		}
		if lineDelta != delta {
			return "", false
		}
	}
	return delta, haveDelta
}
