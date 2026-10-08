package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/tools"
)

type closeTrackingMCPRuntime struct {
	closed chan struct{}
	err    error
}

func (runtime *closeTrackingMCPRuntime) Close() error {
	close(runtime.closed)
	return runtime.err
}

func (*closeTrackingMCPRuntime) Skipped() []mcp.SkippedServer { return nil }

func TestSplitMCPStartupConfigDefersOnlyUnconfiguredDefaults(t *testing.T) {
	defaults := config.DefaultMCPServers()
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"exa":      defaults["exa"],
		"docs":     {Type: "http", URL: "https://docs.example.test/mcp"},
		"disabled": {Type: "http", URL: "https://disabled.example.test/mcp", Disabled: true},
	}}

	critical, optional := splitMCPStartupConfig(cfg)
	if _, ok := critical.Servers["docs"]; !ok || len(critical.Servers) != 1 {
		t.Fatalf("critical servers = %#v, want only docs", critical.Servers)
	}
	if _, ok := optional.Servers["exa"]; !ok || len(optional.Servers) != 1 {
		t.Fatalf("optional servers = %#v, want only exa", optional.Servers)
	}
}

func TestOptionalMCPAwaitTimesOutThenObservesPublishedTools(t *testing.T) {
	registry := tools.NewRegistry()
	release := make(chan struct{})
	started := make(chan struct{})
	startup := startOptionalMCP(
		context.Background(),
		registry,
		config.MCPConfig{Servers: config.DefaultMCPServers()},
		mcp.RegisterOptions{},
		func(ctx context.Context, registry *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			close(started)
			select {
			case <-release:
				registry.RegisterBatch([]tools.Tool{tools.NewScopedReadFileTool(t.TempDir(), nil)})
				return noopMCPRuntime{}, nil
			case <-ctx.Done():
				return noopMCPRuntime{}, ctx.Err()
			}
		},
		nil,
	)
	defer func() { _ = startup.Close() }()
	<-started

	if startup.Await(t.Context(), time.Millisecond) {
		t.Fatal("optional startup unexpectedly completed before release")
	}
	if got := len(registry.All()); got != 0 {
		t.Fatalf("registry published tools before readiness: %d", got)
	}
	close(release)
	if !startup.Await(t.Context(), time.Second) {
		t.Fatal("optional startup did not complete after release")
	}
	if _, ok := registry.Get("read_file"); !ok {
		t.Fatal("completed optional startup did not publish its tool batch")
	}
}

func TestOptionalMCPFailedRegistrationSkipsReadinessCallback(t *testing.T) {
	readyCalls := 0
	startup := startOptionalMCP(
		context.Background(),
		tools.NewRegistry(),
		config.MCPConfig{Servers: config.DefaultMCPServers()},
		mcp.RegisterOptions{},
		func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, errors.New("connect failed")
		},
		func() { readyCalls++ },
	)
	defer func() { _ = startup.Close() }()

	if !startup.Await(t.Context(), time.Second) {
		t.Fatal("failed optional startup did not settle")
	}
	if readyCalls != 0 {
		t.Fatalf("readiness callback ran after failed registration: %d", readyCalls)
	}
}

func TestOptionalMCPCloseCancelsBlockedStartup(t *testing.T) {
	started := make(chan struct{})
	startup := startOptionalMCP(
		context.Background(),
		tools.NewRegistry(),
		config.MCPConfig{Servers: config.DefaultMCPServers()},
		mcp.RegisterOptions{},
		func(ctx context.Context, _ *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			close(started)
			<-ctx.Done()
			return noopMCPRuntime{}, nil
		},
		nil,
	)
	<-started
	if err := startup.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestOptionalMCPCloseReturnsRuntimeErrorWithinGrace(t *testing.T) {
	closeErr := errors.New("close failed")
	runtime := &closeTrackingMCPRuntime{closed: make(chan struct{}), err: closeErr}
	startup := startOptionalMCP(
		context.Background(),
		tools.NewRegistry(),
		config.MCPConfig{Servers: config.DefaultMCPServers()},
		mcp.RegisterOptions{},
		func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return runtime, nil
		},
		nil,
	)
	if !startup.Await(t.Context(), time.Second) {
		t.Fatal("optional startup did not settle")
	}
	if err := startup.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, closeErr)
	}
}

func TestOptionalMCPCloseBoundsRegistrationThatIgnoresCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	runtime := &closeTrackingMCPRuntime{closed: make(chan struct{})}
	startup := startOptionalMCP(
		context.Background(),
		tools.NewRegistry(),
		config.MCPConfig{Servers: config.DefaultMCPServers()},
		mcp.RegisterOptions{},
		func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			close(started)
			<-release
			return runtime, nil
		},
		nil,
	)
	<-started

	closeResult := make(chan error, 1)
	go func() { closeResult <- startup.Close() }()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked on registration after cancellation")
	}

	close(release)
	released = true
	select {
	case <-runtime.closed:
	case <-time.After(time.Second):
		t.Fatal("late optional MCP runtime was not closed")
	}
}
