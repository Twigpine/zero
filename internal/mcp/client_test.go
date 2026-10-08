package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/execution"
)

type mcpExecutionPreparer struct {
	request execution.Request
}

func (preparer *mcpExecutionPreparer) PrepareExecution(ctx context.Context, request execution.Request) (execution.PreparedCommand, error) {
	preparer.request = request
	command := exec.CommandContext(ctx, request.Command.Name, request.Command.Args...)
	command.Dir = request.WorkingDirectory
	command.Env = request.Command.Env
	return execution.PreparedCommand{Command: command}, nil
}

func TestStdioClientListsAndCallsTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	client, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeStdio,
		Command: executable,
		Args:    []string{"-test.run=TestMCPStdioHelperProcess", "--"},
		Env:     map[string]string{"ZERO_MCP_STDIO_HELPER": "1"},
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	listed, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "lookup" {
		t.Fatalf("listed tools = %#v, want lookup", listed)
	}
	if listed[0].InputSchema["type"] != "object" {
		t.Fatalf("lookup schema = %#v, want object schema", listed[0].InputSchema)
	}

	result, err := client.CallTool(ctx, "lookup", map[string]any{"query": "zero"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() result IsError = true: %#v", result)
	}
	if got := TextContent(result.Content); got != "lookup: zero" {
		t.Fatalf("CallTool() text = %q, want lookup result", got)
	}
}

func TestStdioClientUsesTypedMCPExecutionOrigin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	preparer := &mcpExecutionPreparer{}
	workspace := t.TempDir()
	client, err := ConnectWithOptions(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeStdio,
		Command: executable,
		Args:    []string{"-test.run=TestMCPStdioHelperProcess", "--"},
		Env:     map[string]string{"ZERO_MCP_STDIO_HELPER": "1"},
	}, ConnectOptions{Execution: execution.NewRunner(preparer), WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("ConnectWithOptions() error = %v", err)
	}
	defer client.Close()
	if preparer.request.Origin != execution.OriginMCPServer || preparer.request.Mode != execution.ModeDurable {
		t.Fatalf("execution request = %#v", preparer.request)
	}
	if preparer.request.WorkingDirectory != workspace {
		t.Fatalf("working directory = %q, want %q", preparer.request.WorkingDirectory, workspace)
	}
}

func TestStdioClientCloseAllowsConcurrentCallers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	client, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeStdio,
		Command: executable,
		Args:    []string{"-test.run=TestMCPStdioHelperProcess", "--"},
		Env:     map[string]string{"ZERO_MCP_STDIO_HELPER": "1"},
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- client.Close()
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
}

func TestHTTPClientListsAndCallsTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	testServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
			http.Error(response, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Path != "/mcp" {
			t.Errorf("path = %s, want /mcp", request.URL.Path)
			http.Error(response, "bad path", http.StatusNotFound)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test" {
			t.Errorf("Authorization = %q, want bearer header", got)
			http.Error(response, "missing auth", http.StatusUnauthorized)
			return
		}

		message := readHTTPRPCMessage(t, request)
		switch message.Method {
		case "initialize":
			response.Header().Set("Mcp-Session-Id", "session-123")
			writeHTTPRPCResponse(t, response, message.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "http-docs", "version": "1.0.0"},
			})
		case "notifications/initialized":
			if got := request.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Errorf("initialized session header = %q, want session-123", got)
				http.Error(response, "missing session", http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if got := request.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Errorf("tools/list session header = %q, want session-123", got)
				http.Error(response, "missing session", http.StatusBadRequest)
				return
			}
			writeHTTPRPCResponse(t, response, message.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "lookup",
					"description": "Lookup documentation",
					"inputSchema": map[string]any{"type": "object"},
				}},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Errorf("decode tools/call params: %v", err)
				http.Error(response, "bad params", http.StatusBadRequest)
				return
			}
			if params.Name != "lookup" || params.Arguments["query"] != "zero" {
				t.Errorf("tools/call params = %#v", params)
				http.Error(response, "bad tool call", http.StatusBadRequest)
				return
			}
			writeHTTPRPCResponse(t, response, message.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "lookup: zero"}},
			})
		default:
			t.Errorf("unexpected method %q", message.Method)
			writeHTTPRPCError(t, response, message.ID, "method not found")
		}
	}))
	defer testServer.Close()

	client, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeHTTP,
		URL:     testServer.URL + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer test"},
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	listed, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "lookup" {
		t.Fatalf("listed tools = %#v, want lookup", listed)
	}

	result, err := client.CallTool(ctx, "lookup", map[string]any{"query": "zero"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if got := TextContent(result.Content); got != "lookup: zero" {
		t.Fatalf("CallTool() text = %q, want lookup result", got)
	}
}

func TestHTTPClientFollowsSameOriginRedirect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	testServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/mcp" {
			http.Redirect(response, request, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/redirected" {
			t.Errorf("request = %s %s, want POST /redirected", request.Method, request.URL.Path)
			http.Error(response, "bad request", http.StatusNotFound)
			return
		}
		if got := request.Header.Get("X-Api-Key"); got != "secret" {
			t.Errorf("X-Api-Key = %q, want secret", got)
			http.Error(response, "missing auth", http.StatusUnauthorized)
			return
		}

		message := readHTTPRPCMessage(t, request)
		switch message.Method {
		case "initialize":
			writeHTTPRPCResponse(t, response, message.ID, map[string]any{"protocolVersion": "2024-11-05"})
		case "notifications/initialized":
			response.WriteHeader(http.StatusAccepted)
		default:
			writeHTTPRPCResponse(t, response, message.ID, map[string]any{})
		}
	}))
	defer testServer.Close()

	client, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeHTTP,
		URL:     testServer.URL + "/mcp",
		Headers: map[string]string{"X-Api-Key": "secret"},
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestHTTPClientRejectsCrossOriginRedirectBeforeSendingHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		if got := request.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("redirect target received X-Api-Key = %q", got)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeHTTP,
		URL:     redirector.URL + "/mcp",
		Headers: map[string]string{"X-Api-Key": "secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "cross-origin redirect") {
		t.Fatalf("Connect() error = %v, want cross-origin redirect error", err)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

func TestSSEClientListsAndCallsToolsFromRemoteStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan string, 4)
	testServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test" {
			t.Errorf("Authorization = %q, want bearer header", got)
			http.Error(response, "missing auth", http.StatusUnauthorized)
			return
		}

		if request.Method == http.MethodGet && request.URL.Path == "/sse" {
			if got := request.Header.Get("Accept"); !strings.Contains(got, "text/event-stream") {
				t.Errorf("Accept = %q, want text/event-stream", got)
				http.Error(response, "bad accept", http.StatusBadRequest)
				return
			}
			flusher, ok := response.(http.Flusher)
			if !ok {
				t.Errorf("test response writer does not support flushing")
				http.Error(response, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			response.Header().Set("Content-Type", "text/event-stream")
			if _, err := fmt.Fprint(response, "event: endpoint\ndata: /messages\n\n"); err != nil {
				t.Errorf("write endpoint event: %v", err)
				return
			}
			flusher.Flush()
			for {
				select {
				case event := <-events:
					if _, err := fmt.Fprint(response, event); err != nil {
						t.Errorf("write SSE event: %v", err)
						return
					}
					flusher.Flush()
				case <-request.Context().Done():
					return
				}
			}
		}

		if request.Method != http.MethodPost || request.URL.Path != "/messages" {
			t.Errorf("request = %s %s, want POST /messages", request.Method, request.URL.Path)
			http.Error(response, "bad request", http.StatusNotFound)
			return
		}

		message := readHTTPRPCMessage(t, request)
		switch message.Method {
		case "initialize":
			events <- formatSSERPCResponse(t, message.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "sse-docs", "version": "1.0.0"},
			})
			response.WriteHeader(http.StatusAccepted)
		case "notifications/initialized":
			response.WriteHeader(http.StatusNoContent)
		case "tools/list":
			events <- formatSSERPCResponse(t, message.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "lookup",
					"description": "Lookup documentation",
					"inputSchema": map[string]any{"type": "object"},
				}},
			})
			response.WriteHeader(http.StatusAccepted)
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Errorf("decode tools/call params: %v", err)
				http.Error(response, "bad params", http.StatusBadRequest)
				return
			}
			if params.Name != "lookup" || params.Arguments["query"] != "zero" {
				t.Errorf("tools/call params = %#v", params)
				http.Error(response, "bad tool call", http.StatusBadRequest)
				return
			}
			events <- formatSSERPCResponse(t, message.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "lookup: zero"}},
			})
			response.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected method %q", message.Method)
			events <- formatSSERPCError(t, message.ID, "method not found")
			response.WriteHeader(http.StatusAccepted)
		}
	}))
	defer testServer.Close()

	client, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeSSE,
		URL:     testServer.URL + "/sse",
		Headers: map[string]string{"Authorization": "Bearer test"},
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	listed, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "lookup" {
		t.Fatalf("listed tools = %#v, want lookup", listed)
	}

	result, err := client.CallTool(ctx, "lookup", map[string]any{"query": "zero"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if got := TextContent(result.Content); got != "lookup: zero" {
		t.Fatalf("CallTool() text = %q, want lookup result", got)
	}
}

func TestSSEClientRejectsCrossOriginEndpointBeforeSendingHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		if got := request.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("SSE endpoint target received X-Api-Key = %q", got)
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	defer target.Close()

	stream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("X-Api-Key"); got != "secret" {
			t.Errorf("X-Api-Key = %q, want secret on configured SSE server", got)
			http.Error(response, "missing auth", http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodGet || request.URL.Path != "/sse" {
			t.Errorf("request = %s %s, want GET /sse", request.Method, request.URL.Path)
			http.Error(response, "bad request", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(response, "event: endpoint\ndata: %s/messages\n\n", target.URL)
		if flusher, ok := response.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer stream.Close()

	_, err := Connect(ctx, Server{
		Name:    "docs",
		Type:    ServerTypeSSE,
		URL:     stream.URL + "/sse",
		Headers: map[string]string{"X-Api-Key": "secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "endpoint origin") {
		t.Fatalf("Connect() error = %v, want cross-origin endpoint error", err)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("SSE endpoint target hits = %d, want 0", got)
	}
}

func TestHTTPClientReportsNonOKStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	testServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, "server failed", http.StatusBadGateway)
	}))
	defer testServer.Close()

	_, err := Connect(ctx, Server{
		Name: "web",
		Type: ServerTypeHTTP,
		URL:  testServer.URL + "/mcp",
	})
	if err == nil {
		t.Fatal("Connect() error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %q, want HTTP status", err.Error())
	}
}

func TestCloseResponseBodyMergesCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	var err error

	closeResponseBody(&err, Server{Name: "web", Type: ServerTypeHTTP}, errorCloser{err: closeErr})
	if !errors.Is(err, closeErr) {
		t.Fatalf("closeResponseBody() error = %v, want wrapped close error", err)
	}
	if !strings.Contains(err.Error(), "close MCP http response from web") {
		t.Fatalf("closeResponseBody() error = %q, want close context", err.Error())
	}

	baseErr := errors.New("decode failed")
	err = baseErr
	closeResponseBody(&err, Server{Name: "web", Type: ServerTypeHTTP}, errorCloser{err: closeErr})
	if !errors.Is(err, baseErr) || !errors.Is(err, closeErr) {
		t.Fatalf("closeResponseBody() merged error = %v, want base and close errors", err)
	}
}

func TestConnectRejectsUnsupportedTransport(t *testing.T) {
	_, err := Connect(context.Background(), Server{Name: "web", Type: ServerType("websocket")})
	if err == nil {
		t.Fatal("Connect() error = nil, want unsupported transport error")
	}
	if !strings.Contains(err.Error(), "unsupported MCP transport") {
		t.Fatalf("error = %q, want unsupported transport", err.Error())
	}
}

func TestClientRequestWaitsForMatchingResponseID(t *testing.T) {
	// Pipes, not a pre-filled bytes.Buffer: the fake server responds only AFTER
	// reading the outgoing request, matching production ordering. With responses
	// pre-loaded, the client's reader goroutine could consume the matching
	// response — and hit EOF — before request() registered its pending id,
	// flaking as "request() error = EOF" on fast runners.
	serverReader, clientWriter := io.Pipe() // client → server
	clientReader, serverWriter := io.Pipe() // server → client
	t.Cleanup(func() {
		_ = clientWriter.Close()
		_ = serverWriter.Close()
	})

	serverErr := make(chan error, 1)
	go func() {
		requests := newMessageReader(serverReader)
		request, err := requests.read()
		if err != nil {
			serverErr <- err
			return
		}
		responses := newMessageWriter(serverWriter)
		for _, message := range []rpcMessage{
			{Method: "notifications/progress"},
			{ID: 99, Error: &rpcError{Code: -32000, Message: "wrong response"}},
			{ID: request.ID, Result: mustRaw(map[string]any{"value": "matched"})},
		} {
			if err := responses.write(message); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	client := &Client{
		reader: newMessageReader(clientReader),
		writer: newMessageWriter(clientWriter),
		nextID: 1,
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := client.request(context.Background(), "tools/list", map[string]any{}, &result); err != nil {
		t.Fatalf("request() error = %v", err)
	}
	if result.Value != "matched" {
		t.Fatalf("result.Value = %q, want matched response", result.Value)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake server error: %v", err)
	}
}

type errorCloser struct {
	err error
}

func (closer errorCloser) Close() error {
	return closer.err
}

func TestMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_MCP_STDIO_HELPER") != "1" {
		return
	}

	reader := newMessageReader(os.Stdin)
	writer := newMessageWriter(os.Stdout)
	for {
		message, err := reader.read()
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "read helper message: %v\n", err)
			os.Exit(1)
		}
		if message.Method == "notifications/initialized" {
			continue
		}

		switch message.Method {
		case "initialize":
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Result: mustRaw(map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "test-docs", "version": "1.0.0"},
				}),
			})
		case "tools/list":
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Result: mustRaw(map[string]any{
					"tools": []map[string]any{{
						"name":        "lookup",
						"description": "Lookup documentation",
						"inputSchema": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"required":             []string{"query"},
							"properties": map[string]any{
								"query": map[string]any{"type": "string", "description": "Search query"},
							},
						},
					}},
				}),
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(message.Params, &params)
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Result: mustRaw(map[string]any{
					"content": []map[string]any{{
						"type": "text",
						"text": "lookup: " + strings.TrimSpace(fmt.Sprint(params.Arguments["query"])),
					}},
				}),
			})
		default:
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Error:   &rpcError{Code: -32601, Message: "method not found"},
			})
		}
	}
}

func mustRaw(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func readHTTPRPCMessage(t *testing.T, request *http.Request) rpcMessage {
	t.Helper()

	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Fatalf("close HTTP JSON-RPC request body: %v", err)
		}
	}()
	var message rpcMessage
	if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
		t.Fatalf("decode HTTP JSON-RPC request: %v", err)
	}
	return message
}

func writeHTTPRPCResponse(t *testing.T, response http.ResponseWriter, id any, result any) {
	t.Helper()

	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mustRaw(result),
	}); err != nil {
		t.Fatalf("write HTTP JSON-RPC response: %v", err)
	}
}

func writeHTTPRPCError(t *testing.T, response http.ResponseWriter, id any, message string) {
	t.Helper()

	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: -32601, Message: message},
	}); err != nil {
		t.Fatalf("write HTTP JSON-RPC error: %v", err)
	}
}

func formatSSERPCResponse(t *testing.T, id any, result any) string {
	t.Helper()

	message := rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mustRaw(result),
	}
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal SSE JSON-RPC response: %v", err)
	}
	return fmt.Sprintf("event: message\ndata: %s\n\n", body)
}

func formatSSERPCError(t *testing.T, id any, message string) string {
	t.Helper()

	payload := rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: -32601, Message: message},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal SSE JSON-RPC error: %v", err)
	}
	return fmt.Sprintf("event: message\ndata: %s\n\n", body)
}

func TestSchemaFromMCPInputSchema(t *testing.T) {
	schema := SchemaFromMCP(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"query"},
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query",
				"enum":        []any{"zero", "docs"},
			},
			"limit": map[string]any{
				"type":    "integer",
				"default": float64(5),
				"minimum": float64(1),
				"maximum": float64(10),
			},
		},
	})

	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("schema root = %#v", schema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("required = %#v, want query", schema.Required)
	}
	query := schema.Properties["query"]
	if query.Type != "string" || len(query.Enum) != 2 {
		t.Fatalf("query schema = %#v", query)
	}
	limit := schema.Properties["limit"]
	if limit.Type != "integer" || limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != 10 {
		t.Fatalf("limit schema = %#v", limit)
	}
}

func TestStdioClientServerFromConfig(t *testing.T) {
	servers, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp", Args: []string{"--root", "."}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if servers[0].Type != ServerTypeStdio || servers[0].Command != "docs-mcp" {
		t.Fatalf("server = %#v", servers[0])
	}
}

func TestBoundedBufferCapsRetainedBytes(t *testing.T) {
	b := &boundedBuffer{cap: 8}

	// Each Write must report the full input length (no short writes), even past cap.
	if n, err := b.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write 1 = (%d,%v), want (5,nil)", n, err)
	}
	if n, err := b.Write([]byte("world!!!")); n != 8 || err != nil {
		t.Fatalf("Write 2 = (%d,%v), want (8,nil)", n, err)
	}

	// Only the first cap bytes are retained; the overflow is discarded.
	if got := b.String(); got != "hellowor" {
		t.Fatalf("retained %q, want %q (capped at 8 bytes, head kept)", got, "hellowor")
	}
}

func TestStdioClientIgnoresServerInitiatedRequestsInPendingResponses(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()

	client.ensureReader()

	// Register a pending response for ID 1
	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	// Simulate server sending a request with ID 1 ("roots/list")
	serverReq := `{"jsonrpc":"2.0","id":1,"method":"roots/list","params":{}}` + "\n"
	if _, err := inWriter.Write([]byte(serverReq)); err != nil {
		t.Fatalf("write server request: %v", err)
	}

	// The server request must take the separate request path: it receives a
	// method-not-found response and cannot resolve the pending client call.
	courtesyResult := make(chan dispatchResult, 1)
	go func() {
		message, err := newMessageReader(outReader).read()
		courtesyResult <- dispatchResult{message: message, err: err}
	}()
	select {
	case result := <-courtesyResult:
		if result.err != nil {
			t.Fatalf("read courtesy response: %v", result.err)
		}
		if !rpcIDMatches(result.message.ID, 1) || result.message.Error == nil || result.message.Error.Code != -32601 {
			t.Fatalf("courtesy response = %#v, want id 1 and error -32601", result.message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for method-not-found response")
	}
	select {
	case res := <-responses:
		t.Fatalf("pending request 1 received server request: %#v", res.message)
	default:
	}

	// Now send the actual response for ID 1
	serverResp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` + "\n"
	if _, err := inWriter.Write([]byte(serverResp)); err != nil {
		t.Fatalf("write server response: %v", err)
	}

	select {
	case res := <-responses:
		if res.message.Method != "" || len(res.message.Result) == 0 {
			t.Fatalf("expected valid response, got %#v", res.message)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for actual response")
	}
}

// TestStdioClientUndrainedServerDoesNotStallReadLoop proves that when a server
// stops draining its stdin, an asynchronous courtesy -32601 reply does not stall
// the read loop from servicing legitimate responses.
func TestStdioClientUndrainedServerDoesNotStallReadLoop(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()

	client.ensureReader()

	// 1. Register a pending response for call ID 1
	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	// 2. Server sends request ID 2 (we DO NOT drain outReader so server stdin pipe is blocked)
	serverReq := `{"jsonrpc":"2.0","id":2,"method":"roots/list","params":{}}` + "\n"
	go func() {
		_, _ = inWriter.Write([]byte(serverReq))
		// 3. Immediately send response for ID 1
		serverResp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` + "\n"
		_, _ = inWriter.Write([]byte(serverResp))
	}()

	// 4. Verify the response for ID 1 is delivered without being stalled by the undrained pipe
	select {
	case res := <-responses:
		if res.message.Method != "" || len(res.message.Result) == 0 {
			t.Fatalf("expected valid response for ID 1, got %#v", res.message)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("readLoop stalled on undrained error write; pending response was not delivered")
	}
}

func TestStdioClientDropsInvalidServerRequestIDs(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()

	client.ensureReader()

	// Server sends request with boolean ID (invalid JSON-RPC id)
	serverReq := `{"jsonrpc":"2.0","id":true,"method":"roots/list","params":{}}` + "\n"
	if _, err := inWriter.Write([]byte(serverReq)); err != nil {
		t.Fatalf("write invalid server request: %v", err)
	}

	// A following valid request acts as an ordering barrier: receiving its
	// response proves readLoop already processed the invalid request. Because the
	// writer is serial, any incorrect reply to the boolean ID would arrive first.
	validReq := `{"jsonrpc":"2.0","id":7,"method":"roots/list","params":{}}` + "\n"
	if _, err := inWriter.Write([]byte(validReq)); err != nil {
		t.Fatalf("write valid server request: %v", err)
	}
	readResult := make(chan dispatchResult, 1)
	go func() {
		message, err := newMessageReader(outReader).read()
		readResult <- dispatchResult{message: message, err: err}
	}()

	select {
	case result := <-readResult:
		if result.err != nil {
			t.Fatalf("read valid request response: %v", result.err)
		}
		if !rpcIDMatches(result.message.ID, 7) || result.message.Error == nil || result.message.Error.Code != -32601 {
			t.Fatalf("response = %#v, want only valid id 7 and error -32601", result.message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for valid request response")
	}
}

func TestStdioClientRepliesToValidNonIntegerServerRequestIDs(t *testing.T) {
	requests := []struct {
		name string
		wire string
		want string
	}{
		{name: "fractional", wire: `{"jsonrpc":"2.0","id":1.5,"method":"roots/list","params":{}}` + "\n", want: "1.5"},
		{name: "exponent", wire: `{"jsonrpc":"2.0","id":2e2,"method":"roots/list","params":{}}` + "\n", want: "2e2"},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			inReader, inWriter := io.Pipe()
			outReader, outWriter := io.Pipe()
			client := &Client{
				reader:  newMessageReader(inReader),
				writer:  newMessageWriter(outWriter),
				pending: make(map[int]chan dispatchResult),
			}
			t.Cleanup(func() {
				_ = inWriter.Close()
				_ = outReader.Close()
			})
			client.ensureReader()
			if _, err := inWriter.Write([]byte(request.wire)); err != nil {
				t.Fatalf("write server request: %v", err)
			}
			result := make(chan dispatchResult, 1)
			go func() {
				response, err := newMessageReader(outReader).read()
				result <- dispatchResult{message: response, err: err}
			}()
			select {
			case response := <-result:
				if response.err != nil {
					t.Fatalf("read method-not-found response: %v", response.err)
				}
				got, err := json.Marshal(response.message.ID)
				if err != nil {
					t.Fatalf("marshal id: %v", err)
				}
				if string(got) != request.want || response.message.Error == nil || response.message.Error.Code != -32601 {
					t.Fatalf("response = %#v, want id %s and error -32601", response.message, request.want)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for method-not-found response")
			}
		})
	}
}

func TestJSONRPCIDEchoableAcceptsFiniteJSONNumbers(t *testing.T) {
	for _, test := range []struct {
		name string
		id   any
		want bool
	}{
		{name: "fractional float", id: 1.5, want: true},
		{name: "exponent json number", id: json.Number("2e2"), want: true},
		{name: "not a number", id: math.NaN(), want: false},
		{name: "positive infinity", id: math.Inf(1), want: false},
		{name: "invalid json number", id: json.Number("not-a-number"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := jsonRPCIDEchoable(test.id); got != test.want {
				t.Fatalf("jsonRPCIDEchoable(%v) = %v, want %v", test.id, got, test.want)
			}
		})
	}
}

type gatedCaptureWriter struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	buffer      bytes.Buffer
}

func newGatedCaptureWriter() *gatedCaptureWriter {
	return &gatedCaptureWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (writer *gatedCaptureWriter) Write(p []byte) (int, error) {
	writer.startedOnce.Do(func() { close(writer.started) })
	<-writer.release
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.buffer.Write(p)
}

func (writer *gatedCaptureWriter) Release() {
	writer.releaseOnce.Do(func() { close(writer.release) })
}

func (writer *gatedCaptureWriter) Bytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.buffer.Bytes()...)
}

func TestStdioClientDoesNotWriteCanceledQueuedRequest(t *testing.T) {
	reader := newBlockingReader()
	defer reader.Close()

	output := newGatedCaptureWriter()
	defer output.Release()
	client := &Client{
		reader:  newMessageReader(reader),
		writer:  newMessageWriter(output),
		pending: make(map[int]chan dispatchResult),
	}

	blockerDone := make(chan error, 1)
	go func() {
		blockerDone <- client.writeMessage(context.Background(), rpcMessage{Method: "notifications/blocker"})
	}()
	select {
	case <-output.started:
	case <-time.After(time.Second):
		t.Fatal("initial write did not reach the blocked transport")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.request(ctx, "tools/call", map[string]any{"name": "side_effect"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request() error = %v, want context deadline exceeded", err)
	}

	output.Release()
	if err := <-blockerDone; err != nil {
		t.Fatalf("release initial write: %v", err)
	}
	if err := client.writeMessage(context.Background(), rpcMessage{Method: "notifications/sentinel"}); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	messages := newMessageReader(bytes.NewReader(output.Bytes()))
	first, err := messages.read()
	if err != nil {
		t.Fatalf("read initial message: %v", err)
	}
	if first.Method != "notifications/blocker" {
		t.Fatalf("first method = %q, want notifications/blocker", first.Method)
	}
	second, err := messages.read()
	if err != nil {
		t.Fatalf("read sentinel message: %v", err)
	}
	if second.Method != "notifications/sentinel" {
		t.Fatalf("second method = %q, want notifications/sentinel", second.Method)
	}
	if extra, err := messages.read(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected queued message %#v, read error = %v", extra, err)
	}
}

// TestStdioClientUndrainedServerDoesNotBlockCallerWithDeadline verifies that
// when the server's input pipe is completely blocked and courtesy replies are
// queued/dropped, a caller invoking request() with a deadline aborts cleanly
// when the context expires rather than hanging indefinitely on a write or mutex.
func TestStdioClientUndrainedServerDoesNotBlockCallerWithDeadline(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()

	client.ensureReader()

	// Flood server requests to saturate write queue while outReader is NOT drained.
	for i := 1; i <= 50; i++ {
		serverReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"roots/list","params":{}}`+"\n", i+100)
		_, err := inWriter.Write([]byte(serverReq))
		if err != nil {
			t.Fatalf("failed to write server request: %v", err)
		}
	}

	// Caller with short deadline invokes request()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.request(ctx, "tools/list", map[string]any{}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected deadline exceeded, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("request took too long to abort on deadline: %v", elapsed)
	}
}

func TestStdioClientEmptyMethodDoesNotCompletePending(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()
	client.ensureReader()

	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":""}` + "\n")); err != nil {
		t.Fatalf("write empty-method frame: %v", err)
	}

	courtesyResult := make(chan dispatchResult, 1)
	go func() {
		message, err := newMessageReader(outReader).read()
		courtesyResult <- dispatchResult{message: message, err: err}
	}()
	select {
	case result := <-courtesyResult:
		if result.err != nil {
			t.Fatalf("read courtesy response: %v", result.err)
		}
		if !rpcIDMatches(result.message.ID, 1) || result.message.Error == nil || result.message.Error.Code != -32601 {
			t.Fatalf("courtesy response = %#v, want id 1 and error -32601", result.message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for method-not-found response")
	}
	select {
	case res := <-responses:
		t.Fatalf("pending request 1 completed by empty-method frame: %#v", res.message)
	default:
	}

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` + "\n")); err != nil {
		t.Fatalf("write server response: %v", err)
	}
	select {
	case res := <-responses:
		if res.message.Method != "" || len(res.message.Result) == 0 {
			t.Fatalf("expected valid response, got %#v", res.message)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for actual response")
	}
}

func TestStdioClientNullMethodDoesNotCompletePending(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()
	client.ensureReader()

	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":null}` + "\n")); err != nil {
		t.Fatalf("write null-method frame: %v", err)
	}

	courtesyResult := make(chan dispatchResult, 1)
	go func() {
		message, err := newMessageReader(outReader).read()
		courtesyResult <- dispatchResult{message: message, err: err}
	}()
	select {
	case result := <-courtesyResult:
		if result.err != nil {
			t.Fatalf("read courtesy response: %v", result.err)
		}
		if !rpcIDMatches(result.message.ID, 1) || result.message.Error == nil || result.message.Error.Code != -32601 {
			t.Fatalf("courtesy response = %#v, want id 1 and error -32601", result.message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for method-not-found response")
	}
	select {
	case res := <-responses:
		t.Fatalf("pending request 1 completed by null-method frame: %#v", res.message)
	default:
	}

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` + "\n")); err != nil {
		t.Fatalf("write server response: %v", err)
	}
	select {
	case res := <-responses:
		if res.message.Method != "" || len(res.message.Result) == 0 {
			t.Fatalf("expected valid response, got %#v", res.message)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for actual response")
	}
}

func TestStdioClientCourtesyReplySurvivesFullWriteQueue(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()
	client.ensureReader()

	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	const extra = 8
	n := writeQueueCapacity + extra
	for i := 0; i < n; i++ {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"roots/list","params":{}}`+"\n", i+100)
		if _, err := inWriter.Write([]byte(frame)); err != nil {
			t.Fatalf("write server request %d: %v", i, err)
		}
	}
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")); err != nil {
		t.Fatalf("write pending response: %v", err)
	}
	select {
	case res := <-responses:
		if res.err != nil || len(res.message.Result) == 0 {
			t.Fatalf("pending response = %#v err=%v", res.message, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop stalled while the write queue was full")
	}

	drainDone := make(chan []rpcMessage, 1)
	go func() {
		reader := newMessageReader(outReader)
		var got []rpcMessage
		for len(got) < n {
			message, err := reader.read()
			if err != nil {
				drainDone <- got
				return
			}
			got = append(got, message)
		}
		drainDone <- got
	}()
	select {
	case got := <-drainDone:
		if len(got) != n {
			t.Fatalf("courtesy replies = %d, want %d (queue-full requests dropped)", len(got), n)
		}
		seen := make(map[int]bool, n)
		for _, message := range got {
			id, ok := rpcMessageID(message.ID)
			if !ok || message.Error == nil || message.Error.Code != -32601 {
				t.Fatalf("courtesy reply = %#v, want -32601", message)
			}
			seen[id] = true
		}
		for i := 0; i < n; i++ {
			if !seen[i+100] {
				t.Fatalf("missing courtesy reply for id %d after output resumed", i+100)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out draining courtesy replies after output resumed")
	}
}

func TestWriterLoopExitsOnRepeatedClose(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		inReader, inWriter := io.Pipe()
		outReader, outWriter := io.Pipe()
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, outReader)
			close(copied)
		}()
		client := &Client{
			stdin:  outWriter,
			reader: newMessageReader(inReader),
			writer: newMessageWriter(outWriter),
		}
		client.ensureWriter()
		if err := client.writeMessage(context.Background(), rpcMessage{Method: "notifications/ping"}); err != nil {
			t.Fatalf("writeMessage: %v", err)
		}
		if err := client.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Close: %v", err)
		}
		_ = inWriter.Close()
		<-copied
	}
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+8 {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, after)
	}
}

func TestWriteMessageReleasedOnClose(t *testing.T) {
	reader := newBlockingReader()
	defer reader.Close()
	output := newGatedCaptureWriter()
	client := &Client{
		reader: newMessageReader(reader),
		writer: newMessageWriter(output),
	}

	blockerDone := make(chan error, 1)
	go func() {
		blockerDone <- client.writeMessage(context.Background(), rpcMessage{Method: "notifications/blocker"})
	}()
	select {
	case <-output.started:
	case <-time.After(time.Second):
		t.Fatal("initial write did not reach the transport")
	}

	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- client.writeMessage(context.Background(), rpcMessage{Method: "notifications/queued"})
	}()
	deadline := time.Now().Add(time.Second)
	for len(client.writeQueue) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("queued write did not enqueue")
		}
		time.Sleep(time.Millisecond)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- client.Close()
	}()
	select {
	case <-client.writerStop:
	case <-time.After(time.Second):
		t.Fatal("Close did not begin writer shutdown")
	}
	output.Release()
	select {
	case err := <-closeDone:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung waiting for the writer worker")
	}

	select {
	case err := <-queuedDone:
		if err == nil {
			t.Fatal("queued write succeeded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued write was not released on Close")
	}
	select {
	case <-blockerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked write was not released on Close")
	}

	messages := newMessageReader(bytes.NewReader(output.Bytes()))
	for {
		message, err := messages.read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read captured output: %v", err)
		}
		if message.Method == "notifications/queued" {
			t.Fatal("queued request was written after Close")
		}
	}
}

func TestStdioClientPreservesLargeNumericRequestID(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()
	client.ensureReader()

	const rawID = "9007199254740993"
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":` + rawID + `,"method":"roots/list","params":{}}` + "\n")); err != nil {
		t.Fatalf("write server request: %v", err)
	}

	lineDone := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := bufio.NewReader(outReader).ReadString('\n')
		lineDone <- struct {
			line string
			err  error
		}{line: line, err: err}
	}()
	select {
	case got := <-lineDone:
		if got.err != nil {
			t.Fatalf("read courtesy response: %v", got.err)
		}
		if !strings.Contains(got.line, rawID) {
			t.Fatalf("serialized courtesy response %q does not preserve id %s", got.line, rawID)
		}
		if !strings.Contains(got.line, `"-32601"`) && !strings.Contains(got.line, `-32601`) {
			t.Fatalf("serialized courtesy response %q missing -32601", got.line)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for courtesy response")
	}
}

func TestRPCMessageIDAcceptsExponentForm(t *testing.T) {
	got, ok := rpcMessageID(json.Number("1e0"))
	if !ok || got != 1 {
		t.Fatalf("rpcMessageID(1e0) = %d, %v, want 1, true", got, ok)
	}
	got, ok = rpcMessageID(json.Number("2e2"))
	if !ok || got != 200 {
		t.Fatalf("rpcMessageID(2e2) = %d, %v, want 200, true", got, ok)
	}
	if _, ok := rpcMessageID(json.Number("1.5")); ok {
		t.Fatal("fractional json.Number should not match")
	}
	if rpcIDMatches(json.Number("1e0"), 1) != true {
		t.Fatal("rpcIDMatches(1e0, 1) = false")
	}
}

func TestStdioClientMatchesExponentFormResponseID(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()
	client.ensureReader()

	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1e0,"result":{"ok":true}}` + "\n")); err != nil {
		t.Fatalf("write exponent-id response: %v", err)
	}
	select {
	case res := <-responses:
		if res.err != nil || len(res.message.Result) == 0 {
			t.Fatalf("pending 1 not completed by id 1e0: %#v err=%v", res.message, res.err)
		}
	case <-time.After(time.Second):
		t.Fatal("response id 1e0 did not resolve pending request 1")
	}
}

func TestStdioClientCourtesyOverflowIsBounded(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	client := &Client{
		reader:  newMessageReader(inReader),
		writer:  newMessageWriter(outWriter),
		pending: make(map[int]chan dispatchResult),
	}
	defer func() {
		_ = inWriter.Close()
		_ = outReader.Close()
	}()
	client.ensureReader()

	responses := make(chan dispatchResult, 1)
	client.dispatchMu.Lock()
	client.pending[1] = responses
	client.dispatchMu.Unlock()

	flood := writeQueueCapacity + courtesyOverflowCap + 64
	for i := 0; i < flood; i++ {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"roots/list","params":{}}`+"\n", i+100)
		if _, err := inWriter.Write([]byte(frame)); err != nil {
			t.Fatalf("write server request %d: %v", i, err)
		}
	}
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")); err != nil {
		t.Fatalf("write pending response: %v", err)
	}
	select {
	case res := <-responses:
		if res.err != nil {
			t.Fatalf("pending stalled: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop stalled during courtesy flood")
	}

	client.writeMu.Lock()
	n := len(client.courtesyOverflow)
	client.writeMu.Unlock()
	if n > courtesyOverflowCap {
		t.Fatalf("courtesyOverflow = %d, want <= %d", n, courtesyOverflowCap)
	}
}

func TestWriterLoopCloseBeforeScheduleDoesNotHang(t *testing.T) {
	for i := 0; i < 50; i++ {
		client := &Client{
			writer: newMessageWriter(io.Discard),
		}
		if err := client.startWriter(); err != nil {
			t.Fatalf("startWriter: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			done <- client.Close()
		}()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close hung: writeLoop ranged over a nil queue")
		}
	}
}
