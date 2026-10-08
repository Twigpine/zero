package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type networkClient struct {
	server    Server
	client    *http.Client
	mu        sync.Mutex
	nextID    int
	sessionID string
}

type remoteSSEClient struct {
	server       Server
	client       *http.Client
	mu           sync.Mutex
	nextID       int
	endpointURL  string
	streamBody   io.Closer
	streamCancel context.CancelFunc
	pending      map[string]chan ssePendingResponse
	streamErr    error
	closed       bool
}

type ssePendingResponse struct {
	message rpcMessage
	err     error
}

type sseEvent struct {
	Name string
	Data string
}

func connectNetwork(ctx context.Context, server Server) (ToolClient, error) {
	httpClient, err := oauthHTTPClient(server)
	if err != nil {
		return nil, err
	}
	client := &networkClient{
		server: server,
		client: httpClient,
		nextID: 1,
	}
	if err := client.initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize MCP server %s: %w", server.Name, err)
	}
	return client, nil
}

func connectRemoteSSE(ctx context.Context, server Server) (ToolClient, error) {
	httpClient, err := oauthHTTPClient(server)
	if err != nil {
		return nil, err
	}
	client := &remoteSSEClient{
		server:  server,
		client:  httpClient,
		nextID:  1,
		pending: map[string]chan ssePendingResponse{},
	}
	if err := client.openStream(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect MCP SSE server %s: %w", server.Name, err)
	}
	if err := client.initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize MCP server %s: %w", server.Name, err)
	}
	return client, nil
}

func (client *networkClient) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "zero",
			"version": "dev",
		},
	}, &result); err != nil {
		return err
	}
	return client.notify(ctx, "notifications/initialized", map[string]any{})
}

func (client *remoteSSEClient) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "zero",
			"version": "dev",
		},
	}, &result); err != nil {
		return err
	}
	return client.notify(ctx, "notifications/initialized", map[string]any{})
}

func (client *networkClient) ListTools(ctx context.Context) ([]RemoteTool, error) {
	var result struct {
		Tools []RemoteTool `json:"tools"`
	}
	if err := client.request(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (client *remoteSSEClient) ListTools(ctx context.Context) ([]RemoteTool, error) {
	var result struct {
		Tools []RemoteTool `json:"tools"`
	}
	if err := client.request(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (client *networkClient) CallTool(ctx context.Context, name string, args map[string]any) (CallToolResult, error) {
	var result CallToolResult
	if err := client.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, &result); err != nil {
		return CallToolResult{}, err
	}
	return result, nil
}

func (client *remoteSSEClient) CallTool(ctx context.Context, name string, args map[string]any) (CallToolResult, error) {
	var result CallToolResult
	if err := client.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, &result); err != nil {
		return CallToolResult{}, err
	}
	return result, nil
}

func (client *networkClient) Close() error {
	return nil
}

func (client *remoteSSEClient) Close() error {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return nil
	}
	client.closed = true
	pending := client.pending
	client.pending = map[string]chan ssePendingResponse{}
	streamBody := client.streamBody
	cancel := client.streamCancel
	client.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	var err error
	if streamBody != nil {
		err = streamBody.Close()
	}
	closeErr := fmt.Errorf("MCP SSE client for %s is closed", client.server.Name)
	for _, channel := range pending {
		channel <- ssePendingResponse{err: closeErr}
		close(channel)
	}
	return err
}

func (client *networkClient) request(ctx context.Context, method string, params any, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	id := client.nextID
	client.nextID++
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	message, err := client.post(ctx, rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}, true)
	if err != nil {
		return err
	}
	if message.isRequestOrNotification() {
		return fmt.Errorf("MCP %s expected a response from server %s, got method %q instead", method, client.server.Name, message.Method)
	}
	if !rpcIDMatches(message.ID, id) {
		return fmt.Errorf("MCP %s response id mismatch for server %s", method, client.server.Name)
	}
	if message.Error != nil {
		return fmt.Errorf("MCP %s failed: %s", method, message.Error.Message)
	}
	if target != nil && len(message.Result) > 0 {
		if err := json.Unmarshal(message.Result, target); err != nil {
			return fmt.Errorf("decode MCP %s result: %w", method, err)
		}
	}
	return nil
}

func (client *remoteSSEClient) request(ctx context.Context, method string, params any, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	client.mu.Lock()
	if client.streamErr != nil {
		err := client.streamErr
		client.mu.Unlock()
		return err
	}
	if client.closed {
		client.mu.Unlock()
		return fmt.Errorf("MCP SSE client for %s is closed", client.server.Name)
	}
	id := client.nextID
	client.nextID++
	key := rpcResponseKey(id)
	responseChannel := make(chan ssePendingResponse, 1)
	client.pending[key] = responseChannel
	client.mu.Unlock()

	if err := client.post(ctx, rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		client.removePending(key)
		return err
	}

	select {
	case response := <-responseChannel:
		if response.err != nil {
			return response.err
		}
		if !rpcIDMatches(response.message.ID, id) {
			return fmt.Errorf("MCP %s response id mismatch for server %s", method, client.server.Name)
		}
		if response.message.Error != nil {
			return fmt.Errorf("MCP %s failed: %s", method, response.message.Error.Message)
		}
		if target != nil && len(response.message.Result) > 0 {
			if err := json.Unmarshal(response.message.Result, target); err != nil {
				return fmt.Errorf("decode MCP %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		client.removePending(key)
		return ctx.Err()
	}
}

func (client *networkClient) notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	_, err = client.post(ctx, rpcMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}, false)
	return err
}

func (client *remoteSSEClient) notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return client.post(ctx, rpcMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	})
}

func (client *networkClient) post(ctx context.Context, message rpcMessage, expectResponse bool) (result rpcMessage, err error) {
	body, err := json.Marshal(message)
	if err != nil {
		return rpcMessage{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.server.URL, bytes.NewReader(body))
	if err != nil {
		return rpcMessage{}, fmt.Errorf("create MCP %s request for %s: %w", client.server.Type, client.server.Name, err)
	}
	client.applyHeaders(request)

	response, err := client.client.Do(request)
	if err != nil {
		return rpcMessage{}, fmt.Errorf("call MCP %s server %s: %w", client.server.Type, client.server.Name, err)
	}
	defer closeResponseBody(&err, client.server, response.Body)

	if sessionID := strings.TrimSpace(response.Header.Get("Mcp-Session-Id")); sessionID != "" {
		client.sessionID = sessionID
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return rpcMessage{}, client.statusError(response)
	}
	if !expectResponse {
		_, _ = io.Copy(io.Discard, response.Body)
		return rpcMessage{}, nil
	}
	if response.StatusCode == http.StatusNoContent {
		return rpcMessage{}, fmt.Errorf("MCP %s server %s returned no response body", client.server.Type, client.server.Name)
	}
	return client.decodeResponse(response)
}

func (client *remoteSSEClient) openStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	client.streamCancel = cancel

	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, client.server.URL, nil)
	if err != nil {
		return fmt.Errorf("create MCP SSE stream request for %s: %w", client.server.Name, err)
	}
	client.applyStreamHeaders(request)

	response, err := client.client.Do(request)
	if err != nil {
		return fmt.Errorf("open MCP SSE stream for %s: %w", client.server.Name, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		err = httpStatusError(client.server, response)
		closeResponseBody(&err, client.server, response.Body)
		return err
	}
	client.streamBody = response.Body

	ready := make(chan error, 1)
	go client.readStream(response.Body, ready)
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (client *remoteSSEClient) post(ctx context.Context, message rpcMessage) (err error) {
	endpointURL, err := client.currentEndpoint()
	if err != nil {
		return err
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create MCP SSE post for %s: %w", client.server.Name, err)
	}
	client.applyPostHeaders(request)

	response, err := client.client.Do(request)
	if err != nil {
		return fmt.Errorf("post MCP SSE message to %s: %w", client.server.Name, err)
	}
	defer closeResponseBody(&err, client.server, response.Body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return httpStatusError(client.server, response)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func (client *networkClient) applyHeaders(request *http.Request) {
	for key, value := range client.server.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if client.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", client.sessionID)
	}
}

func (client *remoteSSEClient) applyStreamHeaders(request *http.Request) {
	for key, value := range client.server.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Accept", "text/event-stream")
}

func (client *remoteSSEClient) applyPostHeaders(request *http.Request) {
	for key, value := range client.server.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
}

func closeResponseBody(errp *error, server Server, body io.Closer) {
	if closeErr := body.Close(); closeErr != nil {
		*errp = errors.Join(*errp, fmt.Errorf("close MCP %s response from %s: %w", server.Type, server.Name, closeErr))
	}
}

func (client *networkClient) decodeResponse(response *http.Response) (rpcMessage, error) {
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if contentType == "text/event-stream" {
		return decodeSSERPCMessage(response.Body)
	}

	var message rpcMessage
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&message); err != nil {
		return rpcMessage{}, fmt.Errorf("decode MCP %s response from %s: %w", client.server.Type, client.server.Name, err)
	}
	return message, nil
}

func (client *networkClient) statusError(response *http.Response) error {
	return httpStatusError(client.server, response)
}

func (client *remoteSSEClient) readStream(reader io.Reader, ready chan<- error) {
	endpointReady := false
	signalReady := func(err error) {
		if endpointReady {
			return
		}
		endpointReady = true
		ready <- err
	}

	var eventErr error
	err := scanSSEEvents(reader, func(event sseEvent) bool {
		switch event.Name {
		case "endpoint":
			if err := client.setEndpoint(event.Data); err != nil {
				eventErr = err
				signalReady(err)
				return false
			}
			signalReady(nil)
		case "message":
			if err := client.deliverEventMessage(event.Data); err != nil {
				eventErr = err
				return false
			}
		}
		return true
	})
	if eventErr != nil {
		if !endpointReady {
			signalReady(eventErr)
		}
		client.failPending(eventErr)
		return
	}
	if err != nil {
		if !endpointReady {
			signalReady(err)
		}
		client.failPending(err)
		return
	}
	if !endpointReady {
		err = fmt.Errorf("missing MCP SSE endpoint event for server %s", client.server.Name)
		signalReady(err)
		client.failPending(err)
		return
	}
	client.failPending(fmt.Errorf("MCP SSE stream closed for server %s", client.server.Name))
}

func (client *remoteSSEClient) setEndpoint(value string) error {
	endpointURL, err := resolveSSEEndpointURL(client.server.URL, strings.TrimSpace(value))
	if err != nil {
		return err
	}
	client.mu.Lock()
	client.endpointURL = endpointURL
	client.mu.Unlock()
	return nil
}

func (client *remoteSSEClient) deliverEventMessage(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var message rpcMessage
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(&message); err != nil {
		return fmt.Errorf("decode MCP SSE stream message: %w", err)
	}
	if message.isRequestOrNotification() {
		return nil
	}
	key := rpcResponseKey(message.ID)
	if key == "" {
		return nil
	}

	client.mu.Lock()
	channel := client.pending[key]
	if channel != nil {
		delete(client.pending, key)
	}
	client.mu.Unlock()
	if channel != nil {
		channel <- ssePendingResponse{message: message}
		close(channel)
	}
	return nil
}

func (client *remoteSSEClient) currentEndpoint() (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.streamErr != nil {
		return "", client.streamErr
	}
	if client.closed {
		return "", fmt.Errorf("MCP SSE client for %s is closed", client.server.Name)
	}
	if client.endpointURL == "" {
		return "", fmt.Errorf("MCP SSE endpoint is not ready for server %s", client.server.Name)
	}
	return client.endpointURL, nil
}

func (client *remoteSSEClient) removePending(key string) {
	client.mu.Lock()
	channel := client.pending[key]
	if channel != nil {
		delete(client.pending, key)
	}
	client.mu.Unlock()
	if channel != nil {
		close(channel)
	}
}

func (client *remoteSSEClient) failPending(err error) {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
	}
	client.streamErr = err
	pending := client.pending
	client.pending = map[string]chan ssePendingResponse{}
	client.mu.Unlock()
	for _, channel := range pending {
		channel <- ssePendingResponse{err: err}
		close(channel)
	}
}

func httpStatusError(server Server, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fmt.Errorf("MCP %s server %s returned HTTP %d", server.Type, server.Name, response.StatusCode)
	}
	return fmt.Errorf("MCP %s server %s returned HTTP %d: %s", server.Type, server.Name, response.StatusCode, detail)
}

func decodeSSERPCMessage(reader io.Reader) (rpcMessage, error) {
	var decoded rpcMessage
	var decodeErr error
	found := false
	if err := scanSSEEvents(reader, func(event sseEvent) bool {
		if event.Name != "message" {
			return true
		}
		value := strings.TrimSpace(event.Data)
		if value == "" {
			return true
		}
		var candidate rpcMessage
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&candidate); err != nil {
			decodeErr = fmt.Errorf("decode MCP SSE response: %w", err)
			return false
		}
		// The POST's event stream may carry server-initiated notifications or
		// requests (which have a method) before the response to our request. Skip
		// those — the response has no method — and keep scanning. Previously the
		// first message event was returned unconditionally, so a leading
		// notification surfaced to the caller as an id mismatch and failed the call.
		if candidate.isRequestOrNotification() {
			return true
		}
		decoded = candidate
		found = true
		return false
	}); err != nil {
		return rpcMessage{}, err
	}
	if decodeErr != nil {
		return rpcMessage{}, decodeErr
	}
	if found {
		return decoded, nil
	}
	return rpcMessage{}, fmt.Errorf("missing MCP SSE response data")
}

// maxSSEEventBytes bounds a single SSE line/event. The previous 1 MiB cap made a
// large but legitimate MCP message (e.g. a big tool result) hit bufio.ErrTooLong,
// which failed the request permanently with no recovery. Raise it to a generous
// bound that still protects against an unbounded remote server.
const maxSSEEventBytes = 8 * 1024 * 1024

func scanSSEEvents(reader io.Reader, handle func(sseEvent) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEEventBytes)

	event := sseEvent{Name: "message"}
	dataLines := []string{}
	dataBytes := 0
	flush := func() bool {
		if len(dataLines) == 0 {
			event = sseEvent{Name: "message"}
			return true
		}
		event.Data = strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		dataBytes = 0
		keepReading := handle(event)
		event = sseEvent{Name: "message"}
		return keepReading
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field = line
			value = ""
		} else {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			event.Name = value
		case "data":
			// The per-line scanner cap bounds one line, but many `data:` lines
			// accumulate before a blank line flushes the event. Cap the aggregate too
			// so a server can't force unbounded memory by never terminating the event.
			// Only count the joining newline once there is prior content, mirroring
			// strings.Join, so a single line isn't over-counted.
			if dataBytes > 0 {
				dataBytes++ // joining newline between data lines
			}
			dataBytes += len(value)
			if dataBytes > maxSSEEventBytes {
				return fmt.Errorf("MCP SSE event exceeds %d bytes", maxSSEEventBytes)
			}
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

func resolveSSEEndpointURL(baseValue string, endpointValue string) (string, error) {
	if endpointValue == "" {
		return "", fmt.Errorf("MCP SSE endpoint event is empty")
	}
	baseURL, err := url.Parse(baseValue)
	if err != nil {
		return "", fmt.Errorf("parse MCP SSE base URL: %w", err)
	}
	endpointURL, err := url.Parse(endpointValue)
	if err != nil {
		return "", fmt.Errorf("parse MCP SSE endpoint URL: %w", err)
	}
	resolvedURL := baseURL.ResolveReference(endpointURL)
	if !sameMCPOrigin(baseURL, resolvedURL) {
		return "", fmt.Errorf("MCP SSE endpoint origin %s differs from configured server origin %s", mcpOrigin(resolvedURL), mcpOrigin(baseURL))
	}
	return resolvedURL.String(), nil
}

func rpcResponseKey(id any) string {
	if id == nil {
		return ""
	}
	return fmt.Sprint(id)
}

// oauthTokenSource supplies a current bearer token and refreshes it on demand.
type oauthTokenSource interface {
	AccessToken(ctx context.Context) (string, error)
	Refresh(ctx context.Context) (string, error)
}

// oauthRoundTripper attaches a bearer token to every request and, on a 401,
// refreshes the token once and retries the request a single time. A refresh
// failure is surfaced as an actionable error that points at the login command.
type oauthRoundTripper struct {
	base       http.RoundTripper
	source     oauthTokenSource
	serverName string
}

func newOAuthRoundTripper(base http.RoundTripper, source oauthTokenSource, serverName string) *oauthRoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &oauthRoundTripper{base: base, source: source, serverName: serverName}
}

func (transport *oauthRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := transport.source.AccessToken(request.Context())
	if err != nil {
		return nil, err
	}

	first, body, err := cloneRequestWithBearer(request, token)
	if err != nil {
		return nil, err
	}
	response, err := transport.base.RoundTrip(first)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}

	// Drain and close the 401 body before retrying to free the connection.
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	refreshed, refreshErr := transport.source.Refresh(request.Context())
	if refreshErr != nil {
		return nil, fmt.Errorf("MCP OAuth token refresh failed for %s: re-run `zero mcp oauth login %s`: %w", transport.serverName, transport.serverName, refreshErr)
	}

	retry, _, err := cloneRequestWithBearer(request, refreshed)
	if err != nil {
		return nil, err
	}
	if body != nil {
		retry.Body = io.NopCloser(bytes.NewReader(body))
		retry.ContentLength = int64(len(body))
	}
	return transport.base.RoundTrip(retry)
}

// cloneRequestWithBearer copies a request, buffers its body so the request can
// be retried, and sets the Authorization header.
func cloneRequestWithBearer(request *http.Request, token string) (*http.Request, []byte, error) {
	var body []byte
	if request.Body != nil {
		buffered, err := io.ReadAll(request.Body)
		_ = request.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		body = buffered
	}
	clone := request.Clone(request.Context())
	if body != nil {
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
	}
	if strings.TrimSpace(token) != "" {
		clone.Header.Set("Authorization", "Bearer "+token)
	}
	return clone, body, nil
}

// storeTokenSource adapts the persistent token store and refresh logic to the
// oauthTokenSource interface used by the round tripper.
type storeTokenSource struct {
	server     Server
	store      *TokenStore
	httpClient *http.Client
	now        func() time.Time
}

func (source *storeTokenSource) config() OAuthConfig {
	if source.server.OAuth != nil {
		return *source.server.OAuth
	}
	return OAuthConfig{}
}

func (source *storeTokenSource) AccessToken(ctx context.Context) (string, error) {
	token, ok, err := source.store.LoadForServer(source.server)
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(token.AccessToken) == "" {
		return "", fmt.Errorf("no stored OAuth token for MCP server %s: run `zero mcp oauth login %s`", source.server.Name, source.server.Name)
	}
	return token.AccessToken, nil
}

func (source *storeTokenSource) Refresh(ctx context.Context) (string, error) {
	token, ok, err := source.store.LoadForServer(source.server)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no stored OAuth token for MCP server %s", source.server.Name)
	}
	refreshed, err := refreshAccessToken(ctx, source.httpClient, source.config(), token, source.now)
	if err != nil {
		return "", err
	}
	if err := source.store.SaveForServer(source.server, refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// oauthHTTPClient returns an HTTP client whose transport attaches OAuth bearer
// tokens and refreshes them on 401 for OAuth-configured servers. Servers that do
// not declare OAuth use the default transport with the same MCP redirect guard.
func oauthHTTPClient(server Server) (*http.Client, error) {
	if !strings.EqualFold(strings.TrimSpace(server.Auth), ServerAuthOAuth) {
		return mcpHTTPClient(server, nil), nil
	}
	store, err := NewTokenStore(TokenStoreOptions{})
	if err != nil {
		return nil, err
	}
	source := &storeTokenSource{
		server:     server,
		store:      store,
		httpClient: http.DefaultClient,
		now:        time.Now,
	}
	return mcpHTTPClient(server, newOAuthRoundTripper(http.DefaultTransport, source, server.Name)), nil
}
