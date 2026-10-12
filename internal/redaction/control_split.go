package redaction

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A CREDENTIAL IS STILL A CREDENTIAL WITH AN INVISIBLE BYTE IN THE MIDDLE.
//
// The shape matchers (openaiKeyPattern, textSecretPatterns) all describe a
// CONTIGUOUS run of body characters, and none of their character classes admits
// a control or format character. So a single NUL, ESC or zero-width space
// dropped into the body of a key ends the match, and RedactString emits the two
// fragments untouched. Whoever reads that output next, a terminal that eats the
// escape, a log viewer, a JSON consumer that strips control bytes, or a person
// copying the text, sees the original key back, because nothing about those
// characters was ever displayed. Reported in #969 for NUL and ESC; every other
// Cc and Cf character behaves the same way, which is why this is written against
// the classes rather than against the two bytes named there.
//
// HOW: each shape is rewritten so that a run of separators may sit between any
// two characters it consumes, and matched against the ORIGINAL text. Nothing is
// removed before matching, so there are no offsets to map back, and the leading
// \b of every shape is judged by the regexp engine against the real neighbour:
// a key written after a NUL starts at a boundary, exactly as the unsplit key
// does, and a key glued to the end of a word does not.
//
// An earlier version removed the separators first and matched the compacted
// copy. That lost the difference between a separator inside a credential and one
// next to it, and the fixes for that (check the boundary against the source
// afterwards, map offsets back) left a hole: in "xAKIA<NUL>AKIAIOSF<SOH>ODNN7..."
// the leftmost compacted match began at the decoy AKIA, was then rejected for
// the "x" in front of it, and had already consumed the start of the real key, so
// the real key was never considered. Resuming the scan one byte after each
// rejection would close that, but it is quadratic for the greedy shapes on text
// like "ask-ask-ask-..." with a single NUL in it. Keeping the boundary inside
// the pattern lets RE2 do it in one linear pass.
//
// This pass runs AFTER the contiguous matchers, over their output, and never
// across a replacement. A credential that is whole is claimed by its own strict
// match, so two whole credentials with a separator between them stay two
// replacements with the separator still between them. Two split credentials do
// too: see spansFor.
//
// A match never starts or ends on a separator, so the separators on either side
// of a credential stay where they were: "<NUL>AKIA..." becomes
// "<NUL>[REDACTED]", as it does for the unsplit key.
//
// ONE KNOWN LIMIT, from running the contiguous matchers first. When the part of
// a split key BEFORE its first separator is already a complete credential on
// its own, the contiguous matcher claims that part and stops at the separator,
// and what follows is judged by itself. That is the rest of the key, which is no
// credential alone, so it stays visible: a fragment of the key, never the whole
// of it, because the part in front is replaced. For the same reason a credential
// glued straight onto that rest is left, where the unsplit key would have run on
// and swallowed it. Carrying the contiguous match across the separator instead
// would bring back what running first exists to prevent, since nothing tells the
// rest of a key from prose written after a whole one: that prose would be eaten,
// and a following JWT would lose its header.
//
// WHAT THIS DELIBERATELY DOES NOT TREAT AS A SEPARATOR: tab, newline and
// carriage return. Those three are real text structure rather than invisible
// filler, so a credential broken across a line boundary is still two visible
// fragments here and is not covered. Treating them as gaps would also let a
// match span a line break and collapse unrelated lines into one replacement. A
// credential split that way is visible to whoever reads it; the ones handled
// here are not. Invalid UTF-8 is out for the same reason: the engine reads an
// invalid byte as U+FFFD, which is drawn.

// splitSecretSeparator reports a character that can sit inside a credential
// without being seen. Cc covers the C0 and C1 controls (NUL, ESC, DEL, NEL, CSI)
// and Cf the format characters (zero-width space and joiner, word joiner, BOM,
// soft hyphen). Tab, newline and carriage return are excluded: see above.
func splitSecretSeparator(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}

// containsSplitSeparator is the cheap gate in front of the gap-tolerant pass, so
// text with nothing invisible in it, which is nearly all text, costs one scan and
// no allocation. ASCII is settled from the byte alone; only a byte that could
// begin a multi-byte rune needs decoding, and the C1 and Cf blocks all start
// with a lead byte at or above 0xC2.
func containsSplitSeparator(value string) bool {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b < utf8.RuneSelf {
			if b < 0x20 || b == 0x7f {
				if b == '\t' || b == '\n' || b == '\r' {
					continue
				}
				return true
			}
			continue
		}
		r, width := utf8.DecodeRuneInString(value[i:])
		if splitSecretSeparator(r) {
			return true
		}
		i += width - 1
	}
	return false
}

// stripSplitSeparators returns value with every splitSecretSeparator removed.
// Used only on a matched credential, to judge it by its visible characters.
func stripSplitSeparators(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for offset := 0; offset < len(value); {
		r, width := utf8.DecodeRuneInString(value[offset:])
		if !splitSecretSeparator(r) {
			builder.WriteString(value[offset : offset+width])
		}
		offset += width
	}
	return builder.String()
}

// splitSeparatorClass is splitSecretSeparator as a regexp character class,
// generated from the same Unicode tables and the same three exclusions so the
// class and the predicate cannot disagree.
var splitSeparatorClass = func() string {
	var class strings.Builder
	class.WriteByte('[')
	emit := func(lo, hi rune) {
		fmt.Fprintf(&class, `\x{%x}-\x{%x}`, lo, hi)
	}
	for _, table := range []*unicode.RangeTable{unicode.Cc, unicode.Cf} {
		visit := func(lo, hi, stride uint32) {
			for r := lo; r <= hi; r += stride {
				start := rune(r)
				if !splitSecretSeparator(start) {
					continue
				}
				end := start
				if stride == 1 {
					for next := rune(r) + 1; uint32(next) <= hi && splitSecretSeparator(next); next++ {
						end = next
					}
					r = uint32(end)
				}
				emit(start, end)
			}
		}
		for _, r := range table.R16 {
			visit(uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
		}
		for _, r := range table.R32 {
			visit(r.Lo, r.Hi, r.Stride)
		}
	}
	class.WriteByte(']')
	return class.String()
}()

// splitOpenAIPattern and splitTextPatterns are the shape matchers with a run of
// separators allowed between every two characters they consume, derived from
// the originals rather than written out a second time so the two lists cannot
// drift apart.
var (
	splitOpenAIPattern = gapTolerant(openaiKeyPattern)
	splitTextPatterns  = gapTolerantAll(textSecretPatterns)
)

// splitShape is one credential shape as the split pass uses it.
type splitShape struct {
	// free finds the shape anywhere, leftmost first. anchored matches it only
	// where the text begins, and whole only when it spans the entire text.
	free, anchored, whole *regexp.Regexp
	// prefix is the literal every match of the shape begins with.
	prefix string
	// accept is the test a match has to pass to be a credential, or nil when
	// every match is one.
	accept func(match string) bool
}

var splitShapes = func() []splitShape {
	shapes := []splitShape{newSplitShape(openaiKeyPattern, splitOpenAIPattern, func(match string) bool {
		return openAIShapeIsSecret(stripSplitSeparators(match))
	})}
	for i, pattern := range textSecretPatterns {
		shapes = append(shapes, newSplitShape(pattern, splitTextPatterns[i], nil))
	}
	return shapes
}()

func newSplitShape(original, free *regexp.Regexp, accept func(string) bool) splitShape {
	return splitShape{
		free:     free,
		anchored: regexp.MustCompile(`\A(?:` + free.String() + `)`),
		whole:    regexp.MustCompile(`\A(?:` + free.String() + `)\z`),
		prefix:   leadingLiteral(original),
		accept:   accept,
	}
}

// leadingLiteral is the literal every match of pattern begins with, read past a
// leading \b, or "" when the pattern does not open with one.
func leadingLiteral(pattern *regexp.Regexp) string {
	tree, err := syntax.Parse(pattern.String(), syntax.Perl)
	if err != nil {
		panic(fmt.Sprintf("redaction: parse %q: %v", pattern.String(), err))
	}
	nodes := []*syntax.Regexp{tree}
	if tree.Op == syntax.OpConcat {
		nodes = tree.Sub
	}
	for _, node := range nodes {
		if node.Op == syntax.OpWordBoundary {
			continue
		}
		if node.Op == syntax.OpLiteral {
			return string(node.Rune)
		}
		return ""
	}
	return ""
}

// maxRunOnCandidates bounds the anchored matches tried inside one match, so a
// match full of lookalike starts still costs a fixed number of them.
const maxRunOnCandidates = 8

// spansFor turns one match of the shape into the stretches to replace.
//
// A MATCH CAN RUN ON INTO THE NEXT CREDENTIAL OF ITS OWN SHAPE. Gaps are allowed
// inside a match, and a separator between two credentials looks exactly like one
// inside a credential, so a greedy body runs through it into whatever follows. A
// credential of another shape is still found, because every shape is matched on
// its own and the union is replaced. One of the SAME shape is not: matches of one
// pattern never overlap, so the second credential cannot start inside the first
// one's match. Most bodies swallow it whole, so nothing leaks, but a JWT stops at
// the next token's first dot, and two split JWTs side by side left the second
// one's payload and signature in the clear. And a match the filter rejects hides
// whatever it ran into, so kebab-case prose in front of a split key took the key
// down with it. Reported by @jatmn.
//
// So when a match runs through a run of separators into a position where the
// same shape matches again and reaches at least as far, that credential is taken
// on its own, and the first is cut at the separators if it is a complete match by
// itself. The separators between the two then stay outside both replacements, as
// they do between two whole credentials, and each piece is filtered on its own.
// When the first is not complete alone, it keeps the whole of its match and the
// union joins the two into one replacement.
//
// ONE STEP PER MATCH, NOT THE WHOLE CHAIN. The second credential's own match can
// run on into a third, and following that from here would walk the rest of a
// chain once for every match inside it, which is quadratic: a megabyte of split
// JWTs ran for minutes. The scan's own next match starts inside the second
// credential and takes the step after it, so the chain is still covered, and in
// a chain of three or more the markers after the first pair can join into one.
func (shape splitShape) spansFor(text string, start, end int) [][]int {
	next, gap, nextEnd := shape.runOn(text, start, end)
	if next < 0 {
		return shape.keep(nil, text, start, end)
	}
	var spans [][]int
	if shape.whole.MatchString(text[start:gap]) {
		spans = shape.keep(spans, text, start, gap)
	} else {
		spans = shape.keep(spans, text, start, end)
	}
	return shape.keep(spans, text, next, nextEnd)
}

func (shape splitShape) keep(spans [][]int, text string, start, end int) [][]int {
	if shape.accept != nil && !shape.accept(text[start:end]) {
		return spans
	}
	return append(spans, []int{start, end})
}

// runOn looks inside the match [start, end) for where it ran through a run of
// separators into another credential of the same shape. It returns where that
// credential begins, where the separator run in front of it begins, and where the
// credential's own match ends, or next = -1 when there is none.
//
// Latest first, because a match reaches into the next credential only as far as
// its own pattern allows. Only where the text after the separators opens with the
// shape's literal prefix, which is where a match of the shape can begin; the
// separator in front supplies the word boundary, so matching from there judges it
// the same as the whole text would.
func (shape splitShape) runOn(text string, start, end int) (next, gap, nextEnd int) {
	tried := 0
	for at := end; at > start && tried < maxRunOnCandidates; {
		r, size := utf8.DecodeLastRuneInString(text[start:at])
		if !splitSecretSeparator(r) {
			at -= size
			continue
		}
		candidate := at
		for at > start {
			r, size = utf8.DecodeLastRuneInString(text[start:at])
			if !splitSecretSeparator(r) {
				break
			}
			at -= size
		}
		if !hasGappedPrefix(text[candidate:], shape.prefix) {
			continue
		}
		tried++
		if match := shape.anchored.FindStringIndex(text[candidate:]); match != nil && candidate+match[1] >= end {
			return candidate, at, candidate + match[1]
		}
	}
	return -1, 0, 0
}

// hasGappedPrefix reports whether text opens with prefix, allowing a run of
// separators between its characters the way the shapes themselves do. A plain
// prefix test missed a token split inside its first few characters, and the run
// that swallowed it then went unnoticed.
func hasGappedPrefix(text, prefix string) bool {
	at := 0
	for index, want := range prefix {
		if index > 0 {
			for at < len(text) {
				r, size := utf8.DecodeRuneInString(text[at:])
				if !splitSecretSeparator(r) {
					break
				}
				at += size
			}
		}
		if at >= len(text) {
			return false
		}
		r, size := utf8.DecodeRuneInString(text[at:])
		if r != want {
			return false
		}
		at += size
	}
	return true
}

func gapTolerantAll(patterns []*regexp.Regexp) []*regexp.Regexp {
	tolerant := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		tolerant = append(tolerant, gapTolerant(pattern))
	}
	return tolerant
}

// gapTolerant rewrites pattern so that a run of separators may appear before
// every character it consumes except the first. Zero-width assertions, the
// leading \b in particular, are left exactly where they were.
func gapTolerant(pattern *regexp.Regexp) *regexp.Regexp {
	tree, err := syntax.Parse(pattern.String(), syntax.Perl)
	if err != nil {
		panic(fmt.Sprintf("redaction: parse %q: %v", pattern.String(), err))
	}
	separator, err := syntax.Parse(splitSeparatorClass, syntax.Perl)
	if err != nil {
		panic(fmt.Sprintf("redaction: parse separator class: %v", err))
	}
	consumed := false
	return regexp.MustCompile(withGaps(tree, separator, &consumed).String())
}

// withGaps inserts the separator run before each consuming atom. consumed
// tracks whether anything has been consumed yet on this path, because the first
// character of a match must be a real one: a leading gap would let a match start
// on a separator and swallow it.
func withGaps(node, separator *syntax.Regexp, consumed *bool) *syntax.Regexp {
	switch node.Op {
	case syntax.OpLiteral:
		parts := make([]*syntax.Regexp, 0, len(node.Rune))
		for _, r := range node.Rune {
			atom := &syntax.Regexp{Op: syntax.OpLiteral, Rune: []rune{r}, Flags: node.Flags}
			parts = append(parts, gapBefore(atom, separator, consumed))
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return &syntax.Regexp{Op: syntax.OpConcat, Sub: parts}
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return gapBefore(node, separator, consumed)
	case syntax.OpConcat, syntax.OpCapture:
		for i := range node.Sub {
			node.Sub[i] = withGaps(node.Sub[i], separator, consumed)
		}
		return node
	case syntax.OpAlternate:
		start, after := *consumed, *consumed
		for i := range node.Sub {
			branch := start
			node.Sub[i] = withGaps(node.Sub[i], separator, &branch)
			after = after || branch
		}
		*consumed = after
		return node
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		// Every shape opens with a literal prefix, so a repetition is never the
		// first thing a match consumes. If one ever is, its first iteration would
		// need no gap and the rest would; fail loudly rather than guess.
		if !*consumed {
			panic("redaction: gap-tolerant rewrite reached a repetition before any literal")
		}
		node.Sub[0] = withGaps(node.Sub[0], separator, consumed)
		return node
	case syntax.OpWordBoundary, syntax.OpNoWordBoundary, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpEmptyMatch:
		return node
	default:
		panic(fmt.Sprintf("redaction: gap-tolerant rewrite does not handle %v", node.Op))
	}
}

func gapBefore(atom, separator *syntax.Regexp, consumed *bool) *syntax.Regexp {
	if !*consumed {
		*consumed = true
		return atom
	}
	gap := &syntax.Regexp{Op: syntax.OpStar, Sub: []*syntax.Regexp{separator}}
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: []*syntax.Regexp{gap, atom}}
}

// openAIShapeIsSecret is the digit/kebab-case filter RedactString applies to an
// openaiKeyPattern match, named so both callers state the same rule once.
func openAIShapeIsSecret(match string) bool {
	if knownOpenAIKeyPrefix(match) || secretMatchHasDigit(match) {
		return true
	}
	return !strings.Contains(strings.TrimPrefix(match, "sk-"), "-")
}

// redactControlSplitSecrets replaces credentials whose body is interrupted by
// invisible characters. It is a no-op for text containing none of them, which is
// nearly all text, so the contiguous path keeps its behavior exactly.
//
// It runs over the contiguous matchers' output and stops at every replacement
// they left behind: those mark text already accounted for, and a match reaching
// across one would be reaching out of one credential and into the next.
//
// It runs at most twice. The second pass exists for a credential glued straight
// onto the end of another split one: it has no word boundary of its own until
// the first is replaced, and the replacement then supplies one, the same way the
// contiguous shapes cascade into each other. Two passes and no more keeps the
// cost linear; a longer chain of glued split credentials is left as the
// contiguous matchers leave a chain of glued whole ones.
func redactControlSplitSecrets(value, replacement string) string {
	if !containsSplitSeparator(value) {
		return value
	}
	once := redactSplitRegions(value, replacement, true)
	if once == value {
		return value
	}
	// Ungated: the credential the second pass exists for is often contiguous
	// itself (a whole JWT glued onto a split AWS key), so its region can hold no
	// separator at all once the first pass has cut the text around it.
	return redactSplitRegions(once, replacement, false)
}

func redactSplitRegions(value, replacement string, gated bool) string {
	if replacement == "" {
		return redactSplitRegion(value, replacement, 0, gated)
	}
	regions := strings.Split(value, replacement)
	if len(regions) == 1 {
		return redactSplitRegion(value, replacement, 0, gated)
	}
	previous := byte(0)
	for i, region := range regions {
		regions[i] = redactSplitRegion(region, replacement, previous, gated)
		previous = replacement[len(replacement)-1]
	}
	return strings.Join(regions, replacement)
}

// redactSplitRegion runs every gap-tolerant shape over one stretch of text with
// no replacement inside it and replaces the UNION of what they found.
//
// Independently, not one after another. An unbounded body with gaps allowed will
// run on across a separator into whatever follows, and when that is the start of
// another credential it takes that credential's prefix with it: in
// "sk-ant-…<ESC>build<ESC>eyJ….eyJ….sig" the Anthropic shape stops only at the
// JWT's first dot. Applied in sequence, the JWT shape then finds its header gone
// and the payload and signature are left in the clear. Matched against the same
// text, the JWT shape still finds the whole token, and the union covers both.
//
// previous is the byte immediately before the region in the original, or zero at
// the start of the string. It is lent to the engine in front of the region so a
// leading \b is judged against the real neighbour, and a match is never allowed
// to begin on it.
func redactSplitRegion(value, replacement string, previous byte, gated bool) string {
	if gated && !containsSplitSeparator(value) {
		return value
	}
	text, lead := value, 0
	if previous != 0 {
		text, lead = string([]byte{previous})+value, 1
	}
	var spans [][]int
	for _, shape := range splitShapes {
		for _, match := range shape.free.FindAllStringIndex(text, -1) {
			if match[0] >= lead {
				spans = append(spans, shape.spansFor(text, match[0], match[1])...)
			}
		}
	}
	return spliceSpans(text, spans, replacement)[lead:]
}

// spliceSpans replaces every named range of value with replacement, merging
// ranges that overlap so one stretch of text is never replaced twice.
func spliceSpans(value string, spans [][]int, replacement string) string {
	if len(spans) == 0 {
		return value
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i][0] != spans[j][0] {
			return spans[i][0] < spans[j][0]
		}
		return spans[i][1] > spans[j][1]
	})
	var builder strings.Builder
	builder.Grow(len(value))
	written := 0
	for _, span := range spans {
		if span[0] < written {
			// Overlaps the stretch just replaced: extend it rather than emit a
			// second marker, which would claim two credentials where there was one
			// stretch of text.
			if span[1] > written {
				written = span[1]
			}
			continue
		}
		builder.WriteString(value[written:span[0]])
		builder.WriteString(replacement)
		written = span[1]
	}
	builder.WriteString(value[written:])
	return builder.String()
}
