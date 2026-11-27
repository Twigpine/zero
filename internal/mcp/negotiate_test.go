package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNegotiateProtocol(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selected  string
		rejection error
		wantCalls []string
		wantError string
	}{
		{name: "latest", selected: "2025-11-25", wantCalls: []string{"2025-11-25"}},
		{name: "supported counteroffer", selected: "2024-11-05", wantCalls: []string{"2025-11-25"}},
		{name: "unsupported counteroffer", selected: "2026-07-28", wantCalls: []string{"2025-11-25"}, wantError: "unsupported protocol version"},
		{name: "missing counteroffer", wantCalls: []string{"2025-11-25"}, wantError: "unsupported protocol version"},
		{
			name: "newest shared version", selected: "2025-06-18",
			rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`"Unsupported protocol version '2025-11-25'. Supported: 2026-07-28, 2024-11-05, 2025-06-18, 2025-03-26"`)},
			wantCalls: []string{"2025-11-25", "2025-06-18"},
		},
		{
			name: "structured supported list", selected: "2025-03-26",
			rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"supportedVersions":["2025-03-26","2024-11-05"]}`)},
			wantCalls: []string{"2025-11-25", "2025-03-26"},
		},
		{
			name: "escaped supported list", selected: "2025-06-18",
			rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"supportedVersions":["2025\u002d06\u002d18"]}`)},
			wantCalls: []string{"2025-11-25", "2025-06-18"},
		},
		{
			name: "escaped plaintext rejection", selected: "2025-06-18",
			rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`"Unsupported protocol version. Supported: \u0032025-06-18"`)},
			wantCalls: []string{"2025-11-25", "2025-06-18"},
		},
		{
			name: "ignore unsupported list", selected: "2024-11-05",
			rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"supportedVersions":["2024-11-05"],"unsupportedVersions":["2025-06-18"]}`)},
			wantCalls: []string{"2025-11-25", "2024-11-05"},
		},
		{
			name: "nested escaped supported list", selected: "2025-03-26",
			rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"details":{"supportedVersions":["2025-03-\u00326"]},"unsupportedVersions":["2025-06-18"]}`)},
			wantCalls: []string{"2025-11-25", "2025-03-26"},
		},
		{
			name: "empty supported list", rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"supportedVersions":[],"unsupportedVersions":["2025-06-18"]}`)},
			wantCalls: []string{"2025-11-25"}, wantError: "mutually supported versions",
		},
		{
			name: "plaintext supported and unsupported lists", selected: "2024-11-05",
			rejection: &rpcError{Code: -32602, Message: "Unsupported protocol version. Supported: 2024-11-05. Unsupported: 2025-06-18"},
			wantCalls: []string{"2025-11-25", "2024-11-05"},
		},
		{
			name: "no overlap", rejection: &rpcError{Code: -32602, Message: "Unsupported protocol version 2025-11-25; supported: 2026-07-28"},
			wantCalls: []string{"2025-11-25"}, wantError: "mutually supported versions",
		},
		{
			name: "ordinary invalid params", rejection: &rpcError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"missing":"clientInfo"}`)},
			wantCalls: []string{"2025-11-25"}, wantError: "clientInfo",
		},
		{
			name: "authentication failure", rejection: errors.New("HTTP 401: unauthorized"),
			wantCalls: []string{"2025-11-25"}, wantError: "unauthorized",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			version, err := negotiateProtocol(context.Background(), func(_ context.Context, method string, params any, target any) error {
				if method != "initialize" {
					t.Fatalf("method = %q", method)
				}
				calls = append(calls, params.(map[string]any)["protocolVersion"].(string))
				if tc.rejection != nil && (len(calls) == 1 || tc.wantError != "") {
					return fmt.Errorf("wrapped RPC failure: %w", tc.rejection)
				}
				return json.Unmarshal([]byte(fmt.Sprintf(`{"protocolVersion":%q}`, tc.selected)), target)
			})
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("proposals = %v, want %v", calls, tc.wantCalls)
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", err, tc.wantError)
				}
			} else if err != nil || version != tc.selected {
				t.Fatalf("negotiated = %q, %v, want %q", version, err, tc.selected)
			}
		})
	}
}

func TestNegotiateProtocolDoesNotRepeatRejectedVersions(t *testing.T) {
	var calls int
	_, err := negotiateProtocol(context.Background(), func(context.Context, string, any, any) error {
		calls++
		return &rpcError{Code: -32602, Message: "Unsupported protocol version; supported: 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05"}
	})
	if err == nil || calls != 4 {
		t.Fatalf("negotiation = %v after %d requests, want failure after four", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = negotiateProtocol(ctx, func(context.Context, string, any, any) error {
		t.Fatal("canceled handshake sent a request")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled negotiation = %v", err)
	}
}

func TestRPCErrorDataIsPreservedAndScrubbed(t *testing.T) {
	const key = "review-fake-injected-service-key"
	var message rpcMessage
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params","data":{"detail":"protocol version rejected","review-fake-injected-service-key":["review-fake-injected-service-key"]}}}`), &message); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Error.Error(), key) || !strings.Contains(message.Error.Error(), "protocol version rejected") {
		t.Fatalf("RPC error lost data: %v", message.Error)
	}
	scrubbed := scrubAIMLAPIKeys(message, key)
	if strings.Contains(scrubbed.Error.Error(), key) || !strings.Contains(scrubbed.Error.Error(), "protocol version rejected") {
		t.Fatalf("RPC error data was not safely retained: %v", scrubbed.Error)
	}
}

func TestHTTPVersionRejectionRequiresMatchingRPCResponse(t *testing.T) {
	for _, id := range []int{1, 99} {
		response := fakeHTTPResponse(nil, 400, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"Invalid params","data":"Unsupported protocol version; supported: 2025-06-18"}}`, id))
		err := httpRPCStatusError(Server{Name: "versions", Type: ServerTypeHTTP}, response, rpcMessage{ID: 1, Method: "initialize"})
		var rpc *rpcError
		if got := errors.As(err, &rpc); got != (id == 1) {
			t.Fatalf("response id %d: retryable RPC error = %v (%v)", id, got, err)
		}
	}
}

func TestHTTPVersionRejectionScrubsJSONEncodedServiceKeys(t *testing.T) {
	const key = "review-fake-injected-service-key"
	response := fakeHTTPResponse(nil, 400, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params","data":{"detail":"Unsupported protocol version","echo":"\u0072eview-fake-injected-service-key"}}}`)
	err := httpRPCStatusError(Server{Name: "versions", Type: ServerTypeHTTP}, response, rpcMessage{ID: 1, Method: "initialize"}, key)
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		t.Fatalf("version error lost its type: %v", err)
	}
	var data map[string]string
	if err := json.Unmarshal(rpc.Data, &data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data["echo"], key) || data["detail"] != "Unsupported protocol version" {
		t.Fatal("HTTP rejection retained an encoded service key or lost its version detail")
	}
}
