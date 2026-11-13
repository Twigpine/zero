package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
)

// A DELIMITER-HEAVY CONFIGURED VALUE MUST NOT EXPAND WITHOUT BOUND.
//
// credentialCandidates walked every suffix after every space or colon and kept
// them all, and RedactString then ran a replacement pass per candidate. The
// expansion happened on the CONFIG side, outside the raw-error bound, so the
// cost did not depend on the server's error being long at all: opening or
// refreshing /mcp for that server paid it however short the failure was.
func TestADelimiterHeavyValueDoesNotExpandWithoutBound(t *testing.T) {
	value := strings.Repeat("token:part ", 4000)

	candidates := credentialCandidates(value)
	if len(candidates) > maxMCPCredentialCandidates {
		t.Errorf("one configured value expanded into %d candidates, want at most %d", len(candidates), maxMCPCredentialCandidates)
	}

	// And the whole value is still redacted, which is the point of bounding the
	// tails rather than the value.
	raw := config.MCPServerConfig{
		URL:     "https://host.invalid/mcp",
		Headers: map[string]string{"X-Workspace": value},
	}
	started := time.Now()
	got := redactMCPFailureReason(errors.New("502 Bad Gateway from "+value), raw, nil)
	elapsed := time.Since(started)

	if strings.Contains(got, value) {
		t.Error("the configured value survived redaction")
	}
	// Loose on purpose: it fails on the old superlinear behaviour and not on a
	// slow machine.
	if elapsed > 2*time.Second {
		t.Errorf("redacting one failure took %s; the candidate expansion is still superlinear in the configured value", elapsed)
	}
}

// A WORK BOUND MUST NOT DROP A CREDENTIAL SPELLING.
//
// The input cap this replaces returned the whole value and nothing else once a
// configured value crossed 8 KiB. That turned a cost control into a change of
// security semantics: "Bearer <opaque token>" stopped offering the bare token as
// a candidate, so a failed server echoing only the token matched nothing, and an
// opaque token has no shape for the fallback redactor to recognise either. The
// first 400 characters then reached both /mcp render paths and the transcript.
//
// The bound is now on where a separator is looked for, so the tails survive at
// any length.
func TestAnOversizedCredentialStillYieldsItsToken(t *testing.T) {
	token := strings.Repeat("A", 8198)
	value := "Bearer " + token

	candidates := credentialCandidates(value)
	if len(candidates) > maxMCPCredentialCandidates {
		t.Errorf("expanded into %d candidates, want at most %d", len(candidates), maxMCPCredentialCandidates)
	}
	var whole, bare bool
	for _, candidate := range candidates {
		switch candidate {
		case value:
			whole = true
		case token:
			bare = true
		}
	}
	if !whole {
		t.Error("the whole configured value is no longer a candidate")
	}
	if !bare {
		t.Fatal("the bare token is not a candidate, so a server echoing only the token is not redacted")
	}
}

// The header form has two separators, and both tails must survive the same way.
func TestAnOversizedHeaderCredentialYieldsBothTails(t *testing.T) {
	token := strings.Repeat("B", 9000)
	value := "Authorization: Bearer " + token

	var bare, afterHeader bool
	for _, candidate := range credentialCandidates(value) {
		switch candidate {
		case token:
			bare = true
		case "Bearer " + token:
			afterHeader = true
		}
	}
	if !afterHeader {
		t.Error("the <scheme> <credential> tail is missing")
	}
	if !bare {
		t.Error("the bare token is missing")
	}
}

// The separator scan is bounded, and the boundary is stated rather than implied:
// a separator inside the window yields its tail, one past the window does not.
// That is a cost decision, and it is safe because every convention this walks
// puts its separators in a short prefix.
func TestCredentialSeparatorScanBoundary(t *testing.T) {
	token := strings.Repeat("C", 4096)
	for _, testCase := range []struct {
		name     string
		padding  int
		wantTail bool
	}{
		{name: "separator just inside the window", padding: maxMCPCredentialSeparatorScan - 2, wantTail: true},
		{name: "separator at the last scanned byte", padding: maxMCPCredentialSeparatorScan - 1, wantTail: true},
		{name: "separator just past the window", padding: maxMCPCredentialSeparatorScan + 1, wantTail: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value := strings.Repeat("x", testCase.padding) + " " + token
			var found bool
			for _, candidate := range credentialCandidates(value) {
				if candidate == token {
					found = true
				}
			}
			if found != testCase.wantTail {
				t.Errorf("tail present = %v, want %v (padding %d)", found, testCase.wantTail, testCase.padding)
			}
		})
	}
}
