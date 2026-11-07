package specialist

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// budgetRun starts one sub-agent the way a plan task does — with a manifest, so
// the run does not depend on a registered specialist.
func budgetRun(t *testing.T, executor Executor) error {
	t.Helper()
	manifest := planTaskManifest("explorer", "", "", []string{"read_file"})
	_, err := executor.Run(context.Background(), TaskParameters{
		Name: "explorer", Prompt: "x", Manifest: &manifest,
	}, TaskRunOptions{Cwd: t.TempDir()})
	return err
}

func budgetExecutor(t *testing.T, budget *SessionBudget) Executor {
	t.Helper()
	return Executor{
		BinaryPath:    "/bin/true",
		SessionBudget: budget,
		NewSessionID:  func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:          func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{Started: true}, nil
		},
	}
}

// A CONVERSATION THAT KEEPS STARTING WORK IS BOUNDED, which no other limit does.
//
// Depth caps nesting at 8, maxPlanWorkers caps concurrency, a session shows one
// plan at a time, and the run-level token budget bounds ONE run. Every one of
// those resets or applies per run, so a session that starts plan after plan was
// bounded by nothing at all.
func TestASessionStopsStartingSubAgentsAtItsLimit(t *testing.T) {
	executor := budgetExecutor(t, NewSessionBudget(3))
	var lastErr error
	started := 0
	for i := 0; i < 5; i++ {
		err := budgetRun(t, executor)
		if err == nil {
			started++
			continue
		}
		lastErr = err
	}
	if started != 3 {
		t.Fatalf("%d sub-agents started against a limit of 3", started)
	}
	if lastErr == nil {
		t.Fatal("the fourth start was not refused")
	}
	for _, required := range []string{"3", "limit", "new session"} {
		if !strings.Contains(lastErr.Error(), required) {
			t.Errorf("the refusal does not mention %q: %v", required, lastErr)
		}
	}
}

// AN UNWIRED BUDGET IS UNBOUNDED, so every caller that never sets one keeps
// today's behaviour — including every test and the MCP path.
func TestAnUnwiredSessionBudgetBoundsNothing(t *testing.T) {
	executor := budgetExecutor(t, nil)
	for i := 0; i < 12; i++ {
		if err := budgetRun(t, executor); err != nil {
			t.Fatalf("start %d was refused with no budget wired: %v", i, err)
		}
	}
	if got := (*SessionBudget)(nil).Started(); got != 0 {
		t.Errorf("a nil budget reported %d started", got)
	}
}

// COUNTED, NEVER DECREMENTED. The question is how much work this conversation
// has set going, not how much is running now — a session that started three
// hundred children and finished them has still spent that.
func TestFinishedSubAgentsStillCountAgainstTheSession(t *testing.T) {
	budget := NewSessionBudget(2)
	executor := budgetExecutor(t, budget)
	for i := 0; i < 2; i++ {
		if err := budgetRun(t, executor); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	// Both have returned. A third must still be refused.
	if err := budgetRun(t, executor); err == nil {
		t.Fatal("a finished sub-agent gave its slot back; the budget is a spend count, not a concurrency gauge")
	}
	if got := budget.Started(); got < 2 {
		t.Errorf("Started() = %d, want at least 2", got)
	}
}

// The default is a backstop far above observed use: the most any of 75 recorded
// sessions started was 22.
func TestTheDefaultSessionCapSitsWellAboveObservedUse(t *testing.T) {
	if DefaultSessionSubagents < 100 {
		t.Fatalf("DefaultSessionSubagents = %d; the observed maximum was 22 and a cap near it would refuse ordinary work", DefaultSessionSubagents)
	}
}

// A REFUSAL IS NOT A START. admit used to increment before judging, so every
// refused spawn was counted: Started() over-reported, and each later refusal
// quoted a larger "has started" than the session ever reached.
func TestARefusedSpawnIsNotCountedAsStarted(t *testing.T) {
	budget := NewSessionBudget(2)
	for i := 0; i < 2; i++ {
		if err := budget.admit(); err != nil {
			t.Fatalf("admission %d within the limit was refused: %v", i+1, err)
		}
	}
	var last error
	for i := 0; i < 5; i++ {
		if last = budget.admit(); last == nil {
			t.Fatal("a spawn past the limit was admitted")
		}
	}
	if got := budget.Started(); got != 2 {
		t.Fatalf("Started() = %d after 2 admissions and 5 refusals, want 2", got)
	}
	if !strings.Contains(last.Error(), "has started 2 sub-agents") {
		t.Fatalf("the refusal misreports how many were started: %v", last)
	}
}

// And the limit holds exactly under concurrent spawns: never more admissions
// than the limit, and the count equals the admissions.
func TestConcurrentSpawnsAreAdmittedExactlyUpToTheLimit(t *testing.T) {
	budget := NewSessionBudget(25)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if budget.admit() == nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 25 || budget.Started() != 25 {
		t.Fatalf("admitted %d, Started() %d; want exactly 25 each", admitted.Load(), budget.Started())
	}
}
