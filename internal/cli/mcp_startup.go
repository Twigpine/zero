package cli

import (
	"context"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

const (
	optionalMCPPromptGrace = time.Second
	optionalMCPCloseGrace  = 250 * time.Millisecond
)

// splitMCPStartupConfig keeps explicitly configured servers on the
// prompt-critical path. Only unchanged built-in defaults may initialize after
// first paint; their failure was already intentionally silent.
func splitMCPStartupConfig(cfg config.MCPConfig) (critical config.MCPConfig, optional config.MCPConfig) {
	critical.Servers = map[string]config.MCPServerConfig{}
	optional.Servers = map[string]config.MCPServerConfig{}
	for name, server := range cfg.Servers {
		if server.Disabled {
			continue
		}
		if config.IsUnconfiguredDefault(name, server) {
			optional.Servers[name] = server
			continue
		}
		critical.Servers[name] = server
	}
	return critical, optional
}

type optionalMCPStartup struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	runtime mcpToolRuntime

	closeOnce        sync.Once
	runtimeCloseOnce sync.Once
	closeErr         error
}

func startOptionalMCP(
	parent context.Context,
	registry *tools.Registry,
	cfg config.MCPConfig,
	options mcp.RegisterOptions,
	register func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error),
	onReady func(),
) *optionalMCPStartup {
	ctx, cancel := context.WithCancel(parent)
	startup := &optionalMCPStartup{cancel: cancel, done: make(chan struct{})}
	go func() {
		runtime, err := register(ctx, registry, cfg, options)
		if err == nil && onReady != nil {
			onReady()
		}
		startup.mu.Lock()
		startup.runtime = runtime
		startup.mu.Unlock()
		close(startup.done)
	}()
	return startup
}

// Await gives optional tools one bounded opportunity to join the next turn's
// immutable registry snapshot. A timeout leaves this turn on the existing core
// tool set; a later turn observes the completed generation.
func (startup *optionalMCPStartup) Await(ctx context.Context, timeout time.Duration) bool {
	if startup == nil {
		return true
	}
	select {
	case <-startup.done:
		return true
	default:
	}
	if timeout <= 0 {
		select {
		case <-startup.done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-startup.done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (startup *optionalMCPStartup) Close() error {
	if startup == nil {
		return nil
	}
	startup.closeOnce.Do(func() {
		startup.cancel()
		reaped := make(chan struct{})
		go func() {
			defer close(reaped)
			<-startup.done
			startup.closeRuntime()
		}()
		timer := time.NewTimer(optionalMCPCloseGrace)
		defer timer.Stop()
		select {
		case <-reaped:
		case <-timer.C:
		}
	})
	startup.mu.Lock()
	defer startup.mu.Unlock()
	return startup.closeErr
}

func (startup *optionalMCPStartup) closeRuntime() {
	startup.runtimeCloseOnce.Do(func() {
		startup.mu.Lock()
		runtime := startup.runtime
		startup.mu.Unlock()
		if runtime == nil {
			return
		}
		err := runtime.Close()
		startup.mu.Lock()
		if err != nil && startup.closeErr == nil {
			startup.closeErr = err
		}
		startup.mu.Unlock()
	})
}

// Skipped reports the optional servers that failed, or nothing while startup is
// still running.
//
// A NIL RETURN WAS NOT "NOT READY", IT WAS "NONE". These servers are ordinary
// rows on the /mcp panel: splitMCPStartupConfig moves them off the critical path
// so a slow one cannot delay the first response, it does not make them invisible.
// Discarding their failures here left every one of them rendered from
// configuration alone, which reports a server that never connected as enabled --
// the single thing that panel exists to prevent.
// completed exposes the completion signal so an already-open surface can be
// told to rebuild. Nil when there is no background registration, which is what
// the consumer treats as "nothing to wait for".
func (startup *optionalMCPStartup) completed() <-chan struct{} {
	if startup == nil {
		return nil
	}
	return startup.done
}

func (startup *optionalMCPStartup) Skipped() []mcp.SkippedServer {
	if startup == nil {
		return nil
	}
	select {
	case <-startup.done:
	default:
		// Still connecting. There is no observation yet, and blocking here would
		// stall a render on a server deliberately kept off the critical path.
		return nil
	}
	startup.mu.Lock()
	defer startup.mu.Unlock()
	if startup.runtime == nil {
		return nil
	}
	return startup.runtime.Skipped()
}
