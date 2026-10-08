package mcp

import (
	"encoding/json"
	"testing"
)

func TestRPCMessageMethodPresence(t *testing.T) {
	var req rpcMessage
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"method":""}`), &req); err != nil {
		t.Fatal(err)
	}
	if !req.methodPresent {
		t.Fatal(`{"method":""} must set methodPresent so it is not routed as a response`)
	}
	var resp rpcMessage
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.methodPresent {
		t.Fatal("response without method member must not set methodPresent")
	}
	if resp.Method != "" {
		t.Fatalf("response Method = %q", resp.Method)
	}
	var nullMethod rpcMessage
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"method":null}`), &nullMethod); err != nil {
		t.Fatal(err)
	}
	if !nullMethod.methodPresent {
		t.Fatal(`{"method":null} must set methodPresent so it is not routed as a response`)
	}
}
