package mcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/tools"
)

const startupNotice = "denyRead is configured, so the write jail is not confining writes"

// disclosingPreparer plans an MCP server launch that carries an enforcement
// notice, and can fail the way the sandbox does before the child exists.
type disclosingPreparer struct {
	prepareErr error
	missing    bool
}

func (preparer *disclosingPreparer) PrepareExecution(ctx context.Context, request execution.Request) (execution.PreparedCommand, error) {
	if preparer.prepareErr != nil {
		return execution.PreparedCommand{}, preparer.prepareErr
	}
	name, args := request.Command.Name, request.Command.Args
	if preparer.missing {
		name, args = "definitely-not-a-real-binary-zzz", nil
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = request.WorkingDirectory
	command.Env = request.Command.Env
	return execution.PreparedCommand{
		Command:     command,
		Enforcement: execution.Enforcement{Notices: []string{startupNotice}},
	}, nil
}

func helperServer(t *testing.T) Server {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		Name:    "docs",
		Type:    ServerTypeStdio,
		Command: executable,
		Args:    []string{"-test.run=TestMCPStdioHelperProcess", "--"},
		Env:     map[string]string{"ZERO_MCP_STDIO_HELPER": "1"},
	}
}

// THE FACT DESCRIBES STARTUP, SO NOTHING LATER CAN CARRY IT.
//
// The generic adapter puts plan notes on PreparedCommand.Enforcement for
// OriginMCPServer, and connectStdio kept only the command and its cleanup. A
// stdio server launched under the weakened token then served the whole session
// with no path able to tell the operator that its write confinement was
// reduced, and no individual tool result could recover it, because the fact is
// about the process rather than about any response.
func TestAnMCPServerLaunchKeepsItsEnforcementDisclosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := ConnectWithOptions(ctx, helperServer(t), ConnectOptions{
		Execution:     execution.NewRunner(&disclosingPreparer{}),
		WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ConnectWithOptions() error = %v", err)
	}
	defer client.Close()

	disclosing, ok := client.(startupDisclosing)
	if !ok {
		t.Fatal("a launched stdio client does not report its startup enforcement at all")
	}
	notices := disclosing.StartupNotices()
	if len(notices) != 1 || notices[0] != startupNotice {
		t.Fatalf("StartupNotices() = %#v, want the launch disclosure", notices)
	}
}

// A launch with nothing to disclose reports nothing, or every server would carry
// a notice and the statement would mean nothing.
func TestAnUnrestrictedMCPServerLaunchDisclosesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := ConnectWithOptions(ctx, helperServer(t), ConnectOptions{
		Execution:     execution.NewRunner(&mcpExecutionPreparer{}),
		WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ConnectWithOptions() error = %v", err)
	}
	defer client.Close()
	disclosing, ok := client.(startupDisclosing)
	if !ok {
		t.Fatal("a launched stdio client does not report its startup enforcement at all")
	}
	if notices := disclosing.StartupNotices(); len(notices) != 0 {
		t.Errorf("StartupNotices() = %#v, want none", notices)
	}
}

// And a launch that never happened claims nothing, which is the same launch-state
// rule hooks and plugins apply. Here it is expressed by WHERE the notice is
// recorded: every failure above returns before the client exists.
func TestAnMCPServerThatNeverLaunchedClaimsNoEnforcement(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		preparer *disclosingPreparer
	}{
		{"sandbox setup failed", &disclosingPreparer{prepareErr: errors.New("could not build the restricted token")}},
		{"executable not found", &disclosingPreparer{missing: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, err := ConnectWithOptions(ctx, helperServer(t), ConnectOptions{
				Execution:     execution.NewRunner(testCase.preparer),
				WorkspaceRoot: t.TempDir(),
			})
			if err == nil {
				client.Close()
				t.Fatal("the server started even though its launch was supposed to fail")
			}
			// THE CARRIER, NOT THE TEXT. A connect failure hands its notices on
			// startupDisclosureError, whose Error() prints only the cause, so a
			// check of the message could never see a notice that was carried.
			if carried := startupNoticesFromError(err); len(carried) != 0 {
				t.Errorf("a launch that never happened claimed an enforcement trade: %#v", carried)
			}
			if strings.Contains(err.Error(), startupNotice) {
				t.Errorf("a launch that never happened claimed an enforcement trade in its error: %v", err)
			}
		})
	}
}

// disclosingFakeClient is a launched server that carries a disclosure.
type disclosingFakeClient struct {
	fakeToolClient
	notices []string
}

func (client *disclosingFakeClient) StartupNotices() []string { return client.notices }

// Registration is the boundary that reports it, once, for the process it
// launched.
func TestRegistrationCollectsStartupDisclosures(t *testing.T) {
	registry := tools.NewRegistry()
	client := &disclosingFakeClient{
		fakeToolClient: fakeToolClient{listed: []RemoteTool{{Name: "lookup", Description: "Lookup"}}},
		notices:        []string{startupNotice},
	}
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{ClientFactory: func(context.Context, Server) (ToolClient, error) { return client, nil }})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	disclosures := runtime.StartupDisclosures()
	if len(disclosures) != 1 {
		t.Fatalf("StartupDisclosures() = %#v, want one", disclosures)
	}
	if disclosures[0].Name != "docs" {
		t.Errorf("Name = %q, want the server it describes", disclosures[0].Name)
	}
	if len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != startupNotice {
		t.Errorf("Notices = %#v, want the launch disclosure", disclosures[0].Notices)
	}
}

// A network server launches no local process, so it implements nothing and
// reports nothing. That is a different answer from an empty one and the negative
// case that keeps the statement meaningful.
func TestANetworkServerReportsNoStartupDisclosure(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://host.invalid/mcp"},
	}}, RegisterOptions{ClientFactory: func(context.Context, Server) (ToolClient, error) {
		return &fakeToolClient{listed: []RemoteTool{{Name: "lookup", Description: "Lookup"}}}, nil
	}})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()
	if disclosures := runtime.StartupDisclosures(); len(disclosures) != 0 {
		t.Errorf("StartupDisclosures() = %#v, want none for a server that launches no process", disclosures)
	}
}
