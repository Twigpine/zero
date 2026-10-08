package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/workspacetrust"
)

type acpTestReader func([]byte) (int, error)

func (r acpTestReader) Read(p []byte) (int, error) { return r(p) }

type acpTestWriter func([]byte) (int, error)

func (w acpTestWriter) Write(p []byte) (int, error) { return w(p) }

func installACPTestContext(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	previous := acpSignalContext
	acpSignalContext = func() (context.Context, context.CancelFunc) {
		return ctx, cancel
	}
	t.Cleanup(func() {
		cancel()
		acpSignalContext = previous
	})
	return cancel
}

func TestRunACPCancellationPreservesTerminalReadError(t *testing.T) {
	cancel := installACPTestContext(t)
	wantErr := errors.New("transport read failed")
	reader := acpTestReader(func(p []byte) (int, error) {
		return copy(p, `{"jsonrpc":"1.0","id":1}`+"\n"), wantErr
	})
	writeStarted := make(chan struct{}, 1)
	releaseWrite := make(chan struct{})
	var stdout bytes.Buffer
	writer := acpTestWriter(func(p []byte) (int, error) {
		writeStarted <- struct{}{}
		<-releaseWrite
		return stdout.Write(p)
	})
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runACP(nil, writer, &stderr, fillAppDeps(appDeps{stdin: reader}))
	}()

	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("ACP did not reach the response write")
	}
	// Keep Serve in the synchronous write until cancellation is observable. The
	// regression requires the genuine read error to win over that cancellation.
	cancel()
	close(releaseWrite)

	select {
	case code := <-done:
		if code != exitCrash {
			t.Fatalf("exit code = %d, want crash %d", code, exitCrash)
		}
	case <-time.After(time.Second):
		t.Fatal("ACP did not exit after terminal read error")
	}
	if got := stderr.String(); !strings.Contains(got, "acp: "+wantErr.Error()) {
		t.Fatalf("stderr = %q, want terminal read error", got)
	}
}

func TestRunACPIdleCancellationExitsCleanly(t *testing.T) {
	cancel := installACPTestContext(t)
	pipeReader, pipeWriter := io.Pipe()
	t.Cleanup(func() { _ = pipeWriter.Close() })
	readStarted := make(chan struct{}, 1)
	reader := &acpNotifyingReadCloser{ReadCloser: pipeReader, readStarted: readStarted}
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runACP(nil, &stdout, &stderr, fillAppDeps(appDeps{stdin: reader}))
	}()

	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("ACP did not begin reading")
	}
	cancel()

	select {
	case code := <-done:
		if code != exitSuccess {
			t.Fatalf("exit code = %d, want success %d; stderr: %s", code, exitSuccess, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("ACP did not exit after cancellation")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestBuildACPWorkspaceRegistersSandboxedMCPToolsAndClosesThem(t *testing.T) {
	setTrustConfigRoot(t)
	workspaceRoot := t.TempDir()
	if err := workspacetrust.Trust(workspaceRoot); err != nil {
		t.Fatalf("trust workspace: %v", err)
	}
	grantStore, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: t.TempDir() + "/grants.json"})
	if err != nil {
		t.Fatalf("new grant store: %v", err)
	}

	var gotExclude, registered, closed bool
	deps := fillAppDeps(appDeps{
		resolveMCPConfig: func(root string, excludeProject bool) (config.MCPConfig, error) {
			if root != workspaceRoot {
				t.Fatalf("MCP workspace root = %q, want %q", root, workspaceRoot)
			}
			gotExclude = excludeProject
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "fake-docs"},
			}}, nil
		},
		newMCPStore:     func() (*mcp.PermissionStore, error) { return nil, nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return grantStore, nil },
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			if ctx == nil {
				t.Fatal("MCP registration received nil context")
			}
			if len(cfg.Servers) != 1 || cfg.Servers["docs"].Command != "fake-docs" {
				t.Fatalf("MCP config = %+v", cfg)
			}
			if options.Autonomy != mcp.AutonomyLow {
				t.Fatalf("MCP autonomy = %q, want low", options.Autonomy)
			}
			if options.Execution == nil {
				t.Fatal("MCP stdio server was not given the sandbox execution runner")
			}
			if options.WorkspaceRoot != workspaceRoot {
				t.Fatalf("MCP execution workspace = %q, want %q", options.WorkspaceRoot, workspaceRoot)
			}
			if !options.AdvertiseInAuto {
				t.Fatal("ACP MCP tools must be advertised in auto mode without being pre-approved")
			}
			registered = true
			registry.Register(cliFakeDeferredTool{
				name: "mcp_docs_search", permission: tools.PermissionPrompt,
				advertiseInAuto: options.AdvertiseInAuto,
			})
			return closeFunc(func() error {
				closed = true
				return nil
			}), nil
		},
	})

	workspace, err := buildACPWorkspace(context.Background(), workspaceRoot, config.ResolvedConfig{
		Tools: config.ToolsConfig{DeferThreshold: 1},
	}, agent.PermissionModeAuto, deps)
	if err != nil {
		t.Fatalf("buildACPWorkspace: %v", err)
	}
	if gotExclude {
		t.Fatal("trusted ACP workspace excluded project MCP configuration")
	}
	if !registered {
		t.Fatal("ACP workspace did not register configured MCP tools")
	}
	if _, ok := workspace.Registry.Get("mcp_docs_search"); !ok {
		t.Fatal("MCP tool is absent from ACP agent registry")
	}
	if _, ok := workspace.Registry.Get(tools.ToolSearchToolName); !ok {
		t.Fatal("ACP registry is missing tool_search for deferred MCP tools")
	}
	if workspace.DeferThreshold != 1 {
		t.Fatalf("workspace defer threshold = %d, want 1", workspace.DeferThreshold)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("close ACP workspace: %v", err)
	}
	if !closed {
		t.Fatal("ACP workspace did not close its MCP runtime")
	}
}

func TestBuildACPWorkspacePreservesRedactedMCPSetupNotices(t *testing.T) {
	setTrustConfigRoot(t)
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".zero"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectConfig := `{"mcp":{"servers":{"project-docs":{"type":"stdio","command":"project-docs"}}}}`
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".zero", "config.json"), []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	grantStore, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	deps := fillAppDeps(appDeps{
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "fake-docs"},
			}}, nil
		},
		newMCPStore:     func() (*mcp.PermissionStore, error) { return nil, nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return grantStore, nil },
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return fakeMCPRuntimeWithSkips{skipped: []mcp.SkippedServer{{
				Name: "docs", Err: errors.New("token=" + secret),
			}}}, nil
		},
	})

	workspace, err := buildACPWorkspace(context.Background(), workspaceRoot, config.ResolvedConfig{}, agent.PermissionModeAuto, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	joined := strings.Join(workspace.Notices, "\n")
	if !strings.Contains(joined, "untrusted workspace") || !strings.Contains(joined, "MCP server docs unavailable") {
		t.Fatalf("workspace notices = %#v", workspace.Notices)
	}
	if strings.Contains(joined, secret) || !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("workspace notices leaked secret: %q", joined)
	}
}

func TestBuildACPWorkspaceSkipsMCPInPlanMode(t *testing.T) {
	grantStore, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: t.TempDir() + "/grants.json"})
	if err != nil {
		t.Fatalf("new grant store: %v", err)
	}
	called := false
	deps := fillAppDeps(appDeps{
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			called = true
			return config.MCPConfig{}, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) { return grantStore, nil },
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			called = true
			return nil, nil
		},
	})

	workspace, err := buildACPWorkspace(context.Background(), t.TempDir(), config.ResolvedConfig{}, agent.PermissionModePlan, deps)
	if err != nil {
		t.Fatalf("buildACPWorkspace: %v", err)
	}
	if called {
		t.Fatal("plan-mode ACP workspace must not resolve or start MCP servers")
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("close plan workspace: %v", err)
	}
}

func TestBuildACPWorkspaceClosesPartialMCPRuntimeOnRegistrationError(t *testing.T) {
	grantStore, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: t.TempDir() + "/grants.json"})
	if err != nil {
		t.Fatalf("new grant store: %v", err)
	}
	closed := false
	deps := fillAppDeps(appDeps{
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"partial": {Type: "stdio", Command: "fake-partial"},
			}}, nil
		},
		newMCPStore:     func() (*mcp.PermissionStore, error) { return nil, nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return grantStore, nil },
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return closeFunc(func() error {
				closed = true
				return nil
			}), errors.New("second MCP server failed")
		},
	})

	workspace, err := buildACPWorkspace(context.Background(), t.TempDir(), config.ResolvedConfig{}, agent.PermissionModeAuto, deps)
	if err == nil {
		t.Fatal("buildACPWorkspace succeeded after MCP registration error")
	}
	if workspace != nil {
		t.Fatal("buildACPWorkspace returned a workspace after MCP registration error")
	}
	if !closed {
		t.Fatal("partial MCP runtime was not closed after registration error")
	}
}

type acpNotifyingReadCloser struct {
	io.ReadCloser
	readStarted chan<- struct{}
	once        sync.Once
}

func (r *acpNotifyingReadCloser) Read(p []byte) (int, error) {
	r.once.Do(func() { r.readStarted <- struct{}{} })
	return r.ReadCloser.Read(p)
}
