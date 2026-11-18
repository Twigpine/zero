package zeroruntime

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxResolvedModelBytes caps the stored provider-reported model id.
const MaxResolvedModelBytes = 256

// SanitizeResolvedModel returns raw as safe, bounded, printable text.
func SanitizeResolvedModel(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRuneInString(raw[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
		i += size
	}
	s := strings.TrimSpace(b.String())
	if len(s) > MaxResolvedModelBytes {
		end := 0
		for end < len(s) {
			_, size := utf8.DecodeRuneInString(s[end:])
			next := end + size
			if next > MaxResolvedModelBytes {
				break
			}
			end = next
		}
		s = s[:end]
	}
	return strings.TrimRight(s, " ")
}

// ResolvedModelAttribution returns the sanitized reported model, or "" when it
// is empty after sanitizing or equals the sanitized requested model.
func ResolvedModelAttribution(requested, reported string) string {
	r := SanitizeResolvedModel(reported)
	if r == "" || r == SanitizeResolvedModel(requested) {
		return ""
	}
	return r
}
