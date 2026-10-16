package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
)

const defaultProtocolVersion = "2024-11-05"

const (
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
	jsonRPCInternalError  = -32603
)

type ServeOptions struct {
	Name              string
	Version           string
	PermissionGranted bool
	// WorkspaceRoot is the directory whose files are exposed as MCP resources.
	// Empty means the process working directory. Resource reads are confined to
	// this root (and any extra Scope roots).
	WorkspaceRoot string
	// Scope, when set, widens the resource roots beyond WorkspaceRoot using the
	// same multi-root scoping the sandbox/file tools use. nil means
	// workspace-only.
	Scope tools.PathScope
}

func Serve(ctx context.Context, input io.Reader, output io.Writer, registry *tools.Registry, options ServeOptions) error {
	if registry == nil {
		return errors.New("MCP server tool registry is required")
	}
	reader := newMessageReader(input)
	writer := newMessageWriter(output)
	resolvedOptions := options.withDefaults()
	// MCP has no user-policy engine, but ModeDisabled intentionally retains the
	// non-optional daemon-token guard and recursive read exclusions.
	credentialGuard := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: resolvedOptions.WorkspaceRoot,
		Policy:        sandbox.Policy{Mode: sandbox.ModeDisabled},
	})
	server := toolServer{
		registry:        registry,
		options:         resolvedOptions,
		writer:          writer,
		workspaceRoot:   resolvedOptions.WorkspaceRoot,
		scope:           resolvedOptions.Scope,
		credentialGuard: credentialGuard,
	}

	// Run the blocking reads on a goroutine and select on ctx so a
	// non-responsive peer cannot hang shutdown: a cancelled ctx returns
	// immediately instead of blocking on the next read.
	type readResult struct {
		message rpcMessage
		err     error
	}
	reads := make(chan readResult)
	go func() {
		defer close(reads)
		for {
			message, err := reader.read()
			select {
			case reads <- readResult{message: message, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result, ok := <-reads:
			if !ok {
				return nil
			}
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					return nil
				}
				return result.err
			}
			if err := server.handle(ctx, result.message); err != nil {
				return err
			}
		}
	}
}

func (options ServeOptions) withDefaults() ServeOptions {
	if strings.TrimSpace(options.Name) == "" {
		options.Name = "zero"
	}
	if strings.TrimSpace(options.Version) == "" {
		options.Version = "dev"
	}
	if strings.TrimSpace(options.WorkspaceRoot) == "" {
		if cwd, err := os.Getwd(); err == nil {
			options.WorkspaceRoot = cwd
		}
	}
	return options
}

type toolServer struct {
	registry        *tools.Registry
	options         ServeOptions
	writer          *messageWriter
	workspaceRoot   string
	scope           tools.PathScope
	credentialGuard *sandbox.Engine
}

func (server toolServer) handle(ctx context.Context, message rpcMessage) error {
	if message.ID == nil && strings.HasPrefix(message.Method, "notifications/") {
		return nil
	}
	if message.ID == nil {
		return nil
	}

	switch message.Method {
	case "initialize":
		return server.writeResult(message.ID, map[string]any{
			"protocolVersion": defaultProtocolVersion,
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
				"prompts":   map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    server.options.Name,
				"version": server.options.Version,
			},
		})
	case "tools/list":
		return server.writeResult(message.ID, map[string]any{
			"tools": server.remoteTools(),
		})
	case "tools/call":
		result, err := server.callTool(ctx, message.Params)
		if err != nil {
			return server.writeError(message.ID, jsonRPCInvalidParams, err.Error())
		}
		return server.writeResult(message.ID, result)
	case "resources/list":
		return server.writeResult(message.ID, map[string]any{
			"resources": server.listResources(),
		})
	case "resources/read":
		contents, code, err := server.readResource(message.Params)
		if err != nil {
			return server.writeError(message.ID, code, err.Error())
		}
		return server.writeResult(message.ID, map[string]any{
			"contents": contents,
		})
	case "prompts/list":
		return server.writeResult(message.ID, map[string]any{
			"prompts": listPrompts(),
		})
	case "prompts/get":
		result, code, err := getPrompt(message.Params)
		if err != nil {
			return server.writeError(message.ID, code, err.Error())
		}
		return server.writeResult(message.ID, result)
	case "ping":
		// MCP requires servers to answer ping with an empty result; replying
		// method-not-found makes liveness-checking clients drop the session.
		return server.writeResult(message.ID, map[string]any{})
	default:
		return server.writeError(message.ID, jsonRPCMethodNotFound, "method not found")
	}
}

func (server toolServer) remoteTools() []RemoteTool {
	registered := server.registry.All()
	sort.Slice(registered, func(left int, right int) bool {
		return registered[left].Name() < registered[right].Name()
	})

	remote := make([]RemoteTool, 0, len(registered))
	for _, tool := range registered {
		remote = append(remote, RemoteTool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: SchemaToMCP(tool.Parameters()),
		})
	}
	return remote
}

func (server toolServer) callTool(ctx context.Context, rawParams json.RawMessage) (CallToolResult, error) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &params); err != nil {
			return CallToolResult{}, fmt.Errorf("invalid tools/call params: %w", err)
		}
	}
	params.Name = strings.TrimSpace(params.Name)
	if params.Name == "" {
		return CallToolResult{}, errors.New("tools/call requires a tool name")
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}

	result := server.registry.RunWithOptions(ctx, params.Name, params.Arguments, tools.RunOptions{
		PermissionGranted: server.options.PermissionGranted,
		Sandbox:           server.credentialGuard,
	})
	return CallToolResult{
		Content: []Content{{Type: "text", Text: result.Output}},
		IsError: result.Status != tools.StatusOK,
	}, nil
}

func (server toolServer) writeResult(id any, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return server.writeError(id, jsonRPCInternalError, err.Error())
	}
	return server.writer.write(rpcMessage{ID: id, Result: raw})
}

func (server toolServer) writeError(id any, code int, message string) error {
	return server.writer.write(rpcMessage{ID: id, Error: &rpcError{Code: code, Message: message}})
}
