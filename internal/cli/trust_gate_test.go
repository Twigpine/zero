package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/workspacetrust"
)

// setTrustConfigRoot redirects both the workspace-trust store and the user-level
// hooks/plugins config to a fresh temp dir with a GOOS-aware env switch, mirroring
// setUserConfigRoot in internal/config/paths_test.go, and returns that dir so tests
// can build the exact paths the code resolves to.
//
// XDG_CONFIG_HOME is the lever on macOS as well as Linux: config.UserConfigDir is
// XDG-first there (it only falls back to $HOME/.config when XDG_CONFIG_HOME is
// unset), and the hooks loader resolves its user layer from XDG_CONFIG_HOME too. An
// earlier version set HOME on darwin and returned the raw root; that left the store
// at <root>/.config/zero while the tests wrote to <root>/zero, so the store-error
// and user-hook cases silently missed. Only Windows needs its own variable (APPDATA,
// what os.UserConfigDir consults). XDG_DATA_HOME is redirected so the hook audit
// store never touches the user's real data dir.
func setTrustConfigRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	return root
}

// writeMarkerHookScript writes an executable script that creates markerPath when
// run, and returns its absolute path. The hook dispatcher runs commands directly
// (no shell), so a real executable is needed to observe execution.
func writeMarkerHookScript(t *testing.T, markerPath string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("marker-hook script is POSIX-shell based")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "mark.sh")
	body := "#!/bin/sh\n: > " + shellQuote(markerPath) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write marker hook script: %v", err)
	}
	return script
}

func shellQuote(path string) string {
	return "'" + path + "'"
}

// writeProjectHooks writes a project ./.zero/hooks.json under repo with the given
// hooks and top-level enabled flag.
func writeProjectHooks(t *testing.T, repo string, enabled bool, defs ...hooks.Definition) {
	t.Helper()
	path := filepath.Join(repo, ".zero", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir project .zero: %v", err)
	}
	if err := hooks.WriteConfig(path, hooks.Config{Enabled: enabled, Hooks: defs}); err != nil {
		t.Fatalf("write project hooks.json: %v", err)
	}
}

// writeUserHooks writes a user-level hooks.json into the redirected config root.
func writeUserHooks(t *testing.T, configRoot string, enabled bool, defs ...hooks.Definition) {
	t.Helper()
	path := filepath.Join(configRoot, "zero", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir user config dir: %v", err)
	}
	if err := hooks.WriteConfig(path, hooks.Config{Enabled: enabled, Hooks: defs}); err != nil {
		t.Fatalf("write user hooks.json: %v", err)
	}
}

func dispatchBeforeTool(dispatcher *hooks.Dispatcher) hooks.DispatchOutcome {
	return dispatcher.Dispatch(context.Background(), hooks.DispatchInput{
		Event:    hooks.EventBeforeTool,
		ToolName: "bash",
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestHookGateUntrustedExcludesProjectLayer proves R1: an untrusted trustRoot with
// an enabled project beforeTool hook runs nothing and writes no marker.
func TestHookGateUntrustedExcludesProjectLayer(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeMarkerHookScript(t, marker)
	writeProjectHooks(t, repo, true, hooks.Definition{
		ID: "proj.mark", Event: hooks.EventBeforeTool, Command: script, Enabled: true,
	})

	dispatcher, skip := newHookDispatcherWithExtra(repo, nil, repo)
	outcome := dispatchBeforeTool(dispatcher)

	if outcome.Ran != 0 {
		t.Fatalf("untrusted workspace should run 0 project hooks, Ran=%d", outcome.Ran)
	}
	if fileExists(marker) {
		t.Fatalf("untrusted workspace must not execute the project hook (marker exists)")
	}
	if !skip.excludedProjectConfig {
		t.Fatalf("skip report should flag the excluded project hooks file")
	}
	if skip.trustCheckErrored {
		t.Fatalf("a clean untrusted verdict is not a store-read error")
	}
}

// TestHookGateTrustedRunsProjectHook proves R3: after Trust(repo) the project hook
// runs and the marker appears.
func TestHookGateTrustedRunsProjectHook(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeMarkerHookScript(t, marker)
	writeProjectHooks(t, repo, true, hooks.Definition{
		ID: "proj.mark", Event: hooks.EventBeforeTool, Command: script, Enabled: true,
	})

	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}

	dispatcher, skip := newHookDispatcherWithExtra(repo, nil, repo)
	outcome := dispatchBeforeTool(dispatcher)

	if outcome.Ran != 1 {
		t.Fatalf("trusted workspace should run the project hook, Ran=%d", outcome.Ran)
	}
	if !fileExists(marker) {
		t.Fatalf("trusted workspace must execute the project hook (marker missing)")
	}
	if skip.excludedProjectConfig {
		t.Fatalf("a trusted workspace must not report the project layer as excluded")
	}
}

// TestHookGateEmptyTrustRootFailsClosed proves the fail-closed-by-construction
// guard: a caller that forgot to resolve trustRoot (empty) still excludes the
// project layer, so no marker appears.
func TestHookGateEmptyTrustRootFailsClosed(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeMarkerHookScript(t, marker)
	writeProjectHooks(t, repo, true, hooks.Definition{
		ID: "proj.mark", Event: hooks.EventBeforeTool, Command: script, Enabled: true,
	})
	// Even trusting the repo must not help when the caller passes an empty root.
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}

	dispatcher, skip := newHookDispatcherWithExtra(repo, nil, "")
	outcome := dispatchBeforeTool(dispatcher)

	if outcome.Ran != 0 {
		t.Fatalf("empty trustRoot must fail closed, Ran=%d", outcome.Ran)
	}
	if fileExists(marker) {
		t.Fatalf("empty trustRoot must not execute the project hook (marker exists)")
	}
	if !skip.excludedProjectConfig {
		t.Fatalf("empty trustRoot should report the project layer excluded")
	}
}

// TestHookGateFailClosedOnStoreError proves R5: a real IsTrusted error (trust.json
// created as a directory, per U1) is treated as untrusted, the project hook does
// not run, and the skip report marks the store-read error.
func TestHookGateFailClosedOnStoreError(t *testing.T) {
	configRoot := setTrustConfigRoot(t)
	// Create the trust store path as a DIRECTORY so os.ReadFile fails with a
	// non-ErrNotExist error, forcing IsTrusted to return (false, non-nil).
	trustPath := filepath.Join(configRoot, "zero", "trust.json")
	if err := os.MkdirAll(trustPath, 0o700); err != nil {
		t.Fatalf("create trust.json as a directory: %v", err)
	}

	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeMarkerHookScript(t, marker)
	writeProjectHooks(t, repo, true, hooks.Definition{
		ID: "proj.mark", Event: hooks.EventBeforeTool, Command: script, Enabled: true,
	})

	dispatcher, skip := newHookDispatcherWithExtra(repo, nil, repo)
	outcome := dispatchBeforeTool(dispatcher)

	if outcome.Ran != 0 {
		t.Fatalf("a store-read error must fail closed, Ran=%d", outcome.Ran)
	}
	if fileExists(marker) {
		t.Fatalf("a store-read error must not execute the project hook (marker exists)")
	}
	if !skip.trustCheckErrored {
		t.Fatalf("skip report must mark the store-read error")
	}
	if !skip.excludedProjectConfig {
		t.Fatalf("the project layer must be reported excluded on the error path")
	}
}

// TestHookGateUserHookStillRunsWhenProjectExcluded proves R4/R10: with the project
// layer excluded, a user-level hook still fires, and a project hooks.json that sets
// global enabled:false or defines a same-ID hook cannot disable or override it.
func TestHookGateUserHookStillRunsWhenProjectExcluded(t *testing.T) {
	configRoot := setTrustConfigRoot(t)
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeMarkerHookScript(t, marker)

	// User hook that writes the marker.
	writeUserHooks(t, configRoot, true, hooks.Definition{
		ID: "shared.mark", Event: hooks.EventBeforeTool, Command: script, Enabled: true,
	})
	// Project config that would, if loaded, disable the whole surface (enabled:false)
	// AND override the user hook by ID with a disabled no-op. The gate drops the
	// whole project layer, so neither takes effect.
	writeProjectHooks(t, repo, false, hooks.Definition{
		ID: "shared.mark", Event: hooks.EventBeforeTool, Command: "true", Enabled: false,
	})

	// Untrusted: the project layer is excluded.
	dispatcher, _ := newHookDispatcherWithExtra(repo, nil, repo)
	outcome := dispatchBeforeTool(dispatcher)

	if outcome.Ran != 1 {
		t.Fatalf("user hook must still run when the project layer is excluded, Ran=%d", outcome.Ran)
	}
	if !fileExists(marker) {
		t.Fatalf("user hook must fire (marker missing); project config must not disable/override it")
	}
}

// TestHookGateWorktreeInheritsTrust proves the worktree case: with repo trusted,
// calling the chokepoint with a different workspaceRoot (the worktree path) but
// trustRoot=repo loads the project hooks, so trust keys on trustRoot not
// workspaceRoot.
func TestHookGateWorktreeInheritsTrust(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	worktree := t.TempDir() // a distinct, never-trusted path
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeMarkerHookScript(t, marker)
	// The project hooks live under the worktree (that is where config is read from).
	writeProjectHooks(t, worktree, true, hooks.Definition{
		ID: "proj.mark", Event: hooks.EventBeforeTool, Command: script, Enabled: true,
	})

	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}

	// workspaceRoot is the worktree, but trustRoot is the original repo.
	dispatcher, skip := newHookDispatcherWithExtra(worktree, nil, repo)
	outcome := dispatchBeforeTool(dispatcher)

	if outcome.Ran != 1 {
		t.Fatalf("a worktree of a trusted repo should load project hooks, Ran=%d", outcome.Ran)
	}
	if !fileExists(marker) {
		t.Fatalf("worktree run should execute the project hook (marker missing)")
	}
	if skip.excludedProjectConfig {
		t.Fatalf("worktree of a trusted repo must not report the project layer excluded")
	}
}

// TestPluginGateUntrustedExcludesProject proves R2/R5: an untrusted trustRoot makes
// activatePlugins load with ExcludeProject=true and report the skip.
func TestPluginGateUntrustedExcludesProject(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	// A ./.zero/plugins dir so the skip report flags an excluded project config.
	if err := os.MkdirAll(filepath.Join(repo, ".zero", "plugins"), 0o700); err != nil {
		t.Fatalf("mkdir project plugins dir: %v", err)
	}

	var gotExclude bool
	deps := appDeps{
		loadPlugins: func(opts plugins.LoadOptions) (plugins.LoadResult, error) {
			gotExclude = opts.ExcludeProject
			return plugins.LoadResult{}, nil
		},
		skillsDir: func() string { return t.TempDir() },
	}
	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	activation := activatePlugins(repo, registry, deps, &stderr, repo)

	if !gotExclude {
		t.Fatalf("untrusted workspace must pass ExcludeProject=true to loadPlugins")
	}
	if !activation.excludedProjectConfig {
		t.Fatalf("skip report should flag the excluded ./.zero/plugins dir")
	}
	if activation.trustCheckErrored {
		t.Fatalf("a clean untrusted verdict is not a store-read error")
	}
}

// TestPluginGateTrustedIncludesProject proves R3: after Trust(repo), activatePlugins
// loads with ExcludeProject=false.
func TestPluginGateTrustedIncludesProject(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatalf("Trust(repo): %v", err)
	}

	var gotExclude bool
	deps := appDeps{
		loadPlugins: func(opts plugins.LoadOptions) (plugins.LoadResult, error) {
			gotExclude = opts.ExcludeProject
			return plugins.LoadResult{}, nil
		},
		skillsDir: func() string { return t.TempDir() },
	}
	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	activation := activatePlugins(repo, registry, deps, &stderr, repo)

	if gotExclude {
		t.Fatalf("trusted workspace must pass ExcludeProject=false to loadPlugins")
	}
	if activation.excludedProjectConfig {
		t.Fatalf("trusted workspace must not report the project config excluded")
	}
}

// TestPluginGateFailClosedOnStoreError proves R5 for plugins: a store-read error
// forces ExcludeProject=true and marks the error.
func TestPluginGateFailClosedOnStoreError(t *testing.T) {
	configRoot := setTrustConfigRoot(t)
	trustPath := filepath.Join(configRoot, "zero", "trust.json")
	if err := os.MkdirAll(trustPath, 0o700); err != nil {
		t.Fatalf("create trust.json as a directory: %v", err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero", "plugins"), 0o700); err != nil {
		t.Fatalf("mkdir project plugins dir: %v", err)
	}

	var gotExclude bool
	deps := appDeps{
		loadPlugins: func(opts plugins.LoadOptions) (plugins.LoadResult, error) {
			gotExclude = opts.ExcludeProject
			return plugins.LoadResult{}, nil
		},
		skillsDir: func() string { return t.TempDir() },
	}
	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	activation := activatePlugins(repo, registry, deps, &stderr, repo)

	if !gotExclude {
		t.Fatalf("a store-read error must fail closed (ExcludeProject=true)")
	}
	if !activation.trustCheckErrored {
		t.Fatalf("skip report must mark the store-read error")
	}
}

// TestEmitTrustNoticeOneLineWhenSkipped proves R6: exactly one notice when either
// surface skips project config, none when trusted, and distinct error text on a
// store-read failure.
func TestEmitTrustNoticeOneLineWhenSkipped(t *testing.T) {
	t.Run("both surfaces skipped yields one line", func(t *testing.T) {
		var buf bytes.Buffer
		emitTrustNotice(&buf,
			trustSkip{excludedProjectConfig: true},
			trustSkip{excludedProjectConfig: true})
		lines := nonEmptyLines(buf.String())
		if len(lines) != 1 {
			t.Fatalf("expected exactly one notice line, got %d: %q", len(lines), buf.String())
		}
		if !bytes.Contains(buf.Bytes(), []byte("zero trust")) {
			t.Fatalf("notice should point at 'zero trust', got %q", buf.String())
		}
	})

	t.Run("trusted yields no notice", func(t *testing.T) {
		var buf bytes.Buffer
		emitTrustNotice(&buf, trustSkip{}, trustSkip{})
		if buf.Len() != 0 {
			t.Fatalf("a trusted session must emit no notice, got %q", buf.String())
		}
	})

	t.Run("store-read error uses distinct text", func(t *testing.T) {
		var buf bytes.Buffer
		emitTrustNotice(&buf,
			trustSkip{excludedProjectConfig: true, trustCheckErrored: true},
			trustSkip{})
		lines := nonEmptyLines(buf.String())
		if len(lines) != 1 {
			t.Fatalf("expected exactly one notice line, got %d: %q", len(lines), buf.String())
		}
		if !bytes.Contains(buf.Bytes(), []byte("could not be read")) {
			t.Fatalf("error-path notice should name the store read failure, got %q", buf.String())
		}
	})

	t.Run("only one surface skipped still yields one line", func(t *testing.T) {
		var buf bytes.Buffer
		emitTrustNotice(&buf, trustSkip{excludedProjectConfig: true}, trustSkip{})
		lines := nonEmptyLines(buf.String())
		if len(lines) != 1 {
			t.Fatalf("expected exactly one notice line, got %d: %q", len(lines), buf.String())
		}
	})

	t.Run("mcp skip alone yields one line naming mcp", func(t *testing.T) {
		var buf bytes.Buffer
		// Hooks and plugins clean; only the MCP surface (third arg) dropped project config.
		emitTrustNotice(&buf, trustSkip{}, trustSkip{}, trustSkip{excludedProjectConfig: true})
		lines := nonEmptyLines(buf.String())
		if len(lines) != 1 {
			t.Fatalf("expected exactly one notice line, got %d: %q", len(lines), buf.String())
		}
		if !bytes.Contains(buf.Bytes(), []byte("MCP")) {
			t.Fatalf("notice should name MCP when the MCP surface is skipped, got %q", buf.String())
		}
	})

	t.Run("store error with nothing excluded yields no notice", func(t *testing.T) {
		// trustCheckErrored but no surface had project config to skip (excludedProjectConfig
		// false): there is nothing to warn about, so the excluded-gate wins over the error.
		var buf bytes.Buffer
		emitTrustNotice(&buf, trustSkip{trustCheckErrored: true}, trustSkip{trustCheckErrored: true})
		if buf.Len() != 0 {
			t.Fatalf("a store error with no skipped project config must emit nothing, got %q", buf.String())
		}
	})
}

func nonEmptyLines(s string) []string {
	out := []string{}
	for _, line := range bytes.Split([]byte(s), []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, string(line))
		}
	}
	return out
}
