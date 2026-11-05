package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/tools"
)

// helperCommandName is a command that resolves on this platform, so registration
// reaches connectStdio. What actually runs is whatever the preparer returns.
func helperCommandName() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "sh"
}

const adapterLaunchNotice = "denyRead is configured, so the write jail is not confining writes"

// adapterHelperPreparer models the Windows wrapped plan: the command is the
// HELPER, the launch fact is a report file, and the plan's cleanup removes that
// file. The cleanup is the part that matters, because it is what runs inside
// client.Close and destroys the evidence a later decision needs.
type adapterHelperPreparer struct {
	reportPath  string
	reportBody  string
	writeReport bool
	called      bool
}

func (preparer *adapterHelperPreparer) PrepareExecution(_ context.Context, _ execution.Request) (execution.PreparedCommand, error) {
	preparer.called = true
	if preparer.writeReport {
		if err := os.WriteFile(preparer.reportPath, []byte(preparer.reportBody), 0o600); err != nil {
			return execution.PreparedCommand{}, err
		}
	}
	// A helper that starts, says nothing an MCP client understands, and exits, so
	// the handshake fails the way it does when the requested server never existed.
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", "exit 0")
	} else {
		command = exec.Command("/bin/sh", "-c", "exit 0")
	}
	path := preparer.reportPath
	return execution.PreparedCommand{
		Command:                   command,
		ChildLaunchOwnedByAdapter: true,
		Enforcement:               execution.Enforcement{Notices: []string{adapterLaunchNotice}},
		Report: func() (execution.AdapterReport, error) {
			raw, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return execution.AdapterReport{}, nil
			}
			if err != nil {
				return execution.AdapterReport{}, err
			}
			var report execution.AdapterReport
			if err := json.Unmarshal(raw, &report); err != nil {
				return execution.AdapterReport{}, err
			}
			return report, nil
		},
		Cleanup: func() { _ = os.Remove(path) },
	}, nil
}

func registerWithAdapterHelper(t *testing.T, preparer *adapterHelperPreparer) *Runtime {
	t.Helper()
	runtime, err := RegisterTools(context.Background(), tools.NewRegistry(), config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: helperCommandName()},
	}}, RegisterOptions{
		// No ClientFactory on purpose: an injected factory skips connectStdio
		// entirely, which is where the decision under test is made, and the test
		// would pass with the fix removed.
		Execution:     execution.NewRunner(preparer),
		WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

// A HELPER THAT NEVER CREATED THE SERVER HAS NOTHING TO DISCLOSE.
//
// For an adapter-owned launch, cmd.Start proves only that the sandbox helper
// started. It can then fail setup-marker validation, ACL application, network
// validation, token construction, or CreateProcessAsUser without ever creating
// the requested MCP server. The launch sink already made that distinction; the
// initialize-error path did not, and carried the planned notices out
// unconditionally. The operator was told the server had run without write
// confinement when no server had run at all.
func TestAnMCPHelperThatReportedNoChildDisclosesNothing(t *testing.T) {
	directory := t.TempDir()
	preparer := &adapterHelperPreparer{
		reportPath:  filepath.Join(directory, "report.json"),
		reportBody:  `{"childLaunched":false}`,
		writeReport: true,
	}
	runtime := registerWithAdapterHelper(t, preparer)

	// SETUP: the attempt really did fail, or there is no disclosure decision here.
	if len(runtime.Skipped()) == 0 {
		t.Fatal("SETUP INVALID: the server connected, so the initialize-failure path is not under test")
	}
	if got := runtime.StartupDisclosures(); len(got) != 0 {
		t.Fatalf("a helper that reported no child announced %v; no server ran, confined or otherwise", got)
	}
}

// AND ONE THAT DID CREATE IT DISCLOSES ONCE.
//
// The companion case, and the one that keeps the assertion above from being
// satisfied by a path that discloses nothing ever. The child ran under the
// planned token and may have done filesystem work before the handshake failed,
// so the disclosure has to survive the failure.
func TestAnMCPHelperThatLaunchedTheChildDisclosesOnce(t *testing.T) {
	directory := t.TempDir()
	preparer := &adapterHelperPreparer{
		reportPath:  filepath.Join(directory, "report.json"),
		reportBody:  `{"childLaunched":true}`,
		writeReport: true,
	}
	runtime := registerWithAdapterHelper(t, preparer)

	if !preparer.called {
		t.Fatal("SETUP INVALID: the preparer never ran, so connectStdio was never reached")
	}
	disclosures := runtime.StartupDisclosures()
	if len(disclosures) != 1 {
		t.Fatalf("a server that really ran under the weakened token produced %d disclosures, want exactly one: %v", len(disclosures), disclosures)
	}
	if len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != adapterLaunchNotice {
		t.Fatalf("the disclosure carries %v, want the one planned notice", disclosures[0].Notices)
	}
}

// A helper that wrote no report at all is the same answer as one that reported
// no child: absence is not confirmation.
func TestAnMCPHelperThatWroteNoReportDisclosesNothing(t *testing.T) {
	directory := t.TempDir()
	preparer := &adapterHelperPreparer{reportPath: filepath.Join(directory, "report.json")}
	runtime := registerWithAdapterHelper(t, preparer)

	if len(runtime.Skipped()) == 0 {
		t.Fatal("SETUP INVALID: the server connected, so the initialize-failure path is not under test")
	}
	if got := runtime.StartupDisclosures(); len(got) != 0 {
		t.Fatalf("a helper that published nothing announced %v", got)
	}
}
