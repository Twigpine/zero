package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestServeListsAndCallsRegistryTools(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(serverFakeTool{
		name:        "lookup",
		description: "Lookup documentation",
		parameters: tools.Schema{
			Type: "object",
			Properties: map[string]tools.PropertySchema{
				"query": {Type: "string", Description: "Search query"},
			},
			Required:             []string{"query"},
			AdditionalProperties: false,
		},
		safety: tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "test"},
		run: func(args map[string]any) tools.Result {
			return tools.Result{Status: tools.StatusOK, Output: "lookup: " + strings.TrimSpace(args["query"].(string))}
		},
	})

	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{ID: 1, Method: "initialize"})
	writeServerTestMessage(t, &input, rpcMessage{Method: "notifications/initialized"})
	writeServerTestMessage(t, &input, rpcMessage{ID: 2, Method: "tools/list"})
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     3,
		Method: "tools/call",
		Params: mustRaw(map[string]any{
			"name":      "lookup",
			"arguments": map[string]any{"query": "zero"},
		}),
	})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, registry, ServeOptions{Name: "zero-test", Version: "1.2.3"}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := newMessageReader(&output)
	initialize := readServerTestMessage(t, reader)
	var initResult struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	decodeServerTestResult(t, initialize, &initResult)
	if initResult.ProtocolVersion != defaultProtocolVersion || initResult.ServerInfo.Name != "zero-test" || initResult.ServerInfo.Version != "1.2.3" {
		t.Fatalf("initialize result = %#v", initResult)
	}
	if _, ok := initResult.Capabilities["tools"]; !ok {
		t.Fatalf("initialize capabilities = %#v, want tools", initResult.Capabilities)
	}

	list := readServerTestMessage(t, reader)
	var listResult struct {
		Tools []RemoteTool `json:"tools"`
	}
	decodeServerTestResult(t, list, &listResult)
	if len(listResult.Tools) != 1 || listResult.Tools[0].Name != "lookup" || listResult.Tools[0].Description != "Lookup documentation" {
		t.Fatalf("listed tools = %#v", listResult.Tools)
	}
	properties, ok := listResult.Tools[0].InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("input schema = %#v, want properties", listResult.Tools[0].InputSchema)
	}
	if _, ok := properties["query"]; !ok {
		t.Fatalf("properties = %#v, want query", properties)
	}

	call := readServerTestMessage(t, reader)
	var callResult CallToolResult
	decodeServerTestResult(t, call, &callResult)
	if callResult.IsError {
		t.Fatalf("call result IsError = true: %#v", callResult)
	}
	if got := TextContent(callResult.Content); got != "lookup: zero" {
		t.Fatalf("call result text = %q, want lookup: zero", got)
	}
}

func TestServeUsesToolApprovalGate(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(serverFakeTool{
		name:        "write_file",
		description: "Write a file",
		parameters:  tools.Schema{Type: "object", AdditionalProperties: false},
		safety:      tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionPrompt, Reason: "writes files"},
		run: func(map[string]any) tools.Result {
			return tools.Result{Status: tools.StatusOK, Output: "wrote"}
		},
	})

	denied := callServerTool(t, registry, ServeOptions{}, "write_file", map[string]any{})
	if !denied.IsError || !strings.Contains(TextContent(denied.Content), "Permission required for write_file") {
		t.Fatalf("denied result = %#v, want permission error", denied)
	}

	allowed := callServerTool(t, registry, ServeOptions{PermissionGranted: true}, "write_file", map[string]any{})
	if allowed.IsError || TextContent(allowed.Content) != "wrote" {
		t.Fatalf("allowed result = %#v, want success", allowed)
	}
}

func TestServeReportsUnknownMethods(t *testing.T) {
	registry := tools.NewRegistry()
	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{ID: 42, Method: "missing/method"})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, registry, ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	message := readServerTestMessage(t, newMessageReader(&output))
	if message.Error == nil || message.Error.Code != jsonRPCMethodNotFound {
		t.Fatalf("error = %#v, want method not found", message.Error)
	}
}

func TestSchemaToMCPInputSchema(t *testing.T) {
	minimum := 1
	maximum := 10
	schema := SchemaToMCP(tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"query": {
				Type:        "string",
				Description: "Search query",
				Enum:        []string{"zero", "docs"},
			},
			"limit": {
				Type:    "integer",
				Default: 5,
				Minimum: &minimum,
				Maximum: &maximum,
			},
		},
		Required:             []string{"query"},
		AdditionalProperties: false,
	})

	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema root = %#v", schema)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %#v, want query", schema["required"])
	}
	properties := schema["properties"].(map[string]any)
	query := properties["query"].(map[string]any)
	if query["description"] != "Search query" {
		t.Fatalf("query property = %#v", query)
	}
	limit := properties["limit"].(map[string]any)
	if limit["default"] != 5 || limit["minimum"] != 1 || limit["maximum"] != 10 {
		t.Fatalf("limit property = %#v", limit)
	}
}

type serverFakeTool struct {
	name        string
	description string
	parameters  tools.Schema
	safety      tools.Safety
	run         func(map[string]any) tools.Result
}

func (tool serverFakeTool) Name() string {
	return tool.name
}

func (tool serverFakeTool) Description() string {
	return tool.description
}

func (tool serverFakeTool) Parameters() tools.Schema {
	return tool.parameters
}

func (tool serverFakeTool) Safety() tools.Safety {
	return tool.safety
}

func (tool serverFakeTool) Run(_ context.Context, args map[string]any) tools.Result {
	return tool.run(args)
}

func callServerTool(t *testing.T, registry *tools.Registry, options ServeOptions, name string, args map[string]any) CallToolResult {
	t.Helper()

	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     1,
		Method: "tools/call",
		Params: mustRaw(map[string]any{
			"name":      name,
			"arguments": args,
		}),
	})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, registry, options); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	var result CallToolResult
	decodeServerTestResult(t, readServerTestMessage(t, newMessageReader(&output)), &result)
	return result
}

func writeServerTestMessage(t *testing.T, buffer *bytes.Buffer, message rpcMessage) {
	t.Helper()
	if err := newMessageWriter(buffer).write(message); err != nil {
		t.Fatal(err)
	}
}

func readServerTestMessage(t *testing.T, reader *messageReader) rpcMessage {
	t.Helper()
	message, err := reader.read()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func decodeServerTestResult(t *testing.T, message rpcMessage, target any) {
	t.Helper()
	if message.Error != nil {
		t.Fatalf("message error = %#v", message.Error)
	}
	if err := json.Unmarshal(message.Result, target); err != nil {
		t.Fatalf("decode result: %v\n%s", err, string(message.Result))
	}
}
