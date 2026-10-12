package swarm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/testutil"
)

// controllableLauncher records every launched spec and lets a test control each
// member's outcome and (optionally) gate completion until a channel is closed.
type controllableLauncher struct {
	mu        sync.Mutex
	specs     []MemberSpec
	attempts  map[string]int
	gate      chan struct{} // nil => members return immediately
	result    func(spec MemberSpec, attempt int) (MemberResult, error)
	launchErr error
}

func newLauncher(result func(MemberSpec, int) (MemberResult, error)) *controllableLauncher {
	return &controllableLauncher{attempts: map[string]int{}, result: result}
}

func (l *controllableLauncher) Launch(ctx context.Context, spec MemberSpec) (MemberHandle, error) {
	l.mu.Lock()
	if l.launchErr != nil {
		err := l.launchErr
		l.mu.Unlock()
		return nil, err
	}
	l.specs = append(l.specs, spec)
	l.attempts[spec.ID]++
	attempt := l.attempts[spec.ID]
	gate := l.gate
	result := l.result
	l.mu.Unlock()

	h := &funcHandle{id: spec.ID, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				h.err = ctx.Err()
				return
			}
		}
		if result != nil {
			h.res, h.err = result(spec, attempt)
		}
	}()
	return h, nil
}

func (l *controllableLauncher) recorded() []MemberSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]MemberSpec, len(l.specs))
	copy(out, l.specs)
	return out
}

func (l *controllableLauncher) attemptCount(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attempts[id]
}

func newSwarmFor(t *testing.T, l MemberLauncher) *Swarm {
	return newSwarmForWithSize(t, l, 2)
}

func newSwarmForWithSize(t *testing.T, l MemberLauncher, maxTeamSize int) *Swarm {
	t.Helper()
	sw, err := New(Options{BaseDir: t.TempDir(), Launcher: l, MaxTeamSize: maxTeamSize})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(sw.Close)
	return sw
}

func okFor(spec MemberSpec, _ int) (MemberResult, error) {
	return MemberResult{Result: "ok:" + spec.Task, SessionID: "sess-" + spec.ID}, nil
}

func TestSpawnCompletes(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	id, err := sw.Spawn(Policy{Model: "m"}, "team", "teammate", "build widget", "/work")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	testutil.WaitFor(t, "task done", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusDone
	})
	task, _ := sw.Coordinator().Get(id)
	if task.Result != "ok:build widget" {
		t.Fatalf("result = %q", task.Result)
	}
}

func TestSpawnInheritsPolicy(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	_, err := sw.Spawn(Policy{Model: "orch-model", PermissionMode: permissionModeAuto}, "team", "teammate", "task", "/cwd")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	testutil.WaitFor(t, "spec recorded", func() bool { return len(l.recorded()) == 1 })
	spec := l.recorded()[0]
	if spec.Model != "orch-model" {
		t.Fatalf("member model = %q, want inherited orch-model", spec.Model)
	}
	if spec.PermissionMode != permissionModeAuto {
		t.Fatalf("member permission mode = %q, want inherited auto", spec.PermissionMode)
	}
	if spec.Cwd != "/cwd" {
		t.Fatalf("member cwd = %q, want /cwd", spec.Cwd)
	}
	if spec.SystemPrompt == "" {
		t.Fatal("member should carry a resolved system prompt")
	}
}

func TestConcurrencyCapAndQueueDrains(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate
	sw := newSwarmFor(t, l) // MaxTeamSize 2
	pol := Policy{Model: "m"}
	for i := 0; i < 5; i++ {
		if _, err := sw.Spawn(pol, "team", "teammate", "task", ""); err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
	}
	team := sw.team("team")
	if team.Running() != 2 || team.QueueDepth() != 3 {
		t.Fatalf("cap not enforced: running=%d queue=%d, want 2/3", team.Running(), team.QueueDepth())
	}
	// Release everyone; the queue should drain one-per-slot until all are done.
	close(gate)
	testutil.WaitFor(t, "all tasks done", func() bool { return sw.Coordinator().Summarize().Done == 5 })
	if team.Running() != 0 || team.QueueDepth() != 0 {
		t.Fatalf("after drain running=%d queue=%d, want 0/0", team.Running(), team.QueueDepth())
	}
	if got := len(l.recorded()); got != 5 {
		t.Fatalf("launched %d members, want 5", got)
	}
}

func TestRetryOnTemporaryError(t *testing.T) {
	l := newLauncher(func(spec MemberSpec, attempt int) (MemberResult, error) {
		if attempt < 3 {
			return MemberResult{}, ErrMemberTemporary
		}
		return MemberResult{Result: "recovered"}, nil
	})
	sw := newSwarmFor(t, l)
	id, _ := sw.Spawn(Policy{}, "team", "teammate", "task", "")
	testutil.WaitFor(t, "task recovered", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusDone
	})
	if got := l.attemptCount(id); got != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 retries)", got)
	}
	task, _ := sw.Coordinator().Get(id)
	if task.Result != "recovered" {
		t.Fatalf("result = %q, want recovered", task.Result)
	}
}

func TestRetryExhaustionFails(t *testing.T) {
	l := newLauncher(func(MemberSpec, int) (MemberResult, error) {
		return MemberResult{}, ErrMemberTemporary
	})
	sw := newSwarmFor(t, l)
	id, _ := sw.Spawn(Policy{}, "team", "teammate", "task", "")
	testutil.WaitFor(t, "task failed", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusFailed
	})
	if got := l.attemptCount(id); got != maxMemberRestarts+1 {
		t.Fatalf("attempts = %d, want %d", got, maxMemberRestarts+1)
	}
}

func TestPermanentErrorNoRetry(t *testing.T) {
	l := newLauncher(func(MemberSpec, int) (MemberResult, error) {
		return MemberResult{}, errPlain("hard failure")
	})
	sw := newSwarmFor(t, l)
	id, _ := sw.Spawn(Policy{}, "team", "teammate", "task", "")
	testutil.WaitFor(t, "task failed", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusFailed
	})
	if got := l.attemptCount(id); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on permanent error)", got)
	}
	task, _ := sw.Coordinator().Get(id)
	if task.Err == "" {
		t.Fatal("failed task should record an error message")
	}
}

func TestLifecycleAdmissionRejectedAfterClose(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	sw.Close()

	if _, err := sw.Spawn(Policy{}, "team", "teammate", "task", ""); !errors.Is(err, ErrSwarmClosed) {
		t.Fatalf("Spawn after Close error = %v, want ErrSwarmClosed", err)
	}
	if _, err := sw.Handoff(Policy{}, "team", "task", "teammate", "note"); !errors.Is(err, ErrSwarmClosed) {
		t.Fatalf("Handoff after Close error = %v, want ErrSwarmClosed", err)
	}
	if _, err := sw.AdoptOrphans(Policy{}, "team", "teammate"); !errors.Is(err, ErrSwarmClosed) {
		t.Fatalf("AdoptOrphans after Close error = %v, want ErrSwarmClosed", err)
	}
	if got := len(l.recorded()); got != 0 {
		t.Fatalf("launches after Close = %d, want 0", got)
	}
}

func TestCloseDoesNotLaunchQueuedMembers(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate
	sw := newSwarmFor(t, l)

	var ids []string
	for i := 0; i < 3; i++ {
		id, err := sw.Spawn(Policy{}, "team", "teammate", "task", "")
		if err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if got := len(l.recorded()); got != 2 {
		t.Fatalf("initial launches = %d, want 2 with one queued", got)
	}

	sw.Close()
	if got := len(l.recorded()); got != 2 {
		t.Fatalf("launches after Close = %d, want queued member not launched", got)
	}
	for _, id := range ids {
		task, ok := sw.Coordinator().Get(id)
		if !ok || !task.Status.terminal() {
			t.Fatalf("task %s after Close = %+v, want terminal", id, task)
		}
	}
}

func TestClosePreventsMemberRetry(t *testing.T) {
	started := make(chan struct{}, maxMemberRestarts+1)
	release := make(chan struct{})
	l := FuncLauncher{Run: func(context.Context, MemberSpec) (MemberResult, error) {
		started <- struct{}{}
		<-release
		return MemberResult{}, ErrMemberTemporary
	}}
	sw := newSwarmFor(t, l)
	_, err := sw.Spawn(Policy{}, "team", "teammate", "task", "")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("initial member did not start")
	}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	testutil.WaitFor(t, "swarm closed state", func() bool {
		sw.lifecycleMu.RLock()
		defer sw.lifecycleMu.RUnlock()
		return sw.closed
	})
	close(release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after retryable member exit")
	}
	select {
	case <-started:
		t.Fatal("member retried after Close")
	default:
	}
}

func TestCloseWaitsForMemberWatchers(t *testing.T) {
	release := make(chan struct{})
	l := FuncLauncher{Run: func(context.Context, MemberSpec) (MemberResult, error) {
		<-release
		return MemberResult{Result: "done"}, nil
	}}
	sw := newSwarmFor(t, l)
	id, err := sw.Spawn(Policy{}, "team", "teammate", "task", "")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	testutil.WaitFor(t, "task running", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusRunning
	})

	const callers = 3
	closing := make(chan struct{}, callers)
	closed := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		go func() {
			closing <- struct{}{}
			sw.Close()
			closed <- struct{}{}
		}()
	}
	for i := 0; i < callers; i++ {
		<-closing
	}
	select {
	case <-closed:
		t.Fatal("a Close caller returned before the member watcher exited")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < callers; i++ {
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("all Close callers did not return after the member watcher exited")
		}
	}
}

func TestHandoffDeliversNoteAndRetiresOriginal(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate // keep the original member running so it is non-terminal
	sw := newSwarmFor(t, l)
	pol := Policy{Model: "m"}
	origID, _ := sw.Spawn(pol, "team", "teammate", "original task", "/w")
	testutil.WaitFor(t, "original running", func() bool {
		task, ok := sw.Coordinator().Get(origID)
		return ok && task.Status == StatusRunning
	})

	newID, err := sw.Handoff(pol, "team", origID, "subagent", "please continue")
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	// Original retired.
	orig, _ := sw.Coordinator().Get(origID)
	if orig.Status != StatusHandedOff {
		t.Fatalf("original status = %v, want handed-off", orig.Status)
	}
	// Note delivered to the new member's inbox.
	msgs, err := sw.Mailbox().ReadAndConsume("team", newID)
	if err != nil {
		t.Fatalf("read new inbox: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "please continue" || msgs[0].Type != "handoff" {
		t.Fatalf("handoff note not delivered: %+v", msgs)
	}
	// The new member carries the handoff note in its task and preserves cwd.
	close(gate)
	testutil.WaitFor(t, "spec for new member", func() bool {
		for _, s := range l.recorded() {
			if s.ID == newID {
				return true
			}
		}
		return false
	})
	for _, s := range l.recorded() {
		if s.ID == newID {
			if s.Cwd != "/w" {
				t.Fatalf("handoff lost cwd: %q", s.Cwd)
			}
		}
	}
	// A handoff of an already-terminal task is rejected.
	testutil.WaitFor(t, "new task done", func() bool {
		task, ok := sw.Coordinator().Get(newID)
		return ok && task.Status == StatusDone
	})
	if _, err := sw.Handoff(pol, "team", newID, "teammate", "again"); err == nil {
		t.Fatal("handoff of a terminal task must fail")
	}
}

func TestHandoffStopsOriginalBeforeSuccessorStarts(t *testing.T) {
	originalStarted := make(chan struct{})
	originalStopped := make(chan struct{})
	successorStarted := make(chan struct{})
	var successorOverlapped atomic.Bool

	launcher := FuncLauncher{Run: func(ctx context.Context, spec MemberSpec) (MemberResult, error) {
		if spec.AgentType == "teammate" {
			close(originalStarted)
			<-ctx.Done()
			close(originalStopped)
			return MemberResult{}, ctx.Err()
		}
		select {
		case <-originalStopped:
		default:
			successorOverlapped.Store(true)
		}
		close(successorStarted)
		return MemberResult{Result: "continued"}, nil
	}}
	sw := newSwarmFor(t, launcher)
	pol := Policy{Model: "m"}
	origID, err := sw.Spawn(pol, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-originalStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("original member never started")
	}

	if _, err := sw.Handoff(pol, "team", origID, "subagent", "continue safely"); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	select {
	case <-successorStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("successor member never started")
	}
	if successorOverlapped.Load() {
		t.Fatal("successor started before the original member stopped")
	}
}

func TestHandoffMailboxFailureRestoresSourceCompletion(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate
	sw := newSwarmFor(t, l)
	pol := Policy{Model: "m"}
	origID, err := sw.Spawn(pol, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	testutil.WaitFor(t, "original running", func() bool {
		task, ok := sw.Coordinator().Get(origID)
		return ok && task.Status == StatusRunning
	})

	// The successor id is deterministic: the original consumed sequence 1.
	sw.Mailbox().MaxMessages = 1
	if err := sw.Mailbox().Send("team", "subagent-2", Message{From: "test", Body: "occupy inbox"}); err != nil {
		t.Fatalf("prefill successor inbox: %v", err)
	}
	if _, err := sw.Handoff(pol, "team", origID, "subagent", "continue safely"); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("Handoff error = %v, want ErrMailboxFull", err)
	}

	close(gate)
	testutil.WaitFor(t, "source completion after aborted handoff", func() bool {
		task, ok := sw.Coordinator().Get(origID)
		return ok && task.Status == StatusDone
	})
}

func TestHandoffFailsClosedWithoutLocalTaskRun(t *testing.T) {
	coord := NewCoordinator()
	if _, err := coord.Register("external-task", "external-agent", "team", "external work"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := coord.SetStatus("external-task", StatusRunning); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	l := newLauncher(okFor)
	sw, err := New(Options{BaseDir: t.TempDir(), Launcher: l, Coordinator: coord})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(sw.Close)

	if _, err := sw.Handoff(Policy{Model: "m"}, "team", "external-task", "subagent", ""); err == nil {
		t.Fatal("Handoff without a local task run must fail")
	}
	if err := coord.Complete("external-task", "external owner finished"); err != nil {
		t.Fatalf("handoff claim was not aborted: %v", err)
	}
	if got := len(l.recorded()); got != 0 {
		t.Fatalf("unexpected successor launches = %d", got)
	}
}

func TestHandoffSuccessorCollisionDoesNotStopSource(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate
	coord := NewCoordinator()
	sw, err := New(Options{BaseDir: t.TempDir(), Launcher: l, Coordinator: coord, MaxTeamSize: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(sw.Close)
	pol := Policy{Model: "m"}
	origID, err := sw.Spawn(pol, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	testutil.WaitFor(t, "original running", func() bool {
		task, ok := coord.Get(origID)
		return ok && task.Status == StatusRunning
	})
	// Handoff will mint subagent-2. A shared coordinator can already contain it
	// because each Swarm has an independent id sequence.
	if _, err := coord.Register("subagent-2", "other-agent", "other-team", "existing task"); err != nil {
		t.Fatalf("register collision: %v", err)
	}

	if _, err := sw.Handoff(pol, "team", origID, "subagent", "do not misdeliver"); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("Handoff error = %v, want ErrTaskExists", err)
	}
	task, _ := coord.Get(origID)
	if task.Status != StatusRunning {
		t.Fatalf("source status = %s, want running because collision was detected before stop", task.Status)
	}
	msgs, err := sw.Mailbox().ReadAndConsume("team", "subagent-2")
	if err != nil {
		t.Fatalf("read colliding inbox: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("collision misdelivered %d handoff notes to the existing task", len(msgs))
	}
	close(gate)
	testutil.WaitFor(t, "source completion after rejected collision", func() bool {
		task, ok := coord.Get(origID)
		return ok && task.Status == StatusDone
	})
}

func TestCloseReleasesBlockedHandoffBeforeMemberExit(t *testing.T) {
	releaseOriginal := make(chan struct{})
	originalStarted := make(chan struct{})
	originalCanceled := make(chan struct{})
	successorStarted := make(chan struct{}, 1)
	launcher := FuncLauncher{Run: func(ctx context.Context, spec MemberSpec) (MemberResult, error) {
		if spec.AgentType == "teammate" {
			close(originalStarted)
			go func() {
				<-ctx.Done()
				close(originalCanceled)
			}()
			<-releaseOriginal
			return MemberResult{Result: "source finished"}, nil
		}
		successorStarted <- struct{}{}
		return MemberResult{Result: "successor finished"}, nil
	}}
	sw, err := New(Options{BaseDir: t.TempDir(), Launcher: launcher})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(sw.Close)
	defer func() {
		select {
		case <-releaseOriginal:
		default:
			close(releaseOriginal)
		}
	}()
	origID, err := sw.Spawn(Policy{Model: "m"}, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-originalStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("original member never started")
	}

	handoffDone := make(chan error, 1)
	go func() {
		_, err := sw.Handoff(Policy{Model: "m"}, "team", origID, "subagent", "")
		handoffDone <- err
	}()
	select {
	case <-originalCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Handoff never canceled the source run")
	}
	closeDone := make(chan struct{})
	go func() {
		sw.Close()
		close(closeDone)
	}()
	select {
	case err := <-handoffDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Handoff error = %v, want Close cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Handoff stayed blocked after Close canceled the swarm")
	}
	select {
	case <-successorStarted:
		t.Fatal("successor started while the source member was still running")
	default:
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned before the cancellation-insensitive member exited")
	default:
	}

	close(releaseOriginal)
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the source member exited")
	}
	testutil.WaitFor(t, "source completion after handoff cancellation", func() bool {
		task, ok := sw.Coordinator().Get(origID)
		return ok && task.Status == StatusDone
	})
}

func TestHandoffTimesOutWithoutStartingSuccessor(t *testing.T) {
	releaseOriginal := make(chan struct{})
	originalStarted := make(chan struct{})
	successorStarted := make(chan struct{}, 1)
	launcher := FuncLauncher{Run: func(_ context.Context, spec MemberSpec) (MemberResult, error) {
		if spec.AgentType == "teammate" {
			close(originalStarted)
			<-releaseOriginal
			return MemberResult{Result: "source finished"}, nil
		}
		successorStarted <- struct{}{}
		return MemberResult{Result: "successor finished"}, nil
	}}
	sw := newSwarmFor(t, launcher)
	sw.handoffStopTimeout = 50 * time.Millisecond
	defer func() {
		select {
		case <-releaseOriginal:
		default:
			close(releaseOriginal)
		}
	}()
	origID, err := sw.Spawn(Policy{Model: "m"}, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-originalStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("original member never started")
	}

	handoffDone := make(chan error, 1)
	go func() {
		_, err := sw.Handoff(Policy{Model: "m"}, "team", origID, "subagent", "")
		handoffDone <- err
	}()
	select {
	case err := <-handoffDone:
		if !errors.Is(err, ErrHandoffStopTimeout) {
			t.Fatalf("Handoff error = %v, want ErrHandoffStopTimeout", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Handoff exceeded its source-stop deadline")
	}
	select {
	case <-successorStarted:
		t.Fatal("successor started after the source-stop deadline expired")
	default:
	}
	if task, ok := sw.Coordinator().Get(origID); !ok || task.Status != StatusRunning {
		t.Fatalf("source status after timeout = %+v, want running until its member exits", task)
	}

	close(releaseOriginal)
	testutil.WaitFor(t, "source completion after handoff timeout", func() bool {
		task, ok := sw.Coordinator().Get(origID)
		return ok && task.Status == StatusDone
	})
}

func TestHandoffWaitsForOriginalThatIgnoresCancellation(t *testing.T) {
	originalStarted := make(chan struct{})
	releaseOriginal := make(chan struct{})
	originalStopped := make(chan struct{})
	successorStarted := make(chan struct{})
	defer func() {
		select {
		case <-releaseOriginal:
		default:
			close(releaseOriginal)
		}
	}()

	launcher := FuncLauncher{Run: func(_ context.Context, spec MemberSpec) (MemberResult, error) {
		if spec.AgentType == "teammate" {
			close(originalStarted)
			<-releaseOriginal
			close(originalStopped)
			return MemberResult{}, nil
		}
		close(successorStarted)
		return MemberResult{Result: "continued"}, nil
	}}
	sw := newSwarmFor(t, launcher)
	pol := Policy{Model: "m"}
	origID, err := sw.Spawn(pol, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-originalStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("original member never started")
	}

	handoffDone := make(chan error, 1)
	go func() {
		_, err := sw.Handoff(pol, "team", origID, "subagent", "continue safely")
		handoffDone <- err
	}()
	select {
	case err := <-handoffDone:
		t.Fatalf("Handoff returned before the original stopped: %v", err)
	case <-successorStarted:
		t.Fatal("successor started while the original was still running")
	case <-time.After(100 * time.Millisecond):
	}
	if task, ok := sw.Coordinator().Get(origID); !ok || task.Status != StatusRunning {
		t.Fatalf("original status while it is still running = %+v, want running", task)
	}

	close(releaseOriginal)
	select {
	case <-originalStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("original member did not stop")
	}
	select {
	case err := <-handoffDone:
		if err != nil {
			t.Fatalf("Handoff: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Handoff did not return after the original stopped")
	}
	select {
	case <-successorStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("successor member never started")
	}
}

func TestHandoffDoesNotWaitForUnrelatedQueuedLaunch(t *testing.T) {
	originalStarted := make(chan struct{})
	queuedLaunchStarted := make(chan struct{})
	releaseQueuedLaunch := make(chan struct{})
	defer close(releaseQueuedLaunch)

	launcher := &handoffQueueLauncher{
		originalStarted:     originalStarted,
		queuedLaunchStarted: queuedLaunchStarted,
		releaseQueuedLaunch: releaseQueuedLaunch,
	}
	sw := newSwarmForWithSize(t, launcher, 1)
	pol := Policy{Model: "m"}
	origID, err := sw.Spawn(pol, "team", "teammate", "original task", "/w")
	if err != nil {
		t.Fatalf("Spawn original: %v", err)
	}
	select {
	case <-originalStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("original member never started")
	}
	if _, err := sw.Spawn(pol, "team", "teammate", "queued task", "/w"); err != nil {
		t.Fatalf("Spawn queued: %v", err)
	}
	if got := sw.team("team").QueueDepth(); got != 1 {
		t.Fatalf("queue depth = %d, want one queued task before handoff", got)
	}

	handoffDone := make(chan error, 1)
	go func() {
		_, err := sw.Handoff(pol, "team", origID, "subagent", "continue safely")
		handoffDone <- err
	}()
	select {
	case <-queuedLaunchStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("queued launch never started")
	}
	select {
	case err := <-handoffDone:
		if err != nil {
			t.Fatalf("Handoff: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Handoff waited for an unrelated queued launch")
	}
}

type handoffQueueLauncher struct {
	originalStarted     chan struct{}
	queuedLaunchStarted chan struct{}
	releaseQueuedLaunch chan struct{}
}

func (l *handoffQueueLauncher) Launch(ctx context.Context, spec MemberSpec) (MemberHandle, error) {
	switch spec.Task {
	case "original task":
		close(l.originalStarted)
		h := &funcHandle{id: spec.ID, done: make(chan struct{})}
		go func() {
			defer close(h.done)
			<-ctx.Done()
			h.err = ctx.Err()
		}()
		return h, nil
	case "queued task":
		close(l.queuedLaunchStarted)
		<-l.releaseQueuedLaunch
	}
	return &funcHandle{
		id:   spec.ID,
		done: closedChan(),
		res:  MemberResult{Result: "continued"},
	}, nil
}

func TestHandoffRemovesQueuedOriginalBeforeDispatchingSuccessor(t *testing.T) {
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	l := newLauncher(okFor)
	l.gate = gate
	sw := newSwarmForWithSize(t, l, 1)
	pol := Policy{Model: "m"}

	if _, err := sw.Spawn(pol, "team", "teammate", "slot blocker", "/w"); err != nil {
		t.Fatalf("Spawn blocker: %v", err)
	}
	testutil.WaitFor(t, "blocker running", func() bool { return len(l.recorded()) == 1 })
	origID, err := sw.Spawn(pol, "team", "teammate", "queued original", "/w")
	if err != nil {
		t.Fatalf("Spawn queued original: %v", err)
	}
	if got := sw.team("team").QueueDepth(); got != 1 {
		t.Fatalf("queue depth = %d, want original queued", got)
	}

	newID, err := sw.Handoff(pol, "team", origID, "subagent", "continue safely")
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	close(gate)
	testutil.WaitFor(t, "successor launch", func() bool {
		for _, spec := range l.recorded() {
			if spec.ID == newID {
				return true
			}
		}
		return false
	})
	for _, spec := range l.recorded() {
		if spec.ID == origID {
			t.Fatal("queued original launched after it was handed off")
		}
	}
}

func TestAdoptOrphans(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	// Simulate a crashed member: a running task in the coordinator whose owning
	// agent has no live member in the team.
	if _, err := sw.Coordinator().Register("orphan-1", "ghost", "team", "stranded work"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = sw.Coordinator().SetStatus("orphan-1", StatusRunning)

	adopted, err := sw.AdoptOrphans(Policy{Model: "m"}, "team", "subagent")
	if err != nil {
		t.Fatalf("AdoptOrphans: %v", err)
	}
	if len(adopted) != 1 || adopted[0] != "orphan-1" {
		t.Fatalf("adopted = %v, want [orphan-1]", adopted)
	}
	// The orphan is relaunched under a fresh agent and completes.
	testutil.WaitFor(t, "orphan completed", func() bool {
		task, ok := sw.Coordinator().Get("orphan-1")
		return ok && task.Status == StatusDone
	})
	task, _ := sw.Coordinator().Get("orphan-1")
	if task.AgentID == "ghost" {
		t.Fatal("orphan should be reassigned to a new agent")
	}
	// A second adoption finds nothing (the task now has a live/owned outcome).
	again, _ := sw.AdoptOrphans(Policy{}, "team", "subagent")
	if len(again) != 0 {
		t.Fatalf("second adoption = %v, want none", again)
	}
}

func TestLiveAgentsIncludesQueuedSpecs(t *testing.T) {
	// H1 regression: a spec queued over the concurrency cap is owned and about to
	// launch, so it must count as a live agent. Otherwise AdoptOrphans sees its
	// task as orphaned and re-dispatches it — double-executing the same task.
	team := &Team{Name: "t", members: map[string]*Member{}, maxSize: 1}
	if !team.admit(MemberSpec{ID: "a1", TaskID: "t1"}) {
		t.Fatal("first spec should take the only slot and launch immediately")
	}
	team.addMember(&Member{ID: "a1"})
	if team.admit(MemberSpec{ID: "a2", TaskID: "t2"}) {
		t.Fatal("second spec is over the cap and should queue, not launch")
	}
	live := team.liveAgents()
	if _, ok := live["a1"]; !ok {
		t.Error("running member a1 missing from liveAgents")
	}
	if _, ok := live["a2"]; !ok {
		t.Error("queued spec a2 missing from liveAgents — would be double-dispatched by AdoptOrphans")
	}
}

func TestCollectScopesToTeam(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	a, _ := sw.Spawn(Policy{}, "alpha", "teammate", "ta", "")
	_, _ = sw.Spawn(Policy{}, "beta", "teammate", "tb", "")
	testutil.WaitFor(t, "alpha done", func() bool {
		task, ok := sw.Coordinator().Get(a)
		return ok && task.Status == StatusDone
	})
	collected := sw.Collect("alpha")
	if len(collected) != 1 || collected[0].Team != "alpha" {
		t.Fatalf("Collect(alpha) = %+v, want one alpha task", collected)
	}
}

func TestFuncLauncherRecoversPanic(t *testing.T) {
	// A panic inside a member's Run must surface as that member's error, never
	// escape the goroutine and crash the orchestrator.
	l := FuncLauncher{Run: func(context.Context, MemberSpec) (MemberResult, error) {
		panic("boom in member")
	}}
	h, err := l.Launch(context.Background(), MemberSpec{ID: "m1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	_, err = h.Wait()
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic must surface as a member error, got %v", err)
	}
}

func TestSpawnUnknownAgentType(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	if _, err := sw.Spawn(Policy{}, "team", "does-not-exist", "task", ""); err == nil {
		t.Fatal("Spawn with unknown agent type must error")
	}
}

// blockingLauncher blocks inside Launch until its context is cancelled — the
// shape of a real launcher that waits on a slot, a daemon connection, or a
// sandbox handshake before it can return a handle.
type blockingLauncher struct {
	entered chan struct{}
	once    sync.Once
}

func (l *blockingLauncher) Launch(ctx context.Context, spec MemberSpec) (MemberHandle, error) {
	l.once.Do(func() { close(l.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCloseCancelsLauncherBlockedInLaunch is the regression test for holding
// lifecycle admission across MemberLauncher.Launch. When admission was a read lock
// held for the caller's whole duration, this deadlocked: the spawn sat inside
// Launch waiting for its context, while Close waited for the write lock it needed
// before it could cancel that very context. Neither side could progress, so both
// the spawn and every Close caller hung.
//
// Admission is now a counted ticket, so Close can flip the flag, cancel, and then
// wait the ticket out.
func TestCloseCancelsLauncherBlockedInLaunch(t *testing.T) {
	launcher := &blockingLauncher{entered: make(chan struct{})}
	sw := newSwarmFor(t, launcher)

	spawned := make(chan error, 1)
	go func() {
		_, err := sw.Spawn(Policy{}, "team", "teammate", "task", "")
		spawned <- err
	}()
	select {
	case <-launcher.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("launcher never entered Launch")
	}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked against a launcher blocked in Launch")
	}
	select {
	case <-spawned:
	case <-time.After(3 * time.Second):
		t.Fatal("Spawn never returned after Close cancelled the launch context")
	}
}

// TestCloseWaitsForAdmittedSpawn pins the other half of the contract: Close is a
// barrier, so it must not return while an admitted spawn is still running. Without
// the lifecycleWork wait, Close could finish while this launch was mid-flight.
func TestCloseWaitsForAdmittedSpawn(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	launcher := &gatedLaunchLauncher{entered: entered, finish: finish}
	sw := newSwarmFor(t, launcher)

	go func() {
		_, _ = sw.Spawn(Policy{}, "team", "teammate", "task", "")
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("launcher never entered Launch")
	}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while an admitted spawn was still in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(finish)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the admitted spawn finished")
	}
}

// TestAdmittedDispatchDoesNotLaunchAfterClose covers an operation that won a
// lifecycle ticket before shutdown but did not reach dispatch until after Close
// set closed. The ticket keeps Close from returning early, but it must not permit
// a new external launch with the already-cancelled swarm context.
func TestAdmittedDispatchDoesNotLaunchAfterClose(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	sw.maxTeamSize = 1

	if _, err := sw.coord.Register("late-task", "late-agent", "team", "late work"); err != nil {
		t.Fatalf("Register late task: %v", err)
	}
	release, err := sw.beginLifecycleAdmission()
	if err != nil {
		t.Fatalf("beginLifecycleAdmission: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	testutil.WaitFor(t, "swarm closed state", func() bool {
		sw.lifecycleMu.RLock()
		defer sw.lifecycleMu.RUnlock()
		return sw.closed
	})

	sw.dispatchAdmitted(MemberSpec{
		ID: "late-agent", TaskID: "late-task", AgentType: "teammate", Team: "team",
	})
	release()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the admitted dispatch finished")
	}
	if got := len(l.recorded()); got != 0 {
		t.Fatalf("launches after closed was set = %d, want 0", got)
	}
	team := sw.team("team")
	if got := team.Running(); got != 0 {
		t.Fatalf("team running after rejected launch = %d, want 0", got)
	}
	task, ok := sw.Coordinator().Get("late-task")
	if !ok {
		t.Fatal("late task vanished")
	}
	if task.Status != StatusFailed || !strings.Contains(task.Err, ErrSwarmClosed.Error()) {
		t.Fatalf("late task after Close = %+v, want failed with ErrSwarmClosed", task)
	}
}

// TestCloseBetweenLaunchPrecheckAndReturn rejects and reaps a handle created by
// a Launch that straddles shutdown. The launch has passed the pre-check when it
// enters the gate; Close then sets closed and cancels its context before the
// launcher is allowed to return successfully.
func TestCloseBetweenLaunchPrecheckAndReturn(t *testing.T) {
	launcher := &closeRaceLauncher{
		entered: make(chan struct{}),
		finish:  make(chan struct{}),
		waited:  make(chan struct{}),
	}
	sw := newSwarmFor(t, launcher)

	spawned := make(chan string, 1)
	go func() {
		id, _ := sw.Spawn(Policy{}, "team", "teammate", "task", "")
		spawned <- id
	}()
	select {
	case <-launcher.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("launcher never entered Launch")
	}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	testutil.WaitFor(t, "swarm closed state", func() bool {
		sw.lifecycleMu.RLock()
		defer sw.lifecycleMu.RUnlock()
		return sw.closed
	})
	close(launcher.finish)

	var id string
	select {
	case id = <-spawned:
	case <-time.After(3 * time.Second):
		t.Fatal("Spawn did not return after Launch")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after late handle was reaped")
	}
	select {
	case <-launcher.waited:
	default:
		t.Fatal("late handle was not reaped")
	}

	team := sw.team("team")
	if got := team.Running(); got != 0 {
		t.Fatalf("team running after close-raced Launch = %d, want 0", got)
	}
	if got := len(team.liveAgents()); got != 0 {
		t.Fatalf("registered members after close-raced Launch = %d, want 0", got)
	}
	task, ok := sw.Coordinator().Get(id)
	if !ok {
		t.Fatal("task vanished")
	}
	if task.Status != StatusFailed || !strings.Contains(task.Err, ErrSwarmClosed.Error()) {
		t.Fatalf("task after close-raced Launch = %+v, want failed with ErrSwarmClosed", task)
	}
}

type closeRaceLauncher struct {
	entered chan struct{}
	finish  chan struct{}
	waited  chan struct{}
	once    sync.Once
}

func (l *closeRaceLauncher) Launch(ctx context.Context, spec MemberSpec) (MemberHandle, error) {
	l.once.Do(func() { close(l.entered) })
	<-l.finish
	return &closeRaceHandle{id: spec.ID, ctx: ctx, waited: l.waited}, nil
}

type closeRaceHandle struct {
	id     string
	ctx    context.Context
	waited chan struct{}
	once   sync.Once
}

func (h *closeRaceHandle) ID() string { return h.id }

func (h *closeRaceHandle) Wait() (MemberResult, error) {
	<-h.ctx.Done()
	h.once.Do(func() { close(h.waited) })
	return MemberResult{}, h.ctx.Err()
}

// TestCloseBetweenRetryLaunchPrecheckAndReturn is the retry counterpart of
// TestCloseBetweenLaunchPrecheckAndReturn: a bounded relaunch wins admission and
// passes the pre-check, then Close lands while the launcher is inside Launch. The
// relaunched handle must be reaped rather than adopted, so the watcher never
// resumes supervising a member started after shutdown began — a relaunch whose
// member goes on to succeed must not be recorded as the task's outcome.
func TestCloseBetweenRetryLaunchPrecheckAndReturn(t *testing.T) {
	launcher := &retryCloseRaceLauncher{
		entered: make(chan struct{}),
		finish:  make(chan struct{}),
		late:    &retryLateHandle{id: "teammate-1"},
	}
	sw := newSwarmFor(t, launcher)

	id, err := sw.Spawn(Policy{}, "team", "teammate", "task", "")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// The first member exits retryable, so the watcher is now inside the relaunch.
	select {
	case <-launcher.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("launcher never entered the relaunch Launch")
	}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	testutil.WaitFor(t, "swarm closed state", func() bool {
		sw.lifecycleMu.RLock()
		defer sw.lifecycleMu.RUnlock()
		return sw.closed
	})
	close(launcher.finish)

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the late relaunch handle was reaped")
	}
	if got := launcher.launchCount(); got != 2 {
		t.Fatalf("launch attempts = %d, want 2 (initial + one relaunch)", got)
	}
	if got := launcher.late.waitCount(); got != 1 {
		t.Fatalf("late relaunch handle waits = %d, want 1 (reaped, not supervised)", got)
	}

	team := sw.team("team")
	if got := team.Running(); got != 0 {
		t.Fatalf("team running after close-raced relaunch = %d, want 0", got)
	}
	if got := len(team.liveAgents()); got != 0 {
		t.Fatalf("registered members after close-raced relaunch = %d, want 0", got)
	}
	task, ok := sw.Coordinator().Get(id)
	if !ok {
		t.Fatal("task vanished")
	}
	if task.Status != StatusFailed {
		t.Fatalf("task after close-raced relaunch = %+v, want failed, not the late member's outcome", task)
	}
	if task.Result != "" {
		t.Fatalf("task result after close-raced relaunch = %q, want the late member's result discarded", task.Result)
	}
}

// retryCloseRaceLauncher fails its first member with a retryable error, then
// blocks inside the relaunch's Launch until the test releases it.
type retryCloseRaceLauncher struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	finish  chan struct{}
	late    *retryLateHandle
	once    sync.Once
}

func (l *retryCloseRaceLauncher) Launch(_ context.Context, spec MemberSpec) (MemberHandle, error) {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	if first {
		return &temporaryHandle{id: spec.ID}, nil
	}
	l.once.Do(func() { close(l.entered) })
	<-l.finish
	return l.late, nil
}

func (l *retryCloseRaceLauncher) launchCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// temporaryHandle exits immediately with a retryable error, driving the watcher
// into its bounded relaunch path.
type temporaryHandle struct{ id string }

func (h *temporaryHandle) ID() string { return h.id }

func (h *temporaryHandle) Wait() (MemberResult, error) {
	return MemberResult{}, ErrMemberTemporary
}

// retryLateHandle models a member whose work does not consult the cancelled
// launch context and reports success. Adopting it after shutdown would record a
// post-close result on the task, so a correct shutdown reaps it exactly once and
// discards what it returns.
type retryLateHandle struct {
	id    string
	mu    sync.Mutex
	waits int
}

func (h *retryLateHandle) ID() string { return h.id }

func (h *retryLateHandle) Wait() (MemberResult, error) {
	h.mu.Lock()
	h.waits++
	h.mu.Unlock()
	return MemberResult{Result: "late result", SessionID: "late-session"}, nil
}

func (h *retryLateHandle) waitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waits
}

func TestCloseRacesLifecycleAdmission(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate
	sw := newSwarmFor(t, l)

	const spawns = 64
	start := make(chan struct{})
	results := make(chan struct {
		id  string
		err error
	}, spawns)
	var callers sync.WaitGroup
	callers.Add(spawns)
	for i := 0; i < spawns; i++ {
		go func() {
			defer callers.Done()
			<-start
			id, err := sw.Spawn(Policy{}, "team", "teammate", "racing task", "")
			results <- struct {
				id  string
				err error
			}{id: id, err: err}
		}()
	}
	closed := make(chan struct{})
	go func() {
		<-start
		sw.Close()
		close(closed)
	}()

	close(start)
	callers.Wait()
	<-closed
	close(results)

	for result := range results {
		if result.err != nil {
			if !errors.Is(result.err, ErrSwarmClosed) {
				t.Fatalf("Spawn racing Close error = %v, want ErrSwarmClosed", result.err)
			}
			continue
		}
		task, ok := sw.Coordinator().Get(result.id)
		if !ok || !task.Status.terminal() {
			t.Fatalf("admitted task %q after Close = %+v, want terminal", result.id, task)
		}
	}
	for _, task := range sw.Coordinator().List() {
		if !task.Status.terminal() {
			t.Fatalf("task %q remained %s after Close", task.ID, task.Status)
		}
	}
}

// TestCloseFailsQueuedSpecOnLateCreatedTeam is the regression test for queue
// sweeping after shutdown begins. Spawn/Handoff/AdoptOrphans calls that obtained
// tickets before closed flipped can still create a team and append to its queue.
// Close must wait for those calls before both snapshotting teams and sweeping
// their queues, or a late-created team's queued task remains pending forever.
//
// This drives that interleaving directly: two operations obtain tickets before
// Close, then the first creates and fills a previously unseen one-slot team and
// the second queues behind it after Close has flipped closed.
func TestCloseFailsQueuedSpecOnLateCreatedTeam(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	sw.maxTeamSize = 1

	if _, err := sw.coord.Register("late-task", "late-agent", "team", "late work"); err != nil {
		t.Fatalf("Register late task: %v", err)
	}
	firstRelease, err := sw.beginLifecycleAdmission()
	if err != nil {
		t.Fatalf("first beginLifecycleAdmission: %v", err)
	}
	secondRelease, err := sw.beginLifecycleAdmission()
	if err != nil {
		firstRelease()
		t.Fatalf("second beginLifecycleAdmission: %v", err)
	}
	runningSpec := MemberSpec{ID: "running-agent", TaskID: "running-task", AgentType: "teammate", Team: "team"}
	queuedSpec := MemberSpec{ID: "late-agent", TaskID: "late-task", AgentType: "teammate", Team: "team"}

	closed := make(chan struct{})
	go func() {
		sw.Close()
		close(closed)
	}()
	testutil.WaitFor(t, "swarm closed state", func() bool {
		sw.lifecycleMu.RLock()
		defer sw.lifecycleMu.RUnlock()
		return sw.closed
	})
	select {
	case <-closed:
		t.Fatal("Close returned while an admitted ticket was still open")
	default:
	}

	// Neither operation touched a team before Close took its old, buggy snapshot.
	// Finish the team-admission portion of both operations while their tickets are
	// still held: the first creates the team and fills its only slot; the second
	// appends to that late-created team's queue.
	team := sw.team("team")
	if !team.admit(runningSpec) {
		t.Fatal("first spec should take the late-created team's only slot")
	}
	firstRelease()
	if team.admit(queuedSpec) {
		t.Fatal("second spec should queue on the late-created team")
	}
	secondRelease()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the admitted ticket released")
	}

	if got := team.QueueDepth(); got != 0 {
		t.Fatalf("queue depth after Close = %d, want 0 (late spec swept)", got)
	}
	task, ok := sw.Coordinator().Get("late-task")
	if !ok {
		t.Fatal("late-queued task vanished")
	}
	if task.Status != StatusFailed {
		t.Fatalf("late-queued task status = %v, want %v (failed, not stranded)", task.Status, StatusFailed)
	}
	if !strings.Contains(task.Err, ErrSwarmClosed.Error()) {
		t.Fatalf("late-queued task err = %q, want it to mention %q", task.Err, ErrSwarmClosed.Error())
	}
}

// TestAfterExitAdmittedFailsDequeuedSpecAfterClose is the regression test for
// jatmn's second #776 finding: afterExitAdmitted used to dequeue a queued spec
// (t.onExit) and launch it unconditionally once its own ticket was granted,
// even though Close can flip closed in the gap between that dequeue — which
// makes the spec invisible to Close's clearQueue sweep — and the launch. A
// launcher that performs work before honoring its cancelled context could then
// start a brand new worker after shutdown had already begun.
func TestAfterExitAdmittedFailsDequeuedSpecAfterClose(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)

	if _, err := sw.coord.Register("queued-task", "queued-agent", "team", "queued work"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	spec := MemberSpec{ID: "queued-agent", TaskID: "queued-task", AgentType: "teammate", Team: "team"}

	// Occupy the only slot, then queue spec behind it.
	team := &Team{Name: "team", members: map[string]*Member{}, maxSize: 1}
	if !team.admit(MemberSpec{ID: "running-agent", TaskID: "running-task"}) {
		t.Fatal("first spec should take the only slot")
	}
	if team.admit(spec) {
		t.Fatal("second spec should queue, not launch, over the cap")
	}

	// Simulate shutdown racing in between this call's ticket admission (already
	// granted, hence no beginLifecycleAdmission call here) and its dequeue+launch,
	// exactly like the finding describes: closed is already set by the time
	// afterExitAdmitted's post-dequeue check runs.
	sw.lifecycleMu.Lock()
	sw.closed = true
	sw.lifecycleMu.Unlock()

	sw.afterExitAdmitted(team) // running-agent "exits" -> dequeues spec

	if got := team.Running(); got != 0 {
		t.Fatalf("team running after close-raced drain = %d, want 0 (slot released, not held for a launch)", got)
	}
	task, ok := sw.Coordinator().Get("queued-task")
	if !ok {
		t.Fatal("dequeued task vanished")
	}
	if task.Status != StatusFailed {
		t.Fatalf("dequeued task status = %v, want %v (failed, not launched)", task.Status, StatusFailed)
	}
	if !strings.Contains(task.Err, ErrSwarmClosed.Error()) {
		t.Fatalf("dequeued task err = %q, want it to mention %q", task.Err, ErrSwarmClosed.Error())
	}
	for _, s := range l.recorded() {
		if s.ID == "queued-agent" {
			t.Fatal("a spec dequeued after shutdown began must never launch")
		}
	}
}

// gatedLaunchLauncher blocks in Launch until the test releases it, ignoring
// context cancellation so the test controls exactly when the admitted work ends.
type gatedLaunchLauncher struct {
	entered chan struct{}
	finish  chan struct{}
	once    sync.Once
}

func (l *gatedLaunchLauncher) Launch(_ context.Context, spec MemberSpec) (MemberHandle, error) {
	l.once.Do(func() { close(l.entered) })
	<-l.finish
	return &funcHandle{id: spec.ID, done: closedChan()}, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
