package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// THE KEY IS WHAT REMOVES THE AMBIGUITY, SO IT HAS TO SURVIVE TO THE DECISION.
//
// credentialCandidates drops values under shortestMCPSecret so ordinary
// configuration such as mode=sse or v=1 stays readable in an unrelated message.
// Header, environment and query values were flattened into bare strings before
// that heuristic ran, so a six-byte credential under a key that names it as one
// was discarded and never became an exact candidate. A child echoing the value
// on its own then reached the panel and the transcript, where generic shape
// matching has nothing left to recognise.
func TestShortValuesUnderSensitiveKeysAreRedacted(t *testing.T) {
	const short = "s3cr3t" // six bytes, under the floor

	for _, testCase := range []struct {
		name string
		raw  config.MCPServerConfig
	}{
		{"environment key", config.MCPServerConfig{Command: "server", Env: map[string]string{"API_KEY": short}}},
		{"header key", config.MCPServerConfig{URL: "https://host.invalid/mcp", Headers: map[string]string{"X-Api-Key": short}}},
		{"query key", config.MCPServerConfig{URL: "https://host.invalid/mcp?api_key=" + short}},
		{"userinfo password", config.MCPServerConfig{URL: "https://user:" + short + "@host.invalid/mcp"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// A value-only echo: the sensitive key/value syntax is not present in
			// the message, so only an exact candidate can catch it.
			got := redactMCPFailureReason(errors.New("upstream echoed "+short), testCase.raw, nil)
			if strings.Contains(got, short) {
				t.Errorf("a short credential under a key that names it reached the panel: %q", got)
			}
		})
	}
}

// And the floor still protects ordinary configuration, or the fix is just
// blanket over-redaction with a different justification.
func TestShortValuesUnderOrdinaryKeysStayReadable(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		raw     config.MCPServerConfig
		message string
		keep    string
	}{
		{
			"ordinary query parameters",
			config.MCPServerConfig{URL: "https://host.invalid/mcp?mode=sse&v=1"},
			"transport mode=sse rejected", "mode=sse",
		},
		{
			"ordinary environment value",
			config.MCPServerConfig{Command: "server", Env: map[string]string{"LOG_LEVEL": "warn"}},
			"child started with log level warn", "warn",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := redactMCPFailureReason(errors.New(testCase.message), testCase.raw, nil); !strings.Contains(got, testCase.keep) {
				t.Errorf("an ordinary short value was redacted out of the message: %q", got)
			}
		})
	}
}

// A PUBLIC MODE SELECTOR IS NOT CREDENTIAL MATERIAL.
//
// raw.Auth holds the authentication MODE, and normalization accepts only the
// value "oauth", which the panel displays as ordinary metadata. Classifying it
// by the security-sounding name of its field removed the one token that says
// which subsystem failed, from every error the OAuth stack produces. It was
// invisible while ambiguous values ran through the length floor, which
// discarded a five-character string on its own.
func TestTheOAuthModeSelectorStaysReadable(t *testing.T) {
	raw := config.MCPServerConfig{
		URL:   "https://host.invalid/mcp",
		Auth:  "oauth",
		OAuth: &config.MCPOAuthConfig{ClientSecret: "s3cr3t"},
	}
	for _, message := range []string{
		"oauth: fetch authorization server metadata",
		"oauth discovery failed",
	} {
		got := redactMCPFailureReason(errors.New(message), raw, nil)
		if !strings.Contains(got, "oauth") {
			t.Errorf("the subsystem name was redacted out of %q: %q", message, got)
		}
	}
	// The half that must not weaken: a real secret in the same config is still
	// removed, at a length the floor would have discarded.
	if got := redactMCPFailureReason(errors.New("token endpoint replied error_description=s3cr3t"), raw, nil); strings.Contains(got, "s3cr3t") {
		t.Errorf("the client secret reached the panel: %q", got)
	}
}

// ONE PARSER FOR CLASSIFICATION AND FOR RENDERING.
//
// The collector split a flag packed with its value; the display did not. It
// recognised the element as sensitive, printed it verbatim, and then redacted
// the NEXT argument, so the Target row carried the credential and blanked an
// unrelated flag. That row sits directly under the reason, on both /mcp
// surfaces, and is persisted.
func TestPackedSensitiveArgumentsAreRedactedInTheTargetRow(t *testing.T) {
	const secret = "sk-live-9f3c2b7ae1d8c4"
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"packed with a space", []string{"--api-key " + secret, "--verbose"}},
		{"separate elements", []string{"--api-key", secret, "--verbose"}},
		{"joined with equals", []string{"--api-key=" + secret, "--verbose"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rendered := strings.Join(redactedCommandArgs(testCase.args), " ")
			if strings.Contains(rendered, secret) {
				t.Errorf("the Target row prints the credential: %q", rendered)
			}
			if !strings.Contains(rendered, "--api-key") {
				t.Errorf("the flag name was redacted too, losing which credential was rejected: %q", rendered)
			}
			if !strings.Contains(rendered, "--verbose") {
				t.Errorf("an unrelated argument was redacted instead of the value: %q", rendered)
			}
			if !containsValue(sensitiveMCPArgValues(testCase.args), secret) {
				t.Errorf("the collector did not classify the value as sensitive: %v", sensitiveMCPArgValues(testCase.args))
			}
		})
	}
}
