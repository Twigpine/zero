package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/daemon/remote"
)

func TestDaemonTokenNavigationAndImageMatrix(t *testing.T) {
	for _, kind := range []string{"exact", "symlink", "hardlink", "ordinary"} {
		t.Run(kind, func(t *testing.T) {
			ws, token, engine := daemonTokenFixture(t)
			t.Setenv(remote.EnvTokenFileResolved, "")
			t.Setenv(remote.EnvTokenFileIdentity, "")
			// Valid image bytes ensure refusal cannot be an image-type error.
			if err := os.WriteFile(token, onePixelPNG, 0o600); err != nil {
				t.Fatal(err)
			}
			path := token
			if kind != "exact" {
				path = filepath.Join(ws, "candidate.unknownext")
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(token, path)
				case "hardlink":
					err = os.Link(token, path)
				case "ordinary":
					err = os.WriteFile(path, onePixelPNG, 0o600)
				}
				if err != nil {
					t.Skipf("%s unavailable: %v", kind, err)
				}
			}
			registry := NewRegistry()
			registry.Register(NewScopedLSPNavigateTool(ws, nil))
			registry.Register(NewViewImageTool(ws))
			for _, withEngine := range []bool{false, true} {
				options := RunOptions{}
				if withEngine {
					options.Sandbox = engine
				}
				for _, op := range []string{"definition", "workspace_symbol", "image"} {
					name := "lsp_navigate"
					args := map[string]any{"path": path, "op": op, "line": 1, "query": "x"}
					if op == "image" {
						name, args = ViewImageToolName, map[string]any{"path": path}
					}
					result := registry.RunWithOptions(context.Background(), name, args, options)
					if kind == "ordinary" {
						if result.Status != StatusOK {
							t.Fatalf("%s engine=%v ordinary control: %s", op, withEngine, result.Output)
						}
						if op == "image" && (len(result.Images) != 1 || !bytes.Equal(result.Images[0].Data, onePixelPNG)) {
							t.Fatal("ordinary image bytes changed")
						}
					} else if result.Status != StatusError || len(result.Images) != 0 || !strings.Contains(result.Output, "holds the remote bridge token") {
						t.Errorf("%s engine=%v must refuse credential before consumption: %+v", op, withEngine, result)
					}
				}
			}
		})
	}
}

// The gate and the tool must resolve a path argument to the SAME bytes.
// aliasedStringArg does not trim, so while requestPaths ran path args through
// argString (strings.TrimSpace), a token file whose name carries meaningful
// whitespace was protected under its real spelling while the gate inspected the
// trimmed one — read_file opened "bridge-token " after the gate cleared
// "bridge-token". This runs the production path (registry.RunWithOptions with
// the sandbox engine), not a profile builder, because that divergence is
// invisible to a test that calls the gate directly.
func TestEngineDeniesReadFileWithExactSpacedTokenPath(t *testing.T) {
	for _, tokenName := range []string{"bridge-token ", " bridge-token", "bridge token "} {
		t.Run(strings.ReplaceAll(tokenName, " ", "_"), func(t *testing.T) {
			ws, _, engine := daemonTokenFixtureNamed(t, tokenName)

			registry := NewRegistry()
			registry.Register(NewScopedReadFileTool(ws, nil))

			// Send the RELATIVE spelling. The whitespace has to sit at the
			// boundary of the argument string for TrimSpace to reach it: in an
			// absolute path the space is mid-string (after the separator) and
			// the old gate happened to behave. That is exactly why this needs
			// to be a named regression rather than a variant of the existing
			// absolute-path coverage.
			result := registry.RunWithOptions(context.Background(), "read_file",
				map[string]any{"path": tokenName}, RunOptions{Sandbox: engine})
			if result.Status == StatusOK {
				t.Fatalf("read_file served the protected token under its exact spelling: output=%q", result.Output)
			}
			if strings.Contains(result.Output, "bridge-secret") {
				t.Fatalf("denied read still leaked the bearer token: output=%q", result.Output)
			}

			// The trimmed spelling must stay denied too: it is the same
			// credential identity, and the gate is now a superset of what it
			// inspected before this became exact.
			trimmed := strings.TrimSpace(tokenName)
			trimmedResult := registry.RunWithOptions(context.Background(), "read_file",
				map[string]any{"path": trimmed}, RunOptions{Sandbox: engine})
			if strings.Contains(trimmedResult.Output, "bridge-secret") {
				t.Fatalf("trimmed spelling leaked the bearer token: output=%q", trimmedResult.Output)
			}
		})
	}
}

// The Step C matrix: every tool that can name a path, crossed with the
// spellings an attacker controls. Earlier rounds on this branch each closed one
// cell — shell, then read_file, then apply_patch headers, then whitespace — so
// the point here is to run them all through the production entrypoint at once
// and make a future gap fail as a missing row rather than as a new report.
func TestDaemonTokenProtectionMatrix(t *testing.T) {
	// Each column is a spelling of the SAME protected credential.
	spellings := []struct {
		name  string
		token string // token filename on disk
		arg   func(ws, token string) string
	}{
		{
			name:  "exact",
			token: "bridge-token",
			arg:   func(_, token string) string { return token },
		},
		{
			name:  "trailing space",
			token: "bridge-token ",
			arg:   func(_, token string) string { return token },
		},
		{
			name:  "relative",
			token: "bridge-token",
			arg:   func(_, token string) string { return filepath.Base(token) },
		},
		{
			name:  "dot segment",
			token: "bridge-token",
			arg: func(ws, token string) string {
				return filepath.Join(ws, ".", filepath.Base(token))
			},
		},
		{
			name:  "parent traversal",
			token: "bridge-token",
			arg: func(ws, token string) string {
				return filepath.Join(ws, "sub", "..", filepath.Base(token))
			},
		},
	}

	for _, spelling := range spellings {
		t.Run(spelling.name, func(t *testing.T) {
			t.Run("read_file", func(t *testing.T) {
				ws, token, engine := daemonTokenFixtureNamed(t, spelling.token)
				registry := NewRegistry()
				registry.Register(NewScopedReadFileTool(ws, nil))
				result := registry.RunWithOptions(context.Background(), "read_file",
					map[string]any{"path": spelling.arg(ws, token)}, RunOptions{Sandbox: engine})
				assertTokenNotLeaked(t, "read_file", result)
			})

			t.Run("write_file", func(t *testing.T) {
				ws, token, engine := daemonTokenFixtureNamed(t, spelling.token)
				registry := NewRegistry()
				registry.Register(NewScopedWriteFileTool(ws, nil))
				result := registry.RunWithOptions(context.Background(), "write_file",
					map[string]any{"path": spelling.arg(ws, token), "content": "attacker\n"},
					RunOptions{Sandbox: engine, PermissionGranted: true})
				if result.Status == StatusOK {
					t.Fatalf("write_file overwrote the protected token: output=%q", result.Output)
				}
				if !strings.Contains(result.Output, "holds the remote bridge token") {
					t.Fatalf("write_file refusal did not come from the credential gate: output=%q", result.Output)
				}
				// A refused write must leave the bearer intact, not truncate it.
				contents, err := os.ReadFile(token)
				if err != nil || string(contents) != "bridge-secret\n" {
					t.Fatalf("token changed after a denied write: contents=%q err=%v", contents, err)
				}
			})

			t.Run("list_directory", func(t *testing.T) {
				ws, _, engine := daemonTokenFixtureNamed(t, spelling.token)
				registry := NewRegistry()
				registry.Register(NewScopedListDirectoryTool(ws, nil))
				result := registry.RunWithOptions(context.Background(), "list_directory",
					map[string]any{"path": ws}, RunOptions{Sandbox: engine})
				if strings.Contains(result.Output, "bridge-token") {
					t.Fatalf("list_directory surfaced the protected token filename:\n%s", result.Output)
				}
				if !strings.Contains(result.Output, "main.go") {
					t.Fatalf("list_directory dropped ordinary entries while filtering:\n%s", result.Output)
				}
			})

			t.Run("grep", func(t *testing.T) {
				ws, _, engine := daemonTokenFixtureNamed(t, spelling.token)
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
				if strings.Contains(result.Output, "bridge-token") {
					t.Fatalf("grep surfaced the protected token:\n%s", result.Output)
				}
				if !strings.Contains(result.Output, "main.go") {
					t.Fatalf("grep dropped ordinary matches while filtering:\n%s", result.Output)
				}
			})

			t.Run("apply_patch", func(t *testing.T) {
				ws, token, engine := daemonTokenFixtureNamed(t, spelling.token)
				registry := NewRegistry()
				registry.Register(NewScopedApplyPatchTool(ws, nil))
				target := spelling.arg(ws, token)
				oldPath := strconv.Quote("a/" + filepath.ToSlash(target))
				newPath := strconv.Quote("b/" + filepath.ToSlash(target))
				patch := "--- " + oldPath + "\n+++ " + newPath +
					"\n@@ -1 +1 @@\n-bridge-secret\n+attacker\n"
				result := registry.RunWithOptions(context.Background(), "apply_patch",
					map[string]any{"patch": patch},
					RunOptions{Sandbox: engine, PermissionGranted: true})
				if result.Status == StatusOK {
					t.Fatalf("apply_patch rewrote the protected token: output=%q", result.Output)
				}
				contents, err := os.ReadFile(token)
				if err != nil || string(contents) != "bridge-secret\n" {
					t.Fatalf("token changed after a denied patch: contents=%q err=%v", contents, err)
				}
				prepared, err := prepareApplyPatchArguments(map[string]any{"patch": patch})
				if err != nil {
					t.Fatalf("prepareApplyPatchArguments: %v", err)
				}
				if len(prepared.paths) != 1 || prepared.paths[0] != filepath.ToSlash(target) {
					t.Fatalf("executor paths = %q, want exact target spelling", prepared.paths)
				}
				// Every spelling — absolute included — must be refused by the
				// credential gate itself. An absolute path inside the workspace
				// is legitimate for an ordinary target, so the blanket
				// absolute-path rejection cannot be what protects the token.
				if !strings.Contains(result.Output, "holds the remote bridge token") {
					t.Fatalf("apply_patch refusal did not come from the credential gate: output=%q", result.Output)
				}
			})
		})
	}
}

// registry.Run reaches list_directory without a sandbox engine (MCP, legacy
// callers). The protected-credential set does not come from a policy, so the
// bearer filename must not become visible just because no engine was passed.
func TestListDirectoryWithoutEngineStillHidesProtectedToken(t *testing.T) {
	ws, _, _ := daemonTokenFixture(t)

	registry := NewRegistry()
	registry.Register(NewScopedListDirectoryTool(ws, nil))
	result := registry.Run(context.Background(), "list_directory", map[string]any{"path": ws})

	if strings.Contains(result.Output, "bridge-token") {
		t.Fatalf("engine-less list_directory disclosed the protected token filename:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "main.go") {
		t.Fatalf("engine-less list_directory dropped ordinary entries while filtering:\n%s", result.Output)
	}
}

func assertTokenNotLeaked(t *testing.T, tool string, result Result) {
	t.Helper()
	if result.Status == StatusOK {
		t.Fatalf("%s served the protected token: output=%q", tool, result.Output)
	}
	if strings.Contains(result.Output, "bridge-secret") {
		t.Fatalf("%s leaked the bearer token in a denial: output=%q", tool, result.Output)
	}
}
