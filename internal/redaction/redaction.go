package redaction

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	RedactedSecret    = "[REDACTED]"
	CircularReference = "[Circular]"
	maxDepthDefault   = 16
)

type Options struct {
	Replacement        string
	ExtraSensitiveKeys []string
	ExtraSecretValues  []string
	MaxDepth           int
}

type RedactedError struct {
	Name    string         `json:"name"`
	Message string         `json:"message"`
	Stack   string         `json:"stack,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

var sensitiveKeys = map[string]struct{}{
	"access_token":          {},
	"anthropic_api_key":     {},
	"api_key":               {},
	"apikey":                {},
	"auth_token":            {},
	"authorization":         {},
	"aws_secret_access_key": {},
	"aws_session_token":     {},
	"bearer":                {},
	"bearer_token":          {},
	"client_secret":         {},
	"cookie":                {},
	"credential":            {},
	"credentials":           {},
	"gemini_api_key":        {},
	"github_token":          {},
	"gitlab_token":          {},
	"google_api_key":        {},
	"id_token":              {},
	"jwt":                   {},
	"npm_token":             {},
	"oauth_token":           {},
	"openai_api_key":        {},
	"passphrase":            {},
	"password":              {},
	"private_key":           {},
	"proxy_authorization":   {},
	"refresh_token":         {},
	"secret":                {},
	"session_token":         {},
	"set_cookie":            {},
	"token":                 {},
	"x_api_key":             {},
	"zero_api_key":          {},
}

// openaiKeyPattern mirrors secrets.Scan's broad sk- body. Known OpenAI
// prefixes (sk-proj-/sk-svcacct-/sk-admin-) are always redacted; other sk-
// digit-free matches with an interior hyphen are left alone (kebab-case false
// positives), while digit-free legacy sk- credentials are still redacted.
// Applied via ReplaceAllStringFunc rather than the plain list below.
var openaiKeyPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)

// textSecretPatterns mirror secrets.Scan for end-boundary behavior and the
// shared high-confidence shapes. A leading \b keeps each pattern from firing
// mid-word; a trailing \b is omitted so a secret followed by more word
// characters outside its body class (e.g. AKIA…EXAMPLEEXTRA) still matches,
// and a secret that ends in "-" (allowed by some body classes) is fully
// redacted rather than leaving the hyphen behind. glpat is redaction-only
// (not in secrets.Scan); ASIA temporary access keys are kept alongside AKIA.
// openai keys are handled separately (digit filter). JWT has a strict form
// (both segments start with eyJ) and a looser three-segment form.
var textSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-ant-(?:api\d{2}-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{12,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35,}`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

// textSecretPrefixes lists the mandatory literal prefixes for each entry in
// textSecretPatterns so RedactString can skip the regex scan when none of the
// trigger prefixes appear in the input.
var textSecretPrefixes = [][]string{
	{"sk-ant-"},
	{"github_pat_"},
	{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"},
	{"glpat-"},
	{"AIza"},
	{"xox"},
	{"AKIA", "ASIA"},
	{"eyJ"},
	{"eyJ"},
}

type secretTrigger struct {
	literal string
	// shape indexes the anchoredSecretPatterns entry the trigger belongs to,
	// so a cut can chain the bytes after it against the shape the trigger
	// actually starts.
	shape int
}

// secretTriggers flattens textSecretPrefixes into one sorted list for the
// swallowed-trigger search below. Sorted so cuts are discovered earliest-first
// regardless of which pattern list they came from.
var secretTriggers = func() []secretTrigger {
	pairs := []secretTrigger{}
	for shape, prefixes := range textSecretPrefixes {
		for _, prefix := range prefixes {
			pairs = append(pairs, secretTrigger{literal: prefix, shape: shape})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].literal < pairs[j].literal })
	return pairs
}()

// longestTrigger bounds how far past a swallowed-scored span the trigger
// inspection may need to look: an occurrence whose last bytes run past the
// span end (a greedy body stops at a byte outside its class, and several
// triggers end in one).
var longestTrigger = func() int {
	longest := 0
	for _, trigger := range secretTriggers {
		if len(trigger.literal) > longest {
			longest = len(trigger.literal)
		}
	}
	return longest
}()

// anchoredSecretPatterns mirrors textSecretPatterns with the leading \b
// replaced by \A, so each shape can be tested at an exact byte offset. A
// credential that starts exactly where a redacted span ends (two AWS keys
// glued end to end, e.g. AKIA...AKIA...) has no word boundary on its left,
// so the plain pattern misses it; the anchored variant catches it.
var anchoredSecretPatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(textSecretPatterns))
	for i, p := range textSecretPatterns {
		src := p.String()
		if strings.HasPrefix(src, `\b`) {
			src = `\A(?:` + src[len(`\b`):] + `)`
		}
		out[i] = regexp.MustCompile(src)
	}
	return out
}()

// A credential's body can run into the trigger prefix of the NEXT one when
// both are glued end to end. For fixed-length bodies this cannot happen — the
// `{16}` stops the body exactly where the next AKIA...AKIA... key begins,
// which is the case the anchored end-chaining always covered. An unbounded
// body ({36,} on a github key, {20,} on an API key) swallows whichever trigger
// bytes sit inside its class — "ghp" after a github key is all lowercase
// letters — so the first match ends PAST the next credential's trigger, and a
// scan that re-anchors at the span end finds nothing: the second credential
// is never redacted.
//
// redactedSpan is one region of a redaction: the slice s[start:end] and the
// bytes to write in its place. Chained runs can link shapes together, so a
// span says which shape produced it rather than assuming the caller's pattern
// covered it.
type redactedSpan struct {
	start int
	end   int
	// text replaces s[start:end]. Every entry in textSecretPatterns captures
	// nothing, so this is the replacement itself unless a custom replacement
	// template references a group; anchoring the expansion to the shape that
	// matched keeps that working for cross-shape chains.
	text string
}

// redactAdjacent redacts every match of pattern, plus every credential glued
// into the runs those matches start. Runs follow three loss-of-boundary
// mechanisms: end-chaining (a credential glued exactly at the previous span's
// end, the AKIA...AKIA... case), all-shape chaining (a glued credential of a
// different shape), and the swallowed-trigger cut (one credential's greedy
// body runs into the next one's trigger prefix, hiding it from any scan that
// re-anchors at a span end). A run ends at the first byte boundary where no
// shape matches; a mid-word lookalike never matches and never starts one.
func redactAdjacent(s string, pattern, anchored *regexp.Regexp, replacement string) string {
	plain := pattern.FindAllStringIndex(s, -1)
	if len(plain) == 0 {
		return s
	}
	// Bound the outer loop to the original match count. Every span collects
	// its own run; re-reading len(plain) after appends would re-run every
	// appended credential and grow the span count (with the anchored scans
	// each drags) exponentially on long glued runs.
	n := len(plain)
	spans := make([]redactedSpan, 0, n+4)
	for i := 0; i < n; i++ {
		spans = appendCredentialRun(s, plain[i][0], plain[i][1], replacement, spans)
	}
	return writeRedactedSpans(s, spans)
}

func writeRedactedSpans(s string, spans []redactedSpan) string {
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.start < pos {
			continue // overlapping span, already covered
		}
		out.WriteString(s[pos:sp.start])
		out.WriteString(sp.text)
		pos = sp.end
	}
	out.WriteString(s[pos:])
	return out.String()
}

// appendCredentialRun follows the path that begins exactly at start, whose
// first plain match ended at end. Every step finds the shape that matches at
// the current boundary, tries to re-bind that match at a swallowed trigger,
// and otherwise consumes the whole match; the run ends where no shape does.
func appendCredentialRun(s string, start, end int, replacement string, spans []redactedSpan) []redactedSpan {
	cur := start
	for cur < len(s) {
		shape, matchEnd := firstShapeAt(s, cur)
		if shape < 0 || matchEnd == cur {
			break
		}
		if cutStart, cut := earliestSwallowedCut(s, cur, matchEnd, shape); cut {
			// The bytes before the trigger are the credential that matched;
			// from the trigger on the run continues under the next shape.
			spans = append(spans, redactedSpan{start: cur, end: cutStart, text: shapeReplacement(shape, s[cur:cutStart], replacement)})
			cur = cutStart
			continue
		}
		spans = append(spans, redactedSpan{start: cur, end: matchEnd, text: shapeReplacement(shape, s[cur:matchEnd], replacement)})
		cur = matchEnd
	}
	return spans
}

func shapeReplacement(shape int, slice, replacement string) string {
	return anchoredSecretPatterns[shape].ReplaceAllString(slice, replacement)
}

// firstShapeAt returns the first anchored shape that matches exactly at off,
// with the end of its match. Shapes are tried in declaration order, so the
// strict JWT form wins over the loose one; both redact the same bytes, so the
// tie-break only decides which regex expanded the replacement.
func firstShapeAt(s string, off int) (shape int, end int) {
	for shape, anchored := range anchoredSecretPatterns {
		loc := anchored.FindStringIndex(s[off:])
		if loc != nil && loc[0] == 0 && loc[1] > 0 {
			return shape, off + loc[1]
		}
	}
	return -1, off
}

// earliestSwallowedCut searches the matched span [start, end) of shape for a
// trigger literal that begins a valid credential right there: the head (the
// bytes before it) still matches the shape as a whole, and the run chained
// from the trigger reaches past the span. The earliest such trigger is the
// cut a boundary at that byte would have produced.
//
// The head requirement and the past-the-end requirement keep the cut honest:
// a trigger spelled mid-body that does not begin a credential leaves the span
// alone, and a cut that stayed inside the original span recovers no bytes —
// it would only split one redaction in two.
//
// The candidate set is the trigger occurrences inside the span, bounded by
// the span plus one trigger length; every occurrence of every trigger in that
// window is a candidate, earliest-first.
func earliestSwallowedCut(s string, start, end, shape int) (int, bool) {
	if end-start <= 1 {
		return 0, false
	}
	lookahead := end + longestTrigger
	if lookahead > len(s) {
		lookahead = len(s)
	}
	window := s[start+1 : lookahead]

	// A trigger occurrence whose bytes begin inside the span may extend past
	// its end: the greedy body stops at one byte outside its class, and
	// several triggers end in exactly such a byte ("ghp_"'s underscore,
	// "sk-ant-" plus a hyphen).
	anchored := anchoredSecretPatterns[shape]
	examined := 0
	for _, q := range triggerOccurrences(window, start+1, end) {
		if examined >= maxTriggerOccurrences {
			// Occurrences are earliest-first, so the ones past the cap are the
			// farthest into the span; a cut among them is missed and the
			// greedy span stands, as it did before the cut existed.
			break
		}
		examined++
		// The bytes before the trigger must still be a complete match of this
		// shape: a trigger spelling inside a body whose head does not
		// validate is prose-shaped, not a swallowed credential.
		loc := anchored.FindStringIndex(s[start:q])
		if loc == nil || loc[0] != 0 || loc[1] != q-start {
			continue
		}
		if runReach(s, q) > end {
			return q, true
		}
	}
	return 0, false
}

// maxTriggerOccurrences bounds how many trigger occurrences one span is
// examined for, after sorting. Real glued credentials announce their trigger
// within reach of the span's own start, so a distant lookalike run does not
// get walked to its end; the split path makes the same trade ("at most two
// passes and no more"). A cut found past the cap is missed and the greedy
// span stands, as it did before the cut existed.
const maxTriggerOccurrences = 64

// triggerOccurrences returns every occurrence of every trigger literal that
// begins inside [windowStart, windowEnd), where window is the slice rooted at
// windowStart that the caller bounded so occurrences could reach one trigger
// length past its end. Earliest first, duplicates collapsed.
func triggerOccurrences(window string, windowStart, windowEnd int) []int {
	occurrences := make([]int, 0, 4)
	for _, trigger := range secretTriggers {
		at := 0
		for {
			found := strings.Index(window[at:], trigger.literal)
			if found < 0 {
				break
			}
			q := windowStart + at + found
			if q >= windowEnd {
				break // later occurrences for this trigger start even later
			}
			occurrences = append(occurrences, q)
			at += found + 1
		}
	}
	sort.Ints(occurrences)
	// In-place deduplicate: duplicates come only from triggers shared between
	// shapes ("eyJ"), and zero is never an occurrence — the scan starts past it.
	unique := occurrences[:0]
	for i, q := range occurrences {
		if i > 0 && q == occurrences[i-1] {
			continue
		}
		unique = append(unique, q)
	}
	return unique
}

// runReach reports how far the credential run beginning exactly at off
// extends: each boundary picks the first shape that matches there, until one
// does not. The run is followed, not redacted; this is the shared-size check
// that keeps a cut honest.
func runReach(s string, off int) int {
	cur := off
	for cur < len(s) {
		_, matchEnd := firstShapeAt(s, cur)
		if matchEnd == cur {
			break
		}
		cur = matchEnd
	}
	return cur
}

var (
	privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	jsonStringPattern = regexp.MustCompile(`("([^"\\]*(?:\\.[^"\\]*)*)"\s*:\s*)"([^"\\]*(?:\\.[^"\\]*)*)"`)
	assignPattern     = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]*)(\s*=\s*)(?:"([^"]*)"|'([^']*)'|([^\s&]+))`)
	// Redact the ENTIRE credential after the scheme (to end of line), not just the
	// first token: parameterized schemes (Digest, OAuth, AWS4-HMAC-SHA256) spread
	// the secret across comma-separated params (…, response=…, Signature=…), so a
	// single-token capture would leave the actual secret visible. A known scheme is
	// kept for readability; the scheme is OPTIONAL so an opaque or custom-scheme
	// credential (no recognized scheme) still has its whole value redacted (M12).
	headerPattern = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\s*:\s*(?:(bearer|basic|token|apikey|api-key|digest|negotiate|oauth|aws4-hmac-sha256)\s+)?([^\r\n]+)`)
	secretHeader  = regexp.MustCompile(`(?i)\b(x-api-key|api-key|cookie|set-cookie)\s*:\s*([^\r\n]+)`)
	queryPattern  = regexp.MustCompile(`([?&])([^=&#\s]+)=([^&#\s]+)`)
)

func IsSensitiveKey(key string, options Options) bool {
	normalized := normalizeKey(key)
	if normalized == "" {
		return false
	}
	if _, ok := sensitiveKeys[normalized]; ok {
		return true
	}
	for _, extra := range options.ExtraSensitiveKeys {
		if normalizeKey(extra) == normalized {
			return true
		}
	}
	return keyLooksSensitive(normalized)
}

// secretKeySegments are "_"-delimited segments that mark the whole key sensitive
// even when the full name isn't in the exact list, so compound names like
// db_password, session_secret, and stripe_secret_key are caught. "token" is
// handled separately (suffix-only, in keyLooksSensitive) so the agent's many
// token-COUNT fields (max_tokens, prompt_tokens, token_count) are never redacted.
var secretKeySegments = map[string]struct{}{
	"password":    {},
	"passwd":      {},
	"passphrase":  {},
	"secret":      {},
	"credential":  {},
	"credentials": {},
	"apikey":      {},
}

// keyLooksSensitive applies conservative structural heuristics to a normalized
// key (already lower-cased and "_"-delimited by normalizeKey). It is deliberately
// narrow: a bare "token"/"key" segment is NOT enough, so token-count and ordinary
// "*_key" fields (max_tokens, primary_key, public_key) stay un-redacted.
func keyLooksSensitive(normalized string) bool {
	segments := strings.Split(normalized, "_")
	for i, seg := range segments {
		if _, ok := secretKeySegments[seg]; ok {
			return true
		}
		// "<x>_token" (singular, trailing) is a credential — auth_token, csrf_token,
		// vault_token. NOT "tokens" (plural count) and NOT "token_<x>" (token_count,
		// token_usage), where token is pluralized or not the trailing segment.
		if seg == "token" && i > 0 && i == len(segments)-1 {
			return true
		}
		// "api_key" / "private_key" as adjacent segments. A bare "key" stays
		// non-sensitive (primary_key, public_key, cache_key, foreign_key, …).
		if seg == "key" && i > 0 {
			switch segments[i-1] {
			case "api", "private":
				return true
			}
		}
	}
	return false
}

func RedactString(value string, options Options) string {
	replacement := replacement(options)
	redacted := value
	if len(options.ExtraSecretValues) > 0 {
		secrets := append([]string{}, options.ExtraSecretValues...)
		sort.SliceStable(secrets, func(i, j int) bool {
			return len(secrets[i]) > len(secrets[j])
		})
		for _, secret := range secrets {
			if strings.TrimSpace(secret) != "" {
				redacted = strings.ReplaceAll(redacted, secret, replacement)
			}
		}
	}

	if strings.Contains(redacted, "-----BEGIN ") && strings.Contains(redacted, "PRIVATE KEY") {
		redacted = privateKeyPattern.ReplaceAllString(redacted, replacement)
	}
	if strings.Contains(redacted, ":") && strings.Contains(redacted, "\"") {
		redacted = jsonStringPattern.ReplaceAllStringFunc(redacted, func(match string) string {
			parts := jsonStringPattern.FindStringSubmatch(match)
			if len(parts) < 3 || !IsSensitiveKey(parts[2], options) {
				return match
			}
			return parts[1] + `"` + replacement + `"`
		})
	}
	if strings.Contains(redacted, "=") {
		redacted = assignPattern.ReplaceAllStringFunc(redacted, func(match string) string {
			parts := assignPattern.FindStringSubmatch(match)
			if len(parts) < 6 || !IsSensitiveKey(parts[1], options) {
				return match
			}
			if parts[3] != "" {
				return parts[1] + parts[2] + `"` + replacement + `"`
			}
			if parts[4] != "" {
				return parts[1] + parts[2] + `'` + replacement + `'`
			}
			return parts[1] + parts[2] + replacement
		})
	}
	if strings.Contains(redacted, ":") {
		if containsCaseInsensitive(redacted, "authorization") {
			redacted = headerPattern.ReplaceAllStringFunc(redacted, func(match string) string {
				groups := headerPattern.FindStringSubmatch(match)
				if groups[2] != "" {
					return groups[1] + ": " + groups[2] + " " + replacement
				}
				return groups[1] + ": " + replacement
			})
		}
		redacted = secretHeader.ReplaceAllString(redacted, "$1: "+replacement)
	}
	if strings.Contains(redacted, "://") && strings.Contains(redacted, "@") {
		redacted = redactURLPasswords(redacted, replacement)
	}
	if strings.Contains(redacted, "?") || strings.Contains(redacted, "&") {
		redacted = queryPattern.ReplaceAllStringFunc(redacted, func(match string) string {
			parts := queryPattern.FindStringSubmatch(match)
			if len(parts) < 4 || !IsSensitiveKey(parts[2], options) {
				return match
			}
			return parts[1] + parts[2] + "=" + replacement
		})
	}
	// openai keys first so the filter can drop kebab-case false positives
	// before any other pattern rewrites nearby text.
	if strings.Contains(redacted, "sk-") {
		redacted = openaiKeyPattern.ReplaceAllStringFunc(redacted, func(match string) string {
			if !openAIShapeIsSecret(match) {
				return match
			}
			return replacement
		})
	}
	for i, pattern := range textSecretPatterns {
		if hasAnyPrefix(redacted, textSecretPrefixes[i]) {
			redacted = redactAdjacent(redacted, pattern, anchoredSecretPatterns[i], replacement)
		}
	}
	// Credentials broken up by invisible characters last, over what the
	// contiguous matchers left: a whole credential is already claimed by its own
	// strict match, so this pass cannot reach out of one and into the next. It is
	// a no-op for text with no such character, which is nearly all text.
	redacted = redactControlSplitSecrets(redacted, replacement)
	return redacted
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func containsCaseInsensitive(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if hasFoldedASCIIPrefix(s[i:], substr) {
			return true
		}
	}
	return false
}

// hasFoldedASCIIPrefix reports whether s starts with substr under Unicode
// simple case folding (assuming substr is lowercase ASCII). This ensures that
// non-ASCII runes that fold to ASCII in (?i) regular expressions (U+017F 'ſ' ->
// 's' and U+212A 'K' -> 'k') pass the fast-path gate with zero allocations.
func hasFoldedASCIIPrefix(s, substr string) bool {
	si := 0
	for j := 0; j < len(substr); j++ {
		if si >= len(s) {
			return false
		}
		c2 := toLowerASCII(substr[j])
		c1 := s[si]
		if c1 < utf8.RuneSelf {
			if c1 != c2 && toLowerASCII(c1) != c2 {
				return false
			}
			si++
			continue
		}
		r, width := utf8.DecodeRuneInString(s[si:])
		if foldRuneToLowerASCII(r) != c2 {
			return false
		}
		si += width
	}
	return true
}

func foldRuneToLowerASCII(r rune) byte {
	switch r {
	case '\u017f': // LATIN SMALL LETTER LONG S
		return 's'
	case '\u212a': // KELVIN SIGN
		return 'k'
	default:
		return 0
	}
}

func toLowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// knownOpenAIKeyPrefix is the redaction-side twin of secrets.knownOpenAIKeyPrefix:
// known OpenAI-issued forms redact even with an alphabet-only body.
func knownOpenAIKeyPrefix(match string) bool {
	return strings.HasPrefix(match, "sk-proj-") ||
		strings.HasPrefix(match, "sk-svcacct-") ||
		strings.HasPrefix(match, "sk-admin-")
}

// secretMatchHasDigit is the redaction-side twin of secrets.containsDigit.
func secretMatchHasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func RedactValue(value any, options Options) any {
	return redactReflect(reflect.ValueOf(value), redactionContext{
		options:     options,
		replacement: replacement(options),
		maxDepth:    maxDepth(options),
		seen:        map[uintptr]struct{}{},
	}, 0)
}

func RedactError(err error, options Options) RedactedError {
	if err == nil {
		return RedactedError{Name: "Error", Message: ""}
	}
	redacted := RedactedError{
		Name:    errorName(err),
		Message: RedactString(err.Error(), options),
	}
	fields := exportedFields(reflect.ValueOf(err), options)
	if len(fields) > 0 {
		redacted.Fields = fields
	}
	var stackTracer interface{ StackTrace() fmt.Stringer }
	if errors.As(err, &stackTracer) {
		redacted.Stack = RedactString(stackTracer.StackTrace().String(), options)
	}
	return redacted
}

func ErrorMessage(err error, options Options) string {
	return RedactError(err, options).Message
}

type redactionContext struct {
	options     Options
	replacement string
	maxDepth    int
	seen        map[uintptr]struct{}
}

func redactReflect(value reflect.Value, context redactionContext, depth int) any {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if depth >= context.maxDepth {
		return "[MaxDepth]"
	}

	switch value.Kind() {
	case reflect.String:
		return RedactString(value.String(), context.options)
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		return value.Float()
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		ptr := value.Pointer()
		if _, ok := context.seen[ptr]; ok {
			return CircularReference
		}
		context.seen[ptr] = struct{}{}
		// Track only the current DFS path: drop the pointer after recursing so a
		// shared (non-cyclic) reference reached again via a SIBLING branch is not
		// mistaken for a cycle. Only an ancestor still on the path triggers it.
		out := redactReflect(value.Elem(), context, depth+1)
		delete(context.seen, ptr)
		return out
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		ptr := value.Pointer()
		if _, ok := context.seen[ptr]; ok {
			return CircularReference
		}
		context.seen[ptr] = struct{}{}
		out := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := fmt.Sprint(redactReflect(iter.Key(), context, depth+1))
			if IsSensitiveKey(key, context.options) {
				out[key] = context.replacement
				continue
			}
			out[key] = redactReflect(iter.Value(), context, depth+1)
		}
		delete(context.seen, ptr)
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			out[index] = redactReflect(value.Index(index), context, depth+1)
		}
		return out
	case reflect.Struct:
		if value.CanInterface() {
			if err, ok := value.Interface().(error); ok {
				return RedactError(err, context.options)
			}
		}
		out := make(map[string]any, value.NumField())
		valueType := value.Type()
		for index := 0; index < value.NumField(); index++ {
			field := valueType.Field(index)
			if field.PkgPath != "" {
				continue
			}
			name := field.Name
			if tag := field.Tag.Get("json"); tag != "" {
				name = strings.Split(tag, ",")[0]
				if name == "-" {
					continue
				}
			}
			if IsSensitiveKey(name, context.options) {
				out[name] = context.replacement
				continue
			}
			out[name] = redactReflect(value.Field(index), context, depth+1)
		}
		return out
	default:
		if value.CanInterface() {
			return value.Interface()
		}
		return fmt.Sprint(value)
	}
}

func exportedFields(value reflect.Value, options Options) map[string]any {
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return nil
	}
	fields := map[string]any{}
	context := redactionContext{
		options:     options,
		replacement: replacement(options),
		maxDepth:    maxDepth(options),
		seen:        map[uintptr]struct{}{},
	}
	valueType := value.Type()
	for index := 0; index < value.NumField(); index++ {
		field := valueType.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name := field.Name
		if IsSensitiveKey(name, options) {
			fields[name] = context.replacement
		} else {
			fields[name] = redactReflect(value.Field(index), context, 1)
		}
	}
	return fields
}

func errorName(err error) string {
	name := reflect.TypeOf(err).String()
	if name == "" {
		return "Error"
	}
	return name
}

func replacement(options Options) string {
	if options.Replacement != "" {
		return options.Replacement
	}
	return RedactedSecret
}

func maxDepth(options Options) int {
	if options.MaxDepth > 0 {
		return options.MaxDepth
	}
	return maxDepthDefault
}

func normalizeKey(key string) string {
	key = strings.TrimSpace(key)
	var builder strings.Builder
	var lastUnderscore bool
	var prev rune
	var hasPrev bool
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if hasPrev && !lastUnderscore && unicode.IsLower(prev) && unicode.IsUpper(r) {
				builder.WriteByte('_')
			}
			builder.WriteRune(unicode.ToLower(r))
			lastUnderscore = false
			prev = r
			hasPrev = true
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
		hasPrev = false
	}
	return strings.Trim(builder.String(), "_")
}

var urlWithCredsPattern = regexp.MustCompile(`\b(?:https?|wss?|ftp)://[^\s]+`)

func redactURLPasswords(value string, replacement string) string {
	return urlWithCredsPattern.ReplaceAllStringFunc(value, func(candidate string) string {
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.User == nil {
			return candidate
		}
		if _, hasPassword := parsed.User.Password(); !hasPassword {
			return candidate
		}
		username := parsed.User.Username()
		userinfo := url.UserPassword(username, replacement)
		parsed.User = userinfo
		out := parsed.String()
		encodedUserinfo := userinfo.String()
		literalUserinfo := url.User(username).String() + ":" + replacement
		if encodedUserinfo != literalUserinfo {
			out = strings.Replace(out, encodedUserinfo, literalUserinfo, 1)
		}
		return out
	})
}
