package providermodeldiscovery

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/charmbracelet/x/ansi"
)

// RedactError normalizes terminal error text before matching credentials so
// invisible characters cannot split a secret across literal redaction matches.
// Discovery callers and the TUI share this boundary, including injected errors.
func RedactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, err.Error())
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
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, ansi.Strip(terminal.String()))
	return fmt.Errorf("%s", redaction.RedactString(message, redaction.Options{ExtraSecretValues: secrets}))
}
