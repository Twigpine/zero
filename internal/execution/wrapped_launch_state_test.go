package execution

import (
	"context"
	"testing"
)

// A WRAPPER'S START IS NOT THE REQUESTED CHILD'S START.
//
// For a Windows restricted-token plan the command the runner starts is the
// sandbox helper, not the executable the caller asked for. Inside that helper,
// setup-marker validation, unelevated ACL application, network-policy
// validation, capability and offline SID construction, restricted-token creation
// and the CreateProcessAsUser call all happen afterwards, and each of them can
// return with no sandboxed child ever created. exec.Cmd.Process is already
// non-nil by then, so reading the launch state off it reports that reads were
// denied as requested when the only thing that ran was the unsandboxed adapter.
//
// The fact belongs to whoever sees the transition. These pin both directions of
// that boundary.
type wrappedPreparer struct {
	script        string
	owned         bool
	childLaunched *bool
	reportNothing bool
}

func (p *wrappedPreparer) PrepareExecution(ctx context.Context, _ Request) (PreparedCommand, error) {
	prepared := PreparedCommand{
		Command:                   launchStateShell(ctx, p.script),
		Enforcement:               Enforcement{Notices: []string{launchStateNotice}},
		ChildLaunchOwnedByAdapter: p.owned,
	}
	if !p.reportNothing {
		launched := p.childLaunched
		prepared.Report = func() (AdapterReport, error) {
			return AdapterReport{ChildLaunched: launched}, nil
		}
	}
	return prepared, nil
}

func capturedWrapped(t *testing.T, p *wrappedPreparer) CapturedResult {
	t.Helper()
	return NewRunner(p).ExecuteCaptured(context.Background(), CapturedRequest{Request: Request{
		Origin:           OriginHook,
		Mode:             ModeCaptured,
		Command:          Command{Name: "irrelevant"},
		WorkingDirectory: t.TempDir(),
		WorkspaceRoots:   []string{t.TempDir()},
		Approval:         ApprovalContext{PolicyVersion: PolicyVersion},
	}})
}

func TestWrappedPlanDisclosesOnlyWhatTheAdapterConfirms(t *testing.T) {
	// The helper starts and then fails before it can create the restricted child:
	// a bad setup marker, an ACL it could not apply, a network policy it rejected,
	// a token it could not mint. The wrapper process exists; the sandboxed one
	// never did, so nothing may be claimed about enforcement.
	t.Run("helper ran but never created the child", func(t *testing.T) {
		no := false
		result := capturedWrapped(t, &wrappedPreparer{script: "exit 1", owned: true, childLaunched: &no})
		// The wrapper really did run, which is the whole point: its exit code is
		// the script's. Without this the test could pass because nothing executed.
		if result.Outcome.Exit == nil || result.Outcome.Exit.Code != 1 {
			t.Fatalf("SETUP INVALID: the wrapper itself must have run; outcome = %+v", result.Outcome)
		}
		if notices := result.Outcome.AppliedEnforcementNotices(); len(notices) != 0 {
			t.Fatalf("a helper that never created the restricted child disclosed %q", notices)
		}
	})

	// Same shape, but the adapter says nothing at all. Silence from the owner of
	// the fact is not permission to fall back to the wrapper's own start.
	t.Run("adapter that owns the fact stayed silent", func(t *testing.T) {
		result := capturedWrapped(t, &wrappedPreparer{script: "exit 1", owned: true, reportNothing: true})
		if notices := result.Outcome.AppliedEnforcementNotices(); len(notices) != 0 {
			t.Fatalf("an unreported child launch was disclosed as applied enforcement: %q", notices)
		}
	})

	// And the other side of the boundary: a restricted child that really started
	// and then exited non-zero DID run under the disclosed enforcement, so the
	// notice must still be made, exactly once.
	t.Run("restricted child started, then failed", func(t *testing.T) {
		yes := true
		result := capturedWrapped(t, &wrappedPreparer{script: "exit 3", owned: true, childLaunched: &yes})
		notices := result.Outcome.AppliedEnforcementNotices()
		if len(notices) != 1 || notices[0] != launchStateNotice {
			t.Fatalf("a child that ran and then failed disclosed %q, want exactly one %q", notices, launchStateNotice)
		}
	})

	// A direct, unwrapped command is unchanged: the process the runner starts IS
	// the requested one, so its own observation still decides.
	t.Run("direct command keeps its own observation", func(t *testing.T) {
		result := capturedWrapped(t, &wrappedPreparer{script: "exit 0", owned: false, reportNothing: true})
		notices := result.Outcome.AppliedEnforcementNotices()
		if len(notices) != 1 || notices[0] != launchStateNotice {
			t.Fatalf("a direct command that ran disclosed %q, want exactly one %q", notices, launchStateNotice)
		}
	})
}
