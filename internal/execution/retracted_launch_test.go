package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A LAUNCH THE HELPER TAKES BACK MUST NOT SURVIVE IN EITHER RESULT.
//
// The Windows helper publishes childLaunched before it resumes the suspended
// child, so the fact is readable while the child has still executed nothing. A
// poll landing in that window latched it, and the latch outlived the file: the
// terminal read finds the report cleaned away and restores what was observed, so
// a child that never became runnable was reported as launched, and the write-jail
// trade it never made was disclosed as if it had.
//
// Deleting the report on that path cannot fix it, because deletion is also what a
// normal cleanup does. The helper retracts with an explicit false instead, and
// this is the interleaving that pins it: publish, live read, retract, poll, then
// finish and read terminally.
func TestARetractedLaunchIsNotCommittedLiveOrTerminally(t *testing.T) {
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

	// SETUP: the publication window. The live read has to have seen the positive,
	// or the revocation below would have nothing to revoke.
	if result.Exited {
		t.Fatal("SETUP INVALID: the stand-in helper exited, so the live lifecycle is not under test")
	}
	if !ResolveChildLaunched(true, result.ChildLaunchOwnedByAdapter, result.Report) {
		t.Fatal("SETUP INVALID: the published launch was not observed live, so this test cannot show it being taken back")
	}

	// The resume fails: the helper retracts the record it published.
	if err := os.WriteFile(reportPath, []byte(`{"childLaunched":false}`), 0o600); err != nil {
		t.Fatal(err)
	}

	polled, err := manager.Continue(context.Background(), ProcessContinue{ProcessID: result.ProcessID, Wait: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if polled.Exited {
		t.Fatal("SETUP INVALID: the helper exited before the poll, so the live poll is not under test")
	}
	if ResolveChildLaunched(true, polled.ChildLaunchOwnedByAdapter, polled.Report) {
		t.Error("a live poll still reports a launch the helper retracted, so the disclosure stands for a child that never ran")
	}

	// Finish, with the plan's cleanup removing the report the way it always does.
	// The terminal read then finds absence, which is exactly the case the restore
	// exists for, and it must not resurrect the retracted launch.
	if !manager.Stop(result.ProcessID) {
		t.Fatal("Stop: process not found")
	}
	_ = os.Remove(reportPath)
	final, err := manager.Continue(context.Background(), ProcessContinue{ProcessID: result.ProcessID, Wait: time.Second})
	if err != nil {
		t.Fatalf("Continue after stop: %v", err)
	}
	if !final.Exited {
		t.Fatal("SETUP INVALID: the helper never exited, so there is no terminal result under test")
	}
	if ResolveChildLaunched(true, final.ChildLaunchOwnedByAdapter, final.Report) {
		t.Error("the final result reports a launch the helper retracted")
	}
}

// AND A GENUINE LAUNCH STILL SURVIVES ITS OWN CLEANUP. The revocation must key
// on an explicit denial and nothing else: a report removed by the plan's normal
// cleanup is silence, and silence does not take back what was seen.
func TestAConfirmedLaunchSurvivesTheReportCleanup(t *testing.T) {
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
	if !ResolveChildLaunched(true, result.ChildLaunchOwnedByAdapter, result.Report) {
		t.Fatal("SETUP INVALID: the published launch was not observed live")
	}

	if !manager.Stop(result.ProcessID) {
		t.Fatal("Stop: process not found")
	}
	_ = os.Remove(reportPath)
	final, err := manager.Continue(context.Background(), ProcessContinue{ProcessID: result.ProcessID, Wait: time.Second})
	if err != nil {
		t.Fatalf("Continue after stop: %v", err)
	}
	if !final.Exited {
		t.Fatal("SETUP INVALID: the helper never exited")
	}
	if !ResolveChildLaunched(true, final.ChildLaunchOwnedByAdapter, final.Report) {
		t.Error("a confirmed launch was lost when the plan's cleanup removed its report")
	}
}
