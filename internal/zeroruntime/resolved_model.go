package zeroruntime

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxResolvedModelBytes caps the stored provider-reported model id.
const MaxResolvedModelBytes = 256

// SanitizeResolvedModel returns raw as safe, bounded, printable text. It stops
// at the byte cap instead of copying the whole value first, so an oversized id
// never costs more than the cap.
func SanitizeResolvedModel(raw string) string {
	var b strings.Builder
	b.Grow(MaxResolvedModelBytes)
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRuneInString(raw[i:])
		i += size
		if r == utf8.RuneError && size == 1 {
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		if b.Len() == 0 && r == ' ' {
			continue
		}
		if b.Len()+size > MaxResolvedModelBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ")
}

// ReportedModel is a model id reported by a provider or read back from a stored
// usage event. It decodes a JSON string and treats any other JSON type as
// absent, so an optional attribution key can never turn a frame or an event
// that was valid before the key existed into a decode error.
type ReportedModel string

// UnmarshalJSON implements json.Unmarshaler.
func (m *ReportedModel) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		*m = ""
		return nil
	}
	*m = ReportedModel(s)
	return nil
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
