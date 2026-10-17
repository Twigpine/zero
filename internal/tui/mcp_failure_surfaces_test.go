package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// failedServerSurfaces renders a recorded startup failure through BOTH places a
// user reads it: the /mcp panel text, which is also what the transcript keeps,
// and the detail pane of the /mcp manager. Each has its own Error and Target
// row, and a regression that checks one of them leaves the other free to print
// the credential.
//
// Everything goes through the model, the way startup hands it over, rather than
// through the redaction helper the rows happen to share today.
func failedServerSurfaces(t *testing.T, cfg config.MCPConfig, key string, skipped []mcp.SkippedServer) (panel string, detail string) {
	t.Helper()
	m := newModel(context.Background(), Options{MCPConfig: cfg, MCPSkipped: skipped})
	panel = plainRender(t, m.mcpText())
	if !strings.Contains(panel, "failed") {
		t.Fatalf("the panel does not report a failed server, so nothing below is exercised:\n%s", panel)
	}
	m = m.openMCPManager()
	selected := false
	for index, item := range m.mcpManagerItems() {
		if item.Kind == mcpManagerItemServer && item.ConfigKey == key {
			m.mcpManager.selected = index
			selected = true
		}
	}
	if !selected {
		t.Fatalf("the manager lists no server under the key %q", key)
	}
	detail = plainRender(t, strings.Join(m.mcpManagerSelectionDetail(4000), "\n"))
	if !strings.Contains(detail, "failed") {
		t.Fatalf("the manager detail does not report a failed server:\n%s", detail)
	}
	return panel, detail
}

// A PADDED CREDENTIAL IN A PACKED OR ATTACHED ARGUMENT KEEPS ITS BOUNDARY.
//
// Both consumers cut an element at its first "=" before looking for a packed
// "--flag value" or an attached "-HName: value". In these elements the first
// "=" is base64 padding, so the collector kept "=" as the secret and the body
// stayed outside the redaction set, while the Target row printed the body
// followed by "=[REDACTED]".
//
// The assertion is on the BODY, and the echo is tried both whole and without
// its padding, so replacing only the "==" cannot pass either way.
func TestPaddedCredentialKeepsItsBoundaryInPackedAndAttachedArguments(t *testing.T) {
	const credential = "YWJjZGVmZ2hpag=="
	const body = "YWJjZGVmZ2hpag"
	for _, tc := range []struct {
		name  string
		args  []string
		label string
	}{
		{"packed flag and value", []string{"--api-key " + credential, "--verbose"}, "--api-key"},
		{"attached short header", []string{"-HX-Workspace-Id: " + credential, "--verbose"}, "X-Workspace-Id"},
		{"packed long header", []string{"--header X-Workspace-Id: " + credential, "--verbose"}, "X-Workspace-Id"},
		{"header with equals", []string{"--header=X-Workspace-Id: " + credential, "--verbose"}, "X-Workspace-Id"},
		{"control: separate arguments", []string{"--api-key", credential, "--verbose"}, "--api-key"},
		{"control: conventional equals", []string{"--api-key=" + credential, "--verbose"}, "--api-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !containsValue(sensitiveMCPArgValues(tc.args), credential) {
				t.Errorf("the collector did not keep the whole credential: %q", sensitiveMCPArgValues(tc.args))
			}
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "bridge", Args: tc.args},
			}}
			for _, echoed := range []string{credential, body} {
				failure := errors.New("bridge rejected the value " + echoed + " it was started with")
				panel, detail := failedServerSurfaces(t, cfg, "docs", []mcp.SkippedServer{{Name: "docs", Err: failure}})
				for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
					if strings.Contains(text, body) {
						t.Errorf("%s shows the credential body when the child echoes %q:\n%s", surface, echoed, text)
					}
					if !strings.Contains(text, "--verbose") {
						t.Errorf("%s lost the unrelated --verbose argument:\n%s", surface, text)
					}
					if !strings.Contains(text, tc.label) {
						t.Errorf("%s lost the label %q that says which credential was rejected:\n%s", surface, tc.label, text)
					}
					if !strings.Contains(text, "bridge rejected the value") {
						t.Errorf("%s lost the diagnostic around the credential:\n%s", surface, text)
					}
				}
			}
		})
	}
}

// A positional endpoint is a URL, not a key=value element and not a bare flag.
// Cut at its first "=", "https://u:pw@host/mcp?token" reads as a sensitive KEY:
// one reader masked the query value and printed the userinfo beside it, the
// other printed the whole element and masked the NEXT one instead. The short
// query value is here because a long one is caught by accident, as an opaque
// path segment, whichever reader gets it.
func TestPositionalEndpointIsNotReadAsAKeyValueElement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		leaked []string
	}{
		{"long query value", []string{"mcp-remote", "https://svc:hunter2pass@host.invalid/mcp?token=opaque-query-credential-1", "stdio"},
			[]string{"hunter2pass", "opaque-query-credential-1"}},
		{"short query value", []string{"mcp-remote", "https://svc:hunter2pass@host.invalid/mcp?token=abc12", "stdio"},
			[]string{"hunter2pass", "abc12"}},
		{"scheme without slashes", []string{"mcp-remote", "https:host.invalid/mcp?token=abc12", "stdio"},
			[]string{"abc12"}},
		{"after a bare endpoint flag", []string{"--endpoint", "https://svc:hunter2pass@host.invalid/mcp?token=abc12", "stdio"},
			[]string{"hunter2pass", "abc12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			display := strings.Join(redactedCommandArgs(tc.args), " ")
			for _, leaked := range tc.leaked {
				if strings.Contains(display, leaked) {
					t.Errorf("the target row prints %q: %s", leaked, display)
				}
			}
			if !strings.Contains(display, "host.invalid/mcp") {
				t.Errorf("the endpoint itself is no longer readable: %s", display)
			}
			if !strings.HasSuffix(display, " stdio") {
				t.Errorf("the element after the endpoint was masked in its place: %s", display)
			}
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "bridge", Args: tc.args},
			}}
			failure := errors.New("cannot reach " + tc.args[1] + " over stdio")
			panel, detail := failedServerSurfaces(t, cfg, "docs", []mcp.SkippedServer{{Name: "docs", Err: failure}})
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				for _, leaked := range tc.leaked {
					if strings.Contains(text, leaked) {
						t.Errorf("%s prints %q:\n%s", surface, leaked, text)
					}
				}
				if !strings.Contains(text, "over stdio") {
					t.Errorf("%s lost the word after the endpoint:\n%s", surface, text)
				}
			}
		})
	}
}

// A KNOWN AUTHENTICATION VALUE KEEPS ITS PROVENANCE WHEN IT IS TAKEN APART.
//
// "Bearer s3cr3t" was kept whole and its six-byte token dropped by the
// readability floor, which exists for values that might not be credentials at
// all. A server that echoes the bare token then reaches both surfaces, and no
// pattern can recognise an opaque six-byte string.
func TestShortBearerTokenKeepsItsKnownProvenance(t *testing.T) {
	const token = "s3cr3t"
	for _, tc := range []struct {
		name   string
		server config.MCPServerConfig
	}{
		{"configured Authorization header", config.MCPServerConfig{
			Type: "http", URL: "https://host.invalid/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + token},
		}},
		{"stdio header argument", config.MCPServerConfig{
			Type: "stdio", Command: "bridge", Args: []string{"--header", "Authorization: Bearer " + token},
		}},
		{"stdio attached header argument", config.MCPServerConfig{
			Type: "stdio", Command: "bridge", Args: []string{"-HAuthorization: Bearer " + token},
		}},
		{"stdio packed header argument", config.MCPServerConfig{
			Type: "stdio", Command: "bridge", Args: []string{"--header=Authorization: Token " + token},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": tc.server}}
			failure := errors.New("upstream echoed " + token + " while mode=sse was negotiated")
			panel, detail := failedServerSurfaces(t, cfg, "docs", []mcp.SkippedServer{{Name: "docs", Err: failure}})
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				if strings.Contains(text, token) {
					t.Errorf("%s shows the bare token:\n%s", surface, text)
				}
				if !strings.Contains(text, "mode=sse") {
					t.Errorf("%s lost the ordinary short value beside it:\n%s", surface, text)
				}
				if !strings.Contains(text, "upstream echoed") {
					t.Errorf("%s lost the diagnostic:\n%s", surface, text)
				}
			}
		})
	}
}

// The tails are offered only for the supported authentication shapes. A known
// value of several words is not taken apart, so its short words never enter the
// redaction set and blank themselves out of unrelated text.
func TestKnownCredentialTailsOnlyFollowAuthenticationShapes(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{"Bearer s3cr3t", []string{"s3cr3t"}},
		{"Authorization: Bearer s3cr3t", []string{"s3cr3t"}},
		{"X-Api-Key: s3cr3t", []string{"s3cr3t"}},
		{"  Token   abc  ", []string{"abc"}},
		{"s3cr3t", nil},
		{"Bearer", nil},
		{"Bearer ", nil},
		{"correct horse battery staple", nil},
		{"token: a b c", nil},
		{"sk/live secret", nil},
	} {
		got := knownCredentialTails(tc.value)
		if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
			t.Errorf("knownCredentialTails(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
	// The scan stays bounded however long the credential is, and the tail is
	// still found, because every supported shape keeps its separators in a
	// short prefix.
	long := strings.Repeat("x", 4<<20)
	if got := knownCredentialTails("Bearer " + long); len(got) != 1 || got[0] != long {
		t.Errorf("an oversized bearer lost its tail: %d candidates", len(got))
	}
}

// PADDING IS NOT PART OF WHAT MAKES A VALUE SECRET.
//
// A server that normalizes base64 before it echoes drops the "==". Exact-value
// redaction then knows the padded spelling, the message carries the body, and
// the whole recoverable credential is displayed for the sake of two characters.
// Every form a known value is taken apart into gets the same treatment, because
// "Bearer <base64>==" is the usual shape and its TAIL is what gets echoed.
func TestUnpaddedEchoOfAPaddedCredentialIsRedacted(t *testing.T) {
	const credential = "YWJjZGVmZ2hpag=="
	const body = "YWJjZGVmZ2hpag"
	for _, tc := range []struct {
		name   string
		server config.MCPServerConfig
	}{
		{"bearer tail", config.MCPServerConfig{
			Type: "http", URL: "https://host.invalid/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + credential},
		}},
		{"environment value", config.MCPServerConfig{
			Type: "stdio", Command: "bridge", Env: map[string]string{"API_TOKEN": credential},
		}},
		{"query value under an ordinary key", config.MCPServerConfig{
			Type: "http", URL: "https://host.invalid/mcp?workspace=" + credential,
		}},
		{"header argument tail", config.MCPServerConfig{
			Type: "stdio", Command: "bridge", Args: []string{"--header", "Authorization: Bearer " + credential},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": tc.server}}
			failure := errors.New("upstream rejected " + body + " while mode=sse was negotiated")
			panel, detail := failedServerSurfaces(t, cfg, "docs", []mcp.SkippedServer{{Name: "docs", Err: failure}})
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				if strings.Contains(text, body) {
					t.Errorf("%s shows the unpadded credential:\n%s", surface, text)
				}
				if !strings.Contains(text, "upstream rejected") || !strings.Contains(text, "mode=sse") {
					t.Errorf("%s lost the diagnostic:\n%s", surface, text)
				}
			}
		})
	}

	// The floor still belongs to the ambiguous values. "abcdef==" is long enough
	// to be collected, and what is left without its padding is an ordinary six
	// bytes that must not start disappearing from messages.
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://host.invalid/mcp?pad=abcdef=="},
	}}
	failure := errors.New("the abcdef route is not enabled")
	panel, detail := failedServerSurfaces(t, cfg, "docs", []mcp.SkippedServer{{Name: "docs", Err: failure}})
	for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
		if !strings.Contains(text, "the abcdef route is not enabled") {
			t.Errorf("%s lost an ordinary short word to a padded ambiguous value:\n%s", surface, text)
		}
	}

	if got := withUnpaddedSpellings([]string{"==", "abc=", "plain"}, 1); len(got) != 4 || got[2] != "abc" {
		t.Errorf("withUnpaddedSpellings = %q, want one extra spelling for the padded candidate only", got)
	}
}
