package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/tools"
)

// defaultConnectTimeout bounds how long startup waits for ONE MCP server to
// connect and list its tools. A server that exceeds it is abandoned and skipped
// so a slow or unreachable server (e.g. a hosted endpoint blocked by the local
// network) cannot delay the first model response. Servers connect concurrently,
// so total startup cost is the slowest reachable server, not the sum.
const defaultConnectTimeout = 8 * time.Second

type RegisterOptions struct {
	PermissionStore *PermissionStore
	Autonomy        PermissionAutonomy
	// AdvertiseInAuto exposes prompt-gated MCP tools to model surfaces whose
	// default mode is auto while preserving the execution-time prompt.
	AdvertiseInAuto bool
	ClientFactory   func(context.Context, Server) (ToolClient, error)
	// ConnectTimeout bounds the per-server connect+list at startup. Zero uses
	// defaultConnectTimeout.
	ConnectTimeout time.Duration
	Execution      *execution.Runner
	WorkspaceRoot  string
}

// SkippedServer records an MCP server that was not registered because it could
// not be reached or its tools could not be validated. Registration is
// best-effort per server: one unreachable server is skipped (and reported here)
// rather than aborting startup or disabling the others.
type SkippedServer struct {
	Name string
	Err  error
	// UnconfiguredDefault mirrors Server.UnconfiguredDefault: true when this
	// server is an out-of-the-box default the user never configured, so a
	// caller can skip warning loudly about it.
	UnconfiguredDefault bool
}

type Runtime struct {
	clients []ToolClient
	// cancels releases the per-server connect contexts of the clients we KEPT.
	// A stdio server's subprocess is tied to its context, so the context must
	// stay live for the session and be cancelled only at Close (after the client
	// is closed). Same length/order as clients is not required.
	cancels []context.CancelFunc
	skipped []SkippedServer
	once    sync.Once
	err     error
}

// Skipped returns the servers that were skipped during registration (unreachable
// or invalid), so the caller can warn the user without failing the launch.
func (runtime *Runtime) Skipped() []SkippedServer {
	if runtime == nil {
		return nil
	}
	return runtime.skipped
}

var unsafeToolNameChars = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func RegisterTools(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options RegisterOptions) (*Runtime, error) {
	if registry == nil {
		return nil, fmt.Errorf("MCP tool registry is required")
	}
	servers, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{}
	if len(servers) == 0 {
		return runtime, nil
	}

	factory := options.ClientFactory
	if factory == nil {
		factory = func(ctx context.Context, server Server) (ToolClient, error) {
			return ConnectWithOptions(ctx, server, ConnectOptions{Execution: options.Execution, WorkspaceRoot: options.WorkspaceRoot})
		}
	}

	timeout := options.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}

	// Connect every server CONCURRENTLY: connect + list-tools is network/process
	// I/O, so connecting serially makes startup wait for the SUM of all servers —
	// one slow or unreachable server would block every other server AND the first
	// model response. Each server gets its own cancelable context bounded by the
	// startup timeout; a server that does not connect + list in time is abandoned
	// (its context cancelled to tear down the half-open connection/subprocess) and
	// recorded as skipped. The concurrent phase does ONLY I/O and touches no shared
	// state; all validation, conflict detection, and registration happen in the
	// deterministic serial phase below, so the result is identical regardless of
	// completion order.
	type connectResult struct {
		client ToolClient
		remote []RemoteTool
		cancel context.CancelFunc
		err    error
	}
	results := make([]connectResult, len(servers))
	var wg sync.WaitGroup
	for index := range servers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			server := servers[index]
			serverCtx, cancel := context.WithCancel(ctx)
			done := make(chan connectResult, 1)
			go func() {
				client, remote, err := connectAndList(serverCtx, factory, server)
				done <- connectResult{client: client, remote: remote, err: err}
			}()
			select {
			case res := <-done:
				if res.err != nil {
					cancel() // failed: nothing to keep
				} else {
					res.cancel = cancel // keep the context alive; released at Close
				}
				results[index] = res
			case <-time.After(timeout):
				cancel() // abandon the slow connect: tears down the conn/subprocess
				// Reap the goroutine + any partial client in the background so a
				// slow server never blocks startup.
				go func() {
					if res := <-done; res.client != nil {
						_ = res.client.Close()
					}
				}()
				results[index] = connectResult{err: fmt.Errorf("connect timed out after %s", timeout)}
			}
		}(index)
	}
	wg.Wait()

	// Serial, deterministic commit in server order. Building tools reads the
	// permission store and the registry, so it stays single-goroutine. A server is
	// best-effort: one that failed to connect, timed out, returned a nameless tool,
	// or conflicts with an already-committed tool is SKIPPED (recorded, not fatal),
	// and still contributes its tools all-or-none. The conflict check spans the
	// registry plus every tool committed by an earlier server.
	staged := make([]registryTool, 0)
	stagedNames := make(map[string]struct{})
	for index, server := range servers {
		res := results[index]
		if res.err != nil {
			runtime.skipped = append(runtime.skipped, SkippedServer{Name: server.Name, Err: res.err, UnconfiguredDefault: server.UnconfiguredDefault})
			continue
		}
		serverTools, validateErr := buildServerTools(registry, server, res.remote, res.client, options, stagedNames)
		if validateErr != nil {
			if res.cancel != nil {
				res.cancel()
			}
			_ = res.client.Close()
			runtime.skipped = append(runtime.skipped, SkippedServer{Name: server.Name, Err: validateErr, UnconfiguredDefault: server.UnconfiguredDefault})
			continue
		}
		runtime.clients = append(runtime.clients, res.client)
		if res.cancel != nil {
			runtime.cancels = append(runtime.cancels, res.cancel)
		}
		for _, tool := range serverTools {
			stagedNames[tool.Name()] = struct{}{}
			staged = append(staged, tool)
		}
	}
	registered := make([]tools.Tool, 0, len(staged))
	for index := range staged {
		registered = append(registered, staged[index])
	}
	registry.RegisterBatch(registered)
	return runtime, nil
}

// connectAndList connects to one server and lists its tools. It does ONLY I/O
// (no registry, permission-store, or other shared state), so it is safe to run
// concurrently for every server. On a list error it closes the client.
func connectAndList(ctx context.Context, factory func(context.Context, Server) (ToolClient, error), server Server) (ToolClient, []RemoteTool, error) {
	client, err := factory(ctx, server)
	if err != nil {
		return nil, nil, err
	}
	remoteTools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("list MCP tools for %s: %w", server.Name, err)
	}
	return client, remoteTools, nil
}

// buildServerTools validates a server's remote tools against the registry and the
// names already committed by earlier servers, returning the server's tools only
// when every one is named and conflict-free. It runs in the serial commit phase
// (single goroutine), so its registry and permission-store reads are race-free.
// On error the caller closes the client (it owns the result), so this never does.
func buildServerTools(registry *tools.Registry, server Server, remoteTools []RemoteTool, client ToolClient, options RegisterOptions, stagedNames map[string]struct{}) ([]registryTool, error) {
	serverTools := make([]registryTool, 0, len(remoteTools))
	localNames := make(map[string]struct{})
	for _, remote := range remoteTools {
		if strings.TrimSpace(remote.Name) == "" {
			return nil, fmt.Errorf("MCP server %s returned a tool without a name", server.Name)
		}
		tool := newRegistryTool(server, remote, client, options)
		if existing, ok := registry.Get(tool.Name()); ok {
			return nil, fmt.Errorf("MCP tool %s from %s conflicts with existing tool %s", remote.Name, server.Name, existing.Name())
		}
		if _, ok := stagedNames[tool.Name()]; ok {
			return nil, fmt.Errorf("MCP tool %s from %s conflicts with another MCP tool named %s", remote.Name, server.Name, tool.Name())
		}
		if _, ok := localNames[tool.Name()]; ok {
			return nil, fmt.Errorf("MCP tool %s from %s conflicts with another tool from the same server", remote.Name, server.Name)
		}
		localNames[tool.Name()] = struct{}{}
		serverTools = append(serverTools, tool)
	}
	return serverTools, nil
}

func (runtime *Runtime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.once.Do(func() {
		for _, client := range runtime.clients {
			if err := client.Close(); err != nil && runtime.err == nil {
				runtime.err = err
			}
		}
		// Release the kept servers' connect contexts AFTER closing the clients: a
		// stdio subprocess is already terminated by Close, so cancelling is then a
		// no-op; it frees the context (and any tied subprocess) either way.
		for _, cancel := range runtime.cancels {
			cancel()
		}
	})
	return runtime.err
}

type registryTool struct {
	name       string
	server     Server
	remote     RemoteTool
	client     ToolClient
	parameters tools.Schema
	safety     tools.Safety
}

func newRegistryTool(server Server, remote RemoteTool, client ToolClient, options RegisterOptions) registryTool {
	remote.Name = strings.TrimSpace(remote.Name)
	name := registryToolName(server.Name, remote.Name)
	permission := tools.PermissionPrompt
	if isPersistentlyApproved(options.PermissionStore, server, remote.Name, defaultAutonomy(options.Autonomy)) {
		permission = tools.PermissionAllow
	}
	return registryTool{
		name:       name,
		server:     server,
		remote:     remote,
		client:     client,
		parameters: SchemaFromMCP(remote.InputSchema),
		safety: tools.Safety{
			SideEffect:      tools.SideEffectNetwork,
			Permission:      permission,
			Reason:          fmt.Sprintf("MCP tool %s/%s runs through the configured %s server.", server.Name, remote.Name, server.Type),
			AdvertiseInAuto: options.AdvertiseInAuto,
		},
	}
}

func (tool registryTool) Name() string {
	return tool.name
}

func (tool registryTool) Description() string {
	if strings.TrimSpace(tool.remote.Description) != "" {
		return tool.remote.Description
	}
	return fmt.Sprintf("Call MCP tool %s/%s", tool.server.Name, tool.remote.Name)
}

func (tool registryTool) Parameters() tools.Schema {
	return tool.parameters
}

func (tool registryTool) Safety() tools.Safety {
	return tool.safety
}

// Deferred marks every MCP tool as deferred-eligible: when many MCP tools are
// registered the agent loop may withhold their full schema and advertise them
// via tool_search. Built-in tools do not implement this interface and stay
// eager.
func (tool registryTool) Deferred() bool {
	return true
}

// MCPServerName reports the tool's originating MCP server name so the deferred-
// tools reminder labels it correctly, even when the sanitized server token in the
// synthesized tool name contains an underscore (which the name-only parser would
// truncate). It returns the true configured server name, not the sanitized token.
func (tool registryTool) MCPServerName() string {
	return tool.server.Name
}

func (tool registryTool) Run(ctx context.Context, args map[string]any) tools.Result {
	result, err := tool.client.CallTool(ctx, tool.remote.Name, args)
	if err != nil {
		return tools.Result{
			Status: tools.StatusError,
			Output: "Error: MCP tool " + tool.server.Name + "/" + tool.remote.Name + " failed: " + err.Error(),
			Meta:   tool.meta(),
		}
	}
	status := tools.StatusOK
	if result.IsError {
		status = tools.StatusError
	}
	output := TextContent(result.Content)
	// Say what was thrown away. Without this an image-only result reads as
	// "(empty MCP tool result)", the model concludes the call produced nothing
	// and retries, and the user never learns an image came back (#823). The note
	// is appended only when something was actually dropped, so a text-only
	// result is byte-for-byte what it was before.
	//
	// It says retrying cannot RECOVER the payload rather than that a retry
	// returns the same thing. Each retry is a fresh call, so the server may well
	// answer differently; what cannot change is that Zero still has nowhere to
	// put a non-text block. Claiming the response would be identical would be a
	// promise this code is in no position to make.
	if dropped := DroppedContentSummary(result.Content); dropped != "" {
		note := "[zero] this server also returned " + dropped + ", which Zero cannot forward yet. Retrying cannot recover this payload."
		if output == "" {
			note = "[zero] this server returned " + dropped + ", which Zero cannot forward yet. Retrying cannot recover this payload."
		}
		output = strings.TrimSpace(output + "\n\n" + note)
	}
	if output == "" {
		output = "(empty MCP tool result)"
	}
	return tools.Result{
		Status: status,
		Output: output,
		Meta:   tool.meta(),
	}
}

func (tool registryTool) meta() map[string]string {
	return map[string]string{
		"mcp.server":   tool.server.Name,
		"mcp.tool":     tool.remote.Name,
		"mcp.identity": tool.server.Identity,
	}
}

func registryToolName(serverName string, toolName string) string {
	serverPart := sanitizeToolNamePart(serverName)
	toolPart := sanitizeToolNamePart(toolName)
	if toolPart == "" {
		toolPart = "tool"
	}
	return "mcp_" + serverPart + "_" + toolPart
}

func sanitizeToolNamePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	value = unsafeToolNameChars.ReplaceAllString(value, "_")
	value = strings.Trim(value, "_")
	if value == "" {
		return "server"
	}
	return value
}

func isPersistentlyApproved(store *PermissionStore, server Server, toolName string, autonomy PermissionAutonomy) bool {
	if store == nil {
		return false
	}
	approved, err := store.IsToolPersistentlyApproved(CheckToolInput{
		ServerName:        server.Name,
		ServerIdentity:    server.Identity,
		ToolName:          toolName,
		RequestedAutonomy: autonomy,
	})
	return err == nil && approved
}
