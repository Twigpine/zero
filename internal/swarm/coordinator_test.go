package swarm

import (
	"errors"
	"sync"
	"testing"
)

func TestCoordinatorRegisterAndDuplicate(t *testing.T) {
	c := NewCoordinator()
	if _, err := c.Register("t1", "a1", "team", "desc"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	task, ok := c.Get("t1")
	if !ok || task.Status != StatusPending {
		t.Fatalf("Get after register: ok=%v status=%v", ok, task.Status)
	}
	if _, err := c.Register("t1", "a1", "team", "desc"); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("duplicate Register err = %v, want ErrTaskExists", err)
	}
	if _, err := c.Register("", "a", "team", "d"); err == nil {
		t.Fatal("empty id must error")
	}
}

func TestCompleteWithSessionRecordsSessionID(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("t1", "a1", "team", "desc")
	if err := c.CompleteWithSession("t1", "result", "sess-xyz"); err != nil {
		t.Fatalf("CompleteWithSession: %v", err)
	}
	task, _ := c.Get("t1")
	if task.Status != StatusDone || task.Result != "result" {
		t.Fatalf("unexpected task state: %+v", task)
	}
	if task.SessionID != "sess-xyz" {
		t.Fatalf("SessionID = %q, want sess-xyz", task.SessionID)
	}
}

func TestCoordinatorLifecycleTransitions(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("t1", "a1", "team", "desc")
	if err := c.SetStatus("t1", StatusRunning); err != nil {
		t.Fatalf("SetStatus running: %v", err)
	}
	if err := c.Complete("t1", "result-data"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	task, _ := c.Get("t1")
	if task.Status != StatusDone || task.Result != "result-data" {
		t.Fatalf("after Complete: status=%v result=%q", task.Status, task.Result)
	}
	// Terminal guard: cannot move done -> running, nor complete twice.
	if err := c.SetStatus("t1", StatusRunning); err == nil {
		t.Fatal("moving a done task to running must fail")
	}
	if err := c.Complete("t1", "x"); err == nil {
		t.Fatal("completing a done task must fail")
	}
	if err := c.Fail("t1", "boom"); err == nil {
		t.Fatal("failing a done task must fail")
	}
}

func TestCoordinatorRejectsInvalidStatus(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("t1", "a1", "team", "desc")
	if err := c.SetStatus("t1", TaskStatus("bogus")); err == nil {
		t.Fatal("SetStatus must reject an unknown status")
	}
	// The task stays in its prior (valid) state.
	if task, _ := c.Get("t1"); task.Status != StatusPending {
		t.Fatalf("rejected status must not mutate the task, got %v", task.Status)
	}
}

func TestCoordinatorUnknownTask(t *testing.T) {
	c := NewCoordinator()
	if err := c.SetStatus("missing", StatusRunning); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("SetStatus(missing) = %v, want ErrUnknownTask", err)
	}
	if err := c.Complete("missing", "x"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("Complete(missing) = %v, want ErrUnknownTask", err)
	}
}

func TestCoordinatorReassign(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("t1", "a1", "team", "desc")
	_ = c.SetStatus("t1", StatusRunning)
	if err := c.Reassign("t1", "a2"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	task, _ := c.Get("t1")
	if task.AgentID != "a2" || task.Status != StatusPending {
		t.Fatalf("after Reassign: agent=%q status=%v", task.AgentID, task.Status)
	}
	// Cannot reassign a terminal task.
	_ = c.Complete("t1", "done")
	if err := c.Reassign("t1", "a3"); err == nil {
		t.Fatal("reassigning a terminal task must fail")
	}
}

func TestCoordinatorHandoffClaimBlocksCompetingTransitions(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("t1", "a1", "team", "desc")
	_ = c.SetStatus("t1", StatusRunning)
	if _, err := c.BeginHandoff("t1"); err != nil {
		t.Fatalf("BeginHandoff: %v", err)
	}
	if task, _ := c.Get("t1"); task.Status != StatusRunning {
		t.Fatalf("status during handoff = %v, want running until source exits", task.Status)
	}
	if _, err := c.BeginHandoff("t1"); err == nil {
		t.Fatal("a second handoff claim must fail")
	}
	if err := c.Complete("t1", "late result"); err == nil {
		t.Fatal("member completion must not win after handoff is claimed")
	}
	if err := c.Reassign("t1", "a2"); err == nil {
		t.Fatal("orphan adoption must not race a claimed handoff")
	}
	c.AbortHandoff("t1")
}

func TestCoordinatorAbortHandoffRestoresNormalCompletion(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("t1", "a1", "team", "desc")
	if _, err := c.BeginHandoff("t1"); err != nil {
		t.Fatalf("BeginHandoff: %v", err)
	}
	c.AbortHandoff("t1")
	if err := c.Complete("t1", "done"); err != nil {
		t.Fatalf("Complete after AbortHandoff: %v", err)
	}
}

func TestCoordinatorHandoffReservationBlocksRegistrationUntilAbort(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("source", "a1", "team", "desc")
	if _, err := c.BeginHandoff("source"); err != nil {
		t.Fatalf("BeginHandoff: %v", err)
	}
	if err := c.ReserveHandoffSuccessor("source", "successor"); err != nil {
		t.Fatalf("ReserveHandoffSuccessor: %v", err)
	}
	if _, err := c.Register("successor", "other", "team", "collision"); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("Register reserved id error = %v, want ErrTaskExists", err)
	}
	c.AbortHandoff("source")
	if _, err := c.Register("successor", "other", "team", "available again"); err != nil {
		t.Fatalf("reservation was not released by AbortHandoff: %v", err)
	}
	if err := c.Complete("source", "source continued"); err != nil {
		t.Fatalf("source claim was not released by AbortHandoff: %v", err)
	}
}

func TestCoordinatorCommitHandoffPublishesBothSides(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("source", "a1", "team", "desc")
	_ = c.SetStatus("source", StatusRunning)
	if _, err := c.BeginHandoff("source"); err != nil {
		t.Fatalf("BeginHandoff: %v", err)
	}
	if err := c.ReserveHandoffSuccessor("source", "successor"); err != nil {
		t.Fatalf("ReserveHandoffSuccessor: %v", err)
	}
	successor, err := c.CommitHandoff("source", "successor", "a2", "team", "continued")
	if err != nil {
		t.Fatalf("CommitHandoff: %v", err)
	}
	if successor.Status != StatusPending || successor.AgentID != "a2" {
		t.Fatalf("successor = %+v, want pending ownership by a2", successor)
	}
	source, _ := c.Get("source")
	if source.Status != StatusHandedOff {
		t.Fatalf("source status = %s, want handed-off", source.Status)
	}
	if _, err := c.Register("successor", "other", "team", "duplicate"); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("committed successor was not registered: %v", err)
	}
}

func TestCoordinatorColorStability(t *testing.T) {
	c := NewCoordinator()
	first := c.Color("a1")
	if first == "" {
		t.Fatal("Color should assign a non-empty color")
	}
	if again := c.Color("a1"); again != first {
		t.Fatalf("Color not stable: %q vs %q", first, again)
	}
	if other := c.Color("a2"); other == "" {
		t.Fatal("second agent should also get a color")
	}
	if c.Color("") != "" {
		t.Fatal("empty agent id should get no color")
	}
}

func TestCoordinatorSummarize(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Register("p", "a", "team", "")
	_, _ = c.Register("r", "b", "team", "")
	_ = c.SetStatus("r", StatusRunning)
	_, _ = c.Register("d", "c", "team", "")
	_ = c.Complete("d", "ok")
	s := c.Summarize()
	if s.Total != 3 || s.Pending != 1 || s.Running != 1 || s.Done != 1 {
		t.Fatalf("Summarize = %+v", s)
	}
}

func TestCoordinatorConcurrentRegister(t *testing.T) {
	c := NewCoordinator()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "task-" + string(rune('a'+n%26)) + "-" + itoa(n)
			_, _ = c.Register(id, id, "team", "desc")
			_ = c.SetStatus(id, StatusRunning)
			_ = c.Complete(id, "done")
		}(i)
	}
	wg.Wait()
	if got := len(c.List()); got != 50 {
		t.Fatalf("List len = %d, want 50", got)
	}
}

// itoa avoids importing strconv just for the concurrency test id.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
