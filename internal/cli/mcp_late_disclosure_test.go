package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

// A LAUNCH THAT COMPLETES AFTER THE REPORTER HAS RUN MUST STILL BE SAID, ONCE,
// AND ONLY WHILE SOMEONE OWNS THE WRITER.
//
// Both production paths call reportMCPStartupDisclosures exactly once, right
// after RegisterTools returns. A stdio attempt abandoned at the connect timeout
// can still be inside cmd.Start at that moment; the process then starts under
// the reduced write confinement, the reaper closes its client, and a reporter
// that merely SAMPLED the runtime has already come and gone. The retained sink
// held the fact and nobody read it again, so the operator saw the skipped
// server and never the disclosure.
//
// This drives the REAL reporter against a REAL runtime, rather than polling
// StartupDisclosures by hand, which is what an earlier regression did and which
// is precisely how it masked the original bug: a test that re-reads on the
// tester's behalf proves nothing about a production path that does not.
//
// It also never reads stderr while the pump could write. The buffer is examined
// only after stop has joined the pump, which is the same discipline both
// production callers follow, and is why this passes under -race.
func TestLateMCPLaunchReachesTheStartupReporterExactlyOnce(t *testing.T) {
	const notice = "MCP server started without WRITE_RESTRICTED because denyRead is configured (#869)"
	released := make(chan struct{})
	published := make(chan struct{})

	runtime, err := mcp.RegisterTools(context.Background(), tools.NewRegistry(),
		config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"slow": {Type: "stdio", Command: "slow-mcp"},
		}},
		mcp.RegisterOptions{
			ConnectTimeout: 50 * time.Millisecond,
			ClientFactory: func(ctx context.Context, server mcp.Server) (mcp.ToolClient, error) {
				// Held past the registration timeout AND past the settle grace, so
				// registration has already reaped this attempt and returned.
				<-released
				mcp.PublishLaunchForTest(ctx, []string{notice})
				// Publishing is synchronous into the stream, so by the time this
				// closes the disclosure is queued and stop cannot race past it.
				close(published)
				return nil, errors.New("initialize failed long after start")
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	// The reporter runs ONCE, here, exactly as runExec and the interactive
	// startup path run it: before the launch has resolved.
	var stderr bytes.Buffer
	_, stop := reportMCPStartupDisclosures(&stderr, runtime)

	close(released)
	<-published
	// The owner ends delivery and joins the pump. Every write to the buffer has
	// happened by the time this returns, so the reads below are unsynchronised
	// only because there is no longer anything to synchronise with.
	stop()

	got := stderr.String()
	if n := strings.Count(got, notice); n != 1 {
		t.Fatalf("a launch that completed after the reporter ran was disclosed %d time(s), want exactly 1:\n%s", n, got)
	}
	if !strings.Contains(got, "MCP server slow started with reduced enforcement") {
		t.Errorf("the late disclosure does not name the server:\n%s", got)
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 {
		t.Errorf("the server should still be recorded as skipped: %#v", skipped)
	}
}

// And a server whose launch was already known when the reporter ran is said
// once by it, and not again by the late path.
func TestKnownMCPLaunchIsNotReportedTwice(t *testing.T) {
	const notice = "MCP server started under reduced enforcement"
	runtime, err := mcp.RegisterTools(context.Background(), tools.NewRegistry(),
		config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"fast": {Type: "stdio", Command: "fast-mcp"},
		}},
		mcp.RegisterOptions{
			ConnectTimeout: time.Second,
			ClientFactory: func(ctx context.Context, server mcp.Server) (mcp.ToolClient, error) {
				mcp.PublishLaunchForTest(ctx, []string{notice})
				return nil, errors.New("initialize failed after start")
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var stderr bytes.Buffer
	_, stop := reportMCPStartupDisclosures(&stderr, runtime)
	stop()
	if n := strings.Count(stderr.String(), notice); n != 1 {
		t.Fatalf("a launch known at registration was disclosed %d time(s), want exactly 1:\n%s", n, stderr.String())
	}
}

// THE OWNERSHIP BOUNDARY ITSELF: once the caller has stopped delivery, nothing
// may write to its writer again.
//
// This is the property that the retained presentation callback could not hold.
// It invoked the CLI's print function from the abandoned connect goroutine
// whenever the launch happened to resolve, so a write could land after runExec
// had returned or after Bubble Tea had taken the alt screen. The interactive
// path stops delivery on the line before it hands over the terminal, and this
// pins what that buys: a launch resolving afterwards is dropped, not printed.
func TestMCPDisclosureAfterStopIsDroppedNotWritten(t *testing.T) {
	const notice = "MCP server started under reduced enforcement"
	released := make(chan struct{})
	published := make(chan struct{})

	runtime, err := mcp.RegisterTools(context.Background(), tools.NewRegistry(),
		config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"slow": {Type: "stdio", Command: "slow-mcp"},
		}},
		mcp.RegisterOptions{
			ConnectTimeout: 50 * time.Millisecond,
			ClientFactory: func(ctx context.Context, server mcp.Server) (mcp.ToolClient, error) {
				<-released
				mcp.PublishLaunchForTest(ctx, []string{notice})
				close(published)
				return nil, errors.New("initialize failed long after start")
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var stderr bytes.Buffer
	_, stop := reportMCPStartupDisclosures(&stderr, runtime)
	// The owner gives up the writer BEFORE the launch resolves, which is the
	// interactive hand-off to the TUI.
	stop()

	close(released)
	<-published
	// Asserting an absence, so the wrong behaviour is given time to appear: once
	// publishing has returned the disclosure is queued, and a delivery path that
	// outlived stop would have this long to print it. With delivery ended and the
	// pump joined there is no writer left, so this window changes nothing.
	time.Sleep(50 * time.Millisecond)

	if got := stderr.String(); got != "" {
		t.Fatalf("a launch that resolved after the owner stopped still wrote to its writer: %q", got)
	}
}
