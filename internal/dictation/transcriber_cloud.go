package dictation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/Twigpine/zero/internal/providers/providerio"
)

// CloudConfig configures a batch cloud transcriber. Groq and OpenAI share the
// OpenAI-compatible /audio/transcriptions multipart endpoint — the design's
// "each owns its endpoint/model specifics while sharing the request shape"
// (§6). Key and base URL are resolved by the caller (TUI layer) from
// config/credstore, keeping this type decoupled and unit-testable.
type CloudConfig struct {
	Provider string // ProviderGroq or ProviderOpenAI, for error messages
	BaseURL  string // e.g. https://api.groq.com/openai/v1
	APIKey   string
	Model    string // e.g. whisper-large-v3-turbo, whisper-1
	// Language optionally constrains recognition (ISO-639-1). Empty = auto.
	Language string

	// HTTPClient is injectable for tests; nil uses providerio's shared,
	// stall-hardened client.
	HTTPClient *http.Client
}

type cloudTranscriber struct {
	cfg CloudConfig
}

// NewCloudTranscriber builds a batch transcriber for Groq or OpenAI.
func NewCloudTranscriber(cfg CloudConfig) (Transcriber, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, &SetupError{
			Tool: cfg.Provider + " API key",
			Hint: fmt.Sprintf("set a %s API key (run `zero auth`) to use cloud dictation", cfg.Provider),
		}
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("dictation: no model configured for provider %q", cfg.Provider)
	}
	base, err := providerio.NormalizeBaseURL(cfg.BaseURL, cfg.BaseURL, cfg.Provider)
	if err != nil {
		return nil, err
	}
	cfg.BaseURL = base
	return &cloudTranscriber{cfg: cfg}, nil
}

func (c *cloudTranscriber) Transcribe(ctx context.Context, audio []byte) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("no audio to transcribe")
	}
	body, contentType, err := c.buildMultipart(audio)
	if err != nil {
		return "", err
	}
	endpoint := c.cfg.BaseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := providerio.HTTPClient(c.cfg.HTTPClient).Do(req)
	if err != nil {
		return "", fmt.Errorf("%s transcription request failed: %w", c.cfg.Provider, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := providerio.ClassifiedError(resp.StatusCode, strings.TrimSpace(string(respBody)), c.cfg.APIKey)
		// A 401/403 is a fixable credential problem: surface it as a typed AuthError
		// so the TUI can offer an inline key prompt instead of a dead-end message.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", &AuthError{Provider: c.cfg.Provider, Message: msg}
		}
		return "", errors.New(msg)
	}
	var parsed struct {
		Text  string `json:"text"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("%s transcription: could not parse response: %w", c.cfg.Provider, err)
	}
	if parsed.Error.Message != "" {
		return "", fmt.Errorf("%s transcription error: %s", c.cfg.Provider, providerio.Redact(parsed.Error.Message, c.cfg.APIKey))
	}
	return strings.TrimSpace(parsed.Text), nil
}

// buildMultipart assembles the transcription upload. The file field name and
// the "model"/"response_format" fields match the OpenAI audio API that Groq
// mirrors. The filename's extension tells the server the container type —
// SniffFormat keeps WAV/M4A honest so Termux's AAC uploads work.
func (c *cloudTranscriber) buildMultipart(audio []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", SniffFormat(audio).FileName())
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return nil, "", err
	}
	if err := w.WriteField("model", c.cfg.Model); err != nil {
		return nil, "", err
	}
	if err := w.WriteField("response_format", "json"); err != nil {
		return nil, "", err
	}
	if c.cfg.Language != "" {
		if err := w.WriteField("language", c.cfg.Language); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// StreamTranscribe is not supported for the batch cloud providers (Groq has no
// streaming product; OpenAI batch is one-shot). Streaming uses the dedicated
// Deepgram/OpenAI-Realtime transcribers instead.
func (c *cloudTranscriber) StreamTranscribe(context.Context, <-chan []byte, func(string, bool)) (string, error) {
	return "", ErrStreamingUnsupported
}

// ErrStreamingUnsupported reports that a batch-only Transcriber cannot stream;
// the caller falls back to the batch pipeline.
var ErrStreamingUnsupported = errors.New("this transcription provider does not support streaming")
