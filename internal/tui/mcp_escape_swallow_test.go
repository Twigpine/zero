package tui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// Built from code points so the fixtures cannot be rewritten into raw bytes by
// an editor on the way to the repository.
var (
	escByte = string(rune(0x1b))
	belByte = string(rune(0x07))
)

// AN ESCAPE CAN EAT A BYTE OF THE CREDENTIAL.
//
// The sanitizer removes a sequence whole, including the bytes that end it: the
// character after a bare ESC, a CSI's final byte, an OSC payload. Put in front
// of one byte of an echoed credential, the sequence eats that byte, so the
// reader was shown every other byte of it while equality matched nothing, and
// the split-byte tests passed because they only checked that the whole value
// was gone. The reason is withheld in that case. A sequence that eats nothing
// of the credential still rejoins it for ordinary redaction, and an ordinary
// coloured error keeps its text.
func TestAnEscapeThatEatsPartOfACredentialWithholdsTheReason(t *testing.T) {
	const secret = "wk-live-4f9c2b7ae1d8"
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://docs.example/mcp", Headers: map[string]string{"Authorization": secret}},
	}}
	for _, tc := range []struct {
		name     string
		echo     string
		withheld bool
		present  []string
	}{
		{name: "bare ESC eats the next byte", echo: "wk-live-" + escByte + "4f9c2b7ae1d8", withheld: true},
		{name: "CSI final byte is a byte of the credential", echo: "wk-live-" + escByte + "[4f9c2b7ae1d8", withheld: true},
		{name: "OSC payload holds part of the credential", echo: "wk-live-" + escByte + "]4f9c2b7ae1" + belByte + "d8", withheld: true},
		{name: "control: a whole sequence between the halves is redacted", echo: "wk-live-" + escByte + "7" + "4f9c2b7ae1d8", present: []string{"[REDACTED]", "handshake rejected"}},
		{name: "control: an ordinary coloured error keeps its text", echo: escByte + "[31mconnection refused" + escByte + "[0m", present: []string{"handshake rejected", "connection refused"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := failedServerReason(t, cfg, "docs", errors.New("handshake rejected, sent "+tc.echo), nil)
			if withheld := reason == mcpEscapeSwallowedReason; withheld != tc.withheld {
				t.Fatalf("withheld = %v, want %v: %q", withheld, tc.withheld, reason)
			}
			// Any long run of the credential is a leak, not only the whole value.
			for _, part := range []string{secret, "4f9c2b7ae1d8", "f9c2b7ae1d8", "wk-live-f9c"} {
				if strings.Contains(reason, part) {
					t.Errorf("the reason shows %q of the credential: %q", part, reason)
				}
			}
			for _, want := range tc.present {
				if !strings.Contains(reason, want) {
					t.Errorf("the reason lost %q: %q", want, reason)
				}
			}
		})
	}
}

// The same through a real HTTP failure, in both views.
func TestAnEscapeThatEatsPartOfACredentialIsWithheldInBothViews(t *testing.T) {
	const credential = "Qw7ZmPr4aBcD9eFgH2jKlM6nOpR8sTuV"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("rejected " + credential[:10] + escByte + credential[10:]))
	}))
	defer server.Close()
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: server.URL + "/mcp", Headers: map[string]string{"X-Api-Key": credential}},
	}}
	skipped := registerAndCollectSkipped(t, cfg, credential[11:])
	panel, detail := failedServerSurfaces(t, cfg, "docs", skipped)
	for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
		if strings.Contains(text, credential[11:]) {
			t.Errorf("%s shows the rest of the credential:\n%s", surface, text)
		}
		if !strings.Contains(text, "overlapped a credential") {
			t.Errorf("%s does not say why the details were dropped:\n%s", surface, text)
		}
	}
}
