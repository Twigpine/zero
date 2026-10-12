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
// launchSettleGrace bounds how long an abandoned connect attempt is given to
// say whether it had already started. It is paid only after cancel, so an
// attempt that never reached Start returns well inside it.
const launchSettleGrace = 250 * time.Millisecond

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

// StartupDisclosure is a least-privilege statement about one MCP server's
// LAUNCH, as opposed to anything a later tool call does.
//
// A stdio server prepared under a weakened token serves the whole session from
// that process, so the fact describes startup and cannot be recovered from an
// individual tool result afterwards. It is reported once, here, rather than
// appended to every response the server produces.
type StartupDisclosure struct {
	Name    string
	Notices []string
}

type Runtime struct {
	clients []ToolClient
	// cancels releases the per-server connect contexts of the clients we KEPT.
	// A stdio server's subprocess is tied to its context, so the context must
	// stay live for the session and be cancelled only at Close (after the client
	// is closed). Same length/order as clients is not required.
	cancels []context.CancelFunc
	skipped []SkippedServer
	// disclosureSources are the least-privilege statements that applied to each
	// server process this registration LAUNCHED, in server order, each still
	// holding the sink that carries the authoritative launch fact.
	//
	// NOT a frozen snapshot. Registration is bounded and a launch is not: an
	// attempt abandoned at the connect timeout can still be inside cmd.Start when
	// wg.Wait returns, so the serial commit samples an empty sink and the process
	// then starts under the reduced confinement with nobody left to say so. The
	// sink outlives registration and StartupDisclosures reads through it, so a
	// late Start is reported instead of lost. See StartupDisclosures.
	disclosureSources []disclosureSource
	// disclosureStream is the typed hand-off to whoever owns the output. Created
	// on the first StartupDisclosureStream call and closed by Close, so a launch
	// that resolves after the runtime is gone has somewhere defined to land:
	// nowhere.
	disclosureStreamOnce sync.Once
	disclosureStream     *StartupDisclosureStream
	once                 sync.Once
	err                  error
}

// disclosureSource pairs a server with both the notices known at commit time and
// the sink that may still learn them. notices wins when it is already populated,
// so a settled server never re-reads the sink.
type disclosureSource struct {
	name    string
	notices []string
	sink    *launchSink
}

// ReportStartupDisclosures delivers each server's launch disclosure to report
// EXACTLY ONCE, whether the launch had already completed when this was called
// or completes later.
//
// StartupDisclosures reads through the sink, which stopped a late Start from
// being lost, but a value nobody re-reads is still a lost disclosure: both
// production reporters sample once, right after RegisterTools returns, and an
// attempt abandoned at the connect timeout can finish Start after that sample.
// The reaper only closes the late client. So the operator saw the skipped-server
// warning and never learned that a local process had run under the reduced
// enforcement.
//
// Servers whose notices were known at commit are reported now, in server order.
// Every other server subscribes its sink: if the launch already happened the
// subscriber runs before this returns, otherwise it runs from publishLaunch on
// the connect goroutine. Either way each server reaches report once. A server
// that never starts never publishes, so prepare, pipe and Start failures stay
// silent, and network servers, which launch no process, contribute nothing.
//
// Late deliveries arrive in completion order, which is the only order they
// have; the immediate set keeps server order.
//
// A STREAM, NOT A CALLBACK. An earlier version took the presentation function
// and invoked it from whichever goroutine resolved the launch, which for an
// abandoned attempt is the connect goroutine. That put a write to the caller's
// writer on a goroutine and at a time the caller did not control. The runtime
// owns the fact; it appends the fact here and the owner drains it. See
// StartupDisclosureStream.
func (runtime *Runtime) StartupDisclosureStream() *StartupDisclosureStream {
	if runtime == nil {
		return nil
	}
	runtime.disclosureStreamOnce.Do(func() {
		stream := newStartupDisclosureStream()
		runtime.disclosureStream = stream
		for _, source := range runtime.disclosureSources {
			if len(source.notices) > 0 {
				stream.offer(StartupDisclosure{Name: source.name, Notices: append([]string(nil), source.notices...)})
				continue
			}
			name := source.name
			source.sink.subscribe(func(notices []string) {
				stream.offer(StartupDisclosure{Name: name, Notices: notices})
			})
		}
	})
	return runtime.disclosureStream
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
		// notices travels with the indexed result rather than being appended to
		// shared state from inside the goroutine. The concurrent phase touches no
		// shared state, which is the property the comment above promises and the
		// reason the serial phase can be deterministic; appending here broke both,
		// racing the slice header and ordering disclosures by completion time.
		notices []string
	}
	results := make([]connectResult, len(servers))
	// RETAINED PAST THE CONCURRENT PHASE. The timeout branch samples the sink the
	// moment it fires, but connectStdio does not publish until cmd.Start has
	// returned, so a Start that succeeds just after the timeout selected was
	// sampled as "never launched" and its disclosure was lost: the reaper closes
	// the late client and cannot amend a commit that has already happened.
	// Reading the sink again in the serial phase is strictly later than the
	// timeout branch and still deterministic, because it runs after wg.Wait.
	sinks := make([]*launchSink, len(servers))
	var wg sync.WaitGroup
	for index := range servers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			server := servers[index]
			// The sink hears about Start as it happens, so an attempt abandoned below
			// can still report the confinement its process ran under. The connect
			// result cannot supply that: it does not arrive until after this phase.
			sink := &launchSink{}
			sinks[index] = sink
			serverCtx, cancel := context.WithCancel(withLaunchSink(ctx, sink))
			done := make(chan connectResult, 1)
			go func() {
				client, remote, notices, err := connectAndList(serverCtx, factory, server)
				done <- connectResult{client: client, remote: remote, notices: notices, err: err}
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
				timedOut := connectResult{err: fmt.Errorf("connect timed out after %s", timeout)}
				// SYNCHRONIZE WITH THE START, briefly, before deciding there was none.
				//
				// connectStdio publishes only after cmd.Start returns, so sampling the
				// sink the instant the timeout fires races a Start that is about to
				// succeed: the sample reads empty, the result commits with no notice,
				// and the reaper cannot amend a commit that has already happened. The
				// window is microseconds and unreachable from a test seam, which is
				// exactly why it must be closed by construction rather than measured.
				//
				// cancel() has already fired, so an attempt that has NOT started fails
				// fast and this returns immediately; only one that did start can still
				// be in Start, and it publishes on the way out. The grace is therefore
				// paid only when there is something to learn.
				select {
				case res := <-done:
					if res.client != nil {
						_ = res.client.Close()
					}
					if len(res.notices) > 0 {
						timedOut.notices = res.notices
					}
				case <-time.After(launchSettleGrace):
					// Still stuck past the grace. Reap in the background so a slow
					// server never blocks startup.
					go func() {
						if res := <-done; res.client != nil {
							_ = res.client.Close()
						}
					}()
				}
				// A server that reached Start ran under the planned enforcement even
				// though its connection never became usable. One that timed out
				// before Start discloses nothing, so the sink stays empty and this
				// adds nothing.
				if len(timedOut.notices) == 0 {
					if launched, notices := sink.observe(); launched {
						timedOut.notices = notices
					}
				}
				results[index] = timedOut
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
		// Recorded here, in server order, for any server whose PROCESS STARTED,
		// including one whose tools are rejected below: the launch happened under
		// that token either way, and a skip warning does not say what confinement
		// the process ran with while it was alive.
		notices := res.notices
		if len(notices) == 0 {
			// A launch that published after the timeout branch sampled. Checked here
			// rather than only there so the window between Start succeeding and the
			// timeout committing cannot swallow the disclosure.
			if launched, late := sinks[index].observe(); launched {
				notices = late
			}
		}
		// Recorded whether or not notices are known YET. An abandoned attempt can
		// still be inside cmd.Start, and keeping its sink here is what lets
		// StartupDisclosures report that launch after this phase has finished.
		// Order is server order, so a late arrival does not reorder the rest.
		runtime.disclosureSources = append(runtime.disclosureSources, disclosureSource{
			name:    server.Name,
			notices: notices,
			sink:    sinks[index],
		})
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
// connectAndList returns the client, its tools, and the least-privilege
// disclosures that applied to its LAUNCH.
//
// THE LAUNCH FACT OUTLIVES THE CONNECTION. A stdio server can start, and do
// filesystem work, and then fail initialize or tools/list. Returning the notices
// separately rather than leaving them on the client means the fact survives that
// failure: the client is closed and discarded here, so anything reachable only
// through it is gone by the time the caller sees the error, and the skip warning
// on its own does not say the process already ran with reduced write
// confinement.
func connectAndList(ctx context.Context, factory func(context.Context, Server) (ToolClient, error), server Server) (ToolClient, []RemoteTool, []string, error) {
	client, err := factory(ctx, server)
	if err != nil {
		// A failure BEFORE the process started discloses nothing. A failure after it
		// started carries the fact out through the error, because the client that
		// held it has already been closed and discarded by then.
		return nil, nil, startupNoticesFromError(err), err
	}
	notices := startupNoticesOf(client)
	remoteTools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, notices, fmt.Errorf("list MCP tools for %s: %w", server.Name, err)
	}
	return client, remoteTools, notices, nil
}

// startupNoticesOf reads a client's launch disclosures, if it reports any.
func startupNoticesOf(client ToolClient) []string {
	if client == nil {
		return nil
	}
	if disclosing, ok := client.(startupDisclosing); ok {
		return disclosing.StartupNotices()
	}
	return nil
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
		// End disclosure delivery FIRST. A launch that resolves while the clients
		// are being closed has no owner left to print it, and the runtime must not
		// leave a subscriber holding a writer whose lifetime it does not know.
		//
		// THROUGH THE ONCE, NOT THE FIELD. Reading disclosureStream directly raced
		// a first StartupDisclosureStream call on another goroutine, and a Close
		// that came before any call found nil and closed nothing, so a later call
		// built a fresh, open stream and subscribed it to launches from a runtime
		// that was already gone. Reported by CodeRabbit.
		//
		// If nobody asked for the stream before this, nobody is draining it, so it
		// is published EMPTY rather than built the usual way: no sink is
		// subscribed and nothing is queued for an owner that does not exist. Every
		// later caller then gets this stream, already closed. The read after Do is
		// ordered by the Once whichever call got there first.
		runtime.disclosureStreamOnce.Do(func() {
			runtime.disclosureStream = newStartupDisclosureStream()
		})
		runtime.disclosureStream.Close()
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
	images, disp, limitReason := forwardImages(result.Content)
	// Image blocks with valid data ride Result.Images, the same channel capture
	// tools already use. Everything else non-text is still named rather than
	// silently dropped, because Zero still has nowhere to put audio, embedded
	// resources, or a block whose payload could not be decoded (#823).
	//
	// Conversion happens once: the drop note is built from the same pass's
	// per-item disposition, so a valid image is not decoded again just to
	// decide whether it was forwarded.
	//
	// Image-only success has no text block, so a one-line placeholder keeps
	// the tool_result self-describing. ModelOutput and finalizeToolOutcome
	// copy Output through verbatim; an empty string would hand the model an
	// empty body next to the image.
	//
	// Notes are appended only when something was actually dropped, so a
	// text-only result is byte-for-byte what it was before, and a successfully
	// forwarded image is not described as unforwardable.
	//
	// Unforwardable blocks (audio, resource, failed decode) say retrying
	// cannot RECOVER the payload: Zero still has nowhere to put them.
	// Budget-skipped images are different — Zero can forward them, and a
	// retry with fewer images can recover the payload — so they get their
	// own sentence.
	if output == "" && len(images) > 0 {
		output = "[image returned by tool]"
	}
	if exceeded := droppedContentNote(result.Content, disp, dispBudgetExceeded); exceeded != "" {
		output = appendServerNote(output, exceeded,
			", which exceeded this result's remaining image budget. Retrying with fewer images can recover this payload.")
	}
	if uninspected := droppedContentNote(result.Content, disp, dispUninspected); uninspected != "" {
		verb := ", which were not inspected"
		if dispCount(disp, dispUninspected) == 1 {
			verb = ", which was not inspected"
		}
		var cause string
		switch limitReason {
		case imageLimitForwardCount:
			cause = " because the maximum forwarded image count was reached."
		case imageLimitInspectionCount:
			cause = " because the maximum image inspection limit was reached."
		default:
			cause = " because the aggregate image budget was reached."
		}
		output = appendServerNote(output, uninspected, verb+cause)
	}
	if dropped := droppedContentNote(result.Content, disp, dispDropped); dropped != "" {
		output = appendServerNote(output, dropped,
			", which Zero cannot forward yet. Retrying cannot recover this payload.")
	}
	if output == "" {
		output = "(empty MCP tool result)"
	}
	return tools.Result{
		Status: status,
		Output: output,
		Images: images,
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

// StartupDisclosures returns the least-privilege statements that applied to the
// MCP server processes this registration launched, so a caller can report them
// once. Empty when no server was launched under reduced enforcement, and always
// empty for network servers, which launch no local process.
//
// READ THROUGH THE SINK, so this is not fixed at the moment registration
// returned. A server abandoned at the connect timeout may still have been inside
// cmd.Start then, and its process starts under the reduced write confinement
// regardless of whether the connection ever became usable. Registration stays
// bounded; the disclosure does not expire with it.
//
// Server order, and a settled entry never re-reads its sink, so calling this
// twice cannot reorder or duplicate anything.
func (runtime *Runtime) StartupDisclosures() []StartupDisclosure {
	if runtime == nil {
		return nil
	}
	disclosures := make([]StartupDisclosure, 0, len(runtime.disclosureSources))
	for _, source := range runtime.disclosureSources {
		notices := source.notices
		if len(notices) == 0 {
			if launched, late := source.sink.observe(); launched {
				notices = late
			}
		}
		if len(notices) > 0 {
			disclosures = append(disclosures, StartupDisclosure{Name: source.name, Notices: notices})
		}
	}
	if len(disclosures) == 0 {
		return nil
	}
	return disclosures
}

// appendServerNote joins a [zero] note to output, choosing "returned" or
// "also returned" based on whether anything precedes it.
func appendServerNote(output, subject, tail string) string {
	verb := "this server also returned "
	if output == "" {
		verb = "this server returned "
	}
	return strings.TrimSpace(output + "\n\n[zero] " + verb + subject + tail)
}
