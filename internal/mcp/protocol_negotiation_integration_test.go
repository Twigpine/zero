package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// This test also compiles against the unfixed client, so the same wire fixture
// can demonstrate the original failure without depending on the new helper.
func TestHTTPProtocolNegotiationAfterVersionRejection(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			var proposals []string
			var initialized bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				message := readHTTPRPCMessage(t, r)
				if message.Method == "initialize" {
					if got := r.Header.Get("MCP-Protocol-Version"); got != "" {
						t.Errorf("initialize version header = %q, want absent", got)
					}
					var params struct {
						Version string `json:"protocolVersion"`
					}
					if err := json.Unmarshal(message.Params, &params); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					proposals = append(proposals, params.Version)
					if params.Version != "2025-06-18" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"jsonrpc": "2.0", "id": message.ID,
							"error": map[string]any{"code": -32602, "message": "Invalid params",
								"data": fmt.Sprintf("Unsupported protocol version '%s'. Supported: 2026-07-28, 2025-06-18, 2025-03-26", params.Version)},
						})
						return
					}
					w.Header().Set("Mcp-Session-Id", "negotiated-session")
					writeHTTPRPCResponse(t, w, message.ID, map[string]any{"protocolVersion": "2025-06-18"})
					return
				}
				if got := r.Header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
					t.Errorf("%s version header = %q", message.Method, got)
				}
				if got := r.Header.Get("Mcp-Session-Id"); got != "negotiated-session" {
					t.Errorf("%s session header = %q", message.Method, got)
				}
				switch message.Method {
				case "notifications/initialized":
					initialized = true
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					writeHTTPRPCResponse(t, w, message.ID, map[string]any{"tools": []any{}})
				default:
					t.Errorf("unexpected method %s", message.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			client, err := Connect(ctx, Server{Name: "version-test", Type: ServerTypeHTTP, URL: server.URL,
				Headers: map[string]string{"MCP-Protocol-Version": "wrong-configured-version"}})
			if err != nil {
				t.Fatalf("Connect() = %v", err)
			}
			defer client.Close()
			if _, err := client.ListTools(ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !initialized || !reflect.DeepEqual(proposals, []string{"2025-11-25", "2025-06-18"}) {
				t.Fatalf("initialized=%v proposals=%v", initialized, proposals)
			}
		})
	}
}

func TestHTTPProtocolCounterofferAndUnsupportedVersion(t *testing.T) {
	for _, selected := range []string{"2024-11-05", "2026-07-28"} {
		t.Run(selected, func(t *testing.T) {
			var mu sync.Mutex
			var initialized bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				message := readHTTPRPCMessage(t, r)
				if message.Method == "initialize" {
					writeHTTPRPCResponse(t, w, message.ID, map[string]any{"protocolVersion": selected})
					return
				}
				initialized = true
				if got := r.Header.Get("MCP-Protocol-Version"); got != selected {
					t.Errorf("negotiated header = %q, want %q", got, selected)
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Connect(ctx, Server{Name: "counteroffer", Type: ServerTypeHTTP, URL: server.URL})
			if client != nil {
				defer client.Close()
			}
			mu.Lock()
			defer mu.Unlock()
			if selected == "2026-07-28" {
				if err == nil || initialized || !strings.Contains(err.Error(), selected) {
					t.Fatalf("unsupported offer = %v, initialized=%v", err, initialized)
				}
			} else if err != nil || !initialized {
				t.Fatalf("supported offer = %v, initialized=%v", err, initialized)
			}
		})
	}
}

func TestStdioProtocolNegotiation(t *testing.T) {
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()
	defer serverReader.Close()
	defer clientWriter.Close()
	defer clientReader.Close()
	defer serverWriter.Close()
	client := &Client{server: Server{Name: "stdio-versions"}, stdin: clientWriter,
		reader: newMessageReader(clientReader), writer: newMessageWriter(clientWriter), nextID: 1}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		reader, writer := newMessageReader(serverReader), newMessageWriter(serverWriter)
		for _, expected := range []string{"2025-11-25", "2025-03-26"} {
			message, err := reader.read()
			if err != nil {
				done <- err
				return
			}
			var params struct {
				Version string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil || (expected == "2025-03-26" && params.Version != expected) {
				done <- fmt.Errorf("stdio proposal = %q, %v; want %q", params.Version, err, expected)
				return
			}
			response := rpcMessage{JSONRPC: "2.0", ID: message.ID}
			if expected == "2025-11-25" {
				if err := json.Unmarshal([]byte(`{"code":-32602,"message":"Unsupported protocol version; supported: 2025-03-26"}`), &response.Error); err != nil {
					done <- err
					return
				}
			} else {
				response.Result = json.RawMessage(`{"protocolVersion":"2025-03-26"}`)
			}
			if err := writer.write(response); err != nil {
				done <- err
				return
			}
		}
		message, err := reader.read()
		if err == nil && message.Method != "notifications/initialized" {
			err = fmt.Errorf("last method = %q", message.Method)
		}
		done <- err
	}()
	if err := client.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSSEProtocolNegotiation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := make(chan string, 4)
	var mu sync.Mutex
	var proposals []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if got := r.Header.Get("MCP-Protocol-Version"); got != "" {
				t.Errorf("initial stream version header = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case event := <-events:
					_, _ = fmt.Fprint(w, event)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		mu.Lock()
		defer mu.Unlock()
		message := readHTTPRPCMessage(t, r)
		if message.Method == "initialize" {
			if got := r.Header.Get("MCP-Protocol-Version"); got != "" {
				t.Errorf("initialize version header = %q", got)
			}
			var params struct {
				Version string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			proposals = append(proposals, params.Version)
			if params.Version != "2025-06-18" {
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID,
					"error": map[string]any{"code": -32602, "message": "Invalid params",
						"data": "Unsupported protocol version; supported: 2025-06-18"}})
				events <- "event: message\ndata: " + string(data) + "\n\n"
			} else {
				events <- formatSSERPCResponse(t, message.ID, map[string]any{"protocolVersion": params.Version})
			}
		} else {
			if got := r.Header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
				t.Errorf("%s version header = %q", message.Method, got)
			}
			if message.Method == "tools/list" {
				events <- formatSSERPCResponse(t, message.ID, map[string]any{"tools": []any{}})
			} else if message.Method != "notifications/initialized" {
				t.Errorf("unexpected method %q", message.Method)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := Connect(ctx, Server{Name: "sse-versions", Type: ServerTypeSSE, URL: server.URL,
		Headers: map[string]string{"MCP-Protocol-Version": "wrong-configured-version"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(proposals, []string{"2025-11-25", "2025-06-18"}) {
		t.Fatalf("SSE proposals = %v", proposals)
	}
}
