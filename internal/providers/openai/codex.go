package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// Codex-specific headers, lifted from the openai/codex CLI's behavior. The
// Codex backend at https://chatgpt.com/backend-api/codex requires all three on
// every request — the bearer, the `originator` that identifies the client, and
// the `chatgpt-account-id` claim that ties the bearer to a specific ChatGPT
// subscription. Drop any one and the request 401s at Cloudflare.
const (
	codexDefaultOriginator = "codex_cli_rs"
	codexAccountHeader     = "chatgpt-account-id"
	codexOriginatorHeader  = "originator"
)

// ApplyCodexHeaders applies the non-bearer headers required by ChatGPT's Codex
// backend. It is shared by runtime completions and auxiliary Codex endpoints
// such as live model discovery so their authentication cannot drift apart.
func ApplyCodexHeaders(req *http.Request, accountID, userAgent string) {
	req.Header.Set(codexOriginatorHeader, codexDefaultOriginator)
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		req.Header.Set(codexAccountHeader, accountID)
	}
	if userAgent = strings.TrimSpace(userAgent); userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
}

// CodexAccountResolver returns the `chatgpt_account_id` claim for the bearer
// that is about to be sent on a request. It is invoked once per request
// (including the 401-refresh retry) so the value can be re-derived from the
// live OAuth token rather than cached at construction time.
//
// ok=false means "no account id known" — the Codex provider simply omits the
// header in that case (the request will 401, but that's recoverable: the user
// re-auths and the next login persists a fresh id).
type CodexAccountResolver func(ctx context.Context) (accountID string, ok bool, err error)

// CodexOptions configures a Codex-flavored provider. It embeds the standard
// openai.Options so every chat-completions knob (model, custom headers,
// MaxTokens, parse-think-tags, etc.) is supported unchanged. The Codex-specific
// fields below add the headers the Codex backend requires.
type CodexOptions struct {
	Options
	// Originator is the value of the `originator` header. Empty defaults to
	// "codex_cli_rs" (the same value the openai/codex CLI ships). The Codex
	// backend reads this to attribute traffic; changing it is supported but
	// unusual.
	Originator string
	// UserAgent overrides the openai Options.UserAgent when non-empty. The
	// Codex backend logs the User-Agent for diagnostics, so a "codex_cli_rs"
	// / "zero" branded value is recommended.
	UserAgent string
	// AccountID is a static `chatgpt-account-id` that bypasses the resolver.
	// Leave empty in production wiring so the AccountResolver is consulted on
	// every request — that path reads the live OAuth token from the store and
	// survives a refresh that rotates the bearer (and its account claim).
	// The field exists for tests that want a pinned value without standing up
	// a resolver.
	AccountID string
	// AccountResolver, when set, returns the account id dynamically per
	// request (including the 401-refresh retry). The factory wires this so a
	// refresh that updates the stored token's Account field takes effect on
	// the next outgoing request without restarting the agent.
	//
	// ok=false means "no account id known" — the Codex provider simply omits
	// the header in that case (the request will 401, but that's recoverable:
	// the user re-auths and the next login persists a fresh id).
	AccountResolver CodexAccountResolver
	// RequestTimeout caps each outbound Codex request. 0 => 60s. The Codex
	// backend is hosted behind Cloudflare, so a few seconds is plenty for a
	// healthy connection; the cap is a safety net for the rare case the
	// request hangs past the streaming idle watchdog.
	RequestTimeout time.Duration
}

// CodexProvider is the Codex-flavored variant of the openai provider. It is
// a thin shim that adds the Codex-specific request headers
// (`originator`, `chatgpt-account-id`, branded `User-Agent`) on top of a
// Responses-API transport. The Codex backend at
// `https://chatgpt.com/backend-api/codex/responses` serves the OpenAI
// Responses API (not the chat-completions API), so the constructor
// overrides the endpoint AND the transport — the wrapped Provider is used
// only for its validated endpoint / auth / retry / timeout config; the
// actual request body and SSE parser live in codex_responses.go.
type CodexProvider struct {
	inner          *Provider
	originator     string
	userAgent      string
	accountID      string
	accountResolve CodexAccountResolver
}

// NewCodexProvider builds a CodexProvider. It is a thin wrapper over the
// openai.New constructor plus the Codex-specific Options.SetRequestExtra
// callback that injects the Codex headers. The wrapped Provider supplies
// the validated endpoint / auth / retry / idle-timeout config; the
// request body and stream parser are Codex-specific and live in
// codex_responses.go.
func NewCodexProvider(options CodexOptions) (*CodexProvider, error) {
	originator := strings.TrimSpace(options.Originator)
	if originator == "" {
		originator = codexDefaultOriginator
	}
	userAgent := strings.TrimSpace(options.UserAgent)
	if userAgent == "" {
		// Default to the openai Options.UserAgent (typically "zero/<ver>")
		// and fall back to a Codex-branded value when the caller didn't set
		// either — the Codex backend logs the User-Agent and a clearly
		// branded string makes operational issues easier to triage.
		userAgent = strings.TrimSpace(options.Options.UserAgent)
		if userAgent == "" {
			userAgent = codexDefaultOriginator
		}
	}

	// Reuse the openai provider's transport configuration. Embed Options so
	// the openai constructor sees the full struct; here we set
	// SetRequestExtra below. The inner Provider is NOT used for streaming
	// — the Codex provider has its own Responses-API stream path — but its
	// fields (endpoint, httpClient, auth headers, idle timeout) are the
	// single source of truth for the URL / auth / retry plumbing.
	openaiOpts := options.Options
	openaiOpts.UserAgent = userAgent
	// The Codex backend serves the Responses API at `{baseURL}/responses`,
	// not `/chat/completions`. Override the endpoint the openai transport
	// would otherwise default to.
	if baseURL := strings.TrimRight(strings.TrimSpace(openaiOpts.BaseURL), "/"); baseURL != "" {
		openaiOpts.Endpoint = baseURL + "/responses"
	}

	provider := &CodexProvider{
		originator:     originator,
		userAgent:      userAgent,
		accountID:      strings.TrimSpace(options.AccountID),
		accountResolve: options.AccountResolver,
	}
	openaiOpts.SetRequestExtra = provider.injectCodexHeaders
	inner, err := New(openaiOpts)
	if err != nil {
		return nil, fmt.Errorf("openai codex provider: %w", err)
	}
	provider.inner = inner
	return provider, nil
}

// StreamCompletion builds a Responses-API request and dispatches it via
// the Codex-specific stream path in codex_responses.go. The request body
// is the Responses schema (input items, tools, max_output_tokens) and
// the response is parsed from the typed SSE event stream the Codex
// backend emits (response.output_text.delta, response.function_call_
// arguments.delta, response.completed, ...).
func (p *CodexProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	responsesReq, err := p.buildResponsesRequest(request)
	if err != nil {
		return nil, fmt.Errorf("encode codex request: %w", err)
	}
	body, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("encode codex request: %w", err)
	}
	events := make(chan zeroruntime.StreamEvent, 16)
	go func() {
		defer close(events)
		p.streamResponses(ctx, body, events)
	}()
	return events, nil
}

// injectCodexHeaders is the SetRequestExtra callback installed on the wrapped
// openai provider. It sets the three Codex-required headers; the bearer is
// applied separately by the openai provider's auth path.
func (p *CodexProvider) injectCodexHeaders(req *http.Request) {
	account := ""
	if resolved, ok, err := p.resolveAccount(req.Context()); err == nil && ok && resolved != "" {
		account = strings.TrimSpace(resolved)
	}
	ApplyCodexHeaders(req, account, p.userAgent)
	if p.originator != codexDefaultOriginator {
		req.Header.Set(codexOriginatorHeader, p.originator)
	}
}

// resolveAccount returns the account id to inject, preferring the static
// AccountID (set at construction from the OAuth token) and falling back to the
// per-request AccountResolver. ok=false means "omit the header".
func (p *CodexProvider) resolveAccount(ctx context.Context) (string, bool, error) {
	if p.accountID != "" {
		return p.accountID, true, nil
	}
	if p.accountResolve != nil {
		account, ok, err := p.accountResolve(ctx)
		if err != nil {
			return "", false, err
		}
		return account, ok, nil
	}
	return "", false, nil
}
