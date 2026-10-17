package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/workspacetrust"
)

// mcpTrustDeps builds an appDeps whose resolveMCPConfig HONORS excludeProject: it
// returns a project stdio server only when excludeProject is false, mirroring the
// real ResolveMCP gate. It records the excludeProject value it was called with and
// whether registerMCPTools (which spawns servers) actually fired.
func mcpTrustDeps(gotExclude *bool, spawned *bool) appDeps {
	return appDeps{
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			*gotExclude = excludeProject
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["proj-srv"] = config.MCPServerConfig{Type: "stdio", Command: "proj-cmd"}
			}
			return config.MCPConfig{Servers: servers}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) { return nil, nil },
		registerMCPTools: func(_ context.Context, _ *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			*spawned = true
			return closeFunc(func() error { return nil }), nil
		},
	}
}

// TestMCPGateUntrustedExcludesProjectServer proves the P0 fix: an untrusted trustRoot
// makes registerMCPToolsForWorkspace resolve MCP config with excludeProject=true and,
// because that drops the project server, it never spawns anything. This test is
// load-bearing: if the gate is removed (a hardcoded excludeProject=false passed to
// resolveMCPConfig), gotExclude is false, the project server survives, spawned flips
// true, and every assertion below fails.
func TestMCPGateUntrustedExcludesProjectServer(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir() // never trusted

	var gotExclude, spawned bool
	deps := mcpTrustDeps(&gotExclude, &spawned)
	registry := tools.NewRegistry()

	runtime, skip, err := registerMCPToolsForWorkspace(context.Background(), repo, registry, deps, mcp.AutonomyLow, repo)
	if err != nil {
		t.Fatalf("registerMCPToolsForWorkspace: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	if !gotExclude {
		t.Fatalf("untrusted workspace must resolve MCP config with excludeProject=true")
	}
	if spawned {
		t.Fatalf("untrusted workspace must not spawn the project MCP server")
	}
	if _, ok := runtime.(noopMCPRuntime); !ok {
		t.Fatalf("with the project server dropped, the runtime should be the noop runtime, got %T", runtime)
	}
	// This repo has no ./.zero/config.json, so there is no project MCP config to
	// notice about even though it is untrusted; the skip must stay clean.
	if skip.excludedProjectConfig {
		t.Fatalf("no project MCP config on disk, so the skip must not flag an excluded config")
	}
	if skip.trustCheckErrored {
		t.Fatalf("a clean untrusted verdict is not a store-read error")
	}
}

// TestMCPGateEmptyTrustRootFailsClosed proves fail-closed-by-construction: a caller
// that forgot to resolve trustRoot (empty) still excludes the project layer.
func TestMCPGateEmptyTrustRootFailsClosed(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	// Even trusting the repo must not help when the caller passes an empty root.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}

	var gotExclude, spawned bool
	deps := mcpTrustDeps(&gotExclude, &spawned)
	registry := tools.NewRegistry()

	runtime, skip, err := registerMCPToolsForWorkspace(context.Background(), repo, registry, deps, mcp.AutonomyLow, "")
	if err != nil {
		t.Fatalf("registerMCPToolsForWorkspace: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	if !gotExclude {
		t.Fatalf("empty trustRoot must fail closed (excludeProject=true)")
	}
	if spawned {
		t.Fatalf("empty trustRoot must not spawn the project MCP server")
	}
	// Empty trustRoot is a clean fail-closed verdict, not a store-read error.
	if skip.trustCheckErrored {
		t.Fatalf("empty trustRoot is not a store-read error")
	}
}

// TestMCPGateTrustedSpawnsProjectServer proves R3 for MCP: after Trust(repo) the
// project layer is included (excludeProject=false) and the project server spawns.
func TestMCPGateTrustedSpawnsProjectServer(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}

	var gotExclude, spawned bool
	deps := mcpTrustDeps(&gotExclude, &spawned)
	registry := tools.NewRegistry()

	runtime, skip, err := registerMCPToolsForWorkspace(context.Background(), repo, registry, deps, mcp.AutonomyLow, repo)
	if err != nil {
		t.Fatalf("registerMCPToolsForWorkspace: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	if gotExclude {
		t.Fatalf("trusted workspace must resolve MCP config with excludeProject=false")
	}
	if !spawned {
		t.Fatalf("trusted workspace must spawn the project MCP server")
	}
	if skip.excludedProjectConfig {
		t.Fatalf("trusted workspace must not report the project MCP layer excluded")
	}
}

// TestMCPToolsListSurfacesTrustNotice proves `zero mcp tools list` no longer drops the
// project MCP layer silently in an untrusted workspace: the gated skip is surfaced on
// stderr (the list stays on stdout), so an empty list is explained rather than read as
// "nothing configured". Trusting the repo silences it.
func TestMCPToolsListSurfacesTrustNotice(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func() string {
		var out, errBuf bytes.Buffer
		deps := appDeps{
			getwd:            func() (string, error) { return repo, nil },
			resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
		}
		if code := runWithDeps([]string{"mcp", "tools", "list"}, &out, &errBuf, deps); code != exitSuccess {
			t.Fatalf("mcp tools list exit = %d, stderr=%q", code, errBuf.String())
		}
		return errBuf.String()
	}

	// Untrusted: the gated project MCP layer must be explained on stderr.
	if errUntrusted := run(); !strings.Contains(errUntrusted, "MCP servers") || !strings.Contains(errUntrusted, "zero trust") {
		t.Fatalf("untrusted `mcp tools list` must surface the trust notice on stderr, got %q", errUntrusted)
	}

	// Trusted: nothing is skipped, so no notice.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	if errTrusted := run(); strings.Contains(errTrusted, "ignoring project") {
		t.Fatalf("trusted `mcp tools list` must not emit a trust notice, got %q", errTrusted)
	}
}

// TestProjectMCPConfigExists exercises every branch of the notice-gating detector:
// only a ./.zero/config.json that parses AND declares at least one server is true.
func TestProjectMCPConfigExists(t *testing.T) {
	writeCfg := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".zero"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".zero", "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if projectMCPConfigExists("") {
		t.Fatal("empty workspace root must be false")
	}
	cases := []struct {
		name string
		body string // "" means write no config.json at all
		want bool
	}{
		{"no file", "", false},
		{"declares a server", `{"mcp":{"servers":{"a":{"type":"stdio","command":"x"}}}}`, true},
		{"empty servers map", `{"mcp":{"servers":{}}}`, false},
		{"config without mcp key", `{"model":"x"}`, false},
		{"unparseable json", `{not valid`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.body != "" {
				writeCfg(t, dir, tc.body)
			}
			if got := projectMCPConfigExists(dir); got != tc.want {
				t.Fatalf("projectMCPConfigExists = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMCPGateFailClosedOnStoreError proves the MCP surface reports a store-read error
// (trust.json created as a directory) as trustCheckErrored, so the caller's notice can
// name the fail-closed reason -- the same error path the hook and plugin gates cover.
func TestMCPGateFailClosedOnStoreError(t *testing.T) {
	configRoot := setTrustConfigRoot(t)
	trustPath := filepath.Join(configRoot, "zero", "trust.json")
	if err := os.MkdirAll(trustPath, 0o700); err != nil {
		t.Fatalf("create trust.json as a directory: %v", err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatalf("mkdir project .zero: %v", err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write project config.json: %v", err)
	}

	var gotExclude, spawned bool
	deps := mcpTrustDeps(&gotExclude, &spawned)
	runtime, skip, err := registerMCPToolsForWorkspace(context.Background(), repo, tools.NewRegistry(), deps, mcp.AutonomyLow, repo)
	if err != nil {
		t.Fatalf("registerMCPToolsForWorkspace: %v", err)
	}
	defer func() { _ = runtime.Close() }()
	if !gotExclude {
		t.Fatalf("a store-read error must fail closed (excludeProject=true)")
	}
	if spawned {
		t.Fatalf("a store-read error must not spawn the project MCP server")
	}
	if !skip.trustCheckErrored {
		t.Fatalf("skip must mark the store-read error so the notice can name it")
	}
	if !skip.excludedProjectConfig {
		t.Fatalf("the project MCP layer must be reported excluded on the error path")
	}
}

// TestMCPGateUntrustedNoticesProjectMCPConfig proves the notice-surfacing fix: when an
// untrusted workspace actually declares project MCP servers in ./.zero/config.json,
// registerMCPToolsForWorkspace reports excludedProjectConfig=true so the caller can
// warn (the CodeRabbit finding: project MCP was gated silently). Trusting the repo
// clears the skip.
func TestMCPGateUntrustedNoticesProjectMCPConfig(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	// A real project MCP config on disk: projectMCPConfigExists reads this file.
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatalf("mkdir project .zero: %v", err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write project config.json: %v", err)
	}

	var gotExclude, spawned bool
	deps := mcpTrustDeps(&gotExclude, &spawned)
	registry := tools.NewRegistry()

	// Untrusted: the project MCP layer is dropped AND flagged for the notice.
	runtime, skip, err := registerMCPToolsForWorkspace(context.Background(), repo, registry, deps, mcp.AutonomyLow, repo)
	if err != nil {
		t.Fatalf("registerMCPToolsForWorkspace (untrusted): %v", err)
	}
	defer func() { _ = runtime.Close() }()
	if !skip.excludedProjectConfig {
		t.Fatalf("untrusted workspace with project MCP config must flag excludedProjectConfig for the notice")
	}
	if skip.trustCheckErrored {
		t.Fatalf("a clean untrusted verdict is not a store-read error")
	}

	// Trusted: nothing is skipped, so no notice.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}
	_, trustedSkip, err := registerMCPToolsForWorkspace(context.Background(), repo, tools.NewRegistry(), deps, mcp.AutonomyLow, repo)
	if err != nil {
		t.Fatalf("registerMCPToolsForWorkspace (trusted): %v", err)
	}
	if trustedSkip.excludedProjectConfig {
		t.Fatalf("trusted workspace must not flag an excluded project MCP layer")
	}
}

// TestMCPCheckSurfacesTrustNotice proves the `zero mcp check` notice fix: in an untrusted
// workspace whose only definition of a server lives in ./.zero/config.json, the gate
// drops the server and the command must emit the one-line trust notice before the
// "not configured" error, instead of a bare miss that hides the trust exclusion. The R4
// half proves the notice self-gates: a genuinely-absent server in a workspace with no
// project config on disk prints "not configured" with no notice.
func TestMCPCheckSurfacesTrustNotice(t *testing.T) {
	setTrustConfigRoot(t)

	// resolveMCPConfig fake that HONORS excludeProject: `proj` survives only when the
	// project layer is included, mirroring the real gate.
	dropFake := func(_ string, excludeProject bool) (config.MCPConfig, error) {
		servers := map[string]config.MCPServerConfig{}
		if !excludeProject {
			servers["proj"] = config.MCPServerConfig{Type: "stdio", Command: "proj-cmd"}
		}
		return config.MCPConfig{Servers: servers}, nil
	}

	// Untrusted workspace whose ONLY definition of `proj` is project config on disk.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	deps := appDeps{getwd: func() (string, error) { return repo, nil }, resolveMCPConfig: dropFake}
	if code := runWithDeps([]string{"mcp", "check", "proj"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatalf("mcp check on a gated project server must fail, got success; stderr=%q", errBuf.String())
	}
	if got := errBuf.String(); !strings.Contains(got, "not configured") ||
		!strings.Contains(got, "MCP servers") || !strings.Contains(got, "zero trust") {
		t.Fatalf("untrusted mcp check must report not-configured AND surface the trust notice, got %q", got)
	}

	// R4: an untrusted workspace with NO project MCP config on disk has nothing to
	// notice about, so an absent server prints "not configured" with no trust notice.
	bare := t.TempDir()
	var out2, errBuf2 bytes.Buffer
	deps2 := appDeps{getwd: func() (string, error) { return bare, nil }, resolveMCPConfig: dropFake}
	if code := runWithDeps([]string{"mcp", "check", "ghost"}, &out2, &errBuf2, deps2); code == exitSuccess {
		t.Fatalf("mcp check on an absent server must fail")
	}
	if got := errBuf2.String(); !strings.Contains(got, "not configured") {
		t.Fatalf("stderr must report not configured, got %q", got)
	}
	if got := errBuf2.String(); strings.Contains(got, "ignoring project") || strings.Contains(got, "zero trust") {
		t.Fatalf("no project config on disk means no trust notice, got %q", got)
	}
}
