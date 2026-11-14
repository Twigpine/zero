package cli

import (
	"context"
	"sync"

	"github.com/Twigpine/zero/internal/tui"
)

// The background-plan launcher: who owns a background plan's LIFETIME.
//
// The tool does not, and that is the whole point of the seam. A background plan
// must outlive the tool call that started it and must NOT outlive the session,
// and the only thing that knows where the session ends is the surface that runs
// it. So the surface supplies a launcher, keeps the context, and drains on exit;
// the tool asks to be run and is told yes or no.
//
// It also makes headless exec's refusal fall out rather than be special-cased:
// that process exits when the turn ends, so it supplies no launcher at all, and
// a background plan there is refused with a reason instead of being started and
// orphaned.
//
// ONE AT A TIME, deliberately. The panel is keyed by task id and the bridge
// holds one plan's cancel; two concurrent background plans would report into
// one panel and the second would overwrite the first's rows. Refusing the
// second is a stated rule; letting the UI silently pick one is a defect.

// planLauncher runs background plans under a session-scoped context.
type planLauncher struct {
	mu sync.Mutex
	// ctx is the SESSION root, not a tool call's. Cancelling it is what stops a
	// background plan at exit — the failure this exists to prevent is a plan
	// that outlives its session, spends budget nobody sees, and reports nothing.
	ctx context.Context
	// running holds the one in-flight plan's cancel; nil means none.
	running context.CancelFunc
	// closed stops new launches once shutdown has begun. Checked under the same
	// lock that starts a plan, so a launch cannot slip past a concurrent Close.
	closed bool
	// wait lets Close block until the plan actually stops, rather than
	// cancelling and exiting while a child process is still being torn down.
	wait sync.WaitGroup
	// bridge is told a background plan is starting, so every message it posts is
	// marked as one and survives the panel's stale-run guard.
	bridge *tui.PlanProgressBridge
}

func newPlanLauncher(ctx context.Context, bridge *tui.PlanProgressBridge) *planLauncher {
	return &planLauncher{ctx: ctx, bridge: bridge}
}

// Launch starts run on its own goroutine and reports whether it did.
//
// False is returned — never a silent no-op — when the session is closing or a
// background plan is already running, so the tool can say which rather than
// handing back a run id for a plan nobody started.
func (launcher *planLauncher) Launch(run func(ctx context.Context)) bool {
	if launcher == nil || run == nil {
		return false
	}
	launcher.mu.Lock()
	if launcher.closed || launcher.running != nil {
		launcher.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(launcher.ctx)
	launcher.running = cancel
	launcher.wait.Add(1)
	launcher.mu.Unlock()

	launcher.bridge.SetBackground(true)
	go func() {
		defer func() {
			// Every exit converges on the bridge's idempotent terminal transition.
			// Normal execution has already called PlanCompleted; panic, cancellation,
			// or an early return is finalized here before the launcher slot opens.
			panicked := recover() != nil
			launcher.bridge.FinalizeBackgroundExit(panicked, ctx.Err() != nil)
			cancel()
			launcher.mu.Lock()
			launcher.running = nil
			launcher.mu.Unlock()
			launcher.wait.Done()
		}()
		run(ctx)
	}()
	return true
}

// Close stops any background plan and WAITS for it.
//
// Waiting matters: cancelling and returning would let the process exit while a
// child is still being torn down, which is how orphans happen. The recorder's
// terminal event is written by the plan itself on the way out, so the wait is
// also what makes a cancelled background plan land in the session log rather
// than vanish.
func (launcher *planLauncher) Close() {
	if launcher == nil {
		return
	}
	launcher.mu.Lock()
	launcher.closed = true
	cancel := launcher.running
	launcher.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	launcher.wait.Wait()
}
