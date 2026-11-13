package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestACPImageAttachmentHistory(t *testing.T) {
	for _, tc := range []struct {
		name          string
		text          string
		images        int
		withoutVision bool
		want          string
	}{
		{name: "image only", images: 1, want: "[1 image]"},
		{name: "text and images", text: "compare these", images: 2, want: "compare these [2 images]"},
		{name: "ignored by model", images: 1, withoutVision: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			deps.DiscoverModels = func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
				modalities := []string{"text", "image"}
				if tc.withoutVision {
					modalities = []string{"text"}
				}
				return []providermodeldiscovery.Model{{ID: "fake-model", InputModalities: modalities}}, nil
			}
			type observedTurn struct {
				prompt string
				images int
			}
			turns := make(chan observedTurn, 8)
			deps.RunAgent = func(_ context.Context, prompt string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
				turns <- observedTurn{prompt: prompt, images: len(opts.Images)}
				return agent.Result{FinalAnswer: "received"}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			workspace := t.TempDir()
			h := newHarness(t, deps)
			defer h.stop()
			var created NewSessionResult
			if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace}, &created); err != nil {
				t.Fatal(err)
			}
			const imageData = "image-history-bytes-not-for-storage"
			encoded := base64.StdEncoding.EncodeToString([]byte(imageData))
			var blocks []ContentBlock
			if tc.text != "" {
				blocks = append(blocks, TextBlock(tc.text))
			}
			for range tc.images {
				blocks = append(blocks, ImageBlock(encoded, "image/png"))
			}
			if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: blocks}, &PromptResult{}); err != nil {
				t.Fatal(err)
			}
			live := <-turns
			wantLiveImages := tc.images
			if tc.withoutVision {
				wantLiveImages = 0
			}
			if live.images != wantLiveImages {
				t.Fatalf("live images = %d, want %d", live.images, wantLiveImages)
			}
			events, err := deps.Store.ReadEvents(created.SessionID)
			if err != nil || len(events) < 2 {
				t.Fatalf("read completed turn: events=%+v err=%v", events, err)
			}
			var user struct {
				Role        string `json:"role"`
				Content     string `json:"content"`
				Attachments struct {
					Images int `json:"images"`
				} `json:"attachments"`
			}
			if err := json.Unmarshal(events[0].Payload, &user); err != nil {
				t.Fatal(err)
			}
			if user.Role != "user" || user.Content != tc.text || user.Attachments.Images != wantLiveImages {
				t.Errorf("stored user event = %s, want original text and attachments.images=%d", events[0].Payload, wantLiveImages)
			}
			for _, event := range events {
				if bytes.Contains(event.Payload, []byte(encoded)) || bytes.Contains(event.Payload, []byte(imageData)) {
					t.Errorf("image data leaked into session event: %s", event.Payload)
				}
			}
			followUp := func(t *testing.T, client *clientHarness) {
				t.Helper()
				if err := client.client.Call(ctx, MethodSessionPrompt, PromptParams{
					SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("what did I send?")},
				}, &PromptResult{}); err != nil {
					t.Fatal(err)
				}
				got := <-turns
				if want := "User: " + tc.want + "\n"; !strings.Contains(got.prompt, want) {
					t.Errorf("seeded prompt missing user history %q: %q", want, got.prompt)
				}
				if got.images != 0 {
					t.Errorf("historical images reattached: %d", got.images)
				}
			}
			followUp(t, h)
			h.stop()
			// Fresh agents and stores reproduce a process restart, with no shared
			// in-memory history to hide a missing attachment in the event log.
			for _, method := range []string{MethodSessionLoad, MethodSessionResume} {
				t.Run(method, func(t *testing.T) {
					restoredDeps := deps
					restoredDeps.Store = sessions.NewStore(sessions.StoreOptions{RootDir: deps.Store.RootDir})
					loader := newHarness(t, restoredDeps)
					defer loader.stop()
					if err := loader.client.Call(ctx, method, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace}, &LoadSessionResult{}); err != nil {
						t.Fatal(err)
					}
					if method == MethodSessionLoad {
						select {
						case update := <-loader.notifications:
							if update.SessionUpdate != UpdateUserMessageChunk || update.Content.Text != tc.want {
								t.Errorf("replayed user = %+v, want %q", update, tc.want)
							}
						case <-ctx.Done():
							t.Fatal("user turn was not replayed")
						}
					} else {
						select {
						case update := <-loader.notifications:
							t.Errorf("resume replayed transcript: %+v", update)
						default:
						}
					}
					followUp(t, loader)
				})
			}
		})
	}
}

func TestACPAttachmentHistoryReadsExistingSessionMessages(t *testing.T) {
	deps := testDeps(t)
	meta, err := deps.Store.Create(sessions.CreateInput{SessionID: "attachment-compatibility", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for i, payload := range []string{
		`{"role":"user","content":"old text"}`,
		`{"role":"user","content":"","attachments":{"images":1,"documents":2}}`,
		`{"role":"user","content":"still text","attachments":{"images":-1,"documents":1.5}}`,
		`{"role":"assistant","content":"answer","attachments":{"images":1}}`,
	} {
		if _, err := deps.Store.AppendEvent(meta.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: json.RawMessage(payload)}); err != nil {
			t.Fatalf("append message %d: %v", i, err)
		}
	}
	a := &Agent{deps: deps}
	_, messages, _, err := a.loadHistory(meta.SessionID, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"old text", "[1 image] [2 documents]", "still text", "answer"}
	if len(messages) != len(want) {
		t.Fatalf("messages = %+v", messages)
	}
	for i, text := range want {
		if messages[i].content != text {
			t.Errorf("message %d = %q, want %q", i, messages[i].content, text)
		}
	}
}
