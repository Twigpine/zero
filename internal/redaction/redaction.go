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

	redacted = privateKeyPattern.ReplaceAllString(redacted, replacement)
	redacted = jsonStringPattern.ReplaceAllStringFunc(redacted, func(match string) string {
		parts := jsonStringPattern.FindStringSubmatch(match)
		if len(parts) < 3 || !IsSensitiveKey(parts[2], options) {
			return match
		}
		return parts[1] + `"` + replacement + `"`
	})
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
	redacted = headerPattern.ReplaceAllStringFunc(redacted, func(match string) string {
		groups := headerPattern.FindStringSubmatch(match)
		// groups[2] is the known scheme (kept for readability) or "" for an opaque /
		// custom-scheme credential — in which case the whole value is redacted (M12).
		if groups[2] != "" {
			return groups[1] + ": " + groups[2] + " " + replacement
		}
		return groups[1] + ": " + replacement
	})
	redacted = secretHeader.ReplaceAllString(redacted, "$1: "+replacement)
	redacted = redactURLPasswords(redacted, replacement)
	redacted = queryPattern.ReplaceAllStringFunc(redacted, func(match string) string {
		parts := queryPattern.FindStringSubmatch(match)
		if len(parts) < 4 || !IsSensitiveKey(parts[2], options) {
			return match
		}
		return parts[1] + parts[2] + "=" + replacement
	})
	// openai keys first so the filter can drop kebab-case false positives
	// before any other pattern rewrites nearby text.
	redacted = openaiKeyPattern.ReplaceAllStringFunc(redacted, func(match string) string {
		if !knownOpenAIKeyPrefix(match) && !secretMatchHasDigit(match) &&
			strings.Contains(strings.TrimPrefix(match, "sk-"), "-") {
			return match
		}
		return replacement
	})
	for _, pattern := range textSecretPatterns {
		redacted = pattern.ReplaceAllString(redacted, replacement)
	}
	return redacted
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
		parsed.User = url.UserPassword(parsed.User.Username(), replacement)
		return parsed.String()
	})
}
