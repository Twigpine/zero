package openai

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestD2_1_9_StreamCompletionRecordsResolvedModelOnUsageChunk(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"model":"deepseek/deepseek-r1:free","choices":[{"delta":{"content":"ok"}}]}`)
		writeSSE(w, `{"model":"deepseek/deepseek-r1:free","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		writeSSE(w, `[DONE]`)
	})
	events := collectProviderEvents(t, provider)
	var usageEvent *zeroruntime.StreamEvent
	for i := range events {
		if events[i].Type == zeroruntime.StreamEventUsage {
			usageEvent = &events[i]
			break
		}
	}
	if usageEvent == nil {
		t.Fatalf("missing usage event: %#v", events)
	}
	if usageEvent.Usage.ResolvedModel != "deepseek/deepseek-r1:free" {
		t.Fatalf("ResolvedModel = %q, want deepseek/deepseek-r1:free", usageEvent.Usage.ResolvedModel)
	}
}

func TestD2_1_10_StreamCompletionKeepsLastNonEmptyModel(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"model":"x/y","choices":[{"delta":{"content":"ok"}}]}`)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		writeSSE(w, `[DONE]`)
	})
	events := collectProviderEvents(t, provider)
	var usageEvent *zeroruntime.StreamEvent
	for i := range events {
		if events[i].Type == zeroruntime.StreamEventUsage {
			usageEvent = &events[i]
			break
		}
	}
	if usageEvent == nil {
		t.Fatalf("missing usage event: %#v", events)
	}
	if usageEvent.Usage.ResolvedModel != "x/y" {
		t.Fatalf("ResolvedModel = %q, want x/y", usageEvent.Usage.ResolvedModel)
	}
}

func TestD2_1_11_StreamCompletionResolvedModelTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		lines  []string
		wantRM string
		check  func(t *testing.T, rm string)
	}{
		{
			name: "equal-every-chunk",
			lines: []string{
				`{"model":"gpt-test","choices":[{"delta":{"content":"a"}}]}`,
				`{"model":"gpt-test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			},
			wantRM: "",
		},
		{
			name: "evil-on-usage-chunk",
			lines: []string{
				`{"choices":[{"delta":{"content":"a"}}]}`,
				"{\"model\":\"\\u001b[31mgpt-evil\\u001b]0;t\\u0007\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}",
			},
			wantRM: "[31mgpt-evil]0;t",
		},
		{
			name: "split-nul-equals-requested",
			lines: []string{
				"{\"model\":\"gpt-\\u0000test\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}",
				"{\"model\":\"gpt-\\u0000test\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}",
			},
			wantRM: "",
		},
		{
			name: "late-equals-requested",
			lines: []string{
				`{"model":"x/y","choices":[{"delta":{"content":"a"}}]}`,
				`{"model":"gpt-test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			},
			wantRM: "",
		},
		{
			name: "first-wins-when-last-sanitized-empty",
			lines: []string{
				`{"model":"x/y","choices":[{"delta":{"content":"a"}}]}`,
				"{\"model\":\"\\u001b\\u0000\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}",
			},
			wantRM: "x/y",
		},
		{
			name: "huge-model-capped",
			lines: []string{
				`{"model":"` + strings.Repeat("a", 64*1024) + `","choices":[{"delta":{"content":"a"}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			},
			check: func(t *testing.T, rm string) {
				t.Helper()
				if rm == "" {
					t.Fatal("expected non-empty capped ResolvedModel")
				}
				if len(rm) > zeroruntime.MaxResolvedModelBytes {
					t.Fatalf("len %d exceeds cap", len(rm))
				}
				if !utf8.ValidString(rm) {
					t.Fatalf("invalid UTF-8: %q", rm)
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				for _, line := range tc.lines {
					writeSSE(w, line)
				}
				writeSSE(w, `[DONE]`)
			})
			events := collectProviderEvents(t, provider)
			var usageEvent *zeroruntime.StreamEvent
			for i := range events {
				if events[i].Type == zeroruntime.StreamEventUsage {
					usageEvent = &events[i]
					break
				}
			}
			if usageEvent == nil {
				t.Fatalf("missing usage event: %#v", events)
			}
			if tc.check != nil {
				tc.check(t, usageEvent.Usage.ResolvedModel)
				return
			}
			if usageEvent.Usage.ResolvedModel != tc.wantRM {
				t.Fatalf("ResolvedModel = %q, want %q", usageEvent.Usage.ResolvedModel, tc.wantRM)
			}
		})
	}
}

func TestD2_1_12_StreamCompletionWithoutModelUnchangedUsage(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"hello "}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"zero"}}],"usage":{"prompt_tokens":12,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}}`)
		writeSSE(w, `[DONE]`)
	})
	events := collectProviderEvents(t, provider)
	var usageEvent *zeroruntime.StreamEvent
	for i := range events {
		if events[i].Type == zeroruntime.StreamEventUsage {
			usageEvent = &events[i]
			break
		}
	}
	if usageEvent == nil {
		t.Fatalf("missing usage event: %#v", events)
	}
	if usageEvent.Usage.ResolvedModel != "" {
		t.Fatalf("ResolvedModel = %q, want empty", usageEvent.Usage.ResolvedModel)
	}
	if usageEvent.Usage.PromptTokens != 12 || usageEvent.Usage.CompletionTokens != 5 ||
		usageEvent.Usage.CachedInputTokens != 3 || usageEvent.Usage.CacheWriteTokens != 2 {
		t.Fatalf("usage fields changed: %#v", usageEvent.Usage)
	}
}

// A usage chunk that echoes the requested model after a router named another
// one ends the attribution: the collected usage must not keep reporting the
// earlier model next to the later token counts.
func TestStreamCompletionEchoOfRequestedModelClearsEarlierAttribution(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		writeSSE(w, `{"model":"gpt-test","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
		writeSSE(w, `[DONE]`)
	})
	events := collectProviderEvents(t, provider)
	stream := make(chan zeroruntime.StreamEvent, len(events))
	for _, event := range events {
		stream <- event
	}
	close(stream)
	var last zeroruntime.Usage
	zeroruntime.CollectStreamWithOptions(context.Background(), stream, zeroruntime.CollectOptions{
		OnUsage: func(usage zeroruntime.Usage) { last = usage },
	})
	if last.ResolvedModel != "" {
		t.Fatalf("ResolvedModel = %q, want cleared after the echo of the requested model", last.ResolvedModel)
	}
	if last.EffectiveOutputTokens() != 4 {
		t.Fatalf("output tokens = %d, want the later snapshot's 4", last.EffectiveOutputTokens())
	}
}
