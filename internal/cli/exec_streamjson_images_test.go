package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// capturingImageProvider records the images carried on the last user turn of the
// request it receives, so a test can assert what reached agent.Options.Images
// end-to-end (the agent seeds Options.Images onto the initial user message).
type capturingImageProvider struct {
	mu     sync.Mutex
	images []zeroruntime.ImageBlock
	prompt string
}

func (provider *capturingImageProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.mu.Lock()
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == zeroruntime.MessageRoleUser {
			provider.images = append([]zeroruntime.ImageBlock(nil), request.Messages[index].Images...)
			provider.prompt = request.Messages[index].Content
			break
		}
	}
	provider.mu.Unlock()

	ch := make(chan zeroruntime.StreamEvent, 2)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "ok"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func TestRunExecImageAttachmentHistory(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		fileImage   bool
	}{
		{name: "vision", model: "gpt-4.1"},
		{name: "ignored by model", model: "gpt-3.5-turbo"},
		{name: "stream and file images", model: "gpt-4.1", fileImage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The shared isolation also turns off the models.dev refresh that
			// runExec starts in the background. Without it, the refresh downloads
			// the catalog into this HOME and races the TempDir cleanup.
			isolateCLIUserState(t)
			cwd := t.TempDir()
			png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 0x49, 0x48, 0x44, 0x52}
			encoded := base64.StdEncoding.EncodeToString(png)
			input := `{"schemaVersion":2,"type":"message","role":"user","content":"","images":[{"mediaType":"image/png","data":"` + encoded + `"}]}` + "\n"
			provider := &capturingImageProvider{}
			deps := appDeps{
				stdin: strings.NewReader(input),
				getwd: func() (string, error) { return cwd, nil },
				resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
					cfg := execResolvedConfig()
					cfg.Provider.Model = tc.model
					return cfg, nil
				},
				newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) { return provider, nil },
			}
			var stdout, stderr bytes.Buffer
			args := []string{"exec", "--input-format", "stream-json", "--init-session-id", "image_history"}
			wantImages := 1
			if tc.fileImage {
				if err := os.WriteFile(filepath.Join(cwd, "shot.png"), png, 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--image", "shot.png")
				wantImages++
			}
			code := runWithDeps(args, &stdout, &stderr, deps)
			if code != exitSuccess {
				t.Fatalf("image-only exec exit=%d stderr=%s", code, stderr.String())
			}
			provider.mu.Lock()
			liveImages := len(provider.images)
			provider.mu.Unlock()
			wantLiveImages := wantImages
			if tc.model == "gpt-3.5-turbo" {
				wantLiveImages = 0
			}
			if liveImages != wantLiveImages {
				t.Fatalf("live images = %d, want %d", liveImages, wantLiveImages)
			}
			store := sessions.NewStore(sessions.StoreOptions{})
			events, err := store.ReadEvents("image_history")
			if err != nil || len(events) == 0 {
				t.Fatalf("read exec events: %v, %v", events, err)
			}
			var user struct {
				Content     string `json:"content"`
				Attachments struct {
					Images int `json:"images"`
				} `json:"attachments"`
			}
			if err := json.Unmarshal(events[0].Payload, &user); err != nil {
				t.Fatal(err)
			}
			if user.Content != "" || user.Attachments.Images != wantLiveImages {
				t.Errorf("stored exec user event = %s, want empty content and attachments.images=%d", events[0].Payload, wantLiveImages)
			}
			for _, event := range events {
				if bytes.Contains(event.Payload, []byte(encoded)) {
					t.Errorf("image data leaked into session: %s", event.Payload)
				}
			}
			stdout.Reset()
			stderr.Reset()
			deps.stdin = strings.NewReader("")
			code = runWithDeps([]string{"exec", "--resume", "image_history", "what did I send?"}, &stdout, &stderr, deps)
			if code != exitSuccess {
				t.Fatalf("resume exec exit=%d stderr=%s", code, stderr.String())
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			marker := "[1 image]"
			if wantImages > 1 {
				marker = fmt.Sprintf("[%d images]", wantImages)
			}
			if hasMarker := strings.Contains(provider.prompt, marker); hasMarker != (wantLiveImages > 0) {
				t.Errorf("resume marker %q present = %v, want %v: %q", marker, hasMarker, wantLiveImages > 0, provider.prompt)
			}
			if len(provider.images) != 0 {
				t.Errorf("resume reattached %d historical images", len(provider.images))
			}
		})
	}
}

// TestRunExecStreamJSONImageReachesAgent locks the end-to-end wiring: a base64
// image carried on a stream-json `message` event, fed via stdin on a vision
// model, must reach agent.Options.Images (and therefore the provider request's
// user turn). Before the fix, stream-json images were parsed and validated but
// silently dropped — never threaded into the agent run.
func TestRunExecStreamJSONImageReachesAgent(t *testing.T) {
	isolateCLIUserState(t)
	cwd := t.TempDir()

	// A minimal PNG, base64-encoded for the stream-json image payload.
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	}
	encoded := base64.StdEncoding.EncodeToString(png)
	input := `{"schemaVersion":2,"type":"message","role":"user","content":"describe this","images":[{"mediaType":"image/png","data":"` + encoded + `"}]}` + "\n"

	provider := &capturingImageProvider{}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--cwd", cwd,
		"--input-format", "stream-json",
		// gpt-4.1 is a registry-known vision model, so the gate keeps the image.
		"--model", "gpt-4.1",
	}, &stdout, &stderr, appDeps{
		stdin: strings.NewReader(input),
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "gpt-4.1"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			return config.ResolvedConfig{
				ActiveProvider: "echo",
				Provider: config.ProviderProfile{
					Name:         "echo",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        model,
				},
				MaxTurns: 3,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return provider, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.images) != 1 {
		t.Fatalf("expected 1 image to reach the agent run, got %d (stderr=%q)", len(provider.images), stderr.String())
	}
	if provider.images[0].MediaType != "image/png" {
		t.Fatalf("image media type = %q, want image/png", provider.images[0].MediaType)
	}
	if !bytes.Equal(provider.images[0].Data, png) {
		t.Fatalf("image bytes = %v, want decoded png", provider.images[0].Data)
	}
}

// TestRunExecStreamJSONImageOnlyMessageProceeds locks that an IMAGE-ONLY turn
// (a message event with EMPTY content but at least one image) is accepted: the
// run proceeds with an empty prompt and the image still reaches the agent. Before
// the fix, ResolvePrompt's "must include at least one prompt or user message
// event" error was returned before images were considered, rejecting the turn.
func TestRunExecStreamJSONImageOnlyMessageProceeds(t *testing.T) {
	isolateCLIUserState(t)
	cwd := t.TempDir()

	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	}
	encoded := base64.StdEncoding.EncodeToString(png)
	// Empty content, one image: the image-only turn.
	input := `{"schemaVersion":2,"type":"message","role":"user","content":"","images":[{"mediaType":"image/png","data":"` + encoded + `"}]}` + "\n"

	provider := &capturingImageProvider{}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--cwd", cwd,
		"--input-format", "stream-json",
		"--model", "gpt-4.1",
	}, &stdout, &stderr, appDeps{
		stdin: strings.NewReader(input),
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "gpt-4.1"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			return config.ResolvedConfig{
				ActiveProvider: "echo",
				Provider: config.ProviderProfile{
					Name:         "echo",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        model,
				},
				MaxTurns: 3,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return provider, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("image-only turn rejected: exit code %d (want %d): %s", exitCode, exitSuccess, stderr.String())
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.images) != 1 {
		t.Fatalf("expected 1 image to reach the agent run, got %d (stderr=%q)", len(provider.images), stderr.String())
	}
	if provider.images[0].MediaType != "image/png" || !bytes.Equal(provider.images[0].Data, png) {
		t.Fatalf("image not threaded: %#v", provider.images[0])
	}
}
