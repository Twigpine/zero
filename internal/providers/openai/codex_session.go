package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const (
	responsesWebSocketBetaHeader = "OpenAI-Beta"
	responsesWebSocketBetaValue  = "responses_websockets=2026-02-06"
	responsesWebSocketRequest    = "response.create"
	previousResponseNotFoundCode = "previous_response_not_found"
	webSocketConnectionLimitCode = "websocket_connection_limit_reached"
	responsesWebSocketReadLimit  = 16 << 20
)

var errCodexTurnSessionBusy = errors.New("codex turn session already has an active stream")

// NewCodexTurnSessionProvider enables one Responses WebSocket connection for
// each agent run. The session falls back to the existing HTTP/SSE provider when
// the endpoint cannot upgrade or loses its response chain.
func NewCodexTurnSessionProvider(provider *CodexProvider, caps zeroruntime.ProviderCapabilities) zeroruntime.TurnSessionProvider {
	return &codexTurnSessionProvider{provider: provider, caps: caps}
}

type codexTurnSessionProvider struct {
	provider *CodexProvider
	caps     zeroruntime.ProviderCapabilities
}

func (provider *codexTurnSessionProvider) OpenTurnSession(ctx context.Context) (zeroruntime.TurnSession, error) {
	if provider == nil || provider.provider == nil || provider.provider.inner == nil {
		return nil, errors.New("codex turn session requires a provider")
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	return &codexTurnSession{
		ctx:         sessionCtx,
		cancel:      cancel,
		provider:    provider.provider,
		prewarmDone: make(chan struct{}),
	}, nil
}

func (provider *codexTurnSessionProvider) Capabilities() zeroruntime.ProviderCapabilities {
	return provider.caps
}

type codexTurnSession struct {
	ctx      context.Context
	cancel   context.CancelFunc
	provider *CodexProvider

	mu             sync.Mutex
	connection     *websocket.Conn
	webSocketOff   bool
	lastRequest    *responsesRequest
	lastResponseID string
	lastOutput     []inputItem
	streamActive   bool

	prewarmOnce sync.Once
	prewarmDone chan struct{}
	closeOnce   sync.Once
}

// Prewarm starts the WebSocket handshake without blocking prompt assembly. The
// first Stream waits for this bounded attempt; failure simply selects HTTP/SSE.
func (session *codexTurnSession) Prewarm(ctx context.Context) error {
	session.prewarmOnce.Do(func() {
		go func() {
			defer close(session.prewarmDone)
			prewarmCtx, cancel := context.WithTimeout(session.ctx, prewarmTimeout)
			defer cancel()
			recorder := trace.FromContext(ctx)
			span := recorder.Span(trace.SpanProviderPrewarm)
			defer span.End()
			recorder.Counter(trace.CounterPrewarmAttempts, 1)
			connection, err := session.provider.dialResponsesWebSocket(prewarmCtx)
			session.mu.Lock()
			defer session.mu.Unlock()
			if err != nil {
				session.webSocketOff = true
				return
			}
			session.connection = connection
		}()
	})
	return nil
}

func (session *codexTurnSession) Stream(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	_ = session.Prewarm(ctx)
	select {
	case <-session.prewarmDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	fullRequest, err := session.provider.buildResponsesRequest(request)
	if err != nil {
		return nil, err
	}

	session.mu.Lock()
	if session.streamActive {
		session.mu.Unlock()
		return nil, errCodexTurnSessionBusy
	}
	if session.webSocketOff || session.connection == nil {
		session.mu.Unlock()
		trace.FromContext(ctx).Counter(trace.CounterResponsesHTTPFallback, 1)
		return session.provider.StreamCompletion(ctx, request)
	}
	session.streamActive = true
	connection := session.connection
	wireRequest := *fullRequest
	wireRequest.Type = responsesWebSocketRequest
	if delta, ok := responseInputDelta(session.lastRequest, session.lastOutput, fullRequest); ok && session.lastResponseID != "" {
		wireRequest.PreviousResponseID = session.lastResponseID
		wireRequest.Input = delta
		trace.FromContext(ctx).Counter(trace.CounterResponseChainReused, 1)
	} else if session.lastRequest != nil {
		trace.FromContext(ctx).Counter(trace.CounterResponseChainReset, 1)
		session.lastRequest = nil
		session.lastResponseID = ""
		session.lastOutput = nil
	}
	session.mu.Unlock()

	body, err := json.Marshal(wireRequest)
	if err != nil {
		session.releaseStream()
		return nil, fmt.Errorf("encode codex websocket request: %w", err)
	}
	events := make(chan zeroruntime.StreamEvent, 16)
	go func() {
		defer close(events)
		defer session.releaseStream()
		session.streamWebSocket(ctx, connection, body, fullRequest, request, events)
	}()
	return events, nil
}

func (session *codexTurnSession) releaseStream() {
	session.mu.Lock()
	session.streamActive = false
	session.mu.Unlock()
}

func (session *codexTurnSession) Compact(context.Context, zeroruntime.CompletionRequest) ([]zeroruntime.Message, error) {
	return nil, zeroruntime.ErrCompactionUnsupported
}

func (session *codexTurnSession) Close() error {
	session.closeOnce.Do(func() {
		session.cancel()
		session.prewarmOnce.Do(func() { close(session.prewarmDone) })
		select {
		case <-session.prewarmDone:
		case <-time.After(prewarmTimeout):
		}
		session.mu.Lock()
		connection := session.connection
		session.connection = nil
		session.webSocketOff = true
		session.mu.Unlock()
		if connection != nil {
			_ = connection.CloseNow()
		}
	})
	return nil
}

func responseInputDelta(previous *responsesRequest, output []inputItem, current *responsesRequest) ([]inputItem, bool) {
	if previous == nil || current == nil || !responsesRequestPropertiesEqual(previous, current) {
		return nil, false
	}
	prefix := make([]inputItem, 0, len(previous.Input)+len(output))
	prefix = append(prefix, previous.Input...)
	prefix = append(prefix, output...)
	if len(current.Input) <= len(prefix) || !reflect.DeepEqual(current.Input[:len(prefix)], prefix) {
		return nil, false
	}
	return append([]inputItem(nil), current.Input[len(prefix):]...), true
}

func responsesRequestPropertiesEqual(left, right *responsesRequest) bool {
	return left.Model == right.Model &&
		left.Instructions == right.Instructions &&
		left.Stream == right.Stream &&
		left.Store == right.Store &&
		left.ParallelToolCalls == right.ParallelToolCalls &&
		left.MaxOutputTokens == right.MaxOutputTokens &&
		reflect.DeepEqual(left.Tools, right.Tools) &&
		reflect.DeepEqual(left.Reasoning, right.Reasoning) &&
		left.ServiceTier == right.ServiceTier &&
		left.PromptCacheKey == right.PromptCacheKey
}

func (session *codexTurnSession) streamWebSocket(
	ctx context.Context,
	connection *websocket.Conn,
	body []byte,
	fullRequest *responsesRequest,
	runtimeRequest zeroruntime.CompletionRequest,
	events chan<- zeroruntime.StreamEvent,
) {
	if err := connection.Write(ctx, websocket.MessageText, body); err != nil {
		session.disableWebSocket(connection)
		session.forwardHTTP(ctx, runtimeRequest, events)
		return
	}

	state := newResponsesState()
	for {
		readCtx := ctx
		cancel := func() {}
		idleTimeout := session.provider.inner.streamIdleTimeout
		if idleTimeout > 0 {
			readCtx, cancel = context.WithTimeout(ctx, idleTimeout)
		}
		messageType, data, err := connection.Read(readCtx)
		readTimedOut := errors.Is(readCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err != nil {
			session.disableWebSocket(connection)
			if ctx.Err() != nil {
				return
			}
			if !state.emitted {
				session.forwardHTTP(ctx, runtimeRequest, events)
				return
			}
			errorMessage := err.Error()
			if readTimedOut {
				errorMessage = providerio.StreamTimeoutMessage(providerio.ErrStreamIdle, idleTimeout)
			}
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:  zeroruntime.StreamEventError,
				Error: session.provider.redact("provider stream error: " + errorMessage),
			})
			return
		}
		if messageType != websocket.MessageText {
			continue
		}

		var responseEvent responsesEvent
		decodeErr := json.Unmarshal(data, &responseEvent)
		if decodeErr == nil {
			if (responseEvent.Code == previousResponseNotFoundCode || responseEvent.Code == webSocketConnectionLimitCode) &&
				!state.emitted {
				session.disableWebSocket(connection)
				session.forwardHTTP(ctx, runtimeRequest, events)
				return
			}
			if responseEvent.Type == responsesEventIncomplete {
				session.resetResponseChain(connection)
				providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
					Type:         zeroruntime.StreamEventDone,
					FinishReason: zeroruntime.FinishReasonLength,
				})
				return
			}
		}

		keepReading := false
		if decodeErr == nil {
			keepReading = session.provider.emitParsedResponsesEvent(ctx, &responseEvent, state, events)
		} else {
			keepReading = session.provider.emitResponsesEvent(ctx, string(data), state, events)
		}
		if keepReading {
			continue
		}
		if responseEvent.Type == responsesEventCompleted && responseEvent.Response != nil &&
			responseEvent.Response.Error == nil && responseEvent.Response.Status != "failed" && state.responseID != "" {
			session.mu.Lock()
			if session.connection == connection && !session.webSocketOff {
				requestCopy := *fullRequest
				requestCopy.Input = append([]inputItem(nil), fullRequest.Input...)
				session.lastRequest = &requestCopy
				session.lastResponseID = state.responseID
				session.lastOutput = state.outputInputItems()
			}
			session.mu.Unlock()
		}
		return
	}
}

func (session *codexTurnSession) resetResponseChain(connection *websocket.Conn) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.connection != connection {
		return
	}
	session.lastRequest = nil
	session.lastResponseID = ""
	session.lastOutput = nil
}

func (session *codexTurnSession) forwardHTTP(ctx context.Context, request zeroruntime.CompletionRequest, events chan<- zeroruntime.StreamEvent) {
	trace.FromContext(ctx).Counter(trace.CounterResponsesHTTPFallback, 1)
	stream, err := session.provider.StreamCompletion(ctx, request)
	if err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: session.provider.redact(err.Error()),
		})
		return
	}
	for event := range stream {
		providerio.SendEvent(ctx, events, event)
	}
}

func (session *codexTurnSession) disableWebSocket(connection *websocket.Conn) {
	session.mu.Lock()
	if session.connection == connection {
		session.connection = nil
		session.webSocketOff = true
		session.lastRequest = nil
		session.lastResponseID = ""
		session.lastOutput = nil
	}
	session.mu.Unlock()
	_ = connection.CloseNow()
}

func (provider *CodexProvider) dialResponsesWebSocket(ctx context.Context) (*websocket.Conn, error) {
	endpoint, err := responsesWebSocketURL(provider.inner.endpoint)
	if err != nil {
		return nil, err
	}
	for authTry := 0; authTry < 2; authTry++ {
		headers, err := provider.responsesWebSocketHeaders(ctx, authTry == 1)
		if err != nil {
			return nil, err
		}
		connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
			HTTPClient:      provider.inner.httpClient,
			HTTPHeader:      headers,
			CompressionMode: websocket.CompressionNoContextTakeover,
		})
		if err == nil {
			connection.SetReadLimit(responsesWebSocketReadLimit)
			return connection, nil
		}
		if response == nil || response.StatusCode != http.StatusUnauthorized || provider.inner.oauthResolver == nil || authTry == 1 {
			return nil, err
		}
	}
	return nil, errors.New("codex websocket authentication failed")
}

func (provider *CodexProvider) responsesWebSocketHeaders(ctx context.Context, forceRefresh bool) (http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.inner.endpoint, nil)
	if err != nil {
		return nil, err
	}
	base := providerio.AuthHeaders{
		APIKey:            provider.inner.apiKey,
		DefaultAuthHeader: "Authorization",
		DefaultAuthScheme: "Bearer",
		AuthHeader:        provider.inner.authHeader,
		AuthScheme:        provider.inner.authScheme,
		AuthHeaderValue:   provider.inner.authHeaderValue,
		CustomHeaders:     provider.inner.customHeaders,
	}
	if resolver := provider.inner.oauthResolver; resolver != nil {
		header, value, ok, err := resolver(ctx, forceRefresh)
		if err != nil {
			return nil, err
		}
		if ok {
			providerio.ApplyAuthHeaders(request, providerio.AuthHeaders{CustomHeaders: provider.inner.customHeaders})
			if strings.TrimSpace(header) == "" {
				header = "Authorization"
			}
			request.Header.Set(header, value)
		} else {
			providerio.ApplyAuthHeaders(request, base)
		}
	} else {
		providerio.ApplyAuthHeaders(request, base)
	}
	provider.injectCodexHeaders(request)
	request.Header.Set(responsesWebSocketBetaHeader, responsesWebSocketBetaValue)
	return request.Header.Clone(), nil
}

func responsesWebSocketURL(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported Responses websocket URL scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}
