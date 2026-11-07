package execution

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// THE TWO NUMBERS MUST NOT CONTRADICT EACH OTHER.
//
// Retention was 30 seconds while maxEmptyPollYield allowed a five-minute poll,
// so a caller Zero itself invites to wait five minutes could arrive to find the
// answer already forgotten. Pinned as a RELATIONSHIP, not a value, so changing
// the poll bound cannot silently reintroduce the gap.
func TestCompletedRetentionCoversTheLongestPoll(t *testing.T) {
	if defaultCompletedRetention < maxEmptyPollYield {
		t.Fatalf("a finished session is forgotten after %s while a poll may wait %s: "+
			"a caller that waits the full poll can arrive after the answer is gone",
			defaultCompletedRetention, maxEmptyPollYield)
	}
}

// A LATE POLL GETS THE RESULT, NOT AN ACCUSATION.
//
// The measured failure: a 60-second test was started, the caller polled with
// yield_time_ms 40000, and the id was already gone — so the same test was run a
// second time to recover a result the first had produced. The reply told the
// caller not to probe session ids, about an id this manager had issued and
// instructed it to poll.
func TestALatePollOnAFinishedSessionGetsItsResult(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{})
	manager.remember(ProcessResult{
		ProcessID:   1004,
		CommandText: "go test -run=TestDifferentialFuzz60s ./diff/",
		Output:      "--- PASS: TestDifferentialFuzz60s (60.00s)\nok  \tmini/diff\t60.330s\n",
		Exited:      true,
		ExitCode:    0,
	})
	// The process itself is long gone; only the record remains.
	manager.Remove(1004)

	result, err := manager.Continue(t.Context(), ProcessContinue{ProcessID: 1004})
	if err != nil {
		t.Fatalf("a session that ran and finished was refused: %v", err)
	}
	if !result.Exited || result.ExitCode != 0 {
		t.Errorf("the finished result was not carried back: %+v", result)
	}
	if !strings.Contains(result.Output, "TestDifferentialFuzz60s") {
		t.Errorf("the output was lost, so the work has to be done again:\n%s", result.Output)
	}
}

// AN ID THIS MANAGER NEVER ISSUED IS STILL REFUSED. The accusation exists for a
// real probe, and softening it for everything would remove the signal the
// repeated-failure guard keys on.
func TestAnIdThatNeverRanIsStillNotFound(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{})
	if _, err := manager.Continue(t.Context(), ProcessContinue{ProcessID: 9999}); err != ErrProcessNotFound {
		t.Fatalf("a never-issued id returned %v, want ErrProcessNotFound", err)
	}
}

// The record is bounded: a long session cannot accumulate them without limit,
// and the OLDEST is dropped first so the most recent work stays answerable.
func TestRememberedCompletionsAreBoundedOldestFirst(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{})
	total := maxRememberedCompletions + 10
	for i := 0; i < total; i++ {
		manager.remember(ProcessResult{ProcessID: 1000 + i, Exited: true, Output: "x"})
	}
	if got := len(manager.completed); got != maxRememberedCompletions {
		t.Fatalf("kept %d records, want the bound of %d", got, maxRememberedCompletions)
	}
	if _, ok := manager.Completed(1000); ok {
		t.Error("the oldest record survived the bound")
	}
	if _, ok := manager.Completed(1000 + total - 1); !ok {
		t.Error("the newest record was dropped; recent work is what a late poll asks about")
	}
}

// Only a FINISHED session is remembered — a still-running one is answered by the
// live process, and recording it would hand back a result it has not reached.
func TestOnlyFinishedSessionsAreRemembered(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{})
	manager.remember(ProcessResult{ProcessID: 1001, Exited: false, Output: "partial"})
	if _, ok := manager.Completed(1001); ok {
		t.Error("a still-running session was recorded as finished")
	}
}

// Retention is the same terminal result, not a lossy second representation.
func TestARememberedResultPreservesTerminalSemantics(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{})
	long := strings.Repeat("noise\n", recentOutputBytes) + "FINAL: ok"
	want := ProcessResult{
		ProcessID: 1002, CommandText: "go test ./...", RelativeCwd: "sub", TTY: true,
		Exited: true, ExitCode: 7, Interrupted: true, Output: long, OutputTruncated: true,
		Request:     Request{WorkingDirectory: "/workspace", WorkspaceRoots: []string{"/workspace"}},
		Enforcement: Enforcement{Backend: "seatbelt", Level: "workspace-write"},
		Changes:     []Change{{Path: "changed.go", Kind: ChangeModified}},
		Metadata:    map[string]string{"sandbox": "active"},
	}
	manager.remember(want)
	got, ok := manager.Completed(1002)
	if !ok {
		t.Fatal("not remembered")
	}
	if got.Output != want.Output || got.CommandText != want.CommandText || got.RelativeCwd != want.RelativeCwd || got.TTY != want.TTY || got.ExitCode != want.ExitCode || !got.Interrupted || !got.OutputTruncated {
		t.Fatalf("terminal result lost execution fields: got %+v want %+v", got, want)
	}
	if got.Request.WorkingDirectory != want.Request.WorkingDirectory || got.Enforcement != want.Enforcement || len(got.Changes) != 1 || got.Metadata["sandbox"] != "active" {
		t.Fatalf("terminal result lost policy or mutation evidence: %+v", got)
	}
}

func TestTerminalCollectionHasOneOwner(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		process := &managedProcess{
			id: 1000 + attempt, output: newProcessOutputBuffer(), done: make(chan struct{}), reaped: make(chan struct{}),
		}
		_, _ = process.output.Write([]byte("FINAL-RESULT"))
		process.markDone(nil, 0, AdapterReport{}, nil, nil)

		start := make(chan struct{})
		results := make(chan ProcessResult, 2)
		for collector := 0; collector < 2; collector++ {
			go func() {
				<-start
				results <- process.collectResult(context.Background(), 0, false)
			}()
		}
		close(start)
		first, second := <-results, <-results
		if first.Output != "FINAL-RESULT" || second.Output != "FINAL-RESULT" {
			t.Fatalf("attempt %d: collectors observed different terminal output: %q / %q", attempt, first.Output, second.Output)
		}
	}
}

// THE CASE THAT WAS ACTUALLY MEASURED: nobody polls while it runs.
//
// A caller starts a long command, gets a session id, goes away to do other work,
// and comes back after it finished. Recording the result only on the polling
// paths leaves exactly this case with nothing to hand back — and it is the
// common one, because the reason to background a command is to do something else
// meanwhile. In the measured run this cost a 60-second test being run twice.
func TestASessionNobodyPolledIsStillAnswerable(t *testing.T) {
	root := t.TempDir()
	manager := NewProcessManager(ProcessManagerOptions{})
	// The behaviour under test — a finished background process staying
	// answerable — is OS-agnostic, so unlike this package's POSIX-shell tests
	// this one runs everywhere rather than skipping on Windows.
	shell, flag := "/bin/sh", "-c"
	script := "echo FINAL-RESULT; exit 3"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
		script = "echo FINAL-RESULT& exit 3"
	}
	command := exec.Command(shell, flag, script)
	request := Request{
		Origin: OriginInteractiveCommand, Mode: ModeCaptured,
		Command:          Command{Name: shell, Args: []string{flag, script}},
		WorkingDirectory: root, WorkspaceRoots: []string{root},
	}
	// A wait of zero: Start returns while it is still running, exactly as it does
	// for a command that outlives its inline wait.
	started, err := manager.Start(context.Background(), ProcessStart{
		Prepared: PreparedCommand{Command: command}, Request: request,
	}, 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Nobody polls. Wait for the process to finish on its own, the way a caller
	// off doing other work would.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, remembered := manager.Completed(started.ProcessID); remembered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now the late poll. It must get the result, not ErrProcessNotFound.
	manager.Remove(started.ProcessID)
	result, err := manager.Continue(context.Background(), ProcessContinue{ProcessID: started.ProcessID})
	if err != nil {
		t.Fatalf("a session nobody polled was refused after it finished: %v", err)
	}
	if !strings.Contains(result.Output, "FINAL-RESULT") {
		t.Errorf("the output was lost, so the command has to be run again:\n%q", result.Output)
	}
	if result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", result.ExitCode)
	}
}

// manualExpiry captures the expiry each completed record schedules so a test can
// fire it at a chosen moment instead of sleeping past a real deadline.
type manualExpiry struct {
	mu     sync.Mutex
	delays []time.Duration
	fires  []func()
}

func installManualExpiry(manager *ProcessManager) *manualExpiry {
	expiry := &manualExpiry{}
	manager.afterFunc = func(delay time.Duration, fire func()) {
		expiry.mu.Lock()
		defer expiry.mu.Unlock()
		expiry.delays = append(expiry.delays, delay)
		expiry.fires = append(expiry.fires, fire)
	}
	return expiry
}

func (expiry *manualExpiry) fire(t *testing.T, index int) {
	t.Helper()
	expiry.mu.Lock()
	if index >= len(expiry.fires) {
		expiry.mu.Unlock()
		t.Fatalf("expiry %d was never scheduled; %d scheduled", index, len(expiry.fires))
	}
	fire := expiry.fires[index]
	expiry.mu.Unlock()
	fire()
}

// RETENTION IS A PROMISE IN BOTH DIRECTIONS. A late poll inside the window gets
// the result; once the window ends the record, with its output, request, and
// enforcement evidence, is gone rather than surviving until 64 newer
// completions happen to push it out.
func TestACompletedRecordExpiresWhenItsRetentionEnds(t *testing.T) {
	const retention = 50 * time.Millisecond
	manager := NewProcessManager(ProcessManagerOptions{CompletedRetention: retention})
	expiry := installManualExpiry(manager)
	manager.remember(ProcessResult{ProcessID: 1001, Exited: true, Output: "FINAL"})

	if len(expiry.delays) != 1 || expiry.delays[0] != retention {
		t.Fatalf("scheduled expiries = %v, want one after the configured %s", expiry.delays, retention)
	}
	before, err := manager.Continue(t.Context(), ProcessContinue{ProcessID: 1001})
	if err != nil || before.Output != "FINAL" {
		t.Fatalf("late poll before expiry = %+v, %v; want the remembered result", before, err)
	}

	expiry.fire(t, 0)
	if _, err := manager.Continue(t.Context(), ProcessContinue{ProcessID: 1001}); err != ErrProcessNotFound {
		t.Fatalf("late poll after expiry returned %v, want ErrProcessNotFound", err)
	}
	if _, ok := manager.Snapshot(1001); ok {
		t.Fatal("snapshot still answers an expired record")
	}
	if len(manager.completed) != 0 || len(manager.completedOrder) != 0 {
		t.Fatalf("expired record left state behind: completed=%d order=%v", len(manager.completed), manager.completedOrder)
	}
}

// An expiry belongs to the publication that scheduled it. A timer for a record
// the count bound already evicted must not remove a later record stored under
// the same id.
func TestAStaleExpiryDoesNotRemoveAReplacementRecord(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{CompletedRetention: time.Minute})
	expiry := installManualExpiry(manager)
	manager.remember(ProcessResult{ProcessID: 1000, Exited: true, Output: "first"})
	for i := 1; i <= maxRememberedCompletions; i++ {
		manager.remember(ProcessResult{ProcessID: 5000 + i, Exited: true})
	}
	if _, ok := manager.Completed(1000); ok {
		t.Fatal("the count bound did not evict the oldest record")
	}
	manager.remember(ProcessResult{ProcessID: 1000, Exited: true, Output: "replacement"})

	expiry.fire(t, 0) // the evicted publication's timer
	got, ok := manager.Completed(1000)
	if !ok || got.Output != "replacement" {
		t.Fatalf("stale expiry removed the replacement record: %+v, %v", got, ok)
	}
}

// Expiry and the count bound are independent: expiring one record frees its
// slot, so the bound does not then evict a record that is still in its window.
func TestExpiryAndTheCountBoundStayIndependent(t *testing.T) {
	manager := NewProcessManager(ProcessManagerOptions{CompletedRetention: time.Minute})
	expiry := installManualExpiry(manager)
	for i := 0; i < maxRememberedCompletions; i++ {
		manager.remember(ProcessResult{ProcessID: 1000 + i, Exited: true})
	}
	expiry.fire(t, 10) // record 1010's own window ends
	if _, ok := manager.Completed(1010); ok {
		t.Fatal("record 1010 survived its expiry")
	}
	manager.remember(ProcessResult{ProcessID: 2000, Exited: true})
	if _, ok := manager.Completed(1000); !ok {
		t.Fatal("the count bound evicted the oldest record although expiry had already freed a slot")
	}
	if got := len(manager.completed); got != maxRememberedCompletions {
		t.Fatalf("kept %d records, want %d", got, maxRememberedCompletions)
	}

	manager.remember(ProcessResult{ProcessID: 2001, Exited: true})
	if _, ok := manager.Completed(1000); ok {
		t.Fatal("the count bound stopped evicting oldest-first after an expiry")
	}
	if _, ok := manager.Completed(2001); !ok {
		t.Fatal("the newest record was dropped")
	}
}
