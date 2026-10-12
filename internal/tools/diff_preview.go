package tools

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/redaction"
	udiff "github.com/aymanbagabas/go-udiff"
)

// maxToolPreviewBytes caps the inline diff a write tool appends to its result, so
// a large generated file can't flood the transcript or balloon the persisted
// session events. Past this the tool falls back to its summary line alone.
const maxToolPreviewBytes = 48 * 1024

// A structured replacement contains two complete file sides, so its transport
// budget is intentionally separate from the single rendered-preview budget.
// Each side may be as large as a normal preview; aggregate producers apply the
// two-sided result cap in file order.
const maxToolResultFileDiffBytes = 2 * maxToolPreviewBytes

// FileDiff is a human-facing before/after file change. Registry-boundary
// redaction applies to both sides before any caller receives it.
type FileDiff struct {
	// Path is the canonical absolute path required by ACP diff content. The
	// separate ChangedFiles result remains workspace-relative for local UI use.
	Path string
	// OldExists and NewExists distinguish an empty file from a missing side of a
	// create/delete/move. Empty strings alone cannot encode that difference.
	OldExists bool
	NewExists bool
	OldText   string
	NewText   string
}

// boundedFileDiff declines rather than truncating: a truncated side would look
// like an exact file replacement. Callers keep ChangedFiles as the safe
// fallback for large, unsafe, or unchanged content. Each complete side gets the
// same bound as a rendered preview; aggregate result producers apply their own
// cap. Control bytes are rejected. Ordinary Unicode format/space characters are
// retained unless removing them reveals a credential shape that the normal
// redactor could not see in the original text.
func boundedFileDiff(path, oldText, newText string, oldExists, newExists bool) (FileDiff, bool) {
	if !filepath.IsAbs(path) || (!oldExists && !newExists) ||
		(oldExists == newExists && oldText == newText) ||
		!utf8.ValidString(oldText) || !utf8.ValidString(newText) ||
		unsafeDiffText(oldText) || unsafeDiffText(newText) ||
		len(oldText) > maxToolPreviewBytes || len(newText) > maxToolPreviewBytes {
		return FileDiff{}, false
	}
	return FileDiff{Path: path, OldExists: oldExists, NewExists: newExists, OldText: oldText, NewText: newText}, true
}

func unsafeDiffText(text string) bool {
	if !utf8.ValidString(text) {
		return true
	}
	hasCanonicalizableSeparator := false
	for _, r := range text {
		switch r {
		case '\n', '\r', '\t', ' ':
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
		if isDiffCredentialIgnorable(r) {
			hasCanonicalizableSeparator = true
		}
	}
	if !hasCanonicalizableSeparator {
		return false
	}
	return diffTextRevealsObfuscatedSecret(text)
}

func diffTextRevealsObfuscatedSecret(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	canonical := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', ' ':
			return r
		}
		if isDiffCredentialIgnorable(r) {
			return -1
		}
		return r
	}, text)
	return canonical != text && redaction.RedactString(canonical, redaction.Options{}) != canonical
}

// isDiffCredentialIgnorable mirrors Unicode's Default_Ignorable_Code_Point
// derived property using the tables exposed by the Go standard library. Cf
// catches format controls, while the other two tables cover non-Cf default
// ignorables such as U+034F COMBINING GRAPHEME JOINER and variation selectors.
// Unicode whitespace is also removable because it can split a credential that
// the boundary redactor would otherwise recognize. The caller preserves normal
// ASCII layout whitespace before consulting this helper.
func isDiffCredentialIgnorable(r rune) bool {
	return unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r) ||
		unicode.IsSpace(r)
}

// boundedUnifiedDiff returns a unified diff of oldContent -> newContent labelled
// with path, suitable for the TUI's diff card renderer. A create (oldContent "")
// yields an all-additions (green) preview; an overwrite/edit yields red/green.
// Returns "" when there is no change, the rendered diff is unsafe text, or the
// diff exceeds maxToolPreviewBytes. This applies the same unsafe-text gate as
// FileDiff to what reaches Display.Preview, without discarding a safe hunk only
// because an unrelated part of the source file contains an unsafe byte.
func boundedUnifiedDiff(path, oldContent, newContent string) string {
	diff := udiff.Unified(path, path, oldContent, newContent)
	if diff == "" || !utf8.ValidString(diff) || unsafeDiffText(diff) || len(diff) > maxToolPreviewBytes {
		return ""
	}
	return diff
}
