package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/tools"
)

// THE CONTEXT THAT MAKES AN ERROR SAFE IS RECORDED WHERE THE ERROR IS PRODUCED.
//
// A skipped entry keeps the RAW failure, and the surface that displays it
// redacts it against whatever the token store holds at that moment. So the
// consumer has to know whether that set is still the one that was hiding the
// credential, and the only place that can answer honestly is registration.
//
// Sampling at the consumer is too late twice over. Startup registers here and
// the interactive surface is built afterwards, so a refresh or a logout in
// between is invisible; and the connect attempt that PRODUCED the error can
// itself rotate the store, because a 401 triggers a refresh and the error text
// captured on that attempt can contain the bearer that was just replaced. The
// sample therefore has to be taken before connecting, not after: getting it
// wrong in that direction means the consumer compares the new set against
// itself, finds no change, and prints an error containing a bearer that is no
// longer a candidate anywhere.
func TestSkippedFailuresRecordTheCredentialsThatExistedBeforeConnecting(t *testing.T) {
	registry := tools.NewRegistry()
	const before = "bearer-before-rotation"
	const after = "bearer-after-rotation"

	var mu sync.Mutex
	current := []string{before}

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		SecretValues: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), current...)
		},
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			// What a refresh does: the stored bearer is replaced, and the failure
			// reported for this attempt still quotes the old one.
			mu.Lock()
			current = []string{after}
			mu.Unlock()
			return nil, fmt.Errorf("upstream rejected %s", before)
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	skipped := runtime.Skipped()
	if len(skipped) != 1 {
		t.Fatalf("Skipped() = %#v, want one entry", skipped)
	}
	if skipped[0].Credentials == "" {
		t.Fatal("the failure recorded no credential context at all")
	}
	if skipped[0].Credentials == CredentialFingerprint([]string{after}) {
		t.Error("the context was sampled after the connect that rotated the bearer, so the rotation is invisible and the old bearer quoted in the error has nothing left to hide it")
	}
	if want := CredentialFingerprint([]string{before}); skipped[0].Credentials != want {
		t.Errorf("Credentials = %q, want the fingerprint of the material that existed when the error was produced", skipped[0].Credentials)
	}
}

// A validation failure is the other capture site and must record the same
// context, or half the failures carry no claim.
func TestAValidationFailureAlsoRecordsTheCredentialContext(t *testing.T) {
	registry := tools.NewRegistry()
	const bearer = "stored-bearer-value"

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		SecretValues: func() []string { return []string{bearer} },
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			// A nameless tool fails validation in the serial commit phase.
			return &fakeToolClient{listed: []RemoteTool{{Name: "", Description: "nameless"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	skipped := runtime.Skipped()
	if len(skipped) != 1 {
		t.Fatalf("Skipped() = %#v, want one entry", skipped)
	}
	if want := CredentialFingerprint([]string{bearer}); skipped[0].Credentials != want {
		t.Errorf("Credentials = %q, want %q", skipped[0].Credentials, want)
	}
}

// Without a source there is no claim to make, and an empty fingerprint has to
// stay distinguishable from the fingerprint of an empty set: one means "nothing
// recorded", the other means "nothing was there".
func TestNoCredentialSourceRecordsNoClaim(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			return nil, errors.New("unreachable")
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	skipped := runtime.Skipped()
	if len(skipped) != 1 {
		t.Fatalf("Skipped() = %#v, want one entry", skipped)
	}
	if skipped[0].Credentials != "" {
		t.Errorf("Credentials = %q, want empty: nothing was recorded", skipped[0].Credentials)
	}
	if CredentialFingerprint(nil) == "" {
		t.Error("the fingerprint of an empty set must not be empty, or it cannot be told apart from no claim")
	}
}
