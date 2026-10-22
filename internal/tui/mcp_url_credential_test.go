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
