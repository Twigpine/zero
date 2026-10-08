package dictation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/coder/websocket"
)

// OpenAI Realtime transcription-only wants 24kHz PCM16 (§6b); the recorder is
// built to match via the SampleRate hint.
const openAIRealtimeSampleRate = 24000

// OpenAIRealtimeConfig configures the OpenAI Realtime transcription transcriber
// (§6b, the credential-reuse streaming alternative). Reuses the OpenAI key.
type OpenAIRealtimeConfig struct {
	APIKey string
	Model  string // default "gpt-4o-transcribe"
	// BaseURL overrides the wss endpoint (tests point it at a fake server).
	BaseURL string
	// writeErrInjector, when set, is consulted before each JSON websocket write.
	// Used by same-package tests to force controllable write failures that embed
	// secrets; production callers leave it nil.
	writeErrInjector func() error
}

type openAIRealtimeTranscriber struct {
	cfg OpenAIRealtimeConfig
}

// NewOpenAIRealtimeTranscriber builds the OpenAI Realtime streaming transcriber.
func NewOpenAIRealtimeTranscriber(cfg OpenAIRealtimeConfig) (Transcriber, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, &SetupError{
			Tool: "OpenAI API key",
			Hint: "set an OpenAI API key (OPENAI_API_KEY, or `zero auth`) to use OpenAI Realtime streaming dictation",
		}
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-transcribe"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "wss://api.openai.com/v1/realtime?intent=transcription"
	}
	return &openAIRealtimeTranscriber{cfg: cfg}, nil
}

func (o *openAIRealtimeTranscriber) Transcribe(context.Context, []byte) (string, error) {
	return "", ErrStreamingUnsupported
}

func (o *openAIRealtimeTranscriber) SampleRate() int { return openAIRealtimeSampleRate }

func (o *openAIRealtimeTranscriber) StreamTranscribe(ctx context.Context, chunks <-chan []byte, onPartial func(string, bool)) (string, error) {
	conn, _, err := websocket.Dial(ctx, o.cfg.BaseURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + o.cfg.APIKey},
			"OpenAI-Beta":   {"realtime=v1"},
		},
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
		return "", fmt.Errorf("connecting to OpenAI Realtime: %s", providerio.Redact(err.Error(), o.cfg.APIKey))
	}
	defer conn.CloseNow()

	// Configure a transcription-only session (pcm16 in, gpt-4o-transcribe).
	sessionUpdate := map[string]any{
		"type": "transcription_session.update",
		"session": map[string]any{
			"input_audio_format": "pcm16",
			"input_audio_transcription": map[string]any{
				"model": o.cfg.Model,
			},
		},
	}
	if err := o.writeJSON(ctx, conn, sessionUpdate); err != nil {
		// Same cancellation short-circuit as the dial above: an Esc-abort
		// while the session update is in flight must stay context.Canceled.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("configuring OpenAI Realtime session: %s", providerio.Redact(err.Error(), o.cfg.APIKey))
	}

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
					// Commit the buffered audio to force final transcription.
					if err := o.writeJSON(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
						writeErrCh <- err
						// Unblock the reader: without a close, conn.Read stays
						// parked on a silent peer and the error is never drained
						// from writeErrCh. CloseNow aborts instead of waiting on
						// the close handshake, which a silent peer never answers.
						conn.CloseNow()
						return
					}
					writeErrCh <- nil
					return
				}
				appendMsg := map[string]any{
					"type":  "input_audio_buffer.append",
					"audio": base64.StdEncoding.EncodeToString(chunk),
				}
				if err := o.writeJSON(ctx, conn, appendMsg); err != nil {
					writeErrCh <- err
					// Same unblocking close as the commit path: a server that
					// goes silent after session setup would otherwise leave the
					// reader blocked in conn.Read with the error undrained.
					conn.CloseNow()
					return
				}
			}
		}
	}()

	// OpenAI deltas are incremental (append), unlike Deepgram/sherpa's cumulative
	// text. Accumulate completed segments plus the in-progress delta buffer.
	var finalized, current strings.Builder
	compose := func() string {
		return strings.TrimSpace(finalized.String() + current.String())
	}
	committed := false
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
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
			return compose(), fmt.Errorf("OpenAI Realtime stream error: %s", providerio.Redact(err.Error(), o.cfg.APIKey))
		}
		if typ != websocket.MessageText {
			continue
		}
		evt := parseRealtimeEvent(data)
		switch evt.kind {
		case realtimeDelta:
			current.WriteString(evt.text)
			if onPartial != nil {
				onPartial(compose(), false)
			}
			// onPartial may deliver the partial that makes the TUI cancel
			// (Esc). Observe that before the next Read, which can already
			// have a racing OpenAI error frame buffered.
			if ctx.Err() != nil {
				return compose(), ctx.Err()
			}
		case realtimeCompleted:
			// A completed item replaces the in-progress delta buffer with the
			// server's authoritative transcript for that segment.
			if evt.text != "" {
				if finalized.Len() > 0 {
					finalized.WriteString(" ")
				}
				finalized.WriteString(strings.TrimSpace(evt.text))
			}
			current.Reset()
			if onPartial != nil {
				onPartial(compose(), true)
			}
			if ctx.Err() != nil {
				return compose(), ctx.Err()
			}
			// Once we've committed the buffer (user stopped) and the server has
			// returned a completed transcription, the utterance is done.
			if committed {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return compose(), nil
			}
		case realtimeCommitted:
			committed = true
		case realtimeError:
			// Esc can race an incoming error event: conn.Read may already have
			// the OpenAI error in hand by the time cancellation lands. Key on
			// ctx.Err() first, same as every other return point in this loop,
			// so a cancel that wins the race still surfaces as context.Canceled
			// instead of a spurious redacted failure alongside it.
			if ctx.Err() != nil {
				return compose(), ctx.Err()
			}
			return compose(), fmt.Errorf("OpenAI Realtime error: %s", providerio.Redact(evt.text, o.cfg.APIKey))
		}
		// The writer signals commit completion out of band; observe it so a
		// stop with no further audio still flips `committed`.
		select {
		case werr := <-writeErrCh:
			if werr == nil {
				committed = true
			} else if ctx.Err() != nil {
				return compose(), ctx.Err()
			} else {
				return compose(), fmt.Errorf("OpenAI Realtime stream error: %s", providerio.Redact(werr.Error(), o.cfg.APIKey))
			}
		default:
		}
	}
}

type realtimeEventKind int

const (
	realtimeOther realtimeEventKind = iota
	realtimeDelta
	realtimeCompleted
	realtimeCommitted
	realtimeError
)

type realtimeEvent struct {
	kind realtimeEventKind
	text string
}

func parseRealtimeEvent(data []byte) realtimeEvent {
	var msg struct {
		Type       string `json:"type"`
		Delta      string `json:"delta"`
		Transcript string `json:"transcript"`
		Error      struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return realtimeEvent{kind: realtimeOther}
	}
	switch msg.Type {
	case "conversation.item.input_audio_transcription.delta":
		return realtimeEvent{kind: realtimeDelta, text: msg.Delta}
	case "conversation.item.input_audio_transcription.completed":
		return realtimeEvent{kind: realtimeCompleted, text: msg.Transcript}
	case "input_audio_buffer.committed":
		return realtimeEvent{kind: realtimeCommitted}
	case "error":
		return realtimeEvent{kind: realtimeError, text: msg.Error.Message}
	}
	return realtimeEvent{kind: realtimeOther}
}

func (o *openAIRealtimeTranscriber) writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	if o.cfg.writeErrInjector != nil {
		if err := o.cfg.writeErrInjector(); err != nil {
			return err
		}
	}
	return writeJSON(ctx, conn, v)
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
