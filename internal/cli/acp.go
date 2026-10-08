package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Gitlawb/zero/internal/acp"
	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sandbox"
)

const acpUsage = `zero acp — serve the Agent Client Protocol (ACP) over stdio

Editors that speak ACP (Zed, JetBrains, Neovim, ...) spawn this command and drive
ZERO as a backend over JSON-RPC 2.0 on stdin/stdout. ZERO keeps your provider,
model, and API keys (BYOK); the editor only hosts the conversation thread.

Usage:
  zero acp

Not meant to be run interactively — point your editor's ACP / external-agent
setting at "zero acp".`

var acpSignalContext = signalContext

// runACP serves ACP over stdio so an editor can drive ZERO's agent core. It
// speaks JSON-RPC 2.0 (newline-delimited JSON) on stdin/stdout; stderr stays free
// for human-readable diagnostics. The session lifecycle maps onto ZERO's own
// session store, and provider/model/keys remain owned by ZERO.
func runACP(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			fmt.Fprintln(stdout, acpUsage)
			return exitSuccess
		default:
			return writeExecUsageError(stderr, fmt.Sprintf("unknown acp flag %q", arg))
		}
	}

	// The process's own stdin belongs exclusively to this Conn for the life of
	// the command, so cancellation may close it to interrupt an idle read.
	conn := acp.NewOwnedConn(deps.stdin, stdout)
	acp.NewAgent(conn, acp.Deps{
		ResolveConfig: deps.resolveConfig,
		DiscoverModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return defaultDiscoverProviderModels(ctx, discoveryCredentialProfile(profile))
		},
		// deps.newProvider is wrapped in fillAppDeps to apply the stored API key,
		// so ACP is authenticated for apiKeyStored profiles like every other
		// surface — no ACP-specific credential handling needed.
		NewProvider: deps.newProvider,
		RunAgent:    agent.Run,
		BuildWorkspace: func(ctx context.Context, workspaceRoot string, resolved config.ResolvedConfig, mode agent.PermissionMode) (*acp.Workspace, error) {
			return buildACPWorkspace(ctx, workspaceRoot, resolved, mode, deps)
		},
		ResolveWorkspaceRoot: acpWorkspaceRootResolver(deps),
		Store:                deps.newSessionStore(),
		AgentInfo:            acp.Implementation{Name: "zero", Version: version},
	})

	ctx, stop := acpSignalContext()
	defer stop()
	if err := conn.Serve(ctx); err != nil {
		return writeAppError(stderr, "acp: "+err.Error(), exitCrash)
	}
	return exitSuccess
}

// buildACPWorkspace matches exec's registry construction for one ACP turn. MCP
// servers use the same sandbox-prepared execution runner, project configuration is
// gated by the validated workspace's trust state, and their runtime is released
// when acp.Agent completes the turn. ACP deliberately stays at low autonomy: an
// editor connection never upgrades MCP permissions beyond an interactive prompt.
func buildACPWorkspace(ctx context.Context, workspaceRoot string, resolved config.ResolvedConfig, mode agent.PermissionMode, deps appDeps) (*acp.Workspace, error) {
	scope, err := sandbox.NewScope(workspaceRoot, resolved.Sandbox.AdditionalWriteRoots)
	if err != nil {
		return nil, err
	}
	engine, err := buildExecSandboxEngine(workspaceRoot, resolved, deps, scope)
	if err != nil {
		return nil, err
	}
	registry := newCoreRegistryScoped(workspaceRoot, scope)

	workspace := &acp.Workspace{
		Registry: registry, Sandbox: engine,
		DeferThreshold: resolved.Tools.DeferThreshold,
	}
	if mode != agent.PermissionModePlan {
		// MCP stdio servers are subprocesses. Passing the engine to their runner
		// keeps them inside the exact sandbox / lifecycle path used by zero exec.
		runtime, trustSkip, err := registerMCPToolsForWorkspaceWithOptions(
			ctx, workspaceRoot, registry, deps, mcp.AutonomyLow, workspaceRoot,
			mcp.RegisterOptions{Execution: execution.NewRunner(engine), AdvertiseInAuto: true},
		)
		if err != nil {
			// RegisterTools may have connected an earlier server before reporting a
			// later failure, so do not orphan a partial runtime on this error path.
			if runtime != nil {
				_ = runtime.Close()
			}
			return nil, err
		}
		workspace.Cleanup = runtime.Close
		workspace.Notices = acpMCPSetupNotices(trustSkip, runtime)
	}
	registerLocalControlTools(registry, workspaceRoot, resolved.LocalControl)
	// MCP tools are deferred-eligible. Register their loader only after every
	// ACP-visible tool is present, using the same mode the agent receives.
	registerToolSearchIfEligible(registry, resolved.Tools.DeferThreshold, mode, nil, nil)
	return workspace, nil
}

func acpMCPSetupNotices(skip trustSkip, runtime mcpToolRuntime) []string {
	var notices []string
	if skip.excludedProjectConfig {
		if skip.trustCheckErrored {
			notices = append(notices, "The workspace-trust store could not be read; project MCP servers were ignored (fail-closed). Run 'zero trust' to enable them.")
		} else {
			notices = append(notices, "Project MCP servers were ignored in this untrusted workspace. Run 'zero trust' to enable them.")
		}
	}
	if runtime == nil {
		return notices
	}
	for _, skipped := range runtime.Skipped() {
		if skipped.UnconfiguredDefault {
			continue
		}
		message := fmt.Sprintf("MCP server %s unavailable, skipped", skipped.Name)
		if skipped.Err != nil {
			message += ": " + redaction.ErrorMessage(skipped.Err, redaction.Options{})
		}
		notices = append(notices, redaction.RedactString(message, redaction.Options{}))
	}
	return notices
}

// acpWorkspaceRootResolver validates a client-supplied cwd into a confinement
// root. It reuses exec's resolveWorkspaceRoot (abs+clean, must be an existing
// dir) and additionally rejects the filesystem root and the home directory — an
// editor must not be able to point ZERO's file/shell tools at the whole disk.
func acpWorkspaceRootResolver(deps appDeps) func(string) (string, error) {
	return func(cwd string) (string, error) {
		root, err := resolveWorkspaceRoot(cwd, deps)
		if err != nil {
			return "", err
		}
		if root == filepath.Dir(root) {
			return "", fmt.Errorf("cwd must not be the filesystem root: %s", root)
		}
		if home, herr := os.UserHomeDir(); herr == nil && home != "" && filepath.Clean(home) == root {
			return "", fmt.Errorf("cwd must not be the home directory: %s", root)
		}
		return root, nil
	}
}
