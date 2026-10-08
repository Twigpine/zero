package dictation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// Non-cancellation dial failure: handshake returns 101 with an Upgrade header
// equal to the API key so the dial error embeds the key and redaction must strip it.
func TestDeepgramDialFailureRedactsKey(t *testing.T) {
	const key = "sk-test-dial-key-abcdef"
	baseURL := dialFailServerWithUpgrade(t, key)

	tr, err := NewDeepgramTranscriber(DeepgramConfig{APIKey: key, BaseURL: baseURL})
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
	if !strings.Contains(ferr.Error(), "connecting to Deepgram") {
		t.Errorf("expected dial-path error prefix, got: %v", ferr)
	}
}

// dialFailServerWithUpgrade returns a ws URL whose handshake responds 101 with
// Upgrade set to upgradeValue (not "websocket"), so websocket.Dial fails with
// that value in the error string. Hijacks and closes immediately to avoid the
// library's 3s body-drain timeout on failed dials.
func dialFailServerWithUpgrade(t *testing.T, upgradeValue string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		_, _ = bufrw.WriteString("Connection: Upgrade\r\n")
		_, _ = bufrw.WriteString("Upgrade: " + upgradeValue + "\r\n\r\n")
		_ = bufrw.Flush()
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestDeepgramStreamTranscribeErrorRedaction(t *testing.T) {
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		// Drain the client's audio frames, then wait for CloseStream so the
		// client has flushed its writes before we reject the connection. Closing
		// immediately (before CloseStream) risks the client failing on a write
		// error and never observing this API-key-bearing close reason.
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && strings.Contains(string(data), "CloseStream") {
				break
			}
		}
		c.Close(websocket.StatusPolicyViolation, "invalid key sk-test-key")
	})

	tr, err := NewDeepgramTranscriber(DeepgramConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 320)
	close(chunks)
	_, ferr := tr.StreamTranscribe(context.Background(), chunks, func(string, bool) {})
	if ferr == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(ferr.Error(), "sk-test-key") {
		t.Errorf("API key leaked: %v", ferr)
	}
}

func TestDeepgramStreamTranscribeCancelKeepsSentinel(t *testing.T) {
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

	tr, err := NewDeepgramTranscriber(DeepgramConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	defer close(chunks)
	chunks <- make([]byte, 320)
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
func TestDeepgramStreamTranscribeDialCancelKeepsSentinel(t *testing.T) {
	tr, err := NewDeepgramTranscriber(DeepgramConfig{
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

// Esc right after accept can race the first write or the first Read; both
// setup/write redaction sites must still yield context.Canceled.
func TestDeepgramStreamTranscribeStartupCancelKeepsSentinel(t *testing.T) {
	accepted := make(chan struct{})
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		close(accepted)
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})

	tr, err := NewDeepgramTranscriber(DeepgramConfig{APIKey: "sk-test-key", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan []byte, 1)
	defer close(chunks)
	// No frames yet: the writer blocks on chunks or the reader blocks on Read.
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

// TestDeepgramStreamErrorRedactsHeaderShapedKey exercises the same stream-error
// redaction path as TestDeepgramStreamTranscribeErrorRedaction, with a close
// reason that embeds the key next to an X-Api-Key-shaped header label.
func TestDeepgramStreamErrorRedactsHeaderShapedKey(t *testing.T) {
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && strings.Contains(string(data), "CloseStream") {
				break
			}
		}
		c.Close(websocket.StatusPolicyViolation, "X-Api-Key: sk-custom-secret-key-1234567890 failed")
	})

	tr, err := NewDeepgramTranscriber(DeepgramConfig{APIKey: "sk-custom-secret-key-1234567890", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 320)
	close(chunks)
	_, ferr := tr.StreamTranscribe(context.Background(), chunks, func(string, bool) {})
	if ferr == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(ferr.Error(), "sk-custom-secret-key-1234567890") {
		t.Errorf("Custom header API key leaked: %v", ferr)
	}
}

func TestDeepgramSinglePassRedaction(t *testing.T) {
	url := wsTestServer(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && strings.Contains(string(data), "CloseStream") {
				break
			}
		}
		c.Close(websocket.StatusPolicyViolation, "error with sk-key1234567890abcdef1234")
	})

	tr, err := NewDeepgramTranscriber(DeepgramConfig{APIKey: "sk-key1234567890abcdef1234", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte, 1)
	chunks <- make([]byte, 320)
	close(chunks)
	_, ferr := tr.StreamTranscribe(context.Background(), chunks, func(string, bool) {})
	if ferr == nil {
		t.Fatal("expected error")
	}
	got := ferr.Error()
	if strings.Contains(got, "sk-key1234567890abcdef1234") {
		t.Fatalf("API key leaked into stream error: %q", got)
	}
	// One key occurrence must produce exactly one redaction marker (no double wrap).
	if strings.Count(got, "[REDACTED]") != 1 {
		t.Fatalf("redaction markers in %q: want exactly 1 [REDACTED]", got)
	}
}
