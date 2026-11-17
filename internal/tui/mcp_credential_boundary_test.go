package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	mcppkg "github.com/Twigpine/zero/internal/mcp"
)

// aperiodicSecret builds an opaque credential with no period, so a suffix of it
// can never coincidentally equal one of its own prefixes. A repeating fixture
// makes the boundary tests below pass for the wrong reason.
func aperiodicSecret(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	state := uint64(0x9E3779B97F4A7C15)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = alphabet[state%uint64(len(alphabet))]
	}
	return string(out)
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// THE MATCHER HAS TO COVER THE WHOLE CREDENTIAL, NOT A FIXED WINDOW.
//
// The raw error is cut before exact-value redaction, so a credential can
// straddle the cut and leave a prefix behind. The tail repair inspected a fixed
// 4 KiB at the end, which cannot find a credential that BEGINS earlier: the
// inspected span starts partway through it, and a middle is not a prefix.
func TestNoCredentialPrefixSurvivesTheRawBound(t *testing.T) {
	cut := maxMCPReasonRawLen + maxMCPSecretMatchWindow

	t.Run("credential beginning before the tail window", func(t *testing.T) {
		secret := aperiodicSecret(6000)
		raw := config.MCPServerConfig{URL: "https://host.invalid/mcp?workspace=" + secret}
		// Begin the credential 5000 bytes before the cut, so the last 4 KiB
		// starts 904 bytes INSIDE it.
		filler := strings.Repeat("A", cut-5000)
		got := redactMCPFailureReason(errors.New(filler+secret+strings.Repeat("B", 4096)), raw, nil)
		for size := len(secret); size > 0; size-- {
			if strings.Contains(got, secret[:size]) {
				t.Fatalf("a %d-byte prefix of the credential reached the panel", size)
			}
		}
	})

	t.Run("eight-byte credential split after seven", func(t *testing.T) {
		// Exactly shortestMCPSecret. The old floor skipped remnants below eight
		// bytes, so seven of these eight were displayed on purpose.
		const secret = "Qw7ZmPr4"
		raw := config.MCPServerConfig{URL: "https://host.invalid/mcp?workspace=" + secret}
		got := redactMCPFailureReason(errors.New(strings.Repeat("A", cut-7)+secret+strings.Repeat("B", 64)), raw, nil)
		if strings.HasSuffix(strings.TrimSpace(got), secret[:7]) {
			t.Errorf("seven of the eight bytes of the credential survived: %q", got[max(0, len(got)-16):])
		}
	})

	t.Run("an ordinary failure keeps its tail", func(t *testing.T) {
		// The floor exists so a candidate that merely starts with the last
		// character of a message does not eat it.
		raw := config.MCPServerConfig{URL: "https://host.invalid/mcp?workspace=" + aperiodicSecret(40)}
		message := "dial tcp 10.0.0.5:443: connect: connection refused"
		if got := redactMCPFailureReason(errors.New(message), raw, nil); !strings.Contains(got, message) {
			t.Errorf("an unrelated failure lost its tail: %q", got)
		}
	})
}

// PROVENANCE OUTRANKS THE READABILITY HEURISTIC.
//
// credentialCandidates drops anything under shortestMCPSecret so ordinary short
// configuration such as v=1 or mode=sse is not blanked out of unrelated text.
// That trade-off is only defensible while a value might not be a credential.
// Routing values already KNOWN to be secret through it discarded them.
func TestKnownCredentialsSkipTheReadabilityFloor(t *testing.T) {
	const shortSecret = "s3cr3t" // six bytes, under shortestMCPSecret

	for _, testCase := range []struct {
		name string
		raw  config.MCPServerConfig
		echo string
	}{
		{
			name: "oauth client secret",
			raw:  config.MCPServerConfig{URL: "https://host.invalid/mcp", OAuth: &config.MCPOAuthConfig{ClientSecret: shortSecret}},
			echo: "token endpoint replied error_description=" + shortSecret,
		},
		{
			name: "credential-bearing flag value",
			raw:  config.MCPServerConfig{Command: "server", Args: []string{"--api-key", shortSecret}},
			echo: "child rejected invocation: server --api-key " + shortSecret,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if !containsValue(mcpServerSecretValues(testCase.raw), shortSecret) {
				t.Errorf("a credential known by provenance was dropped for being short")
			}
			if got := redactMCPFailureReason(errors.New(testCase.echo), testCase.raw, nil); strings.Contains(got, shortSecret) {
				t.Errorf("the credential reached the panel: %q", got)
			}
		})
	}

	// And the floor still protects ordinary short configuration, or the fix
	// would just be blanket over-redaction.
	raw := config.MCPServerConfig{URL: "https://host.invalid/mcp?mode=sse&v=1"}
	if got := redactMCPFailureReason(errors.New("transport mode=sse rejected"), raw, nil); !strings.Contains(got, "mode=sse") {
		t.Errorf("an ordinary short parameter was redacted out of the message: %q", got)
	}
}

// THE OAUTH ENDPOINTS PARTICIPATE IN STARTUP TOO.
//
// With a stored token a 401 triggers a refresh, oauth.PostToken posts to the
// configured TokenEndpoint, and a dial or TLS failure is wrapped in a url.Error
// that keeps the path and query. Collecting only from the main URL left those
// values outside the candidate set.
func TestOAuthEndpointCredentialsAreRedacted(t *testing.T) {
	const secret = "opaque-workspace-9f3c2b7ae1d8"
	for _, testCase := range []struct {
		name  string
		oauth *config.MCPOAuthConfig
	}{
		{"token endpoint", &config.MCPOAuthConfig{TokenEndpoint: "https://auth.invalid/token?workspace=" + secret}},
		{"authorization endpoint", &config.MCPOAuthConfig{AuthorizationEndpoint: "https://auth.invalid/authorize?workspace=" + secret}},
		{"registration endpoint", &config.MCPOAuthConfig{RegistrationEndpoint: "https://auth.invalid/register?workspace=" + secret}},
		{"issuer url", &config.MCPOAuthConfig{IssuerURL: "https://auth.invalid/issuer?workspace=" + secret}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := config.MCPServerConfig{URL: "https://host.invalid/mcp", OAuth: testCase.oauth}
			got := redactMCPFailureReason(errors.New(`Post "https://auth.invalid/x?workspace=`+secret+`": dial tcp: refused`), raw, nil)
			if strings.Contains(got, secret) {
				t.Errorf("the endpoint credential reached the panel: %q", got)
			}
			if !strings.Contains(got, "auth.invalid") {
				t.Errorf("the host was redacted too, so the failure is no longer diagnosable: %q", got)
			}
		})
	}
}

// ONE PARSER FOR BOTH SURFACES.
//
// The collector and the target row each derived the accepted header spellings
// separately. Neither recognised the conventional attached form, so the value
// was missing from the redaction set AND printed verbatim one row below.
func TestAttachedHeaderArgumentIsRedactedOnBothSurfaces(t *testing.T) {
	const secret = "opaque-workspace-token-9f3c2b7"
	for _, arg := range []string{
		"-HX-Workspace-Id:" + secret,
		"-HX-Workspace-Id: " + secret,
		"-H=X-Workspace-Id: " + secret,
		"-H X-Workspace-Id: " + secret,
		"--header=X-Workspace-Id: " + secret,
		"--HEADER=X-Workspace-Id: " + secret,
	} {
		t.Run(arg, func(t *testing.T) {
			args := []string{"mcp-server", arg}
			if !containsValue(sensitiveMCPArgValues(args), secret) {
				t.Errorf("the header value was not collected for redaction")
			}
			display := strings.Join(redactedCommandArgs(args), " ")
			if strings.Contains(display, secret) {
				t.Errorf("the target row prints the credential: %q", display)
			}
			if !strings.Contains(display, "X-Workspace-Id") {
				t.Errorf("the header NAME was redacted too, which loses the diagnostic: %q", display)
			}
			raw := config.MCPServerConfig{Command: "mcp-server", Args: args}
			if got := redactMCPFailureReason(errors.New("child echoed: "+strings.Join(args, " ")), raw, nil); strings.Contains(got, secret) {
				t.Errorf("the credential reached the failure reason: %q", got)
			}
		})
	}

	// Lowercase -h is help and takes no value. Folding case on the short form
	// would make it consume the next argument and blank an unrelated word.
	if values := sensitiveMCPArgValues([]string{"server", "-h", "status"}); containsValue(values, "status") {
		t.Errorf("-h consumed the following argument: %v", values)
	}
}

// SAFETY MUST BE MONOTONIC OVER AN OBSERVATION.
//
// A retained startup failure keeps the RAW error and is redacted again on every
// render from whatever the token store holds at that moment. Logging out of a
// server deletes the stored bearer and leaves the configuration unchanged, so
// the observation is retained while the candidate set that was hiding the
// bearer disappears, and the next render writes the credential into the panel
// and the transcript AFTER the user asked for it to be forgotten.
func TestRetainedFailureCannotBecomeLessRedacted(t *testing.T) {
	const bearer = "stored-bearer-9f3c2b7ae1d8c4"
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {URL: "https://host.invalid/mcp"},
	}}
	// An echo the generic patterns cannot catch: only the stored-token candidate
	// set was ever hiding this.
	failure := errors.New("upstream rejected the session; echoed credential was " + bearer)

	reasonFor := func(captured string) string {
		t.Helper()
		state := BuildMCPViewState(MCPStateOptions{
			Config:             cfg,
			Skipped:            []mcppkg.SkippedServer{{Name: "docs", Err: failure}},
			SkippedCredentials: captured,
		})
		for _, server := range state.Servers {
			if server.Name == "docs" {
				return server.Error
			}
		}
		t.Fatalf("the failed server is missing from the state")
		return ""
	}

	// Captured while the bearer existed; the store is empty now, as after logout.
	guarded := reasonFor(mcpCredentialFingerprint([]string{bearer}))
	if strings.Contains(guarded, bearer) {
		t.Errorf("the retained failure became less redacted once the credential went away: %q", guarded)
	}
	if !strings.Contains(guarded, "startup failed") {
		t.Errorf("the row stopped reporting the failure at all: %q", guarded)
	}

	// An unchanged credential set is still rendered normally, or the guard would
	// be suppressing every failure.
	if unchanged := reasonFor(mcpCredentialFingerprint(nil)); strings.Contains(unchanged, "credentials changed") {
		t.Errorf("an unchanged credential set was treated as stale: %q", unchanged)
	}
}

func TestCredentialFingerprintIsOrderIndependentAndUnambiguous(t *testing.T) {
	if mcpCredentialFingerprint([]string{"a", "b"}) != mcpCredentialFingerprint([]string{"b", "a"}) {
		t.Error("SecretValues enumerates a map, so the fingerprint must not depend on order")
	}
	if mcpCredentialFingerprint([]string{"ab", "c"}) == mcpCredentialFingerprint([]string{"a", "bc"}) {
		t.Error("values must be length-delimited or two different sets collide")
	}
}
