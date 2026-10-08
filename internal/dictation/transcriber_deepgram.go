package dictation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/coder/websocket"
)

// DeepgramConfig configures the Deepgram streaming transcriber (§6b, default
// cloud streaming provider). The key is resolved by the caller.
type DeepgramConfig struct {
	APIKey     string
	Model      string // default "nova-3"
	Language   string
	SampleRate int // capture rate; default DefaultSampleRate (16kHz)
	// BaseURL overrides the wss endpoint (tests point it at a fake server).
	BaseURL string
}

type deepgramTranscriber struct {
	cfg DeepgramConfig
}

// NewDeepgramTranscriber builds the Deepgram streaming transcriber.
func NewDeepgramTranscriber(cfg DeepgramConfig) (Transcriber, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, &SetupError{
			Tool: "Deepgram API key",
			Hint: "set a Deepgram API key (DEEPGRAM_API_KEY, or `zero auth`) to use Deepgram streaming dictation",
		}
	}
	if cfg.Model == "" {
		cfg.Model = "nova-3"
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = DefaultSampleRate
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "wss://api.deepgram.com/v1/listen"
	}
	return &deepgramTranscriber{cfg: cfg}, nil
}

// Transcribe is unsupported — Deepgram is used only on the streaming path here.
func (d *deepgramTranscriber) Transcribe(context.Context, []byte) (string, error) {
	return "", ErrStreamingUnsupported
}

func (d *deepgramTranscriber) SampleRate() int { return d.cfg.SampleRate }

func (d *deepgramTranscriber) StreamTranscribe(ctx context.Context, chunks <-chan []byte, onPartial func(string, bool)) (string, error) {
	endpoint := d.buildURL()
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Token " + d.cfg.APIKey}},
	})
	if err != nil {
		// Preserve the context.Canceled sentinel (it carries no key) so an
		// Esc-abort during dial still matches the UI's errors.Is check. Key
		// on ctx.Err() itself rather than unwrapping err: a cancellation can
		// surface as a plain transport error (e.g. "closed network
		// connection") rather than context.Canceled directly.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("connecting to Deepgram: %s", providerio.Redact(err.Error(), d.cfg.APIKey))
	}
	defer conn.CloseNow()

	// Deepgram consumes raw linear16 (int16) PCM — the recorder's native format,
	// so no conversion (unlike sherpa-onnx, which wants float32).
	writeErrCh := make(chan error, 1)
	go func() {
		// Select on ctx.Done() while waiting for the next chunk so a cancelled
		// session does not leave this goroutine blocked on an open idle channel.
		for {
			select {
			case <-ctx.Done():
				writeErrCh <- ctx.Err()
				return
			case chunk, ok := <-chunks:
				if !ok {
					// CloseStream flushes any buffered audio and returns final
					// results before Deepgram closes the socket.
					writeErrCh <- conn.Write(ctx, websocket.MessageText, []byte(`{"type":"CloseStream"}`))
					return
				}
				if err := conn.Write(ctx, websocket.MessageBinary, chunk); err != nil {
					writeErrCh <- err
					return
				}
			}
		}
	}()

	// Deepgram results are per-utterance segments, not cumulative: accumulate the
	// is_final segments and append the current interim to build the full text.
	var finals []string
	var interim string
	compose := func() string {
		return strings.TrimSpace(strings.Join(append(finals, interim), " "))
	}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// A clean close after CloseStream is success, not failure.
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return compose(), nil
			}
			select {
			case werr := <-writeErrCh:
				if werr != nil {
					err = werr
				}
			default:
			}
			// A user abort cancels the streaming context; the UI matches it
			// with errors.Is(err, context.Canceled), so return the sentinel
			// itself (it carries no key) instead of a flat redacted string.
			// Key on ctx.Err() rather than unwrapping err: the writeErrCh
			// swap above can replace err with a plain transport error (e.g.
			// "closed network connection") that doesn't itself unwrap to
			// context.Canceled even though the cancellation is what caused it.
			if ctx.Err() != nil {
				return compose(), ctx.Err()
			}
			//nolint:staticcheck // Preserve established user-facing error text.
			return compose(), fmt.Errorf("Deepgram stream error: %s", providerio.Redact(err.Error(), d.cfg.APIKey))
		}
		if typ != websocket.MessageText {
			continue
		}
		transcript, isFinal, ok := parseDeepgramResult(data)
		if !ok {
			continue
		}
		if isFinal {
			if transcript != "" {
				finals = append(finals, transcript)
			}
			interim = ""
		} else {
			interim = transcript
		}
		if onPartial != nil {
			onPartial(compose(), isFinal)
		}
	}
}

func (d *deepgramTranscriber) buildURL() string {
	q := url.Values{}
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(d.cfg.SampleRate))
	q.Set("channels", "1")
	q.Set("interim_results", "true")
	q.Set("punctuate", "true")
	q.Set("model", d.cfg.Model)
	if d.cfg.Language != "" {
		q.Set("language", d.cfg.Language)
	}
	return d.cfg.BaseURL + "?" + q.Encode()
}

// parseDeepgramResult extracts the transcript and is_final flag from a Deepgram
// "Results" message: channel.alternatives[0].transcript.
func parseDeepgramResult(data []byte) (transcript string, isFinal bool, ok bool) {
	var msg struct {
		Type    string `json:"type"`
		IsFinal bool   `json:"is_final"`
		Channel struct {
			Alternatives []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
		} `json:"channel"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return "", false, false
	}
	// Only "Results" messages carry transcripts; ignore Metadata/UtteranceEnd/etc.
	if msg.Type != "" && msg.Type != "Results" {
		return "", false, false
	}
	if len(msg.Channel.Alternatives) == 0 {
		return "", false, false
	}
	return strings.TrimSpace(msg.Channel.Alternatives[0].Transcript), msg.IsFinal, true
}
