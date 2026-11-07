package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// AN ENDPOINT CAN BE A CREDENTIAL. HTTP and SSE send the configured URL
// verbatim, and it accepts userinfo and arbitrary query keys. The candidate
// collector gathered headers, env, args and OAuth material but never looked at
// the URL, and the generic query redaction downstream only recognises
// conventional key names, so a parameter the operator named "workspace" carried
// its token straight into this panel and the transcript.
func TestEndpointCredentialsAreRedactedFromAFailureReason(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"

	for _, testCase := range []struct{ name, endpoint string }{
		{name: "arbitrary query name", endpoint: "https://host.invalid/mcp?workspace=" + token},
		{name: "conventional query name", endpoint: "https://host.invalid/mcp?api_key=" + token},
		{name: "userinfo password", endpoint: "https://svc:" + token + "@host.invalid/mcp"},
		{name: "userinfo username", endpoint: "https://" + token + "@host.invalid/mcp"},
		{name: "second of two parameters", endpoint: "https://host.invalid/mcp?mode=sse&tenant=" + token},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := config.MCPServerConfig{URL: testCase.endpoint}
			// The server echoes its own endpoint back in the failure body, which is
			// what httpStatusError retains.
			failure := errors.New("502 Bad Gateway from " + testCase.endpoint)

			got := redactMCPFailureReason(failure, raw, nil)
			if strings.Contains(got, token) {
				t.Errorf("the endpoint credential survived into the rendered failure, which is also persisted:\n%s", got)
			}
		})
	}
}

// The host and path must survive, or the operator cannot tell which endpoint
// failed. Redacting the whole URL would be safe and useless.
func TestEndpointHostAndPathSurviveRedaction(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"
	endpoint := "https://docs.host.invalid/mcp/v1?workspace=" + token
	raw := config.MCPServerConfig{URL: endpoint}

	got := redactMCPFailureReason(errors.New("502 Bad Gateway from "+endpoint), raw, nil)
	if strings.Contains(got, token) {
		t.Fatalf("credential survived:\n%s", got)
	}
	for _, want := range []string{"docs.host.invalid", "/mcp/v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was redacted too, so the message no longer identifies the endpoint:\n%s", want, got)
		}
	}
}

// Short, ordinary parameters are not credentials, and blanking them would punch
// holes through unrelated text. The shortestMCPSecret floor is what stops it.
func TestOrdinaryShortQueryValuesAreNotTreatedAsSecrets(t *testing.T) {
	raw := config.MCPServerConfig{URL: "https://host.invalid/mcp?v=1&mode=sse"}
	got := redactMCPFailureReason(errors.New("transport mode sse rejected, protocol v1"), raw, nil)
	for _, want := range []string{"sse", "v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was redacted as if it were a credential:\n%s", want, got)
		}
	}
}

// BOTH REPRESENTATIONS OF THE SAME CREDENTIAL.
//
// url.Parse and url.ParseQuery hand back DECODED values, but the network client
// starts from the configured URL string, so an MCP can echo the escaped spelling
// back in its failure body. Exact-value redaction knew only the decoded form, and
// "%2D" decodes to a hyphen, so the credential displayed and persisted in /mcp was
// fully recoverable. The parameter name is the operator's to choose, so the
// generic patterns do not catch it either.
func TestPercentEncodedEndpointCredentialsAreRedacted(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"
	encoded := strings.ReplaceAll(token, "-", "%2D")

	for _, testCase := range []struct{ name, endpoint string }{
		{name: "arbitrary query key, percent-encoded", endpoint: "https://host.invalid/mcp?workspace=" + encoded},
		{name: "percent-encoded userinfo username", endpoint: "https://" + encoded + "@host.invalid/mcp"},
		{name: "percent-encoded userinfo password", endpoint: "https://svc:" + encoded + "@host.invalid/mcp"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := config.MCPServerConfig{URL: testCase.endpoint}
			// The server echoes the configured URL back verbatim, escapes and all.
			got := redactMCPFailureReason(errors.New("502 Bad Gateway from "+testCase.endpoint), raw, nil)

			if strings.Contains(got, encoded) {
				t.Errorf("the escaped credential survived; %%2D decodes to a hyphen, so it is fully recoverable:\n%s", got)
			}
			if strings.Contains(got, token) {
				t.Errorf("the decoded credential survived:\n%s", got)
			}
		})
	}
}

// A QUERY VALUE KEEPS ITS PROVENANCE IN BOTH SPELLINGS.
//
// The raw and decoded spellings came apart two ways. The raw pass judged the
// undecoded key, so "api%5Fkey" never read as api_key, and the escaped spelling
// of a six-byte credential fell to the readability floor. The decoded pass ran
// url.ParseQuery over the whole query, which rejects everything at the first
// pair it cannot parse, so a semicolon or a bad escape in another parameter
// dropped the decoded spelling of a credential that decodes cleanly. Every URL
// source goes through the same collector, so each one is driven here. Reported
// by @jatmn.
func TestQueryCredentialsKeepTheirProvenanceInBothSpellings(t *testing.T) {
	const workspace = "opaque-workspace-9f3c2b7"
	encodedWorkspace := strings.ReplaceAll(workspace, "-", "%2D")

	sources := []struct {
		name string
		raw  func(endpoint string) config.MCPServerConfig
	}{
		{"server URL", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "http", URL: endpoint}
		}},
		{"positional stdio argument", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "stdio", Command: "mcp-remote", Args: []string{endpoint}}
		}},
		{"separate stdio argument", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "stdio", Command: "mcp-remote", Args: []string{"--url", endpoint}}
		}},
		{"packed stdio argument", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "stdio", Command: "mcp-remote", Args: []string{"--url=" + endpoint}}
		}},
		{"OAuth token endpoint", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "http", URL: "https://host.invalid/mcp", Auth: "oauth", OAuth: &config.MCPOAuthConfig{TokenEndpoint: endpoint}}
		}},
		{"OAuth authorization endpoint", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "http", URL: "https://host.invalid/mcp", Auth: "oauth", OAuth: &config.MCPOAuthConfig{AuthorizationEndpoint: endpoint}}
		}},
		{"OAuth registration endpoint", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "http", URL: "https://host.invalid/mcp", Auth: "oauth", OAuth: &config.MCPOAuthConfig{RegistrationEndpoint: endpoint}}
		}},
		{"OAuth issuer", func(endpoint string) config.MCPServerConfig {
			return config.MCPServerConfig{Type: "http", URL: "https://host.invalid/mcp", Auth: "oauth", OAuth: &config.MCPOAuthConfig{IssuerURL: endpoint}}
		}},
	}
	for _, combo := range []struct {
		name    string
		query   string
		echo    string
		absent  []string
		present []string
	}{
		{name: "escaped sensitive key, escaped short value echoed alone", query: "api%5Fkey=%73crt", echo: "%73crt", absent: []string{"%73crt"}},
		{name: "escaped sensitive key, decoded short value echoed alone", query: "api%5Fkey=%73crt", echo: "scrt", absent: []string{"scrt"}},
		{name: "decoded value beside a pair with a semicolon", query: "workspace=" + encodedWorkspace + "&filter=a;b", echo: workspace, absent: []string{workspace}},
		{name: "decoded value beside a pair with a bad escape", query: "workspace=" + encodedWorkspace + "&bad=%zz", echo: workspace, absent: []string{workspace}},
		{name: "escaped value beside a pair with a semicolon", query: "workspace=" + encodedWorkspace + "&filter=a;b", echo: encodedWorkspace, absent: []string{encodedWorkspace}},
		{name: "control: ordinary short metadata stays readable", query: "v=1&mode=sse&filter=a;b", echo: "mode sse is not served for v 1", present: []string{"mode sse is not served for v 1"}},
		{name: "control: an escaped ordinary key keeps the floor", query: "mo%64e=sse", echo: "mode sse is not served", present: []string{"mode sse is not served"}},
	} {
		for _, source := range sources {
			t.Run(combo.name+"/"+source.name, func(t *testing.T) {
				endpoint := "https://host.invalid/mcp?" + combo.query
				got := redactMCPFailureReason(errors.New("request rejected: "+combo.echo), source.raw(endpoint), nil)
				if !strings.Contains(got, "request rejected") {
					t.Fatalf("the reason lost its own text, so the check below proves nothing:\n%s", got)
				}
				for _, absent := range combo.absent {
					if strings.Contains(got, absent) {
						t.Errorf("%q survived the redaction:\n%s", absent, got)
					}
				}
				for _, present := range combo.present {
					if !strings.Contains(got, present) {
						t.Errorf("ordinary text %q was redacted:\n%s", present, got)
					}
				}
			})
		}
	}
}
