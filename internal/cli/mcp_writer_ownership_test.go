package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

// blockingWriter makes the overlap deterministic instead of hoping to hit it.
//
// The first write parks inside Write until the test releases it, which is the
// window the pump and the foreground startup path really share: startup keeps
// emitting plugin, trust, peer and provider output to the same stderr while a
// late MCP disclosure can arrive at any moment. It also records whether it was
// ever entered twice at once, which is the property under test.
type blockingWriter struct {
	mu       sync.Mutex
	inside   int
	overlaps int
	buf      bytes.Buffer

	block    chan struct{}
	blockOne sync.Once
	entered  chan struct{}
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{block: make(chan struct{}), entered: make(chan struct{})}
}

func (w *blockingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.inside++
	if w.inside > 1 {
		w.overlaps++
	}
	w.mu.Unlock()

	// Only the first writer parks, and it announces that it is inside.
	first := false
	w.blockOne.Do(func() {
		first = true
		close(w.entered)
	})
	if first {
		<-w.block
	}

	w.mu.Lock()
	n, err := w.buf.Write(data)
	w.inside--
	w.mu.Unlock()
	return n, err
}

func (w *blockingWriter) overlapCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overlaps
}

func (w *blockingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// ONE CALLER AT A TIME, FOR THE WHOLE OVERLAP.
//
// Joining the pump at stop bounds writes to the pump's lifetime but says nothing
// about what happens DURING it. The startup paths keep writing to the same
// io.Writer the whole time, and the caller may legitimately hand in a plain
// bytes.Buffer, which corrupts under concurrent use. A mutex private to the pump
// would not have helped, because the foreground writes do not go through it; the
// reporter therefore hands back a guarded view of the caller's writer and the
// caller adopts it, so both sides take the same lock.
func TestLateDisclosureAndForegroundStartupNeverShareTheWriter(t *testing.T) {
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

	writer := newBlockingWriter()
	guarded, stop := reportMCPStartupDisclosures(writer, runtime)
	if guarded == io.Writer(writer) {
		t.Fatal("SETUP INVALID: the reporter handed back the raw writer, so the caller cannot share its lock")
	}

	// Foreground startup writes through the guarded writer and parks inside it,
	// exactly as a slow terminal would.
	foregroundDone := make(chan struct{})
	go func() {
		defer close(foregroundDone)
		fmt.Fprintln(guarded, "warning: MCP server other unavailable, skipped: dial tcp: refused")
	}()
	<-writer.entered

	// While the foreground write is parked, the late launch resolves and the pump
	// tries to print. If the two did not share a lock, this would enter Write
	// concurrently.
	close(released)
	<-published
	time.Sleep(50 * time.Millisecond)

	close(writer.block)
	<-foregroundDone
	stop()

	if n := writer.overlapCount(); n != 0 {
		t.Fatalf("the pump and foreground startup were inside the writer together %d time(s)", n)
	}
	got := writer.String()
	if count := strings.Count(got, notice); count != 1 {
		t.Fatalf("the late disclosure was written %d time(s), want exactly 1 after stop drained it:\n%s", count, got)
	}
	if !strings.Contains(got, "unavailable, skipped") {
		t.Errorf("the foreground startup message was lost:\n%s", got)
	}
}
