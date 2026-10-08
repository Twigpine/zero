package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// prewarmTimeout bounds the best-effort prewarm probe. The probe runs in the
// background, so this cap only limits how long the goroutine may linger — it
// can never delay the first turn.
const prewarmTimeout = 3 * time.Second

// NewTurnSessionProvider wraps an already-constructed *Provider in the
// optimized OpenAI turn session: a best-effort connection prewarm at run start
// plus request-prefix stability telemetry. Stream is Provider.StreamCompletion
// verbatim, so runtime request behavior is identical to the default adapter —
// the session adds observation and pool priming, never a different request.
func NewTurnSessionProvider(provider *Provider, caps zeroruntime.ProviderCapabilities) zeroruntime.TurnSessionProvider {
	return &turnSessionProvider{provider: provider, caps: caps}
}

type turnSessionProvider struct {
	provider *Provider
	caps     zeroruntime.ProviderCapabilities
}

func (p *turnSessionProvider) OpenTurnSession(context.Context) (zeroruntime.TurnSession, error) {
	return &turnSession{provider: p.provider}, nil
}

func (p *turnSessionProvider) Capabilities() zeroruntime.ProviderCapabilities {
	return p.caps
}

// turnSession is one run's optimized OpenAI session. The agent loop serializes
// all provider I/O through its session shim, so fields need no mutex. The
// fingerprint counters exist so a future stateful-reuse session (Responses API)
// inherits a proven prefix-stability detector; on chat completions there is no
// server-side response state to invalidate, so drift is telemetry only.
type turnSession struct {
	provider        *Provider
	lastFingerprint string
	prewarmOnce     sync.Once
	// prewarmDone closes when the background probe settles; tests wait on it.
	prewarmDone chan struct{}
}

// Prewarm launches one bounded, unauthenticated HEAD probe to the provider's
// base URL in the background so the TCP+TLS handshake lands in the shared
// connection pool while the loop assembles the first prompt. One attempt, no
// retries, no bearer token — the goal is the handshake, not a 2xx (a 401/404/
// 405 response still primes the pool). Always returns nil: prewarm is advisory
// by contract and never required for correctness.
//
// The probe is issued directly through the client (NOT providerio.SendWithRetry)
// so it stamps ONLY the provider_prewarm span — a nested provider_connect stamp
// would contaminate the exact A/B metric this feature is measured by.
//
// When the client's transport cannot retain idle connections (macOS disables
// keep-alives on the shared transport because degraded pooled connections are
// indistinguishable from backend slowness there), the probe is skipped entirely
// rather than spending a request that can never be reused. The pool's 30s idle
// timeout bounds the warm window; a slower start simply falls back to a cold
// dial, never worse than today.
func (s *turnSession) Prewarm(ctx context.Context) error {
	s.prewarmOnce.Do(func() {
		done := make(chan struct{})
		s.prewarmDone = done
		provider := s.provider
		if !transportRetainsIdleConns(provider.httpClient) {
			close(done)
			return
		}
		recorder := trace.FromContext(ctx)
		go func() {
			defer close(done)
			probeCtx, cancel := context.WithTimeout(ctx, prewarmTimeout)
			defer cancel()
			span := recorder.Span(trace.SpanProviderPrewarm)
			defer span.End()
			recorder.Counter(trace.CounterPrewarmAttempts, 1)
			request, err := http.NewRequestWithContext(probeCtx, http.MethodHead, provider.baseURL, nil)
			if err != nil {
				return
			}
			if provider.userAgent != "" {
				request.Header.Set("User-Agent", provider.userAgent)
			}
			response, err := provider.httpClient.Do(request)
			if err == nil {
				_ = response.Body.Close()
			}
		}()
	})
	return nil
}

// transportRetainsIdleConns reports whether a warmed connection can outlive the
// probe. A *http.Transport with DisableKeepAlives (the shared transport on
// macOS) closes every connection after its request, so a prewarm there is pure
// overhead. A nil transport is net/http's default (keep-alives on); an unknown
// custom transport is assumed reusable — the probe stays a harmless best-effort.
func transportRetainsIdleConns(client *http.Client) bool {
	if client == nil {
		return false
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		return !transport.DisableKeepAlives
	}
	return true
}

func (s *turnSession) Stream(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	s.observePrefix(ctx, request)
	return s.provider.StreamCompletion(ctx, request)
}

func (s *turnSession) Compact(context.Context, zeroruntime.CompletionRequest) ([]zeroruntime.Message, error) {
	return nil, zeroruntime.ErrCompactionUnsupported
}

// Close is an idempotent no-op: the shared transport pool owns the connections.
func (s *turnSession) Close() error { return nil }

// observePrefix tracks whether the request-prefix parameters stayed stable
// between this session's streams. The first stream seeds the fingerprint and
// counts as neither stable nor drift.
func (s *turnSession) observePrefix(ctx context.Context, request zeroruntime.CompletionRequest) {
	fingerprint := s.computeFingerprint(request)
	if s.lastFingerprint == "" {
		s.lastFingerprint = fingerprint
		return
	}
	recorder := trace.FromContext(ctx)
	if fingerprint == s.lastFingerprint {
		recorder.Counter(trace.CounterPrefixStable, 1)
		return
	}
	recorder.Counter(trace.CounterPrefixDrift, 1)
	s.lastFingerprint = fingerprint
}

// computeFingerprint digests the wire-affecting request parameters: the
// session's model and max-tokens (fixed at construction — CompletionRequest
// carries no model field), the normalized reasoning effort as it would appear
// on the wire, the normalized service tier, the prompt-cache key (constant
// within a session, and part of the request the provider serializes), and an order-preserving digest of the
// advertised tools (request serialization preserves tool order, so a reorder
// changes the wire bytes and counts as drift). Messages are excluded — they
// grow every turn; this fingerprints request parameters, not conversation
// content.
//
// SCOPE: this is per-provider request-parameter TELEMETRY, not a complete
// compatibility fingerprint. It deliberately does not incorporate the prompt
// prefix hashes (base instructions / project context / skills — the
// complete_prefix signal the agent loop already emits per turn). Any future
// stateful response reuse MUST combine both signals plus the remaining
// wire-affecting fields before reusing provider-side state; these counters
// alone are not sufficient compatibility protection.
func (s *turnSession) computeFingerprint(request zeroruntime.CompletionRequest) string {
	var builder strings.Builder
	builder.WriteString(s.provider.model)
	builder.WriteByte('|')
	fmt.Fprintf(&builder, "%d", s.provider.maxTokens)
	builder.WriteByte('|')
	builder.WriteString(openAIReasoningEffort(request.ReasoningEffort))
	builder.WriteByte('|')
	builder.WriteString(openAIServiceTier(request.ServiceTier))
	builder.WriteByte('|')
	builder.WriteString(request.PromptCacheKey)
	builder.WriteByte('|')
	builder.WriteString(wireToolsDigest(request.Tools))
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// wireToolsDigest renders the tool set in WIRE ORDER: each tool as name +
// description + its JSON-marshaled parameters (encoding/json sorts map keys,
// so each render is stable). Order is preserved deliberately — the OpenAI
// request serializes tools in the order given, so a reorder changes the
// request bytes and must count as drift. Descriptions are model-visible
// request bytes too. A schema that fails to marshal is recorded under a
// stable sentinel — this digest feeds telemetry counters, so cruder handling
// of that pathological case is acceptable.
func wireToolsDigest(tools []zeroruntime.ToolDefinition) string {
	rendered := make([]string, 0, len(tools))
	for _, tool := range tools {
		wireShape := struct {
			Type       zeroruntime.ToolDefinitionType    `json:"type,omitempty"`
			Parameters map[string]any                    `json:"parameters,omitempty"`
			Format     *zeroruntime.ToolDefinitionFormat `json:"format,omitempty"`
		}{Type: tool.Type, Parameters: tool.Parameters, Format: tool.Format}
		schema, err := json.Marshal(wireShape)
		if err != nil {
			rendered = append(rendered, tool.Name+"\n"+tool.Description+"\n__non_json:"+tool.Name)
			continue
		}
		rendered = append(rendered, tool.Name+"\n"+tool.Description+"\n"+string(schema))
	}
	sum := sha256.Sum256([]byte(strings.Join(rendered, "\x00")))
	return hex.EncodeToString(sum[:])
}
