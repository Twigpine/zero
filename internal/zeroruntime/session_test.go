package zeroruntime

import (
	"context"
	"errors"
	"testing"
)

// recordingProvider captures every request and replays a scripted event stream,
// so tests can assert the default session forwards verbatim.
type recordingProvider struct {
	requests []CompletionRequest
	events   []StreamEvent
	err      error
}

func (p *recordingProvider) StreamCompletion(_ context.Context, request CompletionRequest) (<-chan StreamEvent, error) {
	p.requests = append(p.requests, request)
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan StreamEvent, len(p.events))
	for _, event := range p.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func TestDefaultTurnSessionStreamForwardsVerbatim(t *testing.T) {
	provider := &recordingProvider{events: []StreamEvent{
		{Type: StreamEventText, Content: "hello"},
		{Type: StreamEventDone, FinishReason: "stop"},
	}}
	session, err := NewProviderTurnSessionProvider(provider, ProviderCapabilities{}).OpenTurnSession(context.Background())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}

	request := CompletionRequest{
		Messages:        []Message{{Role: MessageRoleUser, Content: "hi"}},
		ReasoningEffort: "high",
		ServiceTier:     "priority",
		PromptCacheKey:  "session-123",
	}
	stream, err := session.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var got []StreamEvent
	for event := range stream {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Content != "hello" || got[1].FinishReason != "stop" {
		t.Fatalf("stream events not forwarded verbatim: %#v", got)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.requests))
	}
	recorded := provider.requests[0]
	if recorded.PromptCacheKey != request.PromptCacheKey ||
		recorded.ReasoningEffort != request.ReasoningEffort ||
		recorded.ServiceTier != request.ServiceTier ||
		len(recorded.Messages) != 1 || recorded.Messages[0].Content != "hi" {
		t.Fatalf("request not forwarded verbatim: %#v", recorded)
	}
}

func TestDefaultTurnSessionStreamPropagatesError(t *testing.T) {
	wantErr := errors.New("provider unavailable")
	provider := &recordingProvider{err: wantErr}
	session, err := NewProviderTurnSessionProvider(provider, ProviderCapabilities{}).OpenTurnSession(context.Background())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}
	if _, streamErr := session.Stream(context.Background(), CompletionRequest{}); !errors.Is(streamErr, wantErr) {
		t.Fatalf("Stream error = %v, want %v", streamErr, wantErr)
	}
}

func TestDefaultTurnSessionPrewarmCloseNoop(t *testing.T) {
	session, err := NewProviderTurnSessionProvider(&recordingProvider{}, ProviderCapabilities{}).OpenTurnSession(context.Background())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}
	if err := session.Prewarm(context.Background()); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent: a second Close (e.g. after a mid-run swap already closed it)
	// must also succeed.
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDefaultTurnSessionCompactUnsupported(t *testing.T) {
	session, err := NewProviderTurnSessionProvider(&recordingProvider{}, ProviderCapabilities{}).OpenTurnSession(context.Background())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}
	if _, compactErr := session.Compact(context.Background(), CompletionRequest{}); !errors.Is(compactErr, ErrCompactionUnsupported) {
		t.Fatalf("Compact error = %v, want ErrCompactionUnsupported", compactErr)
	}
}

func TestDefaultTurnSessionProviderCapabilities(t *testing.T) {
	caps := ProviderCapabilities{
		Model:               "test-model",
		ContextWindow:       200_000,
		MaxOutputTokens:     64_000,
		SupportsVision:      true,
		SupportsReasoning:   true,
		SupportsPromptCache: true,
		ReasoningEfforts:    []string{"low", "medium", "high"},
	}
	tsp := NewProviderTurnSessionProvider(&recordingProvider{}, caps)
	got := tsp.Capabilities()
	if got.Model != caps.Model || got.ContextWindow != caps.ContextWindow ||
		got.MaxOutputTokens != caps.MaxOutputTokens || !got.SupportsVision ||
		!got.SupportsReasoning || !got.SupportsPromptCache ||
		len(got.ReasoningEfforts) != 3 || got.NativeCompaction {
		t.Fatalf("Capabilities() = %#v, want %#v", got, caps)
	}
}
