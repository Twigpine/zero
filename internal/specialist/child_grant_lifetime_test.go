package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

const (
	sessionRoot = "/grants/session"
	turnRoot    = "/grants/this-turn"
)

// lifetimeSupplier answers like the CLI's scope-backed supplier: the
// turn-scoped root only for a child that ends within the turn.
func lifetimeSupplier(withinTurn bool) []string {
	if withinTurn {
		return []string{sessionRoot, turnRoot}
	}
	return []string{sessionRoot}
}

func lifetimeExecutor(t *testing.T, argv *[]string) Executor {
	t.Helper()
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return Executor{
		BinaryPath:        "/usr/local/bin/zero",
		BackgroundManager: manager,
		NewSessionID:      func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots:   lifetimeSupplier,
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer", Description: "reads"},
				SystemPrompt:  "x",
				ResolvedTools: []string{"read_file"},
				ToolsResolved: true,
				Location:      LocationBuiltin,
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			*argv = append([]string(nil), args...)
			return ChildRunResult{Started: true}, nil
		},
		LaunchBackground: func(_ string, args []string, _ string, _ func(int)) (int, error) {
			*argv = append([]string(nil), args...)
			return 4321, nil
		},
	}
}

// A TURN-SCOPED GRANT MUST NOT OUTLIVE ITS TURN IN A CHILD'S ARGV.
//
// Scope.ExtraRoots returned AddTemporaryWrite roots indistinguishably from
// permanent ones, and every child got them as a plain --add-dir. The parent
// released the grant at the end of the turn; a child still running carried it
// for its whole life and wrote there afterwards. Only a child the tool call
// waits on (foreground Task, resume) may inherit a turn-scoped grant.
func TestATurnScopedGrantReachesOnlyChildrenThatEndWithinTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		wantTurn bool
	}{
		{name: "foreground task", args: map[string]any{"name": "explorer", "prompt": "p"}, wantTurn: true},
		{name: "background task", args: map[string]any{"name": "explorer", "prompt": "p", "run_in_background": true}, wantTurn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argv []string
			result := NewTaskTool(lifetimeExecutor(t, &argv)).RunWithOptions(context.Background(), tc.args,
				tools.RunOptions{Cwd: t.TempDir()})
			if result.Status != tools.StatusOK {
				t.Fatalf("Task failed: %s", result.Output)
			}
			if !argsContainPair(argv, "--add-dir", sessionRoot) {
				t.Fatalf("the session root never reached the child:\n%s", strings.Join(argv, " "))
			}
			if got := argsContainPair(argv, "--add-dir", turnRoot); got != tc.wantTurn {
				t.Fatalf("turn-scoped root in argv = %v, want %v:\n%s", got, tc.wantTurn, strings.Join(argv, " "))
			}
		})
	}
}

// A resumed Task is foreground (a resume cannot run in the background), so it
// keeps the turn's grants exactly as a fresh foreground Task does.
func TestAResumedTaskInheritsTheTurnsGrants(t *testing.T) {
	store, session := resumableSession(t)
	var argv []string
	executor := lifetimeExecutor(t, &argv)
	executor.SessionStore = store
	result := NewTaskTool(executor).RunWithOptions(context.Background(),
		map[string]any{"resume": session.SessionID, "prompt": "carry on"}, tools.RunOptions{Cwd: t.TempDir()})
	if result.Status != tools.StatusOK {
		t.Fatalf("resume failed: %s", result.Output)
	}
	if !argsContainPair(argv, "--add-dir", turnRoot) || !argsContainPair(argv, "--add-dir", sessionRoot) {
		t.Fatalf("the resumed child did not get the turn's grants:\n%s", strings.Join(argv, " "))
	}
}

// A plan task can outlive the turn (a background plan certainly does), so it
// gets only the grants that outlive the turn too.
func TestAPlanTaskIsNotHandedATurnScopedGrant(t *testing.T) {
	var argv []string
	run := NewPlanRunner(PlanTaskContext{Executor: lifetimeExecutor(t, &argv), Cwd: t.TempDir(), SpecialistName: "explorer"})
	if _, err := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "t", Prompt: "p"},
		Tools: []string{"read_file"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !argsContainPair(argv, "--add-dir", sessionRoot) {
		t.Fatalf("the session root never reached the plan task:\n%s", strings.Join(argv, " "))
	}
	if argsContainPair(argv, "--add-dir", turnRoot) {
		t.Fatalf("a plan task was handed a turn-scoped grant:\n%s", strings.Join(argv, " "))
	}
}
