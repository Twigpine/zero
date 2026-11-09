package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/redaction"
)

// The AI/ML API MCP server authenticates with the user's AI/ML API key.
//
// Invariant: the key and the attribution headers are attached only to an
// outgoing request whose own URL is https://mcp.aimlapi.com on the default port
// with no userinfo (exact hostname, see aimlapi.IsAttributedURL), whatever the
// server is named. The key is resolved again for every request, redirect hops
// included (see reapplyAIMLAPIRedirectHeaders), through
// config.LoadAIMLAPIServiceKey, which in the same call decides whether it may
// be sent (no proxy profile, a single token), so a provider change applies to
// the next request and a withheld or invalid key means no Authorization
// header. It lives only in that request's headers, and for the length of the
// call in the collector its context carries (aimlapiSentKeys), so the reply can
// be scrubbed: never in Server, its Identity (OAuth token binding), config.json,
// list output, errors or tool output. Every key the call sent, redirect hops
// included, and on the SSE transport the keys the stream request sent, is
// scrubbed from the response's error text, method and result before anything
// reads them (scrubAIMLAPIKeys) and from any error the call returns, a
// transport error quoting a URL the server chose among them
// (scrubAIMLAPIKeyError). An error status to such a request is also reported
// without its body. A user-configured Authorization header, OAuth, or a
// project-touched server always goes without it.
var loadAIMLAPIServiceKey = config.LoadAIMLAPIServiceKey

var aimlapiMCPHost = func() string {
	parsed, err := url.Parse(aimlapi.MCPServerURL)
	if err != nil {
		panic(err)
	}
	return parsed.Hostname()
}()

func isAIMLAPIMCPURL(target *url.URL) bool {
	return aimlapi.IsAttributedURL(target) && strings.EqualFold(target.Hostname(), aimlapiMCPHost)
}

// aimlapiAutomaticHeaders are the headers applyAIMLAPIRequestHeaders may add.
var aimlapiAutomaticHeaders = [...]string{"Authorization", aimlapi.SourceHeaderName, aimlapi.PartnerHeaderName}

// applyAIMLAPIRequestHeaders runs after the server's own headers are set.
func applyAIMLAPIRequestHeaders(server Server, request *http.Request) {
	if request == nil || !isAIMLAPIMCPURL(request.URL) {
		return
	}
	aimlapi.ApplyAttributionHeaders(request.Header, request.URL)
	if request.Header.Get("Authorization") != "" || strings.EqualFold(server.Auth, ServerAuthOAuth) || server.ProjectConfigured {
		return
	}
	key, err := loadAIMLAPIServiceKey()
	if err != nil || key == "" {
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
	if sent, ok := request.Context().Value(aimlapiSentKeysContextKey{}).(*aimlapiSentKeys); ok {
		sent.add(key)
	}
}

// reapplyAIMLAPIRedirectHeaders gives a redirect hop the automatic headers its
// own URL earns. net/http copies the first request's headers onto every hop
// without calling the header hooks, so without this a key or attribution added
// for the first URL would follow a same-origin hop that fails isAIMLAPIMCPURL
// (userinfo, say), and an eligible hop would reuse the first hop's key instead
// of resolving it again. The hop gets back the server's own values for these
// headers, then the same rule as any other request.
func reapplyAIMLAPIRedirectHeaders(server Server, request *http.Request, first *http.Request) {
	if request == nil || first == nil || !isAIMLAPIMCPURL(first.URL) {
		return
	}
	for _, name := range aimlapiAutomaticHeaders {
		request.Header.Del(name)
	}
	for name, value := range server.Headers {
		for _, automatic := range aimlapiAutomaticHeaders {
			if strings.EqualFold(name, automatic) {
				request.Header.Set(name, value)
			}
		}
	}
	applyAIMLAPIRequestHeaders(server, request)
}

// sentAIMLAPIServiceKey reports whether request went out carrying the key
// applyAIMLAPIRequestHeaders injects, as opposed to the server's own
// Authorization header or OAuth.
func sentAIMLAPIServiceKey(server Server, request *http.Request) bool {
	if request == nil || !isAIMLAPIMCPURL(request.URL) || strings.EqualFold(server.Auth, ServerAuthOAuth) || server.ProjectConfigured {
		return false
	}
	return request.Header.Get("Authorization") != "" && !hasUserAuthorizationHeader(server)
}

func hasUserAuthorizationHeader(server Server) bool {
	for name, value := range server.Headers {
		if strings.EqualFold(name, "Authorization") && value != "" {
			return true
		}
	}
	return false
}

// aimlapiSentKeys collects every key applyAIMLAPIRequestHeaders puts on the
// requests of one call. The first request's context carries it, and net/http
// gives each redirect hop that same context, so a key resolved again for a later
// hop is collected too. The final hop's key alone is not enough: a same-origin
// server answering the last hop has seen every earlier one.
type aimlapiSentKeys struct {
	mu   sync.Mutex
	keys []string
}

type aimlapiSentKeysContextKey struct{}

// withAIMLAPISentKeys returns ctx carrying a fresh collector for one call.
func withAIMLAPISentKeys(ctx context.Context) (context.Context, *aimlapiSentKeys) {
	sent := &aimlapiSentKeys{}
	return context.WithValue(ctx, aimlapiSentKeysContextKey{}, sent), sent
}

func (sent *aimlapiSentKeys) add(key string) {
	sent.mu.Lock()
	defer sent.mu.Unlock()
	for _, existing := range sent.keys {
		if existing == key {
			return
		}
	}
	sent.keys = append(sent.keys, key)
}

// list returns a copy of the keys collected so far.
func (sent *aimlapiSentKeys) list() []string {
	sent.mu.Lock()
	defer sent.mu.Unlock()
	return append([]string(nil), sent.keys...)
}

// scrubAIMLAPIKeys removes the injected keys from a response before any of it
// is read: the error text, the method name and every string in the result,
// object member names included. A server can echo the credential it was sent,
// and MCP reports a failed tool call as a result rather than an error, so the
// result needs it as much as the error text. Only the known values go, split
// forms included (redaction.RedactSecretValues); a result that holds none of
// them keeps its original bytes.
func scrubAIMLAPIKeys(message rpcMessage, keys ...string) rpcMessage {
	var sent []string
	for _, key := range keys {
		if strings.TrimSpace(key) != "" {
			sent = append(sent, key)
		}
	}
	if len(sent) == 0 {
		return message
	}
	if message.Error != nil {
		scrubbed := *message.Error
		scrubbed.Message = redaction.RedactSecretValues(scrubbed.Message, sent...)
		message.Error = &scrubbed
	}
	message.Method = redaction.RedactSecretValues(message.Method, sent...)
	message.Result = scrubAIMLAPIKeysFromJSON(message.Result, sent)
	return message
}

func scrubAIMLAPIKeysFromJSON(raw json.RawMessage, keys []string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		// Decoding it into the caller's type fails the same way, and that
		// error does not quote the text.
		return raw
	}
	scrubbed, changed := scrubAIMLAPIKeysFromValue(value, keys)
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(scrubbed)
	if err != nil {
		// Never hand back the unscrubbed bytes: an empty result reads as
		// nothing rather than as the key.
		return nil
	}
	return encoded
}

func scrubAIMLAPIKeysFromValue(value any, keys []string) (any, bool) {
	switch typed := value.(type) {
	case string:
		scrubbed := redaction.RedactSecretValues(typed, keys...)
		return scrubbed, scrubbed != typed
	case []any:
		changed := false
		for index, item := range typed {
			if scrubbed, itemChanged := scrubAIMLAPIKeysFromValue(item, keys); itemChanged {
				typed[index] = scrubbed
				changed = true
			}
		}
		return typed, changed
	case map[string]any:
		changed := false
		scrubbed := make(map[string]any, len(typed))
		for name, item := range typed {
			scrubbedName := redaction.RedactSecretValues(name, keys...)
			scrubbedItem, itemChanged := scrubAIMLAPIKeysFromValue(item, keys)
			if itemChanged || scrubbedName != name {
				changed = true
			}
			scrubbed[scrubbedName] = scrubbedItem
		}
		return scrubbed, changed
	}
	return value, false
}

// scrubAIMLAPIKeyError returns err with the injected keys removed from its text.
// An error the keys do not appear in keeps its chain. One they do appear in is
// replaced by its scrubbed text, because its chain still holds the key (a
// *url.Error keeps the URL the server chose); the replacement unwraps only to
// the context sentinel the original matched, so a cancellation still reads as
// one.
func scrubAIMLAPIKeyError(err error, keys ...string) error {
	if err == nil || len(keys) == 0 {
		return err
	}
	text := err.Error()
	scrubbed := redaction.RedactSecretValues(text, keys...)
	if scrubbed == text {
		return err
	}
	replacement := &aimlapiScrubbedError{text: scrubbed}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			replacement.cause = sentinel
			break
		}
	}
	return replacement
}

// aimlapiScrubbedError is an error whose text had an injected key removed.
type aimlapiScrubbedError struct {
	text  string
	cause error
}

func (scrubbed *aimlapiScrubbedError) Error() string { return scrubbed.text }

func (scrubbed *aimlapiScrubbedError) Unwrap() error { return scrubbed.cause }

// aimlapiUnauthorizedHint explains how to authenticate when the AI/ML API MCP
// server rejects a request without OAuth. It names no key material.
func aimlapiUnauthorizedHint(server Server, status int) string {
	if status != http.StatusUnauthorized || strings.EqualFold(server.Auth, ServerAuthOAuth) {
		return ""
	}
	target, err := url.Parse(server.URL)
	if err != nil || !isAIMLAPIMCPURL(target) {
		return ""
	}
	oauthHint := fmt.Sprintf("To sign in instead, set \"auth\": \"oauth\" on this server and run `zero mcp oauth login %s`", server.Name)
	if server.ProjectConfigured || hasUserAuthorizationHeader(server) {
		return " (" + oauthHint + ")"
	}
	return fmt.Sprintf(" (set up the aimlapi.com provider with `zero providers setup aimlapi` or set AIMLAPI_API_KEY; automatic injection is also disabled for proxy profiles, OAuth, project config, and non-empty Authorization headers — see docs/EXTENDING.md#built-in-servers. To sign in instead, set \"auth\": \"oauth\" on this server and run `zero mcp oauth login %s`)", server.Name)
}
