package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/tui"
)

// A background plan MUST NOT OUTLIVE ITS SESSION. That is the failure this
// whole seam exists to prevent — a plan still spending after the session ended,
// reporting to nobody.
func TestClosingTheLauncherStopsAndWaitsForTheBackgroundPlan(t *testing.T) {
	launcher := newPlanLauncher(context.Background(), tui.NewPlanProgressBridge())

	started := make(chan struct{})
	stopped := make(chan struct{})
	if !launcher.Launch(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		// A real plan tears a child down here; the sleep stands in for that, and
		// Close must not return until it is over.
		time.Sleep(50 * time.Millisecond)
		close(stopped)
	}) {
		t.Fatal("Launch refused")
	}
	<-started

	launcher.Close()
	select {
	case <-stopped:
	default:
		t.Fatal("Close returned while the plan was still stopping; that is how a child is orphaned")
	}
}

// ONE AT A TIME. Two background plans report into one panel keyed by task id and
// the second would overwrite the first's rows, so the second is refused rather
// than silently winning.
func TestASecondBackgroundPlanIsRefused(t *testing.T) {
	launcher := newPlanLauncher(context.Background(), tui.NewPlanProgressBridge())
	release := make(chan struct{})
	defer close(release)

	if !launcher.Launch(func(context.Context) { <-release }) {
		t.Fatal("the first launch was refused")
	}
	// Give the goroutine a moment to be counted; the refusal is decided under
	// the lock at Launch, so this is about the test not the code.
	time.Sleep(20 * time.Millisecond)
	if launcher.Launch(func(context.Context) {}) {
		t.Fatal("a second background plan was launched over a running one")
	}
}

// ...and once the first finishes, the slot frees.
func TestTheSlotFreesWhenABackgroundPlanEnds(t *testing.T) {
	launcher := newPlanLauncher(context.Background(), tui.NewPlanProgressBridge())
	done := make(chan struct{})
	if !launcher.Launch(func(context.Context) { close(done) }) {
		t.Fatal("the first launch was refused")
	}
	<-done

	deadline := time.After(3 * time.Second)
	for {
		if launcher.Launch(func(context.Context) {}) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the slot never freed after the plan ended")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A launch after Close must be REFUSED, not started into a cancelled context.
// Starting it would record an admission and a cancellation for a plan the user
// never saw.
func TestLaunchingAfterCloseIsRefused(t *testing.T) {
	launcher := newPlanLauncher(context.Background(), tui.NewPlanProgressBridge())
	launcher.Close()
	if launcher.Launch(func(context.Context) { t.Error("a plan ran after Close") }) {
		t.Fatal("Launch succeeded after Close")
	}
}

// A PANIC IN A BACKGROUND PLAN MUST NOT TAKE THE SESSION DOWN, and must still
// free the slot — a crashed plan that held the slot forever would block every
// later one with "already running".
func TestAPanickingBackgroundPlanIsContainedAndFreesTheSlot(t *testing.T) {
	bridge := tui.NewPlanProgressBridge()
	launcher := newPlanLauncher(context.Background(), bridge)
	if !launcher.Launch(func(context.Context) { panic("boom") }) {
		t.Fatal("Launch refused")
	}

	deadline := time.After(3 * time.Second)
	for bridge.BackgroundPlanLive() {
		select {
		case <-deadline:
			t.Fatal("a panicking plan left the bridge busy")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if completion := bridge.DrainCompletedPlans(); !strings.Contains(completion, "failed") || !strings.Contains(completion, "panicked") {
		t.Fatalf("panic produced no explicit failed completion: %q", completion)
	}
	if !launcher.Launch(func(context.Context) {}) {
		t.Fatal("a panicking plan held the launcher slot forever")
	}
}

// Cancelling the SESSION context cancels the plan, which is what makes the
// session root load-bearing rather than decorative.
func TestCancellingTheSessionCancelsTheBackgroundPlan(t *testing.T) {
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	launcher := newPlanLauncher(sessionCtx, tui.NewPlanProgressBridge())

	observed := make(chan error, 1)
	if !launcher.Launch(func(ctx context.Context) {
		<-ctx.Done()
		observed <- ctx.Err()
	}) {
		t.Fatal("Launch refused")
	}
	cancelSession()
	select {
	case err := <-observed:
		if err == nil {
			t.Fatal("the plan's context was not cancelled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the session did not reach the background plan")
	}
}

// The bridge is told BEFORE the plan runs, so its very first message is already
// marked background. Marking it after the goroutine started would race the
// plan's own admission message, and a dropped admission is a panel that never
// shows the plan at all.
func TestTheBridgeIsMarkedBackgroundBeforeThePlanRuns(t *testing.T) {
	bridge := tui.NewPlanProgressBridge()
	launcher := newPlanLauncher(context.Background(), bridge)

	var once sync.Once
	markedAtStart := make(chan bool, 1)
	if !launcher.Launch(func(context.Context) {
		once.Do(func() { markedAtStart <- bridge.PlanIsBackground() })
	}) {
		t.Fatal("Launch refused")
	}
	select {
	case marked := <-markedAtStart:
		if !marked {
			t.Fatal("the plan started before the bridge was marked background")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the plan never ran")
	}
}

// HEADLESS EXEC SUPPLIES NO LAUNCHER, which is what makes its refusal of a
// background plan fall out of the wiring rather than out of a special case.
// Asserted at the registered tool, because a comment saying "exec passes nil"
// is not evidence that exec passes nil.
func TestHeadlessExecSuppliesNoLauncher(t *testing.T) {
	workspace := t.TempDir()
	registry := newCoreRegistry(workspace)
	runtime, err := registerSpecialistTools(registry, workspace, 0, nil, nil, nil, orchestrateWiring{
		Gate: &specialist.PostureGate{},
	})
	if err != nil {
		t.Fatalf("registerSpecialistTools: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(nil, runtime) })

	registered, found := registry.Get(specialist.OrchestrateToolName)
	if !found {
		t.Fatal("orchestrate was not registered")
	}
	tool, ok := registered.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("orchestrate is %T", registered)
	}
	if tool.Launch != nil {
		t.Fatal("a run wired without a launcher was given one; a background plan there could never report")
	}
}
