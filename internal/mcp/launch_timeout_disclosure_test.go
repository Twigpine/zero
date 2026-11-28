package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/tools"
)

const launchNotice = "denyRead is configured, so the Windows sandbox uses the token shape without WRITE_RESTRICTED (#869)"

func registerWithFactory(t *testing.T, factory func(context.Context, Server) (ToolClient, error)) *Runtime {
	t.Helper()
	runtime, err := RegisterTools(context.Background(), tools.NewRegistry(), config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"slow": {Type: "stdio", Command: "slow-mcp"},
	}}, RegisterOptions{
		ConnectTimeout: 50 * time.Millisecond,
		ClientFactory:  factory,
	})
	if err != nil {
		t.Fatalf("RegisterTools error: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

// A SERVER THAT STARTED AND THEN HUNG STILL RAN UNDER THE REDUCED TOKEN.
//
// Registration abandons a server that exceeds the connect timeout and records it
// as skipped. The startup notices used to leave connectStdio only on the returned
// client or the returned error, and the abandoned attempt returns neither before
// the serial commit phase is over, so the process ran with reduced write
// confinement and startup said only that the server was skipped.
func TestTimeoutAfterLaunchKeepsTheStartupDisclosure(t *testing.T) {
	runtime := registerWithFactory(t, func(ctx context.Context, server Server) (ToolClient, error) {
		// The process started under the reduced token, then initialize hangs.
		publishLaunch(ctx, []string{launchNotice})
		<-ctx.Done()
		return nil, ctx.Err()
	})

	disclosures := runtime.StartupDisclosures()
	if len(disclosures) == 0 {
		t.Fatal("a server that started and then timed out disclosed nothing, so it ran under reduced write confinement unannounced")
	}
	if disclosures[0].Name != "slow" || len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != launchNotice {
		t.Errorf("StartupDisclosures() = %#v, want one entry for slow carrying the launch notice", disclosures)
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 || skipped[0].Name != "slow" {
		t.Errorf("the server should still be recorded as skipped: %#v", skipped)
	}
}

// AND A TIMEOUT BEFORE LAUNCH STAYS SILENT.
//
// Without this, retaining the disclosure on timeout could be satisfied by
// disclosing on every timeout, which would claim a token trade for a process
// that was never created.
func TestTimeoutBeforeLaunchDisclosesNothing(t *testing.T) {
	runtime := registerWithFactory(t, func(ctx context.Context, server Server) (ToolClient, error) {
		// Never reached Start: no publish.
		<-ctx.Done()
		return nil, ctx.Err()
	})

	if disclosures := runtime.StartupDisclosures(); len(disclosures) != 0 {
		t.Errorf("a server that never started claimed a token trade: %#v", disclosures)
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 {
		t.Errorf("the server should still be recorded as skipped: %#v", skipped)
	}
}

// THE PUBLISH HAS TO SIT AFTER Start, AND ONLY A REAL LAUNCH PROVES IT.
//
// The two tests above inject a factory, so they exercise the registry's handling
// of the sink and say nothing about where connectStdio publishes to it. Moving
// the call one line up, above cmd.Start, leaves both of them green while every
// failed launch starts claiming the token trade. This one drives the real
// connectStdio with a command that cannot start.
func TestAFailedStartPublishesNoLaunch(t *testing.T) {
	sink := &launchSink{}
	ctx := withLaunchSink(context.Background(), sink)

	client, err := connectStdio(ctx, Server{
		Name:    "missing",
		Type:    "stdio",
		Command: "zero-nonexistent-mcp-binary-for-test",
	}, ConnectOptions{})
	if err == nil {
		if client != nil {
			_ = client.Close()
		}
		t.Fatal("expected a nonexistent executable to fail to start")
	}

	if launched, notices := sink.observe(); launched {
		t.Errorf("a server whose process never started was published as launched (notices %#v)", notices)
	}
}

// A START THAT COMPLETES JUST AFTER THE TIMEOUT MUST STILL BE DISCLOSED.
//
// connectStdio publishes only once cmd.Start has returned, and the timeout
// branch used to sample the sink the instant it fired. Those interleave: the
// sample reads empty, the result commits with no notice, and the reaper closes
// the late client without being able to amend a commit that already happened.
//
// The real window is microseconds wide, so this drives the CONTRACT instead:
// the attempt starts after the registration timeout has elapsed but inside the
// settle grace, which is the case the synchronization exists to catch.
func TestStartJustAfterTheTimeoutIsStillDisclosed(t *testing.T) {
	runtime := registerWithFactory(t, func(ctx context.Context, server Server) (ToolClient, error) {
		// 50ms registration timeout has fired; this lands inside launchSettleGrace.
		time.Sleep(120 * time.Millisecond)
		publishLaunch(ctx, []string{launchNotice})
		return nil, errors.New("initialize failed after start")
	})

	disclosures := runtime.StartupDisclosures()
	if len(disclosures) != 1 || len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != launchNotice {
		t.Fatalf("a process that started just after the timeout was not disclosed: %#v", disclosures)
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 {
		t.Errorf("the server should still be recorded as skipped: %#v", skipped)
	}
}

// And an attempt that never starts is not held for the grace, nor disclosed.
func TestTimeoutBeforeStartIsNotDelayedOrDisclosed(t *testing.T) {
	start := time.Now()
	runtime := registerWithFactory(t, func(ctx context.Context, server Server) (ToolClient, error) {
		<-ctx.Done() // cancel arrives with the timeout; returns immediately
		return nil, ctx.Err()
	})
	elapsed := time.Since(start)

	if disclosures := runtime.StartupDisclosures(); len(disclosures) != 0 {
		t.Errorf("a server that never started claimed a token trade: %#v", disclosures)
	}
	// The grace is 250ms; an attempt that returns on cancel must not pay it.
	if elapsed > 200*time.Millisecond {
		t.Errorf("registration waited %v for an attempt that never started", elapsed)
	}
}

// AND A START THAT COMPLETES AFTER THE SETTLE GRACE MUST STILL BE DISCLOSED.
//
// The grace is only another timeout. Once it expires, registration reaps the
// attempt in the background and returns; the process can still be inside
// cmd.Start at that moment and start successfully afterwards. If Runtime were a
// snapshot taken at commit time, that launch would have no owner: the reaper can
// close the late client but cannot amend a value already returned, so the server
// would have run under the reduced write confinement with startup reporting it
// only as skipped.
//
// A larger grace changes the probability, not the contract, which is why this
// releases the launch strictly AFTER the bound rather than inside it. The sink
// outlives registration and StartupDisclosures reads through it.
func TestStartAfterTheSettleGraceIsStillDisclosed(t *testing.T) {
	released := make(chan struct{})
	runtime := registerWithFactory(t, func(ctx context.Context, server Server) (ToolClient, error) {
		// Held past the 50ms registration timeout AND past launchSettleGrace, so
		// registration has already reaped this attempt and returned.
		<-released
		publishLaunch(ctx, []string{launchNotice})
		return nil, errors.New("initialize failed long after start")
	})

	// Registration is done and the disclosure legitimately is not known yet.
	if disclosures := runtime.StartupDisclosures(); len(disclosures) != 0 {
		t.Fatalf("nothing had started yet, so nothing should be disclosed: %#v", disclosures)
	}

	close(released)

	deadline := time.Now().Add(2 * time.Second)
	var disclosures []StartupDisclosure
	for time.Now().Before(deadline) {
		if disclosures = runtime.StartupDisclosures(); len(disclosures) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if len(disclosures) != 1 || len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != launchNotice {
		t.Fatalf("a process that started after the settle grace was never disclosed: %#v", disclosures)
	}
	// Reading again must not duplicate it.
	if again := runtime.StartupDisclosures(); len(again) != 1 {
		t.Errorf("a second read changed the disclosures: %#v", again)
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 {
		t.Errorf("the server should still be recorded as skipped: %#v", skipped)
	}
}
