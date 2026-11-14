package providermodeldiscovery

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/charmbracelet/x/ansi"
)

// errWithheldDiscoveryDetails replaces an error whose terminal escapes would
// display a damaged copy of a credential that redaction can no longer match.
var errWithheldDiscoveryDetails = errors.New("model discovery failed; error details withheld because they may contain a credential")

// redactionSentinel marks each redacted span so the displayed and raw readings
// can be compared. Its NUL bytes cannot survive either reading's control strip.
const redactionSentinel = "\x00redacted\x00"

// RedactError normalizes terminal error text before matching credentials so
// invisible characters cannot split a secret across literal redaction matches.
// Discovery callers and the TUI share this boundary, including injected errors.
func RedactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := strings.Map(func(r rune) rune {
		if invisibleRune(r) {
			return -1
		}
		return r
	}, err.Error())
	displayed := displayedErrorText(message)
	// ANSI stripping consumes a sequence's final byte, and any letter is a valid
	// final byte, so the display can hold a credential missing one character.
	// A second reading keeps those bytes; if it matches a secret the display
	// does not, the display shows a damaged copy and the whole text is withheld.
	options := redaction.Options{ExtraSecretValues: secrets, Replacement: redactionSentinel}
	shown := strings.Count(redaction.RedactString(displayed, options), redactionSentinel)
	for _, keepParameters := range []bool{true, false} {
		if strings.Count(redaction.RedactString(rawErrorText(message, keepParameters), options), redactionSentinel) > shown {
			return errWithheldDiscoveryDetails
		}
	}
	return fmt.Errorf("%s", redaction.RedactString(displayed, redaction.Options{ExtraSecretValues: secrets}))
}

// invisibleRune reports runes that render as nothing, so they would split a
// secret without leaving a visible gap. Tab and line breaks stay because they
// render as a break.
func invisibleRune(r rune) bool {
	return unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r)
}

// displayedErrorText strips terminal sequences the way a terminal renders them.
func displayedErrorText(message string) string {
	var terminal strings.Builder
	for i := 0; i < len(message); i++ {
		// Preserve CSI and terminal string introducers for ANSI stripping. Treat
		// other ESC bytes as lone controls, rather than consuming the next letter
		// of a credential as a two-byte terminal command.
		if message[i] == '\x1b' && (i+1 == len(message) || !strings.ContainsRune("[]PX^_\\", rune(message[i+1]))) {
			continue
		}
		terminal.WriteByte(message[i])
	}
	return stripControls(ansi.Strip(terminal.String()))
}

// rawErrorText drops only escape introducers (ESC [, ESC ] and lone ESC) and
// other controls, keeping every byte a terminal sequence would consume. A CSI
// parameter such as the 9 in ESC [ 9 n can belong to the sequence or to the
// secret, so callers read the text both with and without CSI parameter bytes.
func rawErrorText(message string, keepParameters bool) string {
	var raw strings.Builder
	for i := 0; i < len(message); i++ {
		if message[i] != '\x1b' {
			raw.WriteByte(message[i])
			continue
		}
		if i+1 < len(message) && message[i+1] == ']' {
			i++
		} else if i+1 < len(message) && message[i+1] == '[' {
			i++
			// Parameter (0x30-0x3F) and intermediate (0x20-0x2F) bytes precede
			// the final byte that ANSI stripping also consumes.
			for !keepParameters && i+1 < len(message) && message[i+1] >= 0x20 && message[i+1] <= 0x3f {
				i++
			}
		}
	}
	return stripControls(raw.String())
}

func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || invisibleRune(r) {
			return -1
		}
		return r
	}, s)
}
