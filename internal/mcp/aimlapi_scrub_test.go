package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/aimlapi"
)

// scrubTestKey has no credential shape, so only the known-value scrub can find
// it; a shape pattern would leave it alone.
const scrubTestKey = "aiml0123456789abcdef0123456789abcd"

// aimlapiEcho stands in for mcp.aimlapi.com over streamable HTTP. It answers
// each JSON-RPC request with reply(method, id), as a JSON body or as one SSE
// event, and records the Authorization header every request carried.
type aimlapiEcho struct {
	sse   bool
	reply func(method string, id json.RawMessage) string
	mu    sync.Mutex
	auth  []string
}

func (echo *aimlapiEcho) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		_ = request.Body.Close()
	}
	echo.mu.Lock()
	echo.auth = append(echo.auth, request.Header.Get("Authorization"))
	echo.mu.Unlock()
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &message)
	if len(message.ID) == 0 {
		return fakeHTTPResponse(request, http.StatusAccepted, ""), nil
	}
	reply := echo.reply(message.Method, message.ID)
	response := fakeHTTPResponse(request, http.StatusOK, reply)
	if echo.sse {
		response.Header.Set("Content-Type", "text/event-stream")
		response.Body = io.NopCloser(strings.NewReader("event: message\ndata: " + reply + "\n\n"))
	}
	return response, nil
}

// assertSent fails unless every request carried want as its Authorization, so a
// test cannot pass by never reaching the injected-key path.
func (echo *aimlapiEcho) assertSent(t *testing.T, want string) {
	t.Helper()
	echo.mu.Lock()
	defer echo.mu.Unlock()
	if len(echo.auth) == 0 {
		t.Fatal("the fake server was never called")
	}
	for _, got := range echo.auth {
		if got != want {
			t.Fatalf("Authorization = %q, want %q", got, want)
		}
	}
}

func rpcReply(id json.RawMessage, field string, value any) string {
	encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, field: value})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// echoedKeys are the spellings of the key a server echoes back: whole, and
// split by a NUL and by a zero-width space, which a plain replace would miss.
func echoedKeys() []string {
	return []string{
		scrubTestKey,
		scrubTestKey[:10] + string(rune(0)) + scrubTestKey[10:],
		scrubTestKey[:20] + string(rune(0x200b)) + scrubTestKey[20:],
	}
}

// assertNoPartOf fails if any eight-byte run of key survives in text.
func assertNoPartOf(t *testing.T, text string, key string) {
	t.Helper()
	for start := 0; start+8 <= len(key); start++ {
		if strings.Contains(text, key[start:start+8]) {
			t.Fatalf("text still holds part of the key (%q): %q", key[start:start+8], text)
		}
	}
}

// assertKeyScrubbed fails if any part of scrubTestKey survives in text, or if
// text does not show the replacement.
func assertKeyScrubbed(t *testing.T, text string) {
	t.Helper()
	assertNoPartOf(t, text, scrubTestKey)
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("text = %q, want the key replaced by [REDACTED]", text)
	}
}

func TestScrubAIMLAPIKeysKeepsAResultWithoutTheKeyByteForByte(t *testing.T) {
	raw := json.RawMessage(`{"tools": [ {"name": "b", "description": "password=hunter2"} ], "a": 1.50}`)
	if got := scrubAIMLAPIKeysFromJSON(raw, []string{scrubTestKey}); string(got) != string(raw) {
		t.Fatalf("result without the key = %s, want the original bytes %s", got, raw)
	}

	withKey := json.RawMessage(`{"content": [ {"type": "text", "text": "key ` + scrubTestKey + `"} ], "n": 1.50}`)
	got := scrubAIMLAPIKeysFromJSON(withKey, []string{scrubTestKey})
	assertKeyScrubbed(t, string(got))
	var decoded struct {
		Content []Content   `json:"content"`
		N       json.Number `json:"n"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil || len(decoded.Content) != 1 || decoded.Content[0].Text != "key [REDACTED]" || decoded.N != "1.50" {
		t.Fatalf("scrubbed result = %s (%v), want valid JSON with only the key replaced and the number as written", got, err)
	}
}

func newEchoNetworkClient(server Server, echo *aimlapiEcho) *networkClient {
	return &networkClient{server: server, client: mcpHTTPClient(server, echo), nextID: 1}
}

func TestAIMLAPIMCPScrubsTheInjectedKeyFromResponses(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, scrubTestKey)
	server := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	for _, sse := range []bool{false, true} {
		transport := "json"
		if sse {
			transport = "sse"
		}
		t.Run(transport+"/error message", func(t *testing.T) {
			for _, echoed := range echoedKeys() {
				echo := &aimlapiEcho{sse: sse, reply: func(_ string, id json.RawMessage) string {
					return rpcReply(id, "error", map[string]any{"code": -32001, "message": "invalid credential " + echoed + " for this account"})
				}}
				err := newEchoNetworkClient(server, echo).initialize(context.Background())
				if err == nil {
					t.Fatal("initialize succeeded against a server that answered with an error")
				}
				echo.assertSent(t, "Bearer "+scrubTestKey)
				assertKeyScrubbed(t, err.Error())
				if !strings.Contains(err.Error(), "invalid credential [REDACTED] for this account") {
					t.Fatalf("error = %q, want the rest of the server text kept", err)
				}
			}
		})
		t.Run(transport+"/tool result", func(t *testing.T) {
			const image = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
			for _, echoed := range echoedKeys() {
				echo := &aimlapiEcho{sse: sse, reply: func(_ string, id json.RawMessage) string {
					return rpcReply(id, "result", map[string]any{
						"isError": true,
						"content": []any{
							map[string]any{"type": "text", "text": "upstream rejected key " + echoed},
							map[string]any{"type": "image", "mimeType": "image/png", "data": image},
						},
					})
				}}
				result, err := newEchoNetworkClient(server, echo).CallTool(context.Background(), "generate", map[string]any{"prompt": "x"})
				if err != nil {
					t.Fatalf("CallTool: %v", err)
				}
				echo.assertSent(t, "Bearer "+scrubTestKey)
				if len(result.Content) != 2 || !result.IsError {
					t.Fatalf("result = %+v, want the error result with both blocks", result)
				}
				assertKeyScrubbed(t, result.Content[0].Text)
				if result.Content[0].Text != "upstream rejected key [REDACTED]" {
					t.Fatalf("text block = %q, want only the key replaced", result.Content[0].Text)
				}
				if result.Content[1].Data != image {
					t.Fatalf("image data changed: %q", result.Content[1].Data)
				}
			}
		})
		t.Run(transport+"/result without the key", func(t *testing.T) {
			// Shapes stay as the server wrote them: only the known value goes.
			const description = "Set password=hunter2 or pass Authorization: Bearer other-token"
			echo := &aimlapiEcho{sse: sse, reply: func(_ string, id json.RawMessage) string {
				return rpcReply(id, "result", map[string]any{"tools": []any{map[string]any{
					"name":        "generate",
					"description": description,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"api_key": map[string]any{"type": "string"}}},
				}}})
			}}
			tools, err := newEchoNetworkClient(server, echo).ListTools(context.Background())
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			echo.assertSent(t, "Bearer "+scrubTestKey)
			if len(tools) != 1 || tools[0].Description != description {
				t.Fatalf("tools = %+v, want the description unchanged", tools)
			}
			properties, _ := tools[0].InputSchema["properties"].(map[string]any)
			if _, ok := properties["api_key"]; !ok {
				t.Fatalf("inputSchema = %v, want the api_key property kept", tools[0].InputSchema)
			}
		})
	}

	t.Run("json/unexpected method", func(t *testing.T) {
		echo := &aimlapiEcho{reply: func(string, json.RawMessage) string {
			return `{"jsonrpc":"2.0","method":"notifications/` + scrubTestKey + `"}`
		}}
		err := newEchoNetworkClient(server, echo).initialize(context.Background())
		if err == nil || !strings.Contains(err.Error(), "got method") {
			t.Fatalf("initialize error = %v, want the unexpected method reported", err)
		}
		echo.assertSent(t, "Bearer "+scrubTestKey)
		assertKeyScrubbed(t, err.Error())
	})
}

func TestAIMLAPIMCPLeavesResponsesAloneWithoutTheInjectedKey(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, scrubTestKey)
	// A user-configured Authorization header means nothing was injected, so the
	// server text is reported as written; this change scrubs only what Zero
	// itself attached.
	server := aimlapiTestServer("aimlapi", aimlapi.MCPServerURL)
	server.Headers = map[string]string{"Authorization": "Bearer " + scrubTestKey}
	echo := &aimlapiEcho{reply: func(_ string, id json.RawMessage) string {
		return rpcReply(id, "error", map[string]any{"code": -32001, "message": "invalid credential " + scrubTestKey})
	}}
	err := newEchoNetworkClient(server, echo).initialize(context.Background())
	echo.assertSent(t, "Bearer "+scrubTestKey)
	if err == nil || !strings.Contains(err.Error(), "invalid credential "+scrubTestKey) {
		t.Fatalf("error = %v, want the server text unchanged when no key was injected", err)
	}
}

// aimlapiSSEEcho stands in for mcp.aimlapi.com over the legacy SSE transport:
// the GET opens a stream that starts with the endpoint event, and each POST is
// answered on that stream with reply(method, id).
type aimlapiSSEEcho struct {
	endpoint string
	reply    func(method string, id json.RawMessage) string
	events   chan string
	mu       sync.Mutex
	auth     []string
}

func newAIMLAPISSEEcho(endpoint string, reply func(string, json.RawMessage) string) *aimlapiSSEEcho {
	return &aimlapiSSEEcho{endpoint: endpoint, reply: reply, events: make(chan string, 8)}
}

func (echo *aimlapiSSEEcho) RoundTrip(request *http.Request) (*http.Response, error) {
	echo.mu.Lock()
	echo.auth = append(echo.auth, request.Header.Get("Authorization"))
	echo.mu.Unlock()
	if request.Method == http.MethodGet {
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			if _, err := io.WriteString(writer, "event: endpoint\ndata: "+echo.endpoint+"\n\n"); err != nil {
				return
			}
			for {
				select {
				case event := <-echo.events:
					if _, err := io.WriteString(writer, event); err != nil {
						return
					}
				case <-request.Context().Done():
					return
				}
			}
		}()
		response := fakeHTTPResponse(request, http.StatusOK, "")
		response.Header.Set("Content-Type", "text/event-stream")
		response.Body = reader
		return response, nil
	}
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		_ = request.Body.Close()
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &message)
	if len(message.ID) > 0 {
		echo.events <- "event: message\ndata: " + echo.reply(message.Method, message.ID) + "\n\n"
	}
	return fakeHTTPResponse(request, http.StatusAccepted, ""), nil
}

func (echo *aimlapiSSEEcho) assertSent(t *testing.T, want string, requests int) {
	t.Helper()
	echo.mu.Lock()
	defer echo.mu.Unlock()
	if len(echo.auth) < requests {
		t.Fatalf("the fake server saw %d requests, want at least %d", len(echo.auth), requests)
	}
	for _, got := range echo.auth {
		if got != want {
			t.Fatalf("Authorization = %q, want %q", got, want)
		}
	}
}

func newEchoSSEClient(echo *aimlapiSSEEcho) *remoteSSEClient {
	server := Server{Name: "aimlapi", Type: ServerTypeSSE, URL: aimlapi.MCPServerURL}
	return &remoteSSEClient{
		server:  server,
		client:  mcpHTTPClient(server, echo),
		nextID:  1,
		pending: map[string]chan ssePendingResponse{},
	}
}

func TestAIMLAPIMCPSSEScrubsTheInjectedKeyFromStreamAnswers(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, scrubTestKey)
	for _, echoed := range echoedKeys() {
		echo := newAIMLAPISSEEcho("/messages", func(method string, id json.RawMessage) string {
			if method == "tools/call" {
				return rpcReply(id, "result", map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "rejected " + echoed}}})
			}
			return rpcReply(id, "error", map[string]any{"code": -32001, "message": "invalid credential " + echoed})
		})
		client := newEchoSSEClient(echo)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := client.openStream(ctx); err != nil {
			cancel()
			t.Fatalf("openStream: %v", err)
		}
		_, listErr := client.ListTools(ctx)
		result, callErr := client.CallTool(ctx, "generate", map[string]any{})
		_ = client.Close()
		cancel()

		echo.assertSent(t, "Bearer "+scrubTestKey, 3)
		if listErr == nil {
			t.Fatal("ListTools succeeded against a server that answered with an error")
		}
		assertKeyScrubbed(t, listErr.Error())
		if callErr != nil || len(result.Content) != 1 {
			t.Fatalf("CallTool = %+v, %v, want the error result", result, callErr)
		}
		assertKeyScrubbed(t, result.Content[0].Text)
	}
}

func TestAIMLAPIMCPSSEScrubsTheStreamKeyAndThePostKey(t *testing.T) {
	// The key is resolved for every request, so the stream and a later POST can
	// carry different keys when the provider changes in between. An answer on
	// the stream comes from a server that was sent both.
	const streamKey = "strm9f8e7d6c5b4a39281706f5e4d3c2b1"
	const postKey = "post1a2b3c4d5e6f708192a3b4c5d6e7f8"
	keys := []string{streamKey, postKey}
	previous := loadAIMLAPIServiceKey
	loadAIMLAPIServiceKey = func() (string, error) {
		key := keys[0]
		if len(keys) > 1 {
			keys = keys[1:]
		}
		return key, nil
	}
	t.Cleanup(func() { loadAIMLAPIServiceKey = previous })

	echo := newAIMLAPISSEEcho("/messages", func(_ string, id json.RawMessage) string {
		return rpcReply(id, "error", map[string]any{"code": -32001, "message": "stream key " + streamKey + ", post key " + postKey})
	})
	client := newEchoSSEClient(echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.openStream(ctx); err != nil {
		t.Fatalf("openStream: %v", err)
	}
	_, err := client.ListTools(ctx)
	_ = client.Close()

	echo.mu.Lock()
	auth := append([]string(nil), echo.auth...)
	echo.mu.Unlock()
	if len(auth) != 2 || auth[0] != "Bearer "+streamKey || auth[1] != "Bearer "+postKey {
		t.Fatalf("Authorization headers = %q, want the stream key and then the post key", auth)
	}
	if err == nil {
		t.Fatal("ListTools succeeded against a server that answered with an error")
	}
	assertNoPartOf(t, err.Error(), streamKey)
	assertNoPartOf(t, err.Error(), postKey)
	if !strings.Contains(err.Error(), "stream key [REDACTED], post key [REDACTED]") {
		t.Fatalf("error = %q, want both keys replaced", err)
	}
}

func TestAIMLAPIMCPSSEScrubsTheInjectedKeyFromAnEndpointError(t *testing.T) {
	isolateAIMLAPIKeyLookup(t, scrubTestKey)
	// An invalid escape makes the endpoint fail to parse, and the parse error
	// quotes the endpoint the server sent.
	echo := newAIMLAPISSEEcho("/messages/"+scrubTestKey+"%zz", func(string, json.RawMessage) string { return "" })
	client := newEchoSSEClient(echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.openStream(ctx)
	_ = client.Close()
	echo.assertSent(t, "Bearer "+scrubTestKey, 1)
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("openStream error = %v, want the endpoint parse failure", err)
	}
	assertKeyScrubbed(t, err.Error())
}
