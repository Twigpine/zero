package tui

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/Twigpine/zero/internal/config"
)

// readerVisible is what a terminal actually shows: default-ignorable code points
// render as nothing, so a secret split by one is contiguous to the eye. The
// assertion has to be made against this rather than against the raw string,
// because the raw string is exactly where the secret looks split.
func readerVisible(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
			unicode.Is(unicode.Variation_Selector, r) {
			return -1
		}
		return r
	}, value)
}

// A configured credential split by a default-ignorable MARK must not reach
// either MCP surface. Cf characters were already covered; these are Mn, which
// the category-preserving second pass deliberately keeps.
func TestMCPFailureRedactsSecretsSplitByDefaultIgnorableMarks(t *testing.T) {
	const secret = "wk-live-4f9c2b7ae1d8"
	raw := config.MCPServerConfig{Env: map[string]string{"TOKEN": secret}}

	for _, splitter := range []struct {
		name string
		r    rune
	}{
		{"combining grapheme joiner", 0x034F},
		{"variation selector 16", 0xFE0F},
		{"zero width space", 0x200B},
	} {
		split := "wk-live-" + string(splitter.r) + "4f9c2b7ae1d8"
		got := redactMCPFailureReason(errors.New("startup failed: "+split), raw, nil)
		if strings.Contains(readerVisible(got), secret) {
			t.Errorf("%s: the reader-visible failure message still contains the credential: %q", splitter.name, got)
		}
	}
}

// A stored OAuth token is a credential by provenance, not by length. The
// readability floor belongs to ambiguous configuration strings only.
func TestMCPFailureRedactsShortStoredTokens(t *testing.T) {
	for _, token := range []string{"a1b2c3", "x", "short"} {
		got := redactMCPFailureReason(
			errors.New(`server rejected the handshake: {"echoed":"`+token+`"}`),
			config.MCPServerConfig{},
			[]string{token},
		)
		if strings.Contains(got, token) {
			t.Errorf("stored token %q survived redaction: %q", token, got)
		}
	}
}

// The ignorable drop must not widen into the Mn category: ordinary combining
// marks are content, and eating them would corrupt the diagnostic this pass
// exists to display.
func TestTerminalRejoinerStrippingKeepsOrdinaryCombiningMarks(t *testing.T) {
	for _, value := range []string{
		"café could not start",        // e + combining acute
		"naïve endpoint",              // combining diaeresis
		"straße refused the handshake", // sharp s
		"مرحبا",                        // arabic
	} {
		if got := stripTerminalRejoiners(value); got != value {
			t.Errorf("legitimate text was altered: %q -> %q", value, got)
		}
	}
}
