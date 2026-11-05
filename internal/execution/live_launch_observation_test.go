package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const liveLaunchNotice = "denyRead is configured, so the write jail is not confining writes"

// liveReportReader mirrors sandbox.CommandPlan.ExecutionReport: read the file the
// helper publishes, treat "not there yet" as nothing recorded.
func liveReportReader(path string) func() (AdapterReport, error) {
	return func() (AdapterReport, error) {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return AdapterReport{}, nil
		}
		if err != nil {
			return AdapterReport{}, err
		}
		var report AdapterReport
		if err := json.Unmarshal(raw, &report); err != nil {
			return AdapterReport{}, err
		}
		return report, nil
	}
}

// liveHelperCommand stands in for the Windows helper: publish the launch fact the
// way it does right after CreateProcessAsUser, then stay alive the way it does
// while waiting on the child.
func liveHelperCommand(t *testing.T, reportPath string, publish bool) *exec.Cmd {
	t.Helper()
	if publish {
		if err := os.WriteFile(reportPath, []byte(`{"childLaunched":true}`), 0o600); err != nil {
			t.Fatalf("publish the launch report: %v", err)
		}
	}
	if runtime.GOOS == "windows" {
		// A child that holds itself open without exiting.
		return exec.Command("cmd.exe", "/c", "pause")
	}
	return exec.Command("/bin/sh", "-c", "sleep 30")
}

// liveRequest is a valid interactive request; ProcessManager.Start validates it
// before anything under test runs.
func liveRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		Origin:           OriginInteractiveCommand,
		Mode:             ModeInteractive,
		Command:          Command{Name: "helper"},
		WorkingDirectory: t.TempDir(),
		WorkspaceRoots:   []string{t.TempDir()},
		Approval:         ApprovalContext{PolicyVersion: PolicyVersion},
	}
}

// A RETAINED SESSION HAS TO DISCLOSE WHILE IT IS STILL RUNNING.
//
// The helper publishes childLaunched immediately after it creates the restricted
// child, and only then waits for it. The manager read that report exclusively in
// the post-Wait goroutine, so for the whole live lifetime of a wrapped session the
// report was the zero value: the first exec_command reply and every write_stdin
// poll resolved Launched=false and disclosed nothing, while the fact sat readable
// on disk. A watcher, or a retained session nobody polls to completion, would
// never be told the write jail had been traded away.
//
// Driven through the real ProcessManager with a real child process, and asserted
// on the ProcessResult the tool layer consumes.
func TestALiveWrappedSessionCarriesTheLaunchFact(t *testing.T) {
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "report.json")
	command := liveHelperCommand(t, reportPath, true)

	manager := NewProcessManager(ProcessManagerOptions{})
	result, err := manager.Start(context.Background(), ProcessStart{
		Prepared: PreparedCommand{
			Command:                   command,
			ChildLaunchOwnedByAdapter: true,
			Enforcement:               Enforcement{Notices: []string{liveLaunchNotice}},
			Report:                    liveReportReader(reportPath),
		},
		Request: liveRequest(t),
	}, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { manager.StopAll() })

	// SETUP: this has to be the LIVE state, and the fact has to be on disk, or the
	// assertion below would be about a terminal read.
	if result.Exited {
		t.Fatal("SETUP INVALID: the stand-in helper exited, so the live lifecycle is not under test")
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("SETUP INVALID: the launch report is not on disk, so there is nothing to observe: %v", statErr)
	}

	if !ResolveChildLaunched(true, result.ChildLaunchOwnedByAdapter, result.Report) {
		t.Fatal("a live wrapped session resolved as not launched, so its enforcement disclosure is withheld while the command runs")
	}

	// And again on a poll, which is the write_stdin leg.
	polled, err := manager.Continue(context.Background(), ProcessContinue{ProcessID: result.ProcessID, Wait: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if polled.Exited {
		t.Fatal("SETUP INVALID: the helper exited before the poll, so the live poll is not under test")
	}
	if !ResolveChildLaunched(true, polled.ChildLaunchOwnedByAdapter, polled.Report) {
		t.Fatal("a live poll of a wrapped session resolved as not launched")
	}
	if got := polled.Enforcement.Notices; len(got) != 1 || got[0] != liveLaunchNotice {
		t.Fatalf("the live poll carries notices %v, want exactly the one planned notice", got)
	}
}

// AND A HELPER THAT NEVER CREATED THE CHILD STAYS SILENT.
//
// This is the negative the live read must not destroy. A helper that starts and
// then fails setup, ACL application, or CreateProcessAsUser has an outer process
// running and no child, so nothing may be promoted from the fact that the
// wrapper itself is alive.
func TestALiveWrappedSessionWithNoReportedChildStaysSilent(t *testing.T) {
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "report.json")
	command := liveHelperCommand(t, reportPath, false)

	manager := NewProcessManager(ProcessManagerOptions{})
	result, err := manager.Start(context.Background(), ProcessStart{
		Prepared: PreparedCommand{
			Command:                   command,
			ChildLaunchOwnedByAdapter: true,
			Enforcement:               Enforcement{Notices: []string{liveLaunchNotice}},
			Report:                    liveReportReader(reportPath),
		},
		Request: liveRequest(t),
	}, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { manager.StopAll() })

	if result.Exited {
		t.Fatal("SETUP INVALID: the stand-in helper exited, so the live lifecycle is not under test")
	}
	if _, statErr := os.Stat(reportPath); statErr == nil {
		t.Fatal("SETUP INVALID: a report exists, so this is not the no-child case")
	}
	if ResolveChildLaunched(true, result.ChildLaunchOwnedByAdapter, result.Report) {
		t.Fatal("a helper that reported no child was promoted to a launch, so the operator is told a write jail was traded away for a child that never existed")
	}
}

// A report that appears mid-flight is observed on the next poll, which is what
// makes this a lifecycle transition rather than a start-time snapshot.
func TestTheLaunchFactIsObservedWhenItAppearsMidFlight(t *testing.T) {
	directory := t.TempDir()
	reportPath := filepath.Join(directory, "report.json")
	command := liveHelperCommand(t, reportPath, false)

	manager := NewProcessManager(ProcessManagerOptions{})
	result, err := manager.Start(context.Background(), ProcessStart{
		Prepared: PreparedCommand{
			Command:                   command,
			ChildLaunchOwnedByAdapter: true,
			Report:                    liveReportReader(reportPath),
		},
		Request: liveRequest(t),
	}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { manager.StopAll() })
	if ResolveChildLaunched(true, result.ChildLaunchOwnedByAdapter, result.Report) {
		t.Fatal("SETUP INVALID: nothing was published yet, so the first result must not be a launch")
	}

	if err := os.WriteFile(reportPath, []byte(`{"childLaunched":true}`), 0o600); err != nil {
		t.Fatalf("publish the launch report: %v", err)
	}
	polled, err := manager.Continue(context.Background(), ProcessContinue{ProcessID: result.ProcessID, Wait: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if !ResolveChildLaunched(true, polled.ChildLaunchOwnedByAdapter, polled.Report) {
		t.Fatal("the launch published while the session was live was never observed")
	}
}
