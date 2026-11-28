package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/mcp"
)

type disclosingRuntime struct {
	noopMCPRuntime
	disclosures []mcp.StartupDisclosure
}

func (runtime disclosingRuntime) StartupDisclosures() []mcp.StartupDisclosure {
	return runtime.disclosures
}

// SAID ONCE, WHERE THE USER IS ALREADY BEING TOLD WHAT STARTED.
//
// The disclosure describes a server PROCESS, which serves the whole session, so
// it cannot ride on a tool result and must not be repeated on every one.
func TestStartupDisclosuresAreReportedOnce(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"
	var stderr bytes.Buffer
	reportMCPStartupDisclosures(&stderr, disclosingRuntime{
		disclosures: []mcp.StartupDisclosure{{Name: "docs", Notices: []string{notice}}},
	})
	output := stderr.String()
	if count := strings.Count(output, notice); count != 1 {
		t.Errorf("the disclosure appears %d times, want exactly 1: %q", count, output)
	}
	if !strings.Contains(output, "docs") {
		t.Errorf("the report does not name the server it is about: %q", output)
	}
}

// A run with nothing to disclose prints nothing at all.
func TestNoStartupDisclosuresPrintNothing(t *testing.T) {
	var stderr bytes.Buffer
	reportMCPStartupDisclosures(&stderr, disclosingRuntime{})
	if stderr.Len() != 0 {
		t.Errorf("a run with no disclosure wrote %q", stderr.String())
	}
	stderr.Reset()
	// And a runtime that launches nothing is not required to answer.
	reportMCPStartupDisclosures(&stderr, noopMCPRuntime{})
	if stderr.Len() != 0 {
		t.Errorf("a runtime that launches nothing wrote %q", stderr.String())
	}
}
