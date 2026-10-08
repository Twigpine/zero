package dictation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Non-cancellation dial failure: handshake returns 101 with an Upgrade header
// equal to the API key so the dial error embeds the key and redaction must strip it.
func TestOpenAIRealtimeDialFailureRedactsKey(t *testing.T) {
	const key = "sk-test-dial-key-abcdef"
	baseURL := dialFailServerWithUpgrade(t, key)

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{APIKey: key, BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte)
	close(chunks)
	_, ferr := tr.StreamTranscribe(context.Background(), chunks, nil)
	if ferr == nil {
		t.Fatal("expected dial failure")
	}
	if errors.Is(ferr, context.Canceled) {
		t.Fatalf("dial failure should not be context.Canceled: %v", ferr)
	}
	if strings.Contains(ferr.Error(), key) {
		t.Errorf("API key leaked from dial failure: %v", ferr)
	}
	if !strings.Contains(ferr.Error(), "connecting to OpenAI Realtime") {
		t.Errorf("expected dial-path error prefix, got: %v", ferr)
	}
}

// Session-configuration write fails after dial: inject a write error that embeds
// the API key (peer close cannot reach this path reliably — the session write
// always lands in the kernel buffer first) and assert redaction.
func TestOpenAIRealtimeSessionConfigFailureRedactsKey(t *testing.T) {
	const key = "sk-test-session-key-abcdef"
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		// Hold open; the client fails on the injected session write before reading.
		<-ctx.Done()
	})

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{
		APIKey:  key,
		BaseURL: url,
		writeErrInjector: func() error {
			return errors.New("invalid API key " + key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte)
	close(chunks)
	_, ferr := tr.StreamTranscribe(context.Background(), chunks, nil)
	if ferr == nil {
		t.Fatal("expected session-config failure")
	}
	if errors.Is(ferr, context.Canceled) {
		t.Fatalf("session-config failure should not be context.Canceled: %v", ferr)
	}
	if strings.Contains(ferr.Error(), key) {
		t.Errorf("API key leaked from session-config failure: %v", ferr)
	}
	if !strings.Contains(ferr.Error(), "configuring OpenAI Realtime session") {
		t.Errorf("expected session-config error prefix, got: %v", ferr)
	}
}

// Asynchronous writer failure: session update succeeds, then the audio writer
// injects a key-bearing write error. The server keeps the connection open and
// emits a delta so the reader loop observes writeErrCh (not a peer close).
func TestOpenAIRealtimeWriterFailureRedactsKey(t *testing.T) {
	const key = "sk-test-writer-key-abcdef"
	sessionDone := make(chan struct{})
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		close(sessionDone)
		// Keep reading and emit a delta so the client processes an event and
		// drains writeErrCh while the connection is still open.
		_ = c.Write(ctx, websocket.MessageText, []byte(
			`{"type":"conversation.item.input_audio_transcription.delta","delta":"hi"}`,
		))
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})

	var writes atomic.Int32
	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{
		APIKey:  key,
		BaseURL: url,
		writeErrInjector: func() error {
			// First write is session.update; fail subsequent audio/commit writes.
			if writes.Add(1) == 1 {
				return nil
			}
			return errors.New("invalid API key " + key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 480)
	// Leave open after the first frame so the writer hits the injected error
	// on that append rather than a clean commit.

	done := make(chan error, 1)
	go func() {
		_, ferr := tr.StreamTranscribe(context.Background(), chunks, func(string, bool) {})
		done <- ferr
	}()

	var ferr error
	select {
	case ferr = <-done:
	case <-time.After(5 * time.Second):
		close(chunks)
		t.Fatal("StreamTranscribe did not complete after writer failure")
	}
	close(chunks)

	if ferr == nil {
		t.Fatal("expected writer failure")
	}
	if errors.Is(ferr, context.Canceled) {
		t.Fatalf("writer failure should not be context.Canceled: %v", ferr)
	}
	if strings.Contains(ferr.Error(), key) {
		t.Errorf("API key leaked from writer failure: %v", ferr)
	}
	if !strings.Contains(ferr.Error(), "OpenAI Realtime stream error") {
		t.Errorf("expected stream/writer error prefix, got: %v", ferr)
	}
	select {
	case <-sessionDone:
	default:
		// Session may race the injected audio write; prefix assertion above is enough.
	}
}

// Writer failure with a silent server: the peer never sends a frame after
// session setup, so the reader is parked in conn.Read. The writer must close
// the connection on failure or the writeErrCh error is never observed and
// StreamTranscribe blocks forever.
func TestOpenAIRealtimeWriterFailureUnblocksSilentServer(t *testing.T) {
	const key = "sk-test-silent-writer-key-abcdef"
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		// Hold the connection open without ever writing a frame.
		<-ctx.Done()
	})

	var writes atomic.Int32
	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{
		APIKey:  key,
		BaseURL: url,
		writeErrInjector: func() error {
			// First write is session.update; fail the subsequent append write.
			if writes.Add(1) == 1 {
				return nil
			}
			return errors.New("invalid API key " + key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 480)
	// Leave the channel open: the writer must fail on the append, not the commit.
	done := make(chan error, 1)
	go func() {
		_, ferr := tr.StreamTranscribe(ctx, chunks, func(string, bool) {})
		done <- ferr
	}()

	select {
	case ferr := <-done:
		if ferr == nil {
			t.Fatal("expected writer failure")
		}
		if errors.Is(ferr, context.Canceled) {
			t.Fatalf("writer failure should not be context.Canceled: %v", ferr)
		}
		if strings.Contains(ferr.Error(), key) {
			t.Errorf("API key leaked from writer failure: %v", ferr)
		}
		if !strings.Contains(ferr.Error(), "OpenAI Realtime stream error") {
			t.Errorf("expected stream/writer error prefix, got: %v", ferr)
		}
	case <-time.After(5 * time.Second):
		close(chunks)
		t.Fatal("StreamTranscribe did not unblock after writer failure with a silent server")
	}
}

func TestOpenAIRealtimeStreamTranscribeErrorRedaction(t *testing.T) {
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		defer c.Close(websocket.StatusNormalClosure, "")
		for {
			typ, _, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","error":{"message":"invalid API key sk-test-key"}}`))
		}
	})

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 480)
	close(chunks)
	_, ferr := tr.StreamTranscribe(context.Background(), chunks, func(string, bool) {})
	if ferr == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(ferr.Error(), "sk-test-key") {
		t.Errorf("API key leaked: %v", ferr)
	}
}

func TestOpenAIRealtimeStreamTranscribeCancelKeepsSentinel(t *testing.T) {
	firstFrame := make(chan struct{})
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		// Hold the connection open, never answering, so the client blocks in
		// Read until its context is cancelled (the Esc-abort path).
		var once sync.Once
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
			once.Do(func() { close(firstFrame) })
		}
	})

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	defer close(chunks)
	chunks <- make([]byte, 480)
	// The channel stays open: the session is live when the user aborts.
	errCh := make(chan error, 1)
	go func() {
		_, ferr := tr.StreamTranscribe(ctx, chunks, nil)
		errCh <- ferr
	}()

	select {
	case <-firstFrame:
		cancel()
		ferr := <-errCh
		if !errors.Is(ferr, context.Canceled) {
			t.Fatalf("cancelled stream error lost the context.Canceled sentinel: %v", ferr)
		}
	case ferr := <-errCh:
		t.Fatalf("StreamTranscribe failed early instead of blocking: %v", ferr)
	}
}

// Esc can cancel while the WebSocket dial is still pending. The dial error is
// redacted with %s, so we must return ctx.Err() rather than a flat string.
func TestOpenAIRealtimeStreamTranscribeDialCancelKeepsSentinel(t *testing.T) {
	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{
		APIKey:  "sk-test-key",
		BaseURL: "ws://127.0.0.1:1", // never reached; ctx is already cancelled
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chunks := make(chan []byte)
	defer close(chunks)

	_, ferr := tr.StreamTranscribe(ctx, chunks, nil)
	if !errors.Is(ferr, context.Canceled) {
		t.Fatalf("dial-cancel lost the context.Canceled sentinel: %v", ferr)
	}
}

// Esc right after accept can hit the session-update write or the first Read;
// both redaction sites must still yield context.Canceled.
func TestOpenAIRealtimeStreamTranscribeStartupCancelKeepsSentinel(t *testing.T) {
	accepted := make(chan struct{})
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		close(accepted)
		// Do not read: leave the client in session-update write or first Read.
		<-ctx.Done()
	})

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	defer close(chunks)
	errCh := make(chan error, 1)
	go func() {
		_, ferr := tr.StreamTranscribe(ctx, chunks, nil)
		errCh <- ferr
	}()

	select {
	case <-accepted:
		cancel()
		ferr := <-errCh
		if !errors.Is(ferr, context.Canceled) {
			t.Fatalf("startup-cancel lost the context.Canceled sentinel: %v", ferr)
		}
	case ferr := <-errCh:
		t.Fatalf("StreamTranscribe failed before accept: %v", ferr)
	}
}

// Cancel while the writer is blocked on more chunks. Whichever path observes
// the cancellation first (the blocked conn.Read or the writeErrCh drain) must
// return context.Canceled rather than a redacted flat string.
func TestOpenAIRealtimeStreamTranscribeWriteCancelKeepsSentinel(t *testing.T) {
	sessionReceived := make(chan struct{})
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		close(sessionReceived)
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 480)
	defer close(chunks)

	errCh := make(chan error, 1)
	go func() {
		_, ferr := tr.StreamTranscribe(ctx, chunks, nil)
		errCh <- ferr
	}()

	select {
	case <-sessionReceived:
		cancel()
		ferr := <-errCh
		if !errors.Is(ferr, context.Canceled) {
			t.Fatalf("write-path cancel lost the context.Canceled sentinel: %v", ferr)
		}
	case ferr := <-errCh:
		t.Fatalf("StreamTranscribe failed early instead of blocking: %v", ferr)
	}
}

// Esc can race an incoming OpenAI error event. conn.Read may already have
// the error frame in hand by the time the cancellation lands. The server
// fires a delta immediately followed by the error, back to back. Cancel from
// inside the delta callback (same path the TUI uses when a partial triggers
// Esc handling) so the cancel is observed before the next event is processed.
// The returned error must still be context.Canceled, not the redacted OpenAI
// error.
func TestOpenAIRealtimeStreamTranscribeErrorRaceKeepsSentinel(t *testing.T) {
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(
			`{"type":"conversation.item.input_audio_transcription.delta","delta":"hi"}`,
		))
		_ = c.Write(ctx, websocket.MessageText, []byte(
			`{"type":"error","error":{"message":"invalid API key sk-test-key"}}`,
		))
		<-ctx.Done()
	})

	tr, err := NewOpenAIRealtimeTranscriber(OpenAIRealtimeConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	defer close(chunks)
	chunks <- make([]byte, 480)
	var once sync.Once
	_, ferr := tr.StreamTranscribe(ctx, chunks, func(string, bool) {
		// Cancel synchronously from the partial callback, before the
		// stream loop continues to the already-buffered error frame.
		once.Do(cancel)
	})
	if !errors.Is(ferr, context.Canceled) {
		t.Fatalf("StreamTranscribe error = %v, want context.Canceled (cancel must win over a racing OpenAI error event)", ferr)
	}
}
