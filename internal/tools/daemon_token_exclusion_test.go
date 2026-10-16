package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/daemon/remote"
	"github.com/Gitlawb/zero/internal/sandbox"
)

// daemonTokenFixture builds a workspace holding the remote bridge's token file
// alongside an ordinary file, with ZERO_DAEMON_REMOTE_TOKEN_FILE pointing at it —
// the shape a remote daemon session takes when its token lives in the session
// workspace. AllowRead deliberately covers the whole workspace so the tests prove
// the exclusion is not re-includable.
func daemonTokenFixture(t *testing.T) (string, string, *sandbox.Engine) {
	t.Helper()
	return daemonTokenFixtureNamed(t, "bridge-token")
}

// daemonTokenFixtureNamed is daemonTokenFixture with the token's filename under
// the caller's control, so a test can pin a spelling the gate and the tool must
// agree on byte for byte (a trailing space, for instance). It skips rather than
// fails when the host filesystem refuses the name — Windows rejects trailing
// spaces and dots — so the matrix still runs everywhere it can.
func daemonTokenFixtureNamed(t *testing.T, tokenName string) (string, string, *sandbox.Engine) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main // bridge-secret\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	token := filepath.Join(ws, tokenName)
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Skipf("filesystem rejects token name %q: %v", tokenName, err)
	}
	// Windows silently strips trailing spaces/dots rather than erroring, and it
	// strips them from the lookup too, so an Lstat of the requested name would
	// happily find the normalized file and the test would assert against a
	// spelling that never existed. Compare against the real directory entry.
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	stored := false
	for _, entry := range entries {
		if entry.Name() == tokenName {
			stored = true
			break
		}
	}
	if !stored {
		t.Skipf("filesystem did not preserve token name %q", tokenName)
	}
	t.Setenv(remote.EnvToken, "")
	t.Setenv(remote.EnvTokenFile, token)

	scope, err := sandbox.NewScope(ws, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: ws,
		Policy: sandbox.Policy{
			Mode:             sandbox.ModeEnforce,
			EnforceWorkspace: true,
			AllowRead:        []string{ws},
		},
		Scope: scope,
	})
	return ws, token, engine
}

// defaultPolicyDaemonTokenEngine is the remote-daemon shape relevant to shell
// escalation: a selected token file with the default policy and no user
// DenyRead entries.
func defaultPolicyDaemonTokenEngine(t *testing.T) *sandbox.Engine {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(ws, "bridge-token")
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv(remote.EnvToken, "")
	t.Setenv(remote.EnvTokenFile, token)
	policy := sandbox.DefaultPolicy()
	if len(policy.DenyRead) != 0 {
		t.Fatalf("default policy DenyRead = %#v, want none", policy.DenyRead)
	}
	return sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: ws, Policy: policy})
}

func TestGrepSkipsDaemonTokenFile(t *testing.T) {
	ws, _, engine := daemonTokenFixture(t)
	tool, ok := NewScopedGrepTool(ws, nil).(sandboxAwareTool)
	if !ok {
		t.Fatal("grep tool must be sandbox-aware")
	}
	args := map[string]any{"pattern": "bridge-secret", "output_mode": "files_with_matches"}

	sandboxed := tool.RunWithSandbox(context.Background(), args, engine)
	if sandboxed.Status != StatusOK {
		t.Fatalf("grep failed: %s", sandboxed.Output)
	}
	if !strings.Contains(sandboxed.Output, "main.go") {
		t.Fatalf("grep must still match ordinary workspace files, got:\n%s", sandboxed.Output)
	}
	if strings.Contains(sandboxed.Output, "bridge-token") {
		t.Fatalf("grep must NOT surface the remote bridge token file, got:\n%s", sandboxed.Output)
	}
}

func TestGlobSkipsDaemonTokenFile(t *testing.T) {
	ws, _, engine := daemonTokenFixture(t)
	tool, ok := NewScopedGlobTool(ws, nil).(sandboxAwareTool)
	if !ok {
		t.Fatal("glob tool must be sandbox-aware")
	}

	sandboxed := tool.RunWithSandbox(context.Background(), map[string]any{"pattern": "**/*"}, engine)
	if sandboxed.Status != StatusOK {
		t.Fatalf("glob failed: %s", sandboxed.Output)
	}
	if !strings.Contains(sandboxed.Output, "main.go") {
		t.Fatalf("glob must still match ordinary workspace files, got:\n%s", sandboxed.Output)
	}
	if strings.Contains(sandboxed.Output, "bridge-token") {
		t.Fatalf("glob must NOT surface the remote bridge token file, got:\n%s", sandboxed.Output)
	}
}

func TestListDirectorySkipsDaemonTokenFile(t *testing.T) {
	ws, _, engine := daemonTokenFixture(t)
	registry := NewRegistry()
	registry.Register(NewScopedListDirectoryTool(ws, nil))

	result := registry.RunWithOptions(context.Background(), "list_directory", map[string]any{
		"path": ".",
	}, RunOptions{Sandbox: engine})
	if result.Status != StatusOK {
		t.Fatalf("list_directory failed: %s", result.Output)
	}
	if !strings.Contains(result.Output, "main.go") {
		t.Fatalf("list_directory must still show ordinary workspace files, got:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "bridge-token") {
		t.Fatalf("list_directory must NOT surface the remote bridge token file, got:\n%s", result.Output)
	}
}

// TestEngineDeniesDaemonTokenFileTools covers the request gate the direct file
// tools go through (read_file, write_file, edit_file, apply_patch): the bridge
// token must be neither readable nor writable, even though AllowRead covers the
// whole workspace and the file is inside it.
func TestEngineDeniesDaemonTokenFileTools(t *testing.T) {
	ws, token, engine := daemonTokenFixture(t)
	for _, tc := range []struct {
		name       string
		toolName   string
		sideEffect sandbox.SideEffect
	}{
		{name: "read_file", toolName: "read_file", sideEffect: sandbox.SideEffectRead},
		{name: "write_file", toolName: "write_file", sideEffect: sandbox.SideEffectWrite},
		{name: "edit_file", toolName: "edit_file", sideEffect: sandbox.SideEffectWrite},
		{name: "apply_patch", toolName: "apply_patch", sideEffect: sandbox.SideEffectWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := engine.Evaluate(context.Background(), sandbox.Request{
				ToolName:      tc.toolName,
				WorkspaceRoot: ws,
				SideEffect:    tc.sideEffect,
				Args:          map[string]any{"path": token},
				// A granted permission must not override the exclusion either.
				Permission: sandbox.PermissionAllow,
			})
			if decision.Action != sandbox.ActionDeny || !strings.Contains(decision.Reason, "remote bridge token") {
				t.Fatalf("%s on the bridge token: action = %q reason = %q, want a bridge-token deny", tc.toolName, decision.Action, decision.Reason)
			}
		})
	}

	// An ordinary workspace file in the same directory stays usable.
	decision := engine.Evaluate(context.Background(), sandbox.Request{
		ToolName:      "read_file",
		WorkspaceRoot: ws,
		SideEffect:    sandbox.SideEffectRead,
		Args:          map[string]any{"path": filepath.Join(ws, "main.go")},
	})
	if decision.Action == sandbox.ActionDeny {
		t.Fatalf("ordinary workspace read was denied: %q", decision.Reason)
	}
}

func TestApplyPatchDeniesQuotedDaemonTokenPathWithSpaces(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(ws, "bridge token")
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv(remote.EnvToken, "")
	t.Setenv(remote.EnvTokenFile, token)
	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: ws, Policy: sandbox.DefaultPolicy()})

	registry := NewRegistry()
	registry.Register(NewScopedApplyPatchTool(ws, nil))
	patch := "diff --git \"a/bridge token\" \"b/bridge token\"\n" +
		"--- \"a/bridge token\"\n" +
		"+++ \"b/bridge token\"\n" +
		"@@ -1 +1 @@\n" +
		"-bridge-secret\n" +
		"+attacker-controlled\n"
	result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
		"patch": patch,
	}, RunOptions{Sandbox: engine, PermissionGranted: true})
	if result.Status == StatusOK || !strings.Contains(result.Output, "remote bridge token") {
		t.Fatalf("apply_patch on quoted protected path: status=%s output=%q, want bridge-token denial", result.Status, result.Output)
	}
	contents, err := os.ReadFile(token)
	if err != nil || string(contents) != "bridge-secret\n" {
		t.Fatalf("token changed after denied patch: contents=%q err=%v", contents, err)
	}
}

func TestApplyPatchDeniesHeaderOnlyAndBinaryDaemonTokenPatches(t *testing.T) {
	original := []byte("bridge-secret\x00original\n")
	for _, tc := range []struct {
		name              string
		tokenName         string
		patch             string
		destination       string
		wantControlSource []byte
		wantControlTarget []byte
		// controlUnsupported marks a patch form the in-process unified-diff
		// engine does not implement, so the control cannot demonstrate an
		// applied effect: apply_patch no longer shells out to git, and these
		// forms now reach no file at all. The control then proves the weaker
		// but still non-vacuous property that the refusal is a format refusal
		// which creates nothing, while the protected case below must still be
		// refused by the credential gate specifically.
		controlUnsupported string
	}{
		{
			name:      "header-only copy",
			tokenName: "bridge token",
			patch: "diff --git a/bridge token b/exposed-token\n" +
				"similarity index 100%\n" +
				"copy from bridge token\n" +
				"copy to exposed-token\n",
			destination:       "exposed-token",
			wantControlSource: original,
			wantControlTarget: original,
		},
		{
			name:      "header-only rename",
			tokenName: "bridge token",
			patch: "diff --git a/bridge token b/renamed-token\n" +
				"similarity index 100%\n" +
				"rename from bridge token\n" +
				"rename to renamed-token\n",
			destination:       "renamed-token",
			wantControlTarget: original,
		},
		{
			name:      "header-only rename aliases",
			tokenName: "bridge token",
			patch: "diff --git a/bridge token b/alias-renamed-token\n" +
				"similarity index 100%\n" +
				"rename old bridge token\n" +
				"rename new alias-renamed-token\n",
			destination:       "alias-renamed-token",
			wantControlTarget: original,
			// Legacy "rename old"/"rename new" spellings; the in-process engine
			// implements only git's "rename from"/"rename to".
			controlUnsupported: `unexpected "rename old bridge token"`,
		},
		{
			name:      "header-only copy preserves leading space",
			tokenName: " bridge-token",
			patch: "diff --git a/ bridge-token b/leading-space-copy\n" +
				"similarity index 100%\n" +
				"copy from  bridge-token\n" +
				"copy to leading-space-copy\n",
			destination:       "leading-space-copy",
			wantControlSource: original,
			// The rename/copy executor now reads its headers through the same
			// byte-preserving parser as the credential gate, so a leading space
			// is copied as pathname data instead of being trimmed away into a
			// different file's name.
			wantControlTarget: original,
		},
		{
			name:      "binary modification",
			tokenName: "bridge token",
			patch: "diff --git a/bridge token b/bridge token\n" +
				"index 6e4018bb778ca15e70706fe1f7a4c22e762f37b6..d3eae57ec6ad245ec7ab173e28141ac2f87cca68 100644\n" +
				"GIT binary patch\n" +
				"literal 23\n" +
				"ecmYc+DM?JuPA$?+Ni0cZ$jwj5Ov_A7;Q|0@R|sMN\n\n" +
				"literal 23\n" +
				"ecmYc)%1lX5)h$j<E=nz7$S=xF&&*5A;Q|0@YY2b<\n\n",
			wantControlSource:  []byte("attacker-data\x00modified\n"),
			controlUnsupported: "binary patches are not supported",
		},
		{
			name:      "binary modification preserves trailing space",
			tokenName: "bridge-token ",
			patch: "diff --git a/bridge-token  b/bridge-token \n" +
				"index 6e4018bb778ca15e70706fe1f7a4c22e762f37b6..d3eae57ec6ad245ec7ab173e28141ac2f87cca68 100644\n" +
				"GIT binary patch\n" +
				"literal 23\n" +
				"ecmYc+DM?JuPA$?+Ni0cZ$jwj5Ov_A7;Q|0@R|sMN\n\n" +
				"literal 23\n" +
				"ecmYc)%1lX5)h$j<E=nz7$S=xF&&*5A;Q|0@YY2b<\n\n",
			wantControlSource:  []byte("attacker-data\x00modified\n"),
			controlUnsupported: "binary patches are not supported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Prove the fixture is a valid patch with the stated effect. The protected
			// case below is rejected before git runs, so without this control an
			// accidentally invalid regression fixture could pass vacuously.
			// Git for Windows rejects trailing-space paths, but the protected case
			// must still exercise our parser and deny the exact header before Git runs.
			if runtime.GOOS != "windows" || !strings.HasSuffix(tc.tokenName, " ") {
				control := t.TempDir()
				if err := os.WriteFile(filepath.Join(control, tc.tokenName), original, 0o600); err != nil {
					t.Fatalf("write control token: %v", err)
				}
				controlRegistry := NewRegistry()
				controlRegistry.Register(NewScopedApplyPatchTool(control, nil))
				controlResult := controlRegistry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
					"patch": tc.patch,
				}, RunOptions{PermissionGranted: true})
				switch {
				case tc.controlUnsupported != "":
					// The form itself is unimplemented, so it must be refused for
					// a format reason — never silently applied — and it must not
					// create the destination it names.
					if controlResult.Status == StatusOK {
						t.Fatalf("control patch was applied despite an unsupported form: %s", controlResult.Output)
					}
					if !strings.Contains(controlResult.Output, tc.controlUnsupported) {
						t.Fatalf("control refusal = %q, want the %q format refusal", controlResult.Output, tc.controlUnsupported)
					}
					if tc.destination != "" {
						if _, err := os.Stat(filepath.Join(control, tc.destination)); !os.IsNotExist(err) {
							t.Fatalf("refused control patch created %q: err=%v", tc.destination, err)
						}
					}
				default:
					if controlResult.Status != StatusOK {
						t.Fatalf("control patch is invalid: %s", controlResult.Output)
					}
					controlSource, sourceErr := os.ReadFile(filepath.Join(control, tc.tokenName))
					if tc.wantControlSource == nil {
						if !os.IsNotExist(sourceErr) {
							t.Fatalf("control source still exists after rename: contents=%q err=%v", controlSource, sourceErr)
						}
					} else if sourceErr != nil || string(controlSource) != string(tc.wantControlSource) {
						t.Fatalf("control source: contents=%q err=%v", controlSource, sourceErr)
					}
					if tc.destination != "" {
						controlTarget, err := os.ReadFile(filepath.Join(control, tc.destination))
						if err != nil || string(controlTarget) != string(tc.wantControlTarget) {
							t.Fatalf("control target: contents=%q err=%v", controlTarget, err)
						}
					}
				}
			}

			ws, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			token := filepath.Join(ws, tc.tokenName)
			if err := os.WriteFile(token, original, 0o600); err != nil {
				t.Fatalf("write token: %v", err)
			}
			t.Setenv(remote.EnvToken, "")
			t.Setenv(remote.EnvTokenFile, token)
			engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: ws, Policy: sandbox.DefaultPolicy()})

			registry := NewRegistry()
			registry.Register(NewScopedApplyPatchTool(ws, nil))
			result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
				"patch": tc.patch,
			}, RunOptions{Sandbox: engine, PermissionGranted: true})
			if result.Status == StatusOK {
				t.Fatalf("apply_patch unexpectedly accepted the protected patch: output=%q", result.Output)
			}
			if tc.controlUnsupported == "" && !strings.Contains(result.Output, "remote bridge token") {
				t.Fatalf("apply_patch: status=%s output=%q, want bridge-token denial", result.Status, result.Output)
			}
			if tc.controlUnsupported != "" && !strings.Contains(result.Output, tc.controlUnsupported) {
				t.Fatalf("unsupported protected patch refusal = %q, want %q", result.Output, tc.controlUnsupported)
			}

			contents, err := os.ReadFile(token)
			if err != nil || string(contents) != string(original) {
				t.Fatalf("token changed after denied patch: contents=%q err=%v", contents, err)
			}
			if tc.destination != "" {
				if _, err := os.Stat(filepath.Join(ws, tc.destination)); !os.IsNotExist(err) {
					t.Fatalf("denied patch created %q: err=%v", tc.destination, err)
				}
			}
		})
	}
}

// TestApplyPatchDeniesTrailingSpaceDaemonTokenPath pins the unified-diff form
// git leaves unquoted: a header operand whose last byte is an ordinary space.
// The parser must keep that byte rather than trim it as header formatting.
func TestApplyPatchDeniesTrailingSpaceDaemonTokenPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot end in a space")
	}
	const original = "bridge-secret\n"
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(ws, "bridge-token ")
	if err := os.WriteFile(token, []byte(original), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	t.Setenv(remote.EnvToken, "")
	// CanonicalizeTokenFileEnv is the production daemon boundary and preserves
	// filename whitespace while converting the selected path to an absolute one.
	t.Setenv(remote.EnvTokenFile, filepath.Join(ws, ".", "bridge-token "))
	t.Setenv(remote.EnvTokenFileResolved, "")
	if err := remote.CanonicalizeTokenFileEnv(); err != nil {
		t.Fatalf("CanonicalizeTokenFileEnv: %v", err)
	}

	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: ws, Policy: sandbox.DefaultPolicy()})
	registry := NewRegistry()
	registry.Register(NewScopedApplyPatchTool(ws, nil))
	result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
		"patch": "--- a/bridge-token \n" +
			"+++ b/bridge-token \n" +
			"@@ -1 +1 @@\n" +
			"-bridge-secret\n" +
			"+attacker-controlled\n",
	}, RunOptions{Sandbox: engine, PermissionGranted: true})
	if result.Status == StatusOK || !strings.Contains(result.Output, "remote bridge token") {
		t.Fatalf("apply_patch on the selected token: status=%s output=%q, want bridge-token denial", result.Status, result.Output)
	}
	if contents, err := os.ReadFile(token); err != nil || string(contents) != original {
		t.Fatalf("token changed after denied patch: contents=%q err=%v", contents, err)
	}
}

// An unresolved diff header must fail before git runs; security must not depend
// on whether the installed git version happens to reject the same ambiguity.
func TestAmbiguousGitHeaderFailsClosedBeforeApply(t *testing.T) {
	patch := "diff --git a/bridge b/token b/exposed-token\n" +
		"new file mode 100644\n"

	workspace := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedApplyPatchTool(workspace, nil))
	for _, options := range []RunOptions{
		{PermissionGranted: true},
		{PermissionGranted: true, Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: workspace,
			Policy:        sandbox.Policy{Mode: sandbox.ModeDisabled},
		})},
	} {
		result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
			"patch": patch,
		}, options)
		if result.Status == StatusOK || !strings.Contains(result.Output, "file mode header without a diff --git path") {
			t.Fatalf("ambiguous patch: status=%s output=%q, want pre-apply parse denial", result.Status, result.Output)
		}
	}
}

// TestDaemonTokenAliasesDeniedEndToEnd covers the in-process tools, which are
// the layer that can close inode aliases: they see every requested path before
// opening it, so a symlink or hard link to the token resolves back to the
// protected inode and is refused.
//
// The OS layer deliberately does not match this. Seatbelt and Bubblewrap rules
// name paths, so a sandboxed shell on macOS can `ln <token> alias && cat alias`
// — the same pathname model a user-configured DenyRead has always had. See
// protectedPathDenied and denyReadRules in internal/sandbox for why that is the
// boundary rather than an omission.
func TestDaemonTokenAliasesDeniedEndToEnd(t *testing.T) {
	for _, aliasKind := range []string{"symlink", "hardlink"} {
		t.Run(aliasKind, func(t *testing.T) {
			ws, token, engine := daemonTokenFixture(t)
			alias := filepath.Join(ws, "token-alias")
			var err error
			switch aliasKind {
			case "symlink":
				err = os.Symlink(token, alias)
			case "hardlink":
				err = os.Link(token, alias)
			}
			if err != nil {
				t.Skipf("%s unsupported: %v", aliasKind, err)
			}

			registry := NewRegistry()
			registry.Register(NewScopedReadFileTool(ws, nil))
			registry.Register(NewScopedWriteFileTool(ws, nil))

			read := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": alias}, RunOptions{Sandbox: engine})
			if read.Status == StatusOK || strings.Contains(read.Output, "bridge-secret") {
				t.Fatalf("read_file followed protected %s: status=%s output=%q", aliasKind, read.Status, read.Output)
			}

			write := registry.RunWithOptions(context.Background(), "write_file", map[string]any{"path": alias, "content": "attacker-controlled\n"}, RunOptions{Sandbox: engine})
			if write.Status == StatusOK {
				t.Fatalf("write_file followed protected %s: output=%q", aliasKind, write.Output)
			}
			contents, err := os.ReadFile(token)
			if err != nil || string(contents) != "bridge-secret\n" {
				t.Fatalf("token changed after denied write through %s: contents=%q err=%v", aliasKind, contents, err)
			}

			grep, ok := NewScopedGrepTool(ws, nil).(sandboxAwareTool)
			if !ok {
				t.Fatal("grep tool must be sandbox-aware")
			}
			result := grep.RunWithSandbox(context.Background(), map[string]any{
				"pattern":     "bridge-secret",
				"output_mode": "files_with_matches",
			}, engine)
			if result.Status != StatusOK {
				t.Fatalf("grep failed: %s", result.Output)
			}
			if strings.Contains(result.Output, "token-alias") || strings.Contains(result.Output, "bridge-token") {
				t.Fatalf("grep surfaced protected %s target:\n%s", aliasKind, result.Output)
			}
			if !strings.Contains(result.Output, "main.go") {
				t.Fatalf("grep omitted ordinary file while filtering %s:\n%s", aliasKind, result.Output)
			}
		})
	}
}

// TestGrepDoesNotScanTokenAliasSwappedInAfterExclusion closes the check/use
// window between grep's walk-time pathname exclusion and the os.Open in
// scanGrepFile. A workspace writer can replace an ordinary candidate with a
// hard link to the token in that window; path confinement cannot catch it,
// because the alias is a real file inside the root under an ordinary name.
// Only a decision bound to the opened handle can, so the seam below performs
// the swap from inside the pathname check itself — deterministically, with no
// scheduling assumptions.
func TestGrepDoesNotScanTokenAliasSwappedInAfterExclusion(t *testing.T) {
	ws, token, _ := daemonTokenFixture(t)
	candidate := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(candidate, []byte("ordinary notes\n"), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	base := sandboxReadExcluderWithin(nil, ws)
	if !base.fileExcluded(token) {
		t.Fatal("fixture must protect the token by pathname")
	}
	swapped := false
	exclude := base
	exclude.file = func(path string) bool {
		if !swapped && path == candidate {
			swapped = true
			if err := os.Remove(candidate); err != nil {
				t.Fatalf("remove candidate: %v", err)
			}
			if err := os.Link(token, candidate); err != nil {
				t.Skipf("workspace filesystem is not hard-linkable: %v", err)
			}
			// Cleared as an ordinary file, exactly as it was when visited.
			return false
		}
		return base.fileExcluded(path)
	}

	var matches []grepMatch
	err := scanGrepMatches(context.Background(), ws, ws, nil, exclude, false,
		presenceGrepLineMatcher(regexp.MustCompile("bridge-secret")),
		func(match grepMatch) bool {
			matches = append(matches, match)
			return true
		})
	if err != nil {
		t.Fatalf("grep walk: %v", err)
	}
	if !swapped {
		t.Fatal("the swap seam never ran, so the window was never exercised")
	}
	for _, match := range matches {
		if strings.Contains(match.file, "notes.txt") || strings.Contains(match.file, "bridge-token") {
			t.Fatalf("grep scanned the token alias swapped in after the exclusion: %+v", match)
		}
	}
	// The ordinary workspace file must still be searched; the handle check may
	// only ever remove the protected object from the results.
	found := false
	for _, match := range matches {
		if strings.Contains(match.file, "main.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("handle check dropped ordinary matches: %+v", matches)
	}
}
