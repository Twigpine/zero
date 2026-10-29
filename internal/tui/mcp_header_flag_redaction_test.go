package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// A CREDENTIAL BEHIND A HEADER FLAG IS STILL A CREDENTIAL.
//
// isSensitiveMCPDisplayKey classifies an argument by its FLAG NAME, matching
// token/secret/auth/credential and friends. "header" and "H" match none of them,
// so the whole header-flag family went uncollected while the credential rides in
// a value whose header name the operator chose. A stdio child that rejects its
// invocation echoes that invocation into captured stderr, which reaches
// SkippedServer.Err, and this panel renders it and the transcript keeps it.
func TestHeaderFlagValuesAreRedactedFromAFailureReason(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"

	for _, testCase := range []struct {
		name string
		args []string
	}{
		{name: "long separated", args: []string{"--header", "X-Workspace-Id: " + token}},
		{name: "long equals", args: []string{"--header=X-Workspace-Id: " + token}},
		{name: "long packed", args: []string{"--header X-Workspace-Id: " + token}},
		{name: "short separated", args: []string{"-H", "X-Workspace-Id: " + token}},
		{name: "short equals", args: []string{"-H=X-Workspace-Id: " + token}},
		{name: "short packed", args: []string{"-H X-Workspace-Id: " + token}},
		{name: "mixed case long flag", args: []string{"--Header", "X-Workspace-Id: " + token}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := config.MCPServerConfig{Command: "mcp-server", Args: testCase.args}
			// The child echoes its own invocation back, which is the everyday shape.
			failure := errors.New("mcp-server: unrecognized option\ninvocation: mcp-server " + strings.Join(testCase.args, " "))

			got := redactMCPFailureReason(failure, raw, nil)
			if strings.Contains(got, token) {
				t.Errorf("the credential survived into the rendered failure, which is also persisted to the transcript:\n%s", got)
			}
			if !strings.Contains(got, mcpDisplayRedacted) {
				t.Errorf("nothing was redacted at all, so the value never reached the candidate set:\n%s", got)
			}
		})
	}
}

// -h IS NOT -H. Help takes no value, so folding case on the short flag would set
// pending on `-h` and put the following argument into the redaction set, blanking
// an unrelated word out of every message that mentions it. Over-collection has
// already cost this panel a readable docker image name once, so the narrow
// behaviour is asserted rather than left to chance.
func TestShortHelpFlagDoesNotClaimTheNextArgumentAsASecret(t *testing.T) {
	const image = "ghcr.io/github/github-mcp-server"
	raw := config.MCPServerConfig{Command: "docker", Args: []string{"-h", image}}

	got := redactMCPFailureReason(errors.New("failed to pull "+image), raw, nil)
	if !strings.Contains(got, image) {
		t.Errorf("the image name was redacted because -h was read as a header flag, so the error no longer explains itself:\n%s", got)
	}
}

// And the header NAME must survive. Redacting it would blank the one token that
// tells the operator which header was rejected.
func TestHeaderNameSurvivesWhileItsValueIsRedacted(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"
	raw := config.MCPServerConfig{Command: "mcp-server", Args: []string{"--header", "X-Workspace-Id: " + token}}

	got := redactMCPFailureReason(errors.New("rejected header X-Workspace-Id: "+token), raw, nil)
	if strings.Contains(got, token) {
		t.Fatalf("credential survived:\n%s", got)
	}
	if !strings.Contains(got, "X-Workspace-Id") {
		t.Errorf("the header name was redacted too, so the message no longer says which header was rejected:\n%s", got)
	}
}
