package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/tools"
)

// TestMCPStdioHangHelperProcess is a stdio MCP server that starts but never
// responds, simulating a non-responsive peer during the handshake.
func TestMCPStdioHangHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_MCP_STDIO_HANG_HELPER") != "1" {
		return
	}
	select {}
}

// Connect must fail fast (within the connect deadline) when a stdio server
// starts but never answers the initialize handshake, instead of hanging.
func TestConnectStdioFailsFastOnNonResponsiveInitialize(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		client, connectErr := Connect(ctx, Server{
			Name:    "hang",
			Type:    ServerTypeStdio,
			Command: executable,
			Args:    []string{"-test.run=TestMCPStdioHangHelperProcess", "--"},
			Env:     map[string]string{"ZERO_MCP_STDIO_HANG_HELPER": "1"},
		})
		if connectErr == nil && client != nil {
			_ = client.Close()
		}
		done <- connectErr
	}()

	select {
	case connectErr := <-done:
		if connectErr == nil {
			t.Fatal("Connect() error = nil, want handshake timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect() hung on a non-responsive initialize handshake")
	}
}

// blockingReader blocks every Read until closed, simulating a hung peer that
// keeps the connection open but never sends data.
type blockingReader struct {
	release chan struct{}
}

type signalingWriter struct {
	writes chan struct{}
}

func (writer *signalingWriter) Write(p []byte) (int, error) {
	writer.writes <- struct{}{}
	return len(p), nil
}

func newBlockingReader() *blockingReader {
	return &blockingReader{release: make(chan struct{})}
}

func (reader *blockingReader) Read(p []byte) (int, error) {
	<-reader.release
	return 0, io.EOF
}

func (reader *blockingReader) Close() error {
	select {
	case <-reader.release:
	default:
		close(reader.release)
	}
	return nil
}

// A hung stdio server must not block Client.request forever: a cancelled
// per-call context must unblock the wait and surface ctx.Err(), and it must
// not hold shared client state that prevents a second caller from proceeding.
func TestClientRequestUnblocksOnContextCancel(t *testing.T) {
	reader := newBlockingReader()
	defer reader.Close()
	writes := make(chan struct{}, 2)

	client := &Client{
		reader: newMessageReader(reader),
		writer: newMessageWriter(&signalingWriter{writes: writes}),
		nextID: 1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.request(ctx, "tools/list", map[string]any{}, nil)
	}()

	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach the transport")
	}
	// The request is now parked waiting for a response that never comes.
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request() error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request() hung on a non-responsive server")
	}

	// The shared state must be free: prove a second live request reaches the
	// transport, then cancel it and verify cancellation releases the caller.
	secondCtx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	second := make(chan error, 1)
	go func() {
		second <- client.request(secondCtx, "tools/list", map[string]any{}, nil)
	}()
	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("second request did not reach the transport")
	}
	cancel2()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second request() error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second request() blocked behind the hung read")
	}
}

// Serve must honor context cancellation even when the input reader is parked in
// a blocking read, so a non-responsive peer cannot hang shutdown.
func TestServeReturnsOnContextCancelWithBlockingReader(t *testing.T) {
	reader := newBlockingReader()
	defer reader.Close()

	registry := tools.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, reader, io.Discard, registry, ServeOptions{})
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve() error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() hung on a non-responsive peer instead of honoring ctx")
	}
}
